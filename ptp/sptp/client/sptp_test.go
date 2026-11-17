/*
Copyright (c) Facebook, Inc. and its affiliates.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"context"
	"fmt"
	"github.com/facebook/time/timestamp"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	ptp "github.com/facebook/time/ptp/protocol"
	gmstats "github.com/facebook/time/ptp/sptp/stats"
	"github.com/facebook/time/servo"

	"go.uber.org/mock/gomock"

	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func init() {
	log.SetLevel(log.DebugLevel)
}

const defaultTestTimeout = time.Second

func TestProcessResultsNoResults(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockStatsServer := NewMockStatsServer(ctrl)
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		eventConns: []UDPConnWithTS{nil},
	}
	results := map[netip.Addr]*RunResult{}
	mockServo.EXPECT().GetState().Return(servo.StateLocked)
	mockServo.EXPECT().MeanFreq()
	mockServo.EXPECT().SetLastFreq(float64(0))
	mockClock.EXPECT().AdjFreqPPB(gomock.Any())
	mockStatsServer.EXPECT().SetGmsTotal(0)
	mockStatsServer.EXPECT().SetGmsAvailable(0)
	mockStatsServer.EXPECT().SetServoState(int(servo.StateHoldover))
	_ = p.processResults(results)

	require.Equal(t, netip.Addr{}, p.bestGM)
}

func TestProcessResultsEmptyResult(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockStatsServer := NewMockStatsServer(ctrl)
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)

	cfg := DefaultConfig()
	cfg.Iface = "lo"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
	}
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	err := p.initClients()
	require.NoError(t, err)
	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {},
	}
	meanFreq := 10.2
	mockServo.EXPECT().GetState().Return(servo.StateLocked)
	mockServo.EXPECT().MeanFreq().Return(meanFreq)
	mockServo.EXPECT().SetLastFreq(float64(10.2))
	mockClock.EXPECT().AdjFreqPPB(-1 * meanFreq)
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(0)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetServoState(int(servo.StateHoldover))
	err = p.processResults(results)
	// as we no interface down error, we expect no error
	require.NoError(t, err)
	require.Equal(t, netip.Addr{}, p.bestGM)
}

func TestProcessResultsNoGMPublishesServoState(t *testing.T) {
	testCases := []struct {
		name      string
		servoWas  servo.State
		wantState servo.State
	}{
		{name: "disciplined then lost every GM", servoWas: servo.StateLocked, wantState: servo.StateHoldover},
		{name: "never locked", servoWas: servo.StateInit, wantState: servo.StateInit},
		{name: "one sample in", servoWas: servo.StateJump, wantState: servo.StateInit},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockClock := NewMockClock(ctrl)
			mockServo := NewMockServo(ctrl)
			mockServo.EXPECT().GetState().Return(tc.servoWas)
			mockServo.EXPECT().MeanFreq()
			mockServo.EXPECT().SetLastFreq(float64(0))
			mockClock.EXPECT().AdjFreqPPB(gomock.Any())

			statsServer, err := NewStats()
			require.NoError(t, err)
			statsServer.SetServoState(int(servo.StateLocked))

			p := &SPTP{
				clock:      mockClock,
				pi:         mockServo,
				stats:      statsServer,
				eventConns: []UDPConnWithTS{nil},
			}
			require.NoError(t, p.processResults(map[netip.Addr]*RunResult{}))

			require.Equal(t, netip.Addr{}, p.bestGM)
			require.Equal(t, int64(tc.wantState), statsServer.GetCounters()["ptp.sptp.servo.state"])
		})
	}
}

func TestProcessResultsSingle(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2021-05-21T13:32:05+01:00")
	require.Nil(t, err)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockClock.EXPECT().AdjFreqPPB(gomock.Any()).Return(nil)
	mockClock.EXPECT().Step(gomock.Any()).Return(nil)
	mockClock.EXPECT().SetSync()
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().IsSpike(int64(-200002000)).Return(false)
	mockServo.EXPECT().Sample(int64(-200002000), gomock.Any()).Return(12.3, servo.StateJump)
	mockServo.EXPECT().IsSpike(int64(-100001000)).Return(false)
	mockServo.EXPECT().Sample(int64(-100001000), gomock.Any()).Return(14.2, servo.StateLocked)
	mockServo.EXPECT().UnsetFirstUpdate()
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetServoState(gomock.Any()).MinTimes(1)
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)

	cfg := DefaultConfig()
	cfg.Iface = "lo"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
	}
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {
			Server: netip.MustParseAddr("192.168.0.10"),
			Measurement: &MeasurementResult{
				Delay:     299995 * time.Microsecond,
				S2CDelay:  100,
				C2SDelay:  110,
				Offset:    -200002 * time.Microsecond,
				Timestamp: ts,
			},
		},
	}
	err = p.initClients()
	require.NoError(t, err)
	// we step here
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)

	// we have to wait a second to avoid "samples are too fast" error
	time.Sleep(time.Second)
	results[netip.MustParseAddr("192.168.0.10")].Measurement.Offset = -100001 * time.Microsecond
	// we adj here
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetTickDuration(gomock.Any())
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)
}

func TestProcessResultsFastSamples(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2021-05-21T13:32:05+01:00")
	require.Nil(t, err)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockClock.EXPECT().AdjFreqPPB(gomock.Any()).Return(nil)
	mockClock.EXPECT().Step(gomock.Any()).Return(nil)
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().IsSpike(int64(-200002000)).Return(false)
	mockServo.EXPECT().IsSpike(int64(-200002000)).Return(false)
	mockServo.EXPECT().Sample(int64(-200002000), gomock.Any()).Return(12.3, servo.StateJump)
	mockServo.EXPECT().MeanFreq().Return(12.3)
	mockServo.EXPECT().SetLastFreq(float64(12.3))
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetServoState(gomock.Any()).MinTimes(1)
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)

	cfg := DefaultConfig()
	cfg.Iface = "lo"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
	}
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {
			Server: netip.MustParseAddr("192.168.0.10"),
			Measurement: &MeasurementResult{
				Delay:     299995 * time.Microsecond,
				S2CDelay:  100,
				C2SDelay:  110,
				Offset:    -200002 * time.Microsecond,
				Timestamp: ts,
			},
		},
	}
	err = p.initClients()
	require.NoError(t, err)
	// we step here
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)

	// we stick to holdover mode and mean freq
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetTickDuration(gomock.Any())
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)
}

func TestProcessResultsMulti(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2021-05-21T13:32:05+01:00")
	require.Nil(t, err)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockClock.EXPECT().AdjFreqPPB(gomock.Any()).Return(nil)
	mockClock.EXPECT().Step(gomock.Any()).Return(nil)
	mockClock.EXPECT().SetSync()
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().IsSpike(int64(-200002000)).Return(false)
	mockServo.EXPECT().Sample(int64(-200002000), gomock.Any()).Return(12.3, servo.StateJump)
	mockServo.EXPECT().IsSpike(int64(-104002000)).Return(false)
	mockServo.EXPECT().Sample(int64(-104002000), gomock.Any()).Return(14.2, servo.StateLocked)
	mockServo.EXPECT().UnsetFirstUpdate()
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(2)
	mockStatsServer.EXPECT().SetGmsAvailable(50)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetServoState(gomock.Any()).MinTimes(1)
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)

	cfg := DefaultConfig()
	cfg.Iface = "lo"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
		"192.168.0.11": 1,
	}
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	err = p.initClients()
	require.NoError(t, err)
	announce0 := announcePkt(0)
	announce0.GrandmasterIdentity = ptp.ClockIdentity(0x001)
	announce0.GrandmasterPriority2 = 2
	announce1 := announcePkt(1)
	announce1.GrandmasterIdentity = ptp.ClockIdentity(0x042)
	announce1.GrandmasterPriority2 = 1
	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {
			Server: netip.MustParseAddr("192.168.0.10"),
			Measurement: &MeasurementResult{
				Delay:     299995 * time.Microsecond,
				S2CDelay:  100,
				C2SDelay:  110,
				Offset:    -200002 * time.Microsecond,
				Timestamp: ts,
				Announce:  *announce0,
			},
		},
		netip.MustParseAddr("192.168.0.11"): {
			Server: netip.MustParseAddr("192.168.0.11"),
			Error:  fmt.Errorf("context deadline exceeded"),
		},
	}
	// we step here
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)

	// we have to wait a second to avoid "samples are too fast" error
	time.Sleep(time.Second)

	results[netip.MustParseAddr("192.168.0.10")].Measurement.Offset = -100001 * time.Microsecond
	results[netip.MustParseAddr("192.168.0.11")].Error = nil
	results[netip.MustParseAddr("192.168.0.11")].Measurement = &MeasurementResult{
		Delay:     299995 * time.Microsecond,
		S2CDelay:  90,
		C2SDelay:  120,
		Offset:    -104002 * time.Microsecond,
		Timestamp: ts,
		Announce:  *announce1,
	}
	// we adj here, while also switching to new best GM
	mockStatsServer.EXPECT().SetGmsTotal(2)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetTickDuration(gomock.Any())
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.11"), p.bestGM)
}

func TestProcessResultsFilteredDelay(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().GetState().Return(servo.StateLocked)
	mockServo.EXPECT().MeanFreq().Times(1)
	mockServo.EXPECT().SetLastFreq(float64(0)).Times(1)
	mockClock.EXPECT().AdjFreqPPB(gomock.Any())
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().IncFiltered()
	mockStatsServer.EXPECT().SetServoState(int(servo.StateHoldover))
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)

	cfg := DefaultConfig()
	cfg.Iface = "lo"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
	}
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	err := p.initClients()
	require.NoError(t, err)
	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {
			Server: netip.MustParseAddr("192.168.0.10"),
			Measurement: &MeasurementResult{
				BadDelay:          true,
				CorrectionFieldRX: -42,
			},
		},
	}
	// we step here
	_ = p.processResults(results)
	require.Equal(t, netip.Addr{}, p.bestGM)
}

func TestRunInternalAllDead(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(4)
	mockClock := NewMockClock(ctrl)
	mockClock.EXPECT().AdjFreqPPB((float64(0))).Times(3)
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().SyncInterval(float64(1))
	mockServo.EXPECT().GetState().Return(servo.StateLocked).Times(2)
	mockServo.EXPECT().MeanFreq().Times(3)
	mockServo.EXPECT().SetLastFreq(float64(0)).Times(2)
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(2).Times(2)
	mockStatsServer.EXPECT().SetGmsAvailable(0).Times(2)
	mockStatsServer.EXPECT().SetTickDuration(gomock.Any())
	mockStatsServer.EXPECT().IncTXDelayReq().Times(4)
	mockStatsServer.EXPECT().SetGMStats(&gmstats.Stat{GMAddress: "192.168.0.10", Error: context.DeadlineExceeded.Error(), Priority3: 1, SearchState: unknownWire()}).Times(2)
	mockStatsServer.EXPECT().SetGMStats(&gmstats.Stat{GMAddress: "192.168.0.11", Error: context.DeadlineExceeded.Error(), Priority3: 2, SearchState: unknownWire()}).Times(2)
	mockStatsServer.EXPECT().SetServoState(int(servo.StateHoldover)).Times(2)

	p := &SPTP{
		clock: mockClock,
		pi:    mockServo,
		stats: mockStatsServer,
		cfg: &Config{
			Iface:    "lo",
			Interval: time.Second,
			Servers: map[string]int{
				"192.168.0.10": 1,
				"192.168.0.11": 2,
			},
			Measurement: MeasurementConfig{
				PathDelayFilterLength:         59,
				PathDelayFilter:               "median",
				PathDelayDiscardFilterEnabled: true,
				PathDelayDiscardBelow:         2 * time.Microsecond,
			},
		},
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	err := p.initClients()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err = p.runInternal(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRunInternalBackoffNamesFailureCause(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	// A watchdog, not a schedule: the loop stops on the first skipped tick below.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	firstTick := &gmstats.Stat{
		GMAddress:   "192.168.0.10",
		Error:       context.DeadlineExceeded.Error(),
		Priority3:   1,
		SearchState: unknownWire(),
	}
	skippedTick := &gmstats.Stat{
		GMAddress:   "192.168.0.10",
		Error:       "backoff for faulty GM: context deadline exceeded",
		Priority3:   1,
		SearchState: unknownWire(),
	}

	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any())
	mockClock := NewMockClock(ctrl)
	mockClock.EXPECT().AdjFreqPPB((float64(0))).MinTimes(3)
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().SyncInterval(float64(1))
	mockServo.EXPECT().GetState().Return(servo.StateLocked).MinTimes(2)
	mockServo.EXPECT().MeanFreq().MinTimes(3)
	mockServo.EXPECT().SetLastFreq(float64(0)).MinTimes(2)
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(1).MinTimes(2)
	mockStatsServer.EXPECT().SetGmsAvailable(0).MinTimes(2)
	mockStatsServer.EXPECT().SetTickDuration(gomock.Any()).MinTimes(1)
	mockStatsServer.EXPECT().IncTXDelayReq()
	mockStatsServer.EXPECT().SetGMStats(firstTick)
	mockStatsServer.EXPECT().SetGMStats(skippedTick).MinTimes(1).Do(func(*gmstats.Stat) { cancel() })
	mockStatsServer.EXPECT().SetServoState(int(servo.StateHoldover)).MinTimes(2)

	p := &SPTP{
		clock: mockClock,
		pi:    mockServo,
		stats: mockStatsServer,
		cfg: &Config{
			Iface:    "lo",
			Interval: time.Second,
			Servers: map[string]int{
				"192.168.0.10": 1,
			},
			Measurement: MeasurementConfig{
				PathDelayFilterLength:         59,
				PathDelayFilter:               "median",
				PathDelayDiscardFilterEnabled: true,
				PathDelayDiscardBelow:         2 * time.Microsecond,
			},
			// cfgen's production shape: tick 1 arms a 10s backoff, so tick 2 is skipped
			Backoff:         BackoffConfig{Mode: backoffLinear, Step: 10, MaxValue: 60},
			ExchangeTimeout: 100 * time.Millisecond,
		},
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	err := p.initClients()
	require.NoError(t, err)
	err = p.runInternal(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRunFiltered(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2021-05-21T13:32:05+01:00")
	require.Nil(t, err)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().IsSpike(int64(-200002000)).Return(false)
	mockServo.EXPECT().Sample(int64(-200002000), gomock.Any()).Return(12.3, servo.StateFilter)
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetServoState(gomock.Any()).MinTimes(1)
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)

	cfg := DefaultConfig()
	cfg.Iface = "lo"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
	}
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {
			Server: netip.MustParseAddr("192.168.0.10"),
			Measurement: &MeasurementResult{
				Delay:     299995 * time.Microsecond,
				S2CDelay:  100,
				C2SDelay:  110,
				Offset:    -200002 * time.Microsecond,
				Timestamp: ts,
			},
		},
	}
	err = p.initClients()
	require.NoError(t, err)
	// we step here
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)
	require.Nil(t, nil, results[netip.MustParseAddr("192.168.0.10")])
}

func TestProcessResultsSpikePublishesSelectedGMWithoutTheSpike(t *testing.T) {
	testCases := []struct {
		name           string
		servoState     servo.State
		wantServoState servo.State
	}{
		{name: "servo locked", servoState: servo.StateLocked, wantServoState: servo.StateFilter},
		{name: "servo not locked", servoState: servo.StateInit, wantServoState: servo.StateInit},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := time.Parse(time.RFC3339, "2021-05-21T13:32:05+01:00")
			require.NoError(t, err)
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockClock := NewMockClock(ctrl)
			mockClock.EXPECT().AdjFreqPPB(float64(0))
			mockServo := NewMockServo(ctrl)
			mockServo.EXPECT().IsSpike(int64(-200002000)).Return(true)
			mockServo.EXPECT().GetState().Return(tc.servoState)
			mockServo.EXPECT().MeanFreq()
			mockServo.EXPECT().SetLastFreq(float64(0))
			mockEventConn := NewMockUDPConnWithTS(ctrl)
			mockEventConn.EXPECT().ConnFd().Return(0)

			statsServer, err := NewStats()
			require.NoError(t, err)

			cfg := DefaultConfig()
			cfg.Iface = "lo"
			cfg.Servers = map[string]int{
				"192.168.0.10": 1,
			}
			p := &SPTP{
				clock:      mockClock,
				pi:         mockServo,
				stats:      statsServer,
				cfg:        cfg,
				eventConns: []UDPConnWithTS{mockEventConn},
			}
			require.NoError(t, p.initClients())

			announce := announcePkt(0)
			announce.GrandmasterIdentity = ptp.ClockIdentity(0x001)
			announce.GrandmasterClockQuality = ptp.ClockQuality{
				ClockClass:    ptp.ClockClass6,
				ClockAccuracy: ptp.ClockAccuracyMicrosecond10,
			}
			results := map[netip.Addr]*RunResult{
				netip.MustParseAddr("192.168.0.10"): {
					Server: netip.MustParseAddr("192.168.0.10"),
					Measurement: &MeasurementResult{
						Delay:     299995 * time.Microsecond,
						S2CDelay:  100,
						C2SDelay:  110,
						Offset:    -200002 * time.Microsecond,
						Timestamp: ts,
						Announce:  *announce,
					},
				},
			}
			require.NoError(t, p.processResults(results))
			require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)

			published := statsServer.GetGMStats()
			require.Len(t, published, 1)

			var selected *gmstats.Stat
			for _, s := range published {
				if s.Selected {
					selected = s
					break
				}
			}
			require.NotNil(t, selected, "a spike-filtered tick must still name the grandmaster sptp steers to")
			require.Equal(t, int(tc.wantServoState), selected.ServoState)
			require.Zero(t, selected.GMPresent)
			require.Zero(t, selected.Offset)
			require.Zero(t, selected.IngressTime)
			require.Equal(t, "Measurement is missing on RunResult", selected.Error)
		})
	}
}

// Missed Broadcom 48bit wraps leave the PHC whole wraps ahead or behind.
func TestProcessResultsBroadcomWrapStepsEitherDirection(t *testing.T) {
	wrap := time.Duration(1 << 48)
	testCases := []struct {
		name     string
		offset   time.Duration
		wantStep bool
	}{
		{name: "behind by a wrap", offset: -wrap, wantStep: true},
		{name: "ahead by three wraps", offset: 3 * wrap, wantStep: true},
		{name: "spike under a wrap is filtered", offset: 76 * time.Hour, wantStep: false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := time.Parse(time.RFC3339, "2021-05-21T13:32:05+01:00")
			require.NoError(t, err)
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockClock := NewMockClock(ctrl)
			mockClock.EXPECT().AdjFreqPPB(float64(0))
			mockServo := NewMockServo(ctrl)
			mockServo.EXPECT().IsSpike(tc.offset.Nanoseconds()).Return(true)
			mockServo.EXPECT().MeanFreq()
			mockServo.EXPECT().SetLastFreq(float64(0))
			if tc.wantStep {
				gomock.InOrder(
					mockClock.EXPECT().Time().Return(ts, nil),
					mockClock.EXPECT().SetTime(ts.Add(-tc.offset)).Return(nil),
					mockClock.EXPECT().Time().Return(ts.Add(-tc.offset), nil),
				)
			} else {
				mockServo.EXPECT().GetState().Return(servo.StateLocked)
			}
			mockEventConn := NewMockUDPConnWithTS(ctrl)
			mockEventConn.EXPECT().ConnFd().Return(0)

			statsServer, err := NewStats()
			require.NoError(t, err)

			cfg := DefaultConfig()
			cfg.Iface = "lo"
			cfg.Servers = map[string]int{
				"192.168.0.10": 1,
			}
			p := &SPTP{
				clock:      mockClock,
				pi:         mockServo,
				stats:      statsServer,
				cfg:        cfg,
				eventConns: []UDPConnWithTS{mockEventConn},
			}
			require.NoError(t, p.initClients())

			announce := announcePkt(0)
			announce.GrandmasterIdentity = ptp.ClockIdentity(0x001)
			announce.GrandmasterClockQuality = ptp.ClockQuality{
				ClockClass:    ptp.ClockClass6,
				ClockAccuracy: ptp.ClockAccuracyMicrosecond10,
			}
			results := map[netip.Addr]*RunResult{
				netip.MustParseAddr("192.168.0.10"): {
					Server: netip.MustParseAddr("192.168.0.10"),
					Measurement: &MeasurementResult{
						Delay:     299995 * time.Microsecond,
						S2CDelay:  100,
						C2SDelay:  110,
						Offset:    tc.offset,
						Timestamp: ts,
						Announce:  *announce,
					},
				},
			}
			require.NoError(t, p.processResults(results))
		})
	}
}

func TestRunStalled(t *testing.T) {
	ts, err := time.Parse(time.RFC3339, "2021-05-21T13:32:05+01:00")
	require.Nil(t, err)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockClock.EXPECT().AdjFreqPPB((float64(0))).Times(2)
	mockClock.EXPECT().Step(time.Duration(200002000))
	mockServo := NewMockServo(ctrl)
	mockServo.EXPECT().IsSpike(int64(-200002000)).Return(true)
	mockServo.EXPECT().GetState().Return(servo.StateLocked)
	mockServo.EXPECT().MeanFreq().Times(2)
	mockServo.EXPECT().SetLastFreq(float64(0)).Times(2)

	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().SetGmsTotal(0)
	mockStatsServer.EXPECT().SetGmsAvailable(0)
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(100)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	gomock.InOrder(
		mockStatsServer.EXPECT().SetServoState(int(servo.StateHoldover)),
		mockStatsServer.EXPECT().SetServoState(int(servo.StateJump)),
	)
	mockStatsServer.EXPECT().SetTickDuration(gomock.Any()).Times(2)
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)

	cfg := DefaultConfig()
	cfg.Iface = "lo"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
	}
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{mockEventConn},
		lastTick:   time.Now().Add(-time.Minute * 2),
	}
	emptyResults := map[netip.Addr]*RunResult{}

	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {
			Server: netip.MustParseAddr("192.168.0.10"),
			Measurement: &MeasurementResult{
				Delay:     299995 * time.Microsecond,
				S2CDelay:  100,
				C2SDelay:  110,
				Offset:    -200002 * time.Microsecond,
				Timestamp: ts,
			},
		},
	}
	err = p.initClients()
	require.NoError(t, err)
	_ = p.processResults(emptyResults)
	require.True(t, p.isStalled)
	p.lastTick = time.Now().Add(-time.Second)
	// we step here
	_ = p.processResults(results)
	require.Equal(t, netip.MustParseAddr("192.168.0.10"), p.bestGM)
	require.Nil(t, nil, results[netip.MustParseAddr("192.168.0.10")])
	require.False(t, p.isStalled)
}

func TestRunListenerErr(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)
	mockEventConn.EXPECT().ReadPacketBuf(gomock.Any(), gomock.Any()).AnyTimes().Return(0, 0, &unix.SockaddrInet6{}, fmt.Errorf("oops"))
	mockEventConn.EXPECT().RXTimestamp(gomock.Any(), gomock.Any()).Return(time.Now(), nil).AnyTimes()
	mockGenConn := NewMockUDPConnNoTS(ctrl)
	mockGenConn.EXPECT().ReadPacketBuf(gomock.Any()).AnyTimes()
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockStatsServer := NewMockStatsServer(ctrl)

	p := &SPTP{
		clock: mockClock,
		pi:    mockServo,
		stats: mockStatsServer,
		cfg: &Config{
			Iface:    "lo",
			Interval: time.Second,
			Servers: map[string]int{
				"192.168.0.10": 1,
				"192.168.0.11": 2,
			},
		},
		eventConns: []UDPConnWithTS{mockEventConn},
		genConn:    mockGenConn,
	}
	err := p.initClients()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	err = p.RunListener(ctx)
	require.Error(t, err)
}

func TestRunListenerError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)
	mockEventConn.EXPECT().ReadPacketBuf(gomock.Any(), gomock.Any()).Return(0, 0, &unix.SockaddrInet6{}, fmt.Errorf("some error")).AnyTimes()
	mockEventConn.EXPECT().RXTimestamp(gomock.Any(), gomock.Any()).Return(time.Now(), nil).AnyTimes()
	mockGenConn := NewMockUDPConnNoTS(ctrl)
	mockGenConn.EXPECT().ReadPacketBuf(gomock.Any()).Return(2, netip.Addr{}, fmt.Errorf("some error")).AnyTimes()
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockStatsServer := NewMockStatsServer(ctrl)

	p := &SPTP{
		clock: mockClock,
		pi:    mockServo,
		stats: mockStatsServer,
		cfg: &Config{
			Iface:    "lo",
			Interval: time.Second,
			Servers: map[string]int{
				"192.168.0.10": 1,
				"192.168.0.11": 2,
			},
		},
		eventConns: []UDPConnWithTS{mockEventConn},
		genConn:    mockGenConn,
	}
	err := p.initClients()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	err = p.RunListener(ctx)
	require.EqualError(t, err, "some error")
}

func TestRunListenerGood(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)
	sentEvent := 0
	sentGen := 0
	syncBytes, _ := ptp.Bytes(&ptp.SyncDelayReq{})
	announceBytes, _ := ptp.Bytes(&ptp.Announce{})

	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
	mockEventConn.EXPECT().RXTimestamp(gomock.Any(), gomock.Any()).Return(time.Now(), nil).AnyTimes()
	mockEventConn.EXPECT().ReadPacketBuf(gomock.Any(), gomock.Any()).DoAndReturn(func(b, _ []byte) (int, int, unix.Sockaddr, error) {
		// limit how many we send, so we don't overwhelm the client. packets from uknown IPs will be discarded
		if sentEvent > 10 {
			time.Sleep(2 * defaultTestTimeout) // idle socket; no busy-spin, ctx cancels the listener
			return 0, 0, &unix.SockaddrInet4{}, nil
		}
		addr := "192.168.0.11"
		if sentEvent%2 == 0 {
			addr = "192.168.0.10"
		}
		var addrBytes [4]byte
		copy(addrBytes[:], net.ParseIP(addr).To4())
		sentEvent++
		clear(b)
		b = syncBytes
		b[0] = 1
		b[1] = 2
		b[2] = 3
		b[3] = 4
		return len(syncBytes), 0, &unix.SockaddrInet4{Addr: addrBytes, Port: 319}, nil
	}).AnyTimes()

	mockGenConn := NewMockUDPConnNoTS(ctrl)
	mockGenConn.EXPECT().ReadPacketBuf(gomock.Any()).DoAndReturn(func(b []byte) (int, netip.Addr, error) {
		// limit how many we send, so we don't overwhelm the client. packets from uknown IPs will be discarded
		if sentGen > 10 {
			time.Sleep(2 * defaultTestTimeout) // idle socket; no busy-spin, ctx cancels the listener
			return 0, netip.MustParseAddr("4.4.4.4"), nil
		}
		sentGen++
		addr := "192.168.0.11"
		if sentGen%2 == 0 {
			addr = "192.168.0.10"
		}
		clear(b)
		b = announceBytes
		b[0] = 9
		b[1] = 3
		b[2] = 7
		b[3] = 4
		return len(announceBytes), netip.MustParseAddr(addr), nil
	}).AnyTimes()

	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockStatsServer := NewMockStatsServer(ctrl)
	mockStatsServer.EXPECT().IncRXAnnounce().Times(11)
	mockStatsServer.EXPECT().IncRXSync().Times(11)

	p := &SPTP{
		clock: mockClock,
		pi:    mockServo,
		stats: mockStatsServer,
		cfg: &Config{
			Iface:    "lo",
			Interval: time.Second,
			Servers: map[string]int{
				"192.168.0.10": 1,
				"192.168.0.11": 2,
			},
		},
		eventConns: []UDPConnWithTS{mockEventConn},
		genConn:    mockGenConn,
	}
	err := p.initClients()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	err = p.RunListener(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	received10 := 0
	received11 := 0
	nctx, ncancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer ncancel()
LOOP:
	for {
		select {
		case <-nctx.Done():
			break LOOP
		case <-p.clients[netip.MustParseAddr("192.168.0.10")].inChan:
			received10++
		case <-p.clients[netip.MustParseAddr("192.168.0.11")].inChan:
			received11++
		}
	}
	require.Equal(t, 11, sentGen)
	require.Equal(t, 11, sentEvent)
	require.Equal(t, sentGen/2+sentEvent/2+1, received10, "expect to receive N packets to client 192.168.0.10")
	require.Equal(t, sentGen/2+1+sentEvent/2, received11, "expect to receive N packets to client 192.168.0.11")
}

func TestPTPing(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockGenConn := NewMockUDPConnNoTS(ctrl)
	// DELAY_RESP is an event message and is stamped, ANNOUNCE goes out plain
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any())
	mockGenConn.EXPECT().WriteTo(gomock.Any(), gomock.Any())
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockStatsServer := NewMockStatsServer(ctrl)
	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        &Config{Iface: "lo"},
		genConn:    mockGenConn,
		eventConns: []UDPConnWithTS{mockEventConn},
	}

	response := []byte{}
	ip := netip.MustParseAddr("1.2.3.4")

	err := p.ptping(ip, 1234, response, time.Time{})
	require.Equal(t, "failed to read DELAY_REQ not enough data to decode SyncDelayReq", err.Error())

	response, err = ReqDelay(ptp.ClockIdentity(0xbb), 1).MarshalBinary()
	require.Nil(t, err)
	err = p.ptping(ip, 1234, response, time.Time{})
	require.Nil(t, err)
}

// two hosts that don't sync to each other must not keep answering each other's
// answers: one spoofed probe would otherwise start a loop that never ends
func TestPtpingIgnoresReplies(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockGenConn := NewMockUDPConnNoTS(ctrl)
	var reply []byte
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(b []byte, _, _ unix.Sockaddr, _ uint16) (time.Time, error) {
			reply = slices.Clone(b)
			return time.Now(), nil
		})
	mockGenConn.EXPECT().WriteTo(gomock.Any(), gomock.Any())
	p := &SPTP{
		cfg:        &Config{Iface: "lo"},
		genConn:    mockGenConn,
		eventConns: []UDPConnWithTS{mockEventConn},
	}
	peer := netip.MustParseAddr("1.2.3.4")

	probe, err := ReqDelay(ptp.ClockIdentity(0xbb), 1).MarshalBinary()
	require.NoError(t, err)
	require.NoError(t, p.ptping(peer, ptp.PortEvent, probe, time.Now()))

	// what we just sent, as another sptp answering us would send it
	require.ErrorContains(t, p.ptping(peer, ptp.PortEvent, reply, time.Now()), "not a ptping request")

	// a GM we don't sync to answers a DELAY_REQ with a SYNC
	sync := ReqDelay(ptp.ClockIdentity(0xcc), 1)
	sync.SdoIDAndMsgType = ptp.NewSdoIDAndMsgType(ptp.MessageSync, 0)
	sync.OriginTimestamp = ptp.NewTimestamp(time.Now())
	b, err := sync.MarshalBinary()
	require.NoError(t, err)
	require.ErrorContains(t, p.ptping(peer, ptp.PortEvent, b, time.Now()), "not a ptping request")
}

// ptping parses the datagram, not the rest of the receive buffer behind it
func TestRunListenerPtpingShortDatagram(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockEventConn.EXPECT().ConnFd().Return(0)
	var read atomic.Bool
	mockEventConn.EXPECT().ReadPacketBuf(gomock.Any(), gomock.Any()).DoAndReturn(func(b, _ []byte) (int, int, unix.Sockaddr, error) {
		if read.Swap(true) {
			time.Sleep(2 * defaultTestTimeout) // idle socket; ctx cancels the listener
			return 0, 0, &unix.SockaddrInet4{}, nil
		}
		// a lone DELAY_REQ type octet from a host we don't sync to
		b[0] = byte(ptp.MessageDelayReq)
		return 1, 0, &unix.SockaddrInet4{Addr: [4]byte{198, 51, 100, 7}, Port: 4242}, nil
	}).AnyTimes()
	mockEventConn.EXPECT().RXTimestamp(gomock.Any(), gomock.Any()).Return(time.Now(), nil).AnyTimes()
	var sent atomic.Int32
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ []byte, _, _ unix.Sockaddr, _ uint16) (time.Time, error) {
			sent.Add(1)
			return time.Now(), nil
		}).AnyTimes()
	mockGenConn := NewMockUDPConnNoTS(ctrl)
	mockGenConn.EXPECT().ReadPacketBuf(gomock.Any()).DoAndReturn(func(_ []byte) (int, netip.Addr, error) {
		time.Sleep(2 * defaultTestTimeout)
		return 0, netip.MustParseAddr("4.4.4.4"), nil
	}).AnyTimes()
	mockGenConn.EXPECT().WriteTo(gomock.Any(), gomock.Any()).DoAndReturn(func(b []byte, _ unix.Sockaddr) (int, error) {
		sent.Add(1)
		return len(b), nil
	}).AnyTimes()

	p := &SPTP{
		stats: NewMockStatsServer(ctrl),
		cfg: &Config{
			Iface:    "lo",
			Interval: time.Second,
			Servers:  map[string]int{"192.168.0.10": 1},
		},
		eventConns: []UDPConnWithTS{mockEventConn},
		genConn:    mockGenConn,
	}
	require.NoError(t, p.initClients())
	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()
	require.ErrorIs(t, p.RunListener(ctx), context.DeadlineExceeded)
	require.True(t, read.Load())
	require.Zero(t, sent.Load(), "a 1-octet datagram must not be answered")
}

func TestShiftPriorities(t *testing.T) {
	o := netip.MustParseAddr("1.1.1.1")
	l := netip.MustParseAddr("1.1.1.2")
	e := netip.MustParseAddr("1.1.1.3")
	g := netip.MustParseAddr("1.1.1.4")
	p := &SPTP{
		priorities: map[netip.Addr]int{
			o: 1,
			l: 2,
			e: 3,
			g: 4,
		},
	}

	p.reprioritize(l)
	require.Equal(t, 1, p.priorities[l])
	require.Equal(t, 2, p.priorities[e])
	require.Equal(t, 3, p.priorities[g])
	require.Equal(t, 4, p.priorities[o])

	p.reprioritize(o)
	require.Equal(t, 1, p.priorities[o])
	require.Equal(t, 2, p.priorities[l])
	require.Equal(t, 3, p.priorities[e])
	require.Equal(t, 4, p.priorities[g])

	// Shift by 0 (no change)
	p.reprioritize(o)
	require.Equal(t, 1, p.priorities[o])
	require.Equal(t, 2, p.priorities[l])
	require.Equal(t, 3, p.priorities[e])
	require.Equal(t, 4, p.priorities[g])
}

func TestCheckInterfaceDown(t *testing.T) {
	// Test interface that exists and is up (loopback)
	iface, err := net.InterfaceByName("lo")
	require.NoError(t, err)

	// Verify loopback has both required flags
	require.NotEqual(t, 0, iface.Flags&net.FlagUp)
	require.NotEqual(t, 0, iface.Flags&net.FlagRunning)

	// Test checkInterfaceDown with loopback - should succeed
	err = checkInterfaceDown("lo")
	require.NoError(t, err)

	// Test interface that doesn't exist - should fail
	err = checkInterfaceDown("nonexistent_iface_xyz123")
	require.Error(t, err)
	require.Contains(t, err.Error(), "interface not found")
}

func TestProcessResultsInterfaceDown(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClock := NewMockClock(ctrl)
	mockServo := NewMockServo(ctrl)
	mockStatsServer := NewMockStatsServer(ctrl)

	// Use a non-existent interface to trigger the interface-down path
	cfg := DefaultConfig()
	cfg.Iface = "nonexistent_iface_xyz123"
	cfg.Servers = map[string]int{
		"192.168.0.10": 1,
	}

	p := &SPTP{
		clock:      mockClock,
		pi:         mockServo,
		stats:      mockStatsServer,
		cfg:        cfg,
		eventConns: []UDPConnWithTS{nil},
		priorities: map[netip.Addr]int{
			netip.MustParseAddr("192.168.0.10"): 1,
		},
		backoff: map[netip.Addr]*backoff{
			netip.MustParseAddr("192.168.0.10"): newBackoff(BackoffConfig{}),
		},
	}

	// Results with error - this triggers gmsAvailable == 0
	results := map[netip.Addr]*RunResult{
		netip.MustParseAddr("192.168.0.10"): {
			Server: netip.MustParseAddr("192.168.0.10"),
			Error:  fmt.Errorf("connection timeout"),
		},
	}

	meanFreq := 49651.0
	mockStatsServer.EXPECT().SetGmsTotal(1)
	mockStatsServer.EXPECT().SetGmsAvailable(0)
	mockStatsServer.EXPECT().SetGMStats(gomock.Any())
	mockStatsServer.EXPECT().SetServoState(int(servo.StateHoldover))
	mockServo.EXPECT().GetState().Return(servo.StateLocked)
	mockServo.EXPECT().MeanFreq().Return(meanFreq)
	mockServo.EXPECT().SetLastFreq(meanFreq)
	mockClock.EXPECT().AdjFreqPPB(-1 * meanFreq)

	// Interface is down, but processResults should enter holdover instead of returning an error
	err := p.processResults(results)
	require.NoError(t, err)
	require.Equal(t, netip.Addr{}, p.bestGM)
}

func TestHandlePDelayReqParseError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)

	p := &SPTP{
		clockID: ptp.ClockIdentity(0x123456789abcdef0),
	}

	// Invalid buffer - should fail to parse
	invalidBuf := []byte{0x01, 0x02}
	addr := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 319}
	rxts := time.Now()

	err := p.handlePDelayReq(mockEventConn, invalidBuf, addr, rxts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parsing Pdelay_Req")
}

func TestHandlePDelayReqNoTimestamp(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)

	clockID := ptp.ClockIdentity(0x123456789abcdef0)
	p := &SPTP{
		clockID: clockID,
	}

	// Build a valid PDelayReq
	reqClockID := ptp.ClockIdentity(0xfedcba9876543210)
	req := ptp.ReqPDelay(reqClockID, 1, 42)
	reqBytes, err := ptp.Bytes(req)
	require.NoError(t, err)

	addr := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 319}

	// Pass zero timestamp — should be rejected before any response is sent
	err = p.handlePDelayReq(mockEventConn, reqBytes, addr, time.Time{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no receive timestamp")
	// No WriteToWithTS calls expected — mock will fail if any are made
}

func TestHandlePDelayReqSuccess(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockGeneralConn := NewMockUDPConnNoTS(ctrl)

	clockID := ptp.ClockIdentity(0x123456789abcdef0)
	p := &SPTP{
		clockID: clockID,
		genConn: mockGeneralConn,
	}

	// Build a valid PDelayReq
	reqClockID := ptp.ClockIdentity(0xfedcba9876543210)
	req := ptp.ReqPDelay(reqClockID, 1, 42)
	reqBytes, err := ptp.Bytes(req)
	require.NoError(t, err)

	addr := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 319}
	addrGeneral := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 320}
	addrEphemeral := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 31900}
	rxts := time.Now()
	txts := rxts.Add(100 * time.Microsecond)

	// Expect Pdelay_Resp to be sent and return TX timestamp
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), nil, addr, uint16(42)).Return(txts, nil)
	// Expect Pdelay_Resp_Follow_Up to be sent via general msg port
	mockGeneralConn.EXPECT().WriteTo(gomock.Any(), addrGeneral).Return(0, nil)

	err = p.handlePDelayReq(mockEventConn, reqBytes, addr, rxts)
	require.NoError(t, err)

	// Expect Pdelay_Resp to be sent and return TX timestamp
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), nil, addrEphemeral, uint16(42)).Return(txts, nil)
	// Expect Pdelay_Resp_Follow_Up to be sent via general msg port
	mockGeneralConn.EXPECT().WriteTo(gomock.Any(), addrEphemeral).Return(0, nil)

	err = p.handlePDelayReq(mockEventConn, reqBytes, addrEphemeral, rxts)
	require.NoError(t, err)
}

func TestHandlePDelayReqRespSendError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)

	clockID := ptp.ClockIdentity(0x123456789abcdef0)
	p := &SPTP{
		clockID: clockID,
	}

	// Build a valid PDelayReq
	reqClockID := ptp.ClockIdentity(0xfedcba9876543210)
	req := ptp.ReqPDelay(reqClockID, 1, 42)
	reqBytes, err := ptp.Bytes(req)
	require.NoError(t, err)

	addr := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 319}
	rxts := time.Now()

	// Pdelay_Resp send fails
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), nil, addr, uint16(42)).Return(time.Time{}, fmt.Errorf("send error"))

	err = p.handlePDelayReq(mockEventConn, reqBytes, addr, rxts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sending Pdelay_Resp")
}

func TestHandlePDelayReqFollowUpSendError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockEventConn := NewMockUDPConnWithTS(ctrl)
	mockGeneralConn := NewMockUDPConnNoTS(ctrl)

	clockID := ptp.ClockIdentity(0x123456789abcdef0)
	p := &SPTP{
		clockID: clockID,
		genConn: mockGeneralConn,
	}

	// Build a valid PDelayReq
	reqClockID := ptp.ClockIdentity(0xfedcba9876543210)
	req := ptp.ReqPDelay(reqClockID, 1, 42)
	reqBytes, err := ptp.Bytes(req)
	require.NoError(t, err)

	addr := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 319}
	addrGeneral := &unix.SockaddrInet4{Addr: [4]byte{192, 168, 0, 10}, Port: 320}
	rxts := time.Now()
	txts := rxts.Add(100 * time.Microsecond)

	// Pdelay_Resp sent successfully
	mockEventConn.EXPECT().WriteToWithTS(gomock.Any(), nil, addr, uint16(42)).Return(txts, nil)
	// Pdelay_Resp_Follow_Up send fails
	mockGeneralConn.EXPECT().WriteTo(gomock.Any(), addrGeneral).Return(0, fmt.Errorf("send error"))

	err = p.handlePDelayReq(mockEventConn, reqBytes, addr, rxts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sending Pdelay_Resp_Follow_Up")
}

// a peer replies to the port we probed from, so a Pdelay_Resp_Follow_Up arrives on
// the event port. Parsing it as a DELAY_REQ reads its RequestingPortIdentity as a
// TLV header and drops the response, losing T3.
func TestPtpingRejectsPDelayRespFollowUp(t *testing.T) {
	followUp := ptp.RespFollowUpPDelay(ptp.ClockIdentity(0xc470bdfffe857d36), 1,
		ptp.NewTimestamp(time.Unix(1700000000, 0)), &ptp.PDelayReq{})
	b, err := ptp.Bytes(followUp)
	require.NoError(t, err)

	msgType, err := ptp.ProbeMsgType(b)
	require.NoError(t, err)
	require.Equal(t, ptp.MessagePDelayRespFollowUp, msgType,
		"the listener must dispatch on this type instead of falling through to ptping")

	peer := netip.MustParseAddr("fe80::c670:bdff:fe85:7d36")
	p := &SPTP{cfg: &Config{Iface: "lo"}}
	require.ErrorContains(t, p.ptping(peer, ptp.PortEvent, b, time.Time{}), "not yet implemented")

	// the dispatched handler records T3 instead
	require.NoError(t, p.handlePDelayRespFollowup(b, peer))
}

// init() runs before initClients(), so the source has to come from the config
func TestPeerDelaySourceUsesConfiguredServers(t *testing.T) {
	p := &SPTP{cfg: &Config{Servers: map[string]int{"::1": 1}}}
	srcs := p.peerDelaySources()
	require.True(t, srcs[false].IsValid(), "an IPv6 server yields an IPv6 source")
	require.Empty(t, p.clients, "clients are not populated yet at this point")
}

// ptping answers on the event port with an ANNOUNCE that no NIC stamps
func TestReadPacketBufKeepsUnstampedPacket(t *testing.T) {
	// no SO_TIMESTAMPING on this socket, so nothing will be stamped
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, 0)
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, &unix.SockaddrInet6{Addr: [16]byte{15: 1}}))
	// the promoted-field form embedlit wants is go1.27; github.com/facebook/time is go1.26
	conn := &UDPConnTS{UDPConn: UDPConn{connFd: fd}}
	defer conn.Close()

	local, err := unix.Getsockname(fd)
	require.NoError(t, err)
	announce, err := ptp.Bytes(&ptp.Announce{Header: ptp.Header{
		SdoIDAndMsgType: ptp.NewSdoIDAndMsgType(ptp.MessageAnnounce, 0),
	}})
	require.NoError(t, err)
	require.NoError(t, unix.Sendto(fd, announce, 0, local))

	buf := make([]byte, timestamp.PayloadSizeBytes)
	oob := make([]byte, timestamp.ControlSizeBytes)
	bbuf, boob, saddr, err := conn.ReadPacketBuf(buf, oob)
	require.NoError(t, err)
	require.Positive(t, bbuf, "a general message must survive having no timestamp")
	require.NotNil(t, saddr)
	_, err = conn.RXTimestamp(oob, boob)
	require.ErrorIs(t, err, timestamp.ErrNoTimestamp, "a general message is never stamped")
}

func TestStepClock(t *testing.T) {
	now := time.Unix(1789992261, 0)
	wrap := time.Duration(1 << 48)

	t.Run("SmallStepStaysRelative", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockClock := NewMockClock(ctrl)
		mockClock.EXPECT().Step(time.Second).Return(nil)

		p := &SPTP{clock: mockClock}
		require.NoError(t, p.stepClock(time.Second))
	})

	t.Run("LargeStepRebasesAbsolutely", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockClock := NewMockClock(ctrl)
		gomock.InOrder(
			mockClock.EXPECT().Time().Return(now, nil),
			mockClock.EXPECT().SetTime(now.Add(wrap)).Return(nil),
			mockClock.EXPECT().Time().Return(now.Add(wrap), nil),
		)

		p := &SPTP{clock: mockClock}
		require.NoError(t, p.stepClock(wrap))
	})

	t.Run("LargeStepErrorsWhenClockDidNotMove", func(t *testing.T) {
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()
		mockClock := NewMockClock(ctrl)
		gomock.InOrder(
			mockClock.EXPECT().Time().Return(now, nil),
			mockClock.EXPECT().SetTime(now.Add(wrap)).Return(nil),
			mockClock.EXPECT().Time().Return(now, nil),
		)

		p := &SPTP{clock: mockClock}
		require.ErrorContains(t, p.stepClock(wrap), "clock rejected absolute set")
	})

	// a monotonic reading would make Sub ignore the wall clock SetTime moves, so
	// every large step would report rejection even after succeeding
	t.Run("SettableClocksReturnWallOnlyTime", func(t *testing.T) {
		for name, c := range map[string]Clock{"sys": &SysClock{}, "freerunning": &FreeRunningClock{}} {
			got, err := c.Time()
			require.NoError(t, err, name)
			require.Equal(t, got.Round(0), got, name)
		}
	})

	t.Run("FreeRunningClockReflectsSetTime", func(t *testing.T) {
		c := &FreeRunningClock{}
		target := time.Now().Round(0).Add(wrap)
		require.NoError(t, c.SetTime(target))

		got, err := c.Time()
		require.NoError(t, err)
		require.WithinDuration(t, target, got, time.Second)
	})

	t.Run("FreeRunningClockAcceptsLargeStep", func(t *testing.T) {
		p := &SPTP{clock: &FreeRunningClock{}}
		require.NoError(t, p.stepClock(wrap))
	})
}
