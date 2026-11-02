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

package daemon

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/facebook/time/fbclock"
	"github.com/facebook/time/fbclock/stats"
	"github.com/facebook/time/ptp/linearizability"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	ptp "github.com/facebook/time/ptp/protocol"
)

type testLogger struct {
	samples []*LogSample
}

func (l *testLogger) Log(s *LogSample) error {
	l.samples = append(l.samples, s)
	return nil
}

func newTestDaemon(cfg *Config, stats stats.Server) *Daemon {
	s := &Daemon{
		stats:       stats,
		cfg:         cfg,
		state:       newDaemonState(cfg.RingSize),
		l:           &testLogger{samples: []*LogSample{}},
		DataFetcher: &SockFetcher{},
		// what New sets on a PTP host
		sources: fbclock.SourcePTP | fbclock.SourcePRTC | fbclock.SourceNTP,
	}
	return s
}

func TestDaemonStateLinearizabilityRing(t *testing.T) {
	s := newDaemonState(3)

	probes := []linearizability.TestResult{
		linearizability.PTPTestResult{
			Server:      "server01",
			TXTimestamp: time.Unix(0, 1647359186979431100),
			RXTimestamp: time.Unix(0, 1647359186979431635),
		},
		linearizability.PTPTestResult{
			Server:      "server02",
			TXTimestamp: time.Unix(0, 1647359186979431200),
			RXTimestamp: time.Unix(0, 1647359186979431735),
		},
		linearizability.PTPTestResult{
			Server:      "server01",
			TXTimestamp: time.Unix(0, 1647359186979431300),
			RXTimestamp: time.Unix(0, 1647359186979431835),
		},
	}

	for _, tr := range probes {
		s.pushLinearizabilityTestResult(tr)
	}
	got := s.takeLinearizabilityTestResult(3)
	require.ElementsMatch(t, probes, got)
}

func TestDaemonStateAggregateMax(t *testing.T) {
	s := newDaemonState(3)

	probes := []*DataPoint{
		{
			MasterOffsetNS:    123.0,
			PathDelayNS:       3,
			FreqAdjustmentPPB: 4,
		},
		{
			MasterOffsetNS:    -2000.0,
			PathDelayNS:       300,
			FreqAdjustmentPPB: 2,
		},
		{
			MasterOffsetNS:    1009.0,
			PathDelayNS:       200,
			FreqAdjustmentPPB: 5,
		},
	}

	for _, tr := range probes {
		s.pushDataPoint(tr)
	}
	got := s.aggregateDataPointsMax(3)
	want := &DataPoint{
		MasterOffsetNS:    2000.0,
		PathDelayNS:       300,
		FreqAdjustmentPPB: 5,
	}
	require.Equal(t, want, got)
}

func TestTargetsChange(t *testing.T) {
	testCases := []struct {
		name        string
		oldTargets  []string
		targets     []string
		wantAdded   []string
		wantRemoved []string
	}{
		{
			name: "no new targets",
			oldTargets: []string{
				"server1",
				"server2",
			},
			targets:   []string{},
			wantAdded: []string{},
			wantRemoved: []string{
				"server1",
				"server2",
			},
		},
		{
			name:       "no old targets",
			oldTargets: []string{},
			targets:    []string{"server1", "server2"},
			wantAdded: []string{
				"server1",
				"server2",
			},
			wantRemoved: []string{},
		},
		{
			name: "removed old tester",
			oldTargets: []string{
				"server1",
				"server3",
				"server2",
			},
			targets:     []string{"server1", "server2"},
			wantAdded:   []string{},
			wantRemoved: []string{"server3"},
		},
		{
			name: "added target",
			oldTargets: []string{
				"server1",
				"server2",
			},
			targets:     []string{"server1", "server2", "server3"},
			wantAdded:   []string{"server3"},
			wantRemoved: []string{},
		},
		{
			name: "equal",
			oldTargets: []string{
				"server2",
				"server1",
			},
			targets:     []string{"server1", "server2"},
			wantAdded:   []string{},
			wantRemoved: []string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotAdded, gotRemoved := targetsDiff(tc.oldTargets, tc.targets)
			require.ElementsMatch(t, tc.wantAdded, gotAdded, "added")
			require.ElementsMatch(t, tc.wantRemoved, gotRemoved, "removed")
		})
	}
}

func TestMathPrepare(t *testing.T) {
	testCases := []struct {
		name    string
		in      *Math
		wantErr bool
	}{
		{
			name: "basic math",
			in: &Math{
				M:     "1 + 3",
				W:     "4 / 1",
				Drift: "1 - 0",
			},
			wantErr: false,
		},
		{
			name: "unknown function",
			in: &Math{
				M:     "1 + 3",
				W:     "missing(m)",
				Drift: "1 - 0",
			},
			wantErr: true,
		},
		{
			name: "known function",
			in: &Math{
				M:     "1 + 3",
				W:     "mean(m)",
				Drift: "1 - 0",
			},
			wantErr: false,
		},
		{
			name: "unknown variable",
			in: &Math{
				M:     "1 + 3",
				W:     "mean(missing)",
				Drift: "1 - 0",
			},
			wantErr: true,
		},
		{
			name: "no data",
			in: &Math{
				M:     "",
				W:     "",
				Drift: "",
			},
			wantErr: true,
		},
		{
			name: "all good",
			in: &Math{
				M:     "abs(mean(offset, 30)) + 1.0 * stddev(offset, 30) + 1.0 * stddev(delay, 10) + 1.0 * stddev(freq, 5)",
				W:     "mean(m, 30) + 4.0 * stddev(m, 30)",
				Drift: "mean(freqchangeabs, 29)",
			},
			wantErr: false,
		},
		{
			name: "all good with clock accuracy",
			in: &Math{
				M:     "mean(clockaccuracy, 30) + abs(mean(offset, 30)) + 1.0 * stddev(offset, 30)",
				W:     "mean(m, 30) + 4.0 * stddev(m, 30)",
				Drift: "mean(freqchangeabs, 29)",
			},
			wantErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Prepare()
			if tc.wantErr {
				require.Error(t, got)
			} else {
				require.NoError(t, got)
			}
		})
	}
}

func TestDataPointSanityCheck(t *testing.T) {
	testCases := []struct {
		name    string
		in      *DataPoint
		wantErr bool
		errMsg  string
	}{
		{
			name: "no ingress time",
			in: &DataPoint{
				IngressTimeNS:     0,
				MasterOffsetNS:    23.0,
				PathDelayNS:       213.0,
				FreqAdjustmentPPB: 212131,
				ClockAccuracyNS:   25.0,
			},
			wantErr: true,
		},
		{
			name: "zero offset",
			in: &DataPoint{
				IngressTimeNS:     1647359186979431900,
				MasterOffsetNS:    0,
				PathDelayNS:       213.0,
				FreqAdjustmentPPB: 212131,
				ClockAccuracyNS:   25.0,
			},
			wantErr: true,
		},
		{
			name: "zero delay",
			in: &DataPoint{
				IngressTimeNS:     1647359186979431900,
				MasterOffsetNS:    123,
				PathDelayNS:       0,
				FreqAdjustmentPPB: 212131,
				ClockAccuracyNS:   25.0,
			},
			wantErr: true,
		},
		{
			name: "zero freq",
			in: &DataPoint{
				IngressTimeNS:     1647359186979431900,
				MasterOffsetNS:    123,
				PathDelayNS:       213.0,
				FreqAdjustmentPPB: 0,
				ClockAccuracyNS:   25.0,
			},
			wantErr: true,
		},
		{
			name: "zero clock accuracy",
			in: &DataPoint{
				IngressTimeNS:     1647359186979431900,
				MasterOffsetNS:    123,
				PathDelayNS:       213.0,
				FreqAdjustmentPPB: 212131,
				ClockAccuracyNS:   0,
			},
			wantErr: true,
		},
		{
			name: "unknown clock accuracy",
			in: &DataPoint{
				IngressTimeNS:     1647359186979431900,
				MasterOffsetNS:    123,
				PathDelayNS:       213.0,
				FreqAdjustmentPPB: 212131,
				ClockAccuracyNS:   float64(ptp.ClockAccuracyUnknown.Duration()),
			},
			wantErr: true,
		},
		{
			name: "servo not locked",
			in: &DataPoint{
				IngressTimeNS:     1647359186979431900,
				MasterOffsetNS:    123,
				PathDelayNS:       213.0,
				FreqAdjustmentPPB: 212131,
				ClockAccuracyNS:   25.0,
				ServoState:        1,
			},
			wantErr: true,
			errMsg:  "servo state is 1, not locked",
		},
		{
			name: "no servo state to report",
			in: &DataPoint{
				IngressTimeNS:         1647359186979431900,
				MasterOffsetNS:        123,
				PathDelayNS:           213.0,
				FreqAdjustmentPPB:     212131,
				ClockAccuracyNS:       25.0,
				ServoStateUnavailable: true,
			},
			wantErr: false,
		},
		{
			name: "all good",
			in: &DataPoint{
				IngressTimeNS:     1647359186979431900,
				MasterOffsetNS:    123,
				PathDelayNS:       213.0,
				FreqAdjustmentPPB: 212131,
				ClockAccuracyNS:   25.0,
				ServoState:        2,
			},
			wantErr: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.SanityCheck()
			if tc.wantErr {
				require.Error(t, got)
			} else {
				require.NoError(t, got)
			}
			if tc.errMsg != "" {
				require.EqualError(t, got, tc.errMsg)
			}
		})
	}
}

func TestDaemonCalculateSHMData(t *testing.T) {
	cfg := &Config{
		RingSize: 30,
		Math: Math{
			M:     "mean(clockaccuracy, 30) + abs(mean(offset, 30)) + 1.0 * stddev(offset, 30)",
			W:     "mean(m, 30) + 4.0 * stddev(m, 30)",
			Drift: "1.5 * mean(freqchangeabs, 29)",
		},
	}

	err := cfg.Math.Prepare()
	require.NoError(t, err)
	stats := stats.NewStats()
	s := newTestDaemon(cfg, stats)
	startTime := time.Duration(1647359186979431900)
	var d *DataPoint
	adj := 212131.0
	for i := range 58 {
		if i%2 == 0 {
			adj += float64(i)
		} else {
			adj -= float64(i)
		}
		d = &DataPoint{
			IngressTimeNS:     int64(startTime + time.Duration(i)*time.Second),
			MasterOffsetNS:    23.0,
			PathDelayNS:       213.0,
			FreqAdjustmentPPB: adj,
			ClockAccuracyNS:   100.0,
			ServoState:        2,
		}
		shmData, err := s.calculateSHMData(d)
		if i < 29 {
			require.Nil(t, shmData)
			require.Error(t, err, "not enough data should give us error when calculating shm state")
		} else {
			require.NotNil(t, shmData)
			require.NoError(t, err)
		}
	}
	d = &DataPoint{
		IngressTimeNS:     int64(startTime + 61*time.Second),
		MasterOffsetNS:    23.0,
		PathDelayNS:       213.0,
		FreqAdjustmentPPB: 212131,
		ClockAccuracyNS:   100.0,
		ServoState:        2,
	}

	want := &fbclock.Data{
		IngressTimeNS:        d.IngressTimeNS,
		ErrorBoundNS:         123.0,
		HoldoverMultiplierNS: 64.5,
		SmearingStartS:       1483228836,
		SmearingEndS:         1483291336,
		UTCOffsetPreS:        36,
		UTCOffsetPostS:       37,
	}
	shmData, err := s.calculateSHMData(d)
	require.NoError(t, err)
	require.Equal(t, want, shmData)

	// ptp4l got restarted, not yet syncing, all values are zeroes
	d = &DataPoint{
		IngressTimeNS:     0,
		MasterOffsetNS:    0,
		PathDelayNS:       0,
		FreqAdjustmentPPB: 0,
	}
	shmData, err = s.calculateSHMData(d)
	require.Error(t, err, "we expect calculateSHMData to produce no new shm state when new input is invalid")
	require.Nil(t, shmData)
	// ptp4l started syncing, but haven't started updating the clock
	d = &DataPoint{
		IngressTimeNS:     int64(startTime + 65*time.Second),
		MasterOffsetNS:    0,
		PathDelayNS:       213,
		FreqAdjustmentPPB: 0,
	}
	shmData, err = s.calculateSHMData(d)
	require.Nil(t, shmData)
	require.Error(t, err, "we expect calculateSHMData to produce no new shm state when new input is incomplete")

	// ptp4l is back to normal operations
	d = &DataPoint{
		IngressTimeNS:     int64(startTime + 66*time.Second),
		MasterOffsetNS:    234,
		PathDelayNS:       213,
		FreqAdjustmentPPB: 32333,
		ClockAccuracyNS:   100,
		ServoState:        2,
	}
	shmData, err = s.calculateSHMData(d)
	want = &fbclock.Data{
		IngressTimeNS:        d.IngressTimeNS,
		ErrorBoundNS:         157,
		HoldoverMultiplierNS: 9362.84482758621,
		SmearingStartS:       1483228836,
		SmearingEndS:         1483291336,
		UTCOffsetPreS:        36,
		UTCOffsetPostS:       37,
	}
	require.NoError(t, err)
	require.Equal(t, want, shmData)
}

func TestDaemonDoWork(t *testing.T) {
	cfg := &Config{
		Interval: time.Second,
		RingSize: 30,
		Math: Math{
			M:     "mean(clockaccuracy, 30) + abs(mean(offset, 30)) + 1.0 * stddev(offset, 30)",
			W:     "mean(m, 30) + 4.0 * stddev(m, 30)",
			Drift: "1.5 * mean(freqchangeabs, 29)",
		},
	}
	err := cfg.Math.Prepare()
	require.NoError(t, err)
	stats := stats.NewStats()
	s := newTestDaemon(cfg, stats)
	startTime := time.Duration(1647359186979431900)
	phcTime := startTime // we modify this during the test
	// override function to get PHC time
	s.getPHCTime = func() (time.Time, error) { return time.Unix(0, int64(phcTime)), nil }
	// shared mem
	tmpFile, err := os.CreateTemp("", "daemon_test")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	shm, err := fbclock.OpenFBClockShmCustomVer(tmpFile.Name(), 1)
	require.NoError(t, err)
	defer shm.Close()

	// populate the data
	var d *DataPoint

	// bad data (ptp4l is just starting)
	for i := range 10 {
		d = &DataPoint{
			IngressTimeNS:     0,
			MasterOffsetNS:    0,
			PathDelayNS:       0,
			FreqAdjustmentPPB: 0,
		}
		err = s.doWork(shm, d)
		require.Error(t, err, "not enough data should give us error when calculating shm state")
		c := stats.Get()
		// not enough data for those
		require.Equal(t, int64(0), c["ingress_time_ns"])
		require.Equal(t, int64(0), c["master_offset_ns"])
		require.Equal(t, int64(0), c["path_delay_ns"])
		require.Equal(t, int64(0), c["freq_adj_ppb"])
		require.Equal(t, int64(0), c["dift_ppb"])
		require.Equal(t, int64(0), c["master_offset_ns.60.abs_max"])
		require.Equal(t, int64(0), c["path_delay_ns.60.abs_max"])
		require.Equal(t, int64(0), c["freq_adj_ppb.60.abs_max"])
		require.Equal(t, int64(i+1), c["data_sanity_check_error"])
	}
	// good data
	adj := 212131.0
	for i := range 58 {
		if i%2 == 0 {
			adj += float64(i)
		} else {
			adj -= float64(i)
		}
		tme := startTime + time.Duration(i)*time.Second
		d = &DataPoint{
			IngressTimeNS:     int64(tme),
			MasterOffsetNS:    23.0,
			PathDelayNS:       213.0,
			FreqAdjustmentPPB: adj,
			ClockAccuracyNS:   25.0,
			ServoState:        2,
		}
		phcTime = tme + time.Microsecond
		err = s.doWork(shm, d)
		require.NoError(t, err, "not enough data should give us error when calculating shm state, which we log and continue")
		// check exported stats
		c := stats.Get()
		require.Equal(t, int64(tme), c["ingress_time_ns"])
		require.Equal(t, int64(d.MasterOffsetNS), c["master_offset_ns"])
		require.Equal(t, int64(d.PathDelayNS), c["path_delay_ns"])
		require.Equal(t, int64(d.FreqAdjustmentPPB), c["freq_adj_ppb"])
		require.Equal(t, int64(0), c["data_sanity_check_error"])
		// we can calculate M after 30 seconds
		require.Equal(t, int64(48), c["m_ns"])
		require.Equal(t, int64(48), c["m_ns"])

		if i < 29 {
			require.Equal(t, int64(0), c["w_ns"])
			require.Equal(t, int64(0), c["master_offset_ns.60.abs_max"])
			require.Equal(t, int64(0), c["path_delay_ns.60.abs_max"])
			require.Equal(t, int64(0), c["freq_adj_ppb.60.abs_max"])
		} else {
			require.Equal(t, int64(48), c["w_ns"])
			require.Equal(t, int64(23), c["master_offset_ns.60.abs_max"])
			require.Equal(t, int64(213), c["path_delay_ns.60.abs_max"])
			require.LessOrEqual(t, int64(212145), c["freq_adj_ppb.60.abs_max"])
		}

		// not enough data for those
		require.Equal(t, int64(0), c["dift_ppb"])
	}

	// another data point, now that we have enough in the ring buffer to write to shm
	d = &DataPoint{
		IngressTimeNS:     int64(startTime + 61*time.Second),
		MasterOffsetNS:    23.0,
		PathDelayNS:       213.0,
		FreqAdjustmentPPB: 212131,
		ClockAccuracyNS:   25.0,
		ServoState:        2,
	}
	phcTime = startTime + 62*time.Second

	err = s.doWork(shm, d)
	require.NoError(t, err)
	// check that we have proper stats reported
	c := stats.Get()
	require.Equal(t, int64(startTime+61*time.Second), c["ingress_time_ns"], "ingress_time_ns after good data")
	require.Equal(t, int64(d.MasterOffsetNS), c["master_offset_ns"], "master_offset_ns after good data")
	require.Equal(t, int64(d.PathDelayNS), c["path_delay_ns"], "path_delay_ns after good data")
	require.Equal(t, int64(d.FreqAdjustmentPPB), c["freq_adj_ppb"], "freq_adj_ppb after good data")
	require.Equal(t, int64(48), c["m_ns"], "m_ns after good data")
	require.Equal(t, int64(48), c["w_ns"], "w_ns after good data")
	require.Equal(t, int64(64), c["drift_ppb"], "drift_ppb after good data")
	require.Equal(t, int64(23), c["master_offset_ns.60.abs_max"], "master_offset_ns.60.abs_max after good data")
	require.Equal(t, int64(213), c["path_delay_ns.60.abs_max"], "path_delay_ns.60.abs_max after good data")
	require.Equal(t, int64(212159), c["freq_adj_ppb.60.abs_max"], "freq_adj_ppb.60.abs_max after good data")
	require.Equal(t, int64(0), c["data_sanity_check_error"])

	// check that we wrote data correctly
	want := &fbclock.Data{
		IngressTimeNS:        d.IngressTimeNS,
		ErrorBoundNS:         48.0,
		HoldoverMultiplierNS: 64.5,
	}
	shmpData, err := fbclock.MmapShmpDataV1(shm.File.Fd())
	require.NoError(t, err)
	got, err := fbclock.ReadFBClockDataV1(shmpData)
	require.NoError(t, err)
	require.Equal(t, want.IngressTimeNS, got.IngressTimeNS)
	require.Equal(t, want.ErrorBoundNS, got.ErrorBoundNS)
	require.InDelta(t, want.HoldoverMultiplierNS, got.HoldoverMultiplierNS, 0.001)

	// ptp4l has a hiccup, but that should be okay
	d = &DataPoint{
		IngressTimeNS:     0,
		MasterOffsetNS:    0,
		PathDelayNS:       0,
		FreqAdjustmentPPB: 0,
	}
	phcTime = startTime + 63*time.Second

	err = s.doWork(shm, d)
	require.Error(t, err, "data point fails sanity check")
	// check that we have proper stats reported
	c = stats.Get()
	require.Equal(t, int64(startTime+61*time.Second), c["ingress_time_ns"], "ingress_time_ns after bad data")
	require.Equal(t, int64(23), c["master_offset_ns"], "master_offset_ns after bad data")
	require.Equal(t, int64(213), c["path_delay_ns"], "path_delay_ns after bad data")
	require.Equal(t, int64(212131), c["freq_adj_ppb"], "freq_adj_ppb after bad data")
	require.Equal(t, int64(0), c["clock_accuracy_ns"], "clock_accuracy_ns after bad data")
	require.Equal(t, int64(48), c["m_ns"], "m_ns after bad data")
	require.Equal(t, int64(48), c["w_ns"], "w_ns after bad data")
	require.Equal(t, int64(64), c["drift_ppb"], "drift_ppb after bad data")
	require.Equal(t, int64(23), c["master_offset_ns.60.abs_max"], "master_offset_ns.60.abs_max after bad data")
	require.Equal(t, int64(213), c["path_delay_ns.60.abs_max"], "path_delay_ns.60.abs_max after bad data")
	require.Equal(t, int64(212159), c["freq_adj_ppb.60.abs_max"], "freq_adj_ppb.60.abs_max after bad data")
	require.Equal(t, int64(1), c["data_sanity_check_error"])

	// check that we wrote data correctly (should be the same as before, as new data point was discarded)
	got, err = fbclock.ReadFBClockDataV1(shmpData)
	require.NoError(t, err)
	require.Equal(t, want.IngressTimeNS, got.IngressTimeNS)
	require.Equal(t, want.ErrorBoundNS, got.ErrorBoundNS)
	require.InDelta(t, want.HoldoverMultiplierNS, got.HoldoverMultiplierNS, 0.001)
}

func TestPublishRawDataPointGauges(t *testing.T) {
	accepted := DataPoint{
		IngressTimeNS:     1647359186979431900,
		MasterOffsetNS:    23,
		PathDelayNS:       213,
		FreqAdjustmentPPB: 212131,
		ClockAccuracyNS:   25,
		ServoState:        2,
	}
	require.NoError(t, accepted.SanityCheck())

	base := DataPoint{
		IngressTimeNS:     accepted.IngressTimeNS + int64(time.Second),
		MasterOffsetNS:    31,
		PathDelayNS:       217,
		FreqAdjustmentPPB: 212137,
		ClockAccuracyNS:   29,
		ServoState:        2,
	}
	require.NoError(t, base.SanityCheck())

	gauges := func(data DataPoint) map[string]int64 {
		return map[string]int64{
			"ingress_time_ns":   data.IngressTimeNS,
			"master_offset_ns":  int64(data.MasterOffsetNS),
			"path_delay_ns":     int64(data.PathDelayNS),
			"freq_adj_ppb":      int64(data.FreqAdjustmentPPB),
			"clock_accuracy_ns": int64(data.ClockAccuracyNS),
		}
	}

	zeroIngress, zeroOffset, zeroDelay, zeroFreq := base, base, base, base
	zeroIngress.IngressTimeNS = 0
	zeroOffset.MasterOffsetNS = 0
	zeroDelay.PathDelayNS = 0
	zeroFreq.FreqAdjustmentPPB = 0
	zeroAccuracy, unlocked, unknownAccuracy := base, base, base
	zeroAccuracy.ClockAccuracyNS = 0
	unlocked.ServoState = 1
	unknownAccuracy.ClockAccuracyNS = float64(ptp.ClockAccuracyUnknown.Duration())

	testCases := []struct {
		name      string
		data      DataPoint
		wantError string
		heldGauge string
	}{
		{"zero ingress time", zeroIngress, "ingress time is 0", "ingress_time_ns"},
		{"zero master offset", zeroOffset, "master offset is 0", "master_offset_ns"},
		{"zero path delay", zeroDelay, "path delay is 0", "path_delay_ns"},
		{"zero frequency adjustment", zeroFreq, "frequency adjustment is 0", "freq_adj_ppb"},
		{"zero clock accuracy", zeroAccuracy, "clock accuracy is 0", ""},
		{"unlocked servo", unlocked, "servo state is 1, not locked", ""},
		{"unknown clock accuracy", unknownAccuracy, "clock accuracy is unknown", ""},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			st := stats.NewStats()
			s := newTestDaemon(&Config{RingSize: 1}, st)
			s.publishRawDataPointGauges(&accepted)

			require.EqualError(t, tc.data.SanityCheck(), tc.wantError)
			s.publishRawDataPointGauges(&tc.data)

			want := gauges(tc.data)
			if tc.heldGauge != "" {
				want[tc.heldGauge] = gauges(accepted)[tc.heldGauge]
			}
			require.Equal(t, want, st.Get())
		})
	}
}

func TestRunLinearizabilityTestsNoGMs(t *testing.T) {
	cfg := &Config{
		LinearizabilityTestInterval: time.Second,
	}
	stats := stats.NewStats()
	s := newTestDaemon(cfg, stats)
	go s.runLinearizabilityTests(context.Background())
	time.Sleep(100 * time.Millisecond)
	c := stats.Get()
	t.Log(c)
	require.Equal(t, int64(1), c["linearizability.failed"])
}

func TestNoTestResults(t *testing.T) {
	targets := []string{"o", "l", "e", "g"}
	want := map[string]linearizability.TestResult{
		"o": linearizability.SPTPHTTPTestResult{Error: errNoTestResults},
		"l": linearizability.SPTPHTTPTestResult{Error: errNoTestResults},
		"e": linearizability.SPTPHTTPTestResult{Error: errNoTestResults},
		"g": linearizability.SPTPHTTPTestResult{Error: errNoTestResults},
	}

	got := noTestResults(targets)
	require.Equal(t, want, got)
}

func TestNoPHC(t *testing.T) {
	cfg := &Config{}
	stats := stats.NewStats()
	s := newTestDaemon(cfg, stats)
	s.getPHCTime = func() (time.Time, error) { return time.Time{}, errNoPHC }

	sample := &DataPoint{
		IngressTimeNS:     1647359186979431900,
		MasterOffsetNS:    23,
		PathDelayNS:       213,
		FreqAdjustmentPPB: 212131,
		ClockAccuracyNS:   25,
		ServoState:        2,
	}
	err := s.doWork(&fbclock.Shm{}, sample)
	require.ErrorIs(t, err, errNoPHC)

	got := stats.Get()
	require.Equal(t, sample.IngressTimeNS, got["ingress_time_ns"])
	require.Equal(t, int64(sample.MasterOffsetNS), got["master_offset_ns"])
	require.Equal(t, int64(sample.PathDelayNS), got["path_delay_ns"])
	require.Equal(t, int64(sample.FreqAdjustmentPPB), got["freq_adj_ppb"])
	require.Equal(t, int64(sample.ClockAccuracyNS), got["clock_accuracy_ns"])
}

func TestCoeffV2(t *testing.T) {
	prevDataV2 := fbclock.DataV2{}
	curDataV2 := fbclock.DataV2{}
	c, err := calcCoeffPPB(&prevDataV2, &curDataV2)
	require.Equal(t, int64(0), c)
	require.NoError(t, err)
	prevDataV2.SysclockTimeNS = 1749167822494826022
	prevDataV2.PHCTimeNS = 1749167859494830869
	curDataV2.SysclockTimeNS = 1749167822504951677
	curDataV2.PHCTimeNS = 1749167859504956519
	c, err = calcCoeffPPB(&prevDataV2, &curDataV2)
	require.Equal(t, int64(-493), c)
	require.NoError(t, err)
}

// TestDaemonGradualWindowFromN2 verifies that with GradualWindow on the daemon skips n=1 and publishes
// from n=2 (with the precomputed tolerance factor), and that with the flag off the same formula keeps
// today's "nothing until the ring is full" behavior.
func TestDaemonGradualWindowFromN2(t *testing.T) {
	makeCfg := func(gradual bool) *Config {
		cfg := &Config{
			PTPClientAddress: "/tmp/fbclock-test",
			RingSize:         30,
			GradualWindow:    gradual,
			Interval:         time.Second,
			Math: Math{
				M:     MathDefaultM,
				W:     MathDefaultW,
				Drift: MathDefaultDrift,
			},
		}
		// EvalAndValidate (not Math.Prepare) so the warm-up factor table is precomputed.
		require.NoError(t, cfg.EvalAndValidate())
		return cfg
	}
	startTime := time.Duration(1647359186979431900)
	// Varying offset so stddev(m) > 0 and the warm-up factor actually widens the window.
	sample := func(i int) *DataPoint {
		return &DataPoint{
			IngressTimeNS:     int64(startTime + time.Duration(i)*time.Second),
			MasterOffsetNS:    float64(20 + (i%5)*3),
			PathDelayNS:       213.0,
			FreqAdjustmentPPB: float64(212131 + i),
			ClockAccuracyNS:   100.0,
			ServoState:        2,
		}
	}

	// Flag ON: n=1 is skipped; publishes from n=2; window stays under the uint32 sentinel.
	dOn := newTestDaemon(makeCfg(true), stats.NewStats())
	for i := range 30 {
		shmData, err := dOn.calculateSHMData(sample(i))
		if i == 0 {
			require.Error(t, err, "n=1 must be skipped (stddev=0, no margin)")
			require.Nil(t, shmData)
			continue
		}
		require.NoErrorf(t, err, "gradual window should publish from n=2 (i=%d)", i)
		require.NotNil(t, shmData)
		require.Greater(t, shmData.ErrorBoundNS, uint64(0))
		require.Less(t, shmData.ErrorBoundNS, uint64(math.MaxUint32), "window must never reach the uint32 WOU sentinel")
	}

	// Flag OFF (same gradual formula): unchanged behavior, nothing until the ring is full.
	dOff := newTestDaemon(makeCfg(false), stats.NewStats())
	for i := range 30 {
		shmData, err := dOff.calculateSHMData(sample(i))
		if i < 29 {
			require.Error(t, err, "flag off must not publish before the ring is full")
			require.Nil(t, shmData)
		} else {
			require.NoError(t, err)
			require.NotNil(t, shmData)
		}
	}
}

// TestDaemonGradualWindowSteadyStateIdentical locks the core invariant: once the ring is full the
// GradualWindow flag changes nothing. It drives a flag-on and a flag-off daemon through the SAME ring
// size and SAME sample sequence, and asserts the published ErrorBoundNS is identical at n == RingSize.
func TestDaemonGradualWindowSteadyStateIdentical(t *testing.T) {
	const ringSize = 30
	makeCfg := func(gradual bool) *Config {
		cfg := &Config{
			PTPClientAddress: "/tmp/fbclock-test",
			RingSize:         ringSize,
			GradualWindow:    gradual,
			Interval:         time.Second,
			Math: Math{
				M:     MathDefaultM,
				W:     MathDefaultW,
				Drift: MathDefaultDrift,
			},
		}
		// EvalAndValidate (not Math.Prepare) so the warm-up factor table is precomputed.
		require.NoError(t, cfg.EvalAndValidate())
		return cfg
	}
	startTime := time.Duration(1647359186979431900)
	sample := func(i int) *DataPoint {
		return &DataPoint{
			IngressTimeNS:     int64(startTime + time.Duration(i)*time.Second),
			MasterOffsetNS:    float64(20 + (i%5)*3),
			PathDelayNS:       213.0,
			FreqAdjustmentPPB: float64(212131 + i),
			ClockAccuracyNS:   100.0,
			ServoState:        2,
		}
	}

	dOn := newTestDaemon(makeCfg(true), stats.NewStats())
	dOff := newTestDaemon(makeCfg(false), stats.NewStats())
	var onFull, offFull *fbclock.Data
	for i := range ringSize {
		onData, onErr := dOn.calculateSHMData(sample(i))
		offData, offErr := dOff.calculateSHMData(sample(i))
		if i == ringSize-1 {
			require.NoError(t, onErr)
			require.NoError(t, offErr)
			onFull, offFull = onData, offData
		}
	}
	require.NotNil(t, onFull)
	require.NotNil(t, offFull)
	require.Equal(t, offFull.ErrorBoundNS, onFull.ErrorBoundNS,
		"with the ring full, GradualWindow must not change the published window")
}

// TestMeanCoeffPPB covers the composition wired into populateDataV2: the synthetic
// first-run coefficient must not be averaged into the mean, so the first real
// measurement is published unbiased rather than halved by a phantom zero.
func TestMeanCoeffPPB(t *testing.T) {
	d := newTestDaemon(&Config{RingSize: 30, Interval: time.Second}, stats.NewStats())

	// First run: prev has no sysclock anchor, so calcCoeffPPB yields the synthetic
	// 0 and nothing is enqueued into the mean ring.
	prev := fbclock.DataV2{}
	cur := fbclock.DataV2{}
	c := d.meanCoeffPPB(&prev, &cur)
	require.Equal(t, int64(0), c)

	// First real measurement. Because the synthetic 0 was skipped, the mean is the
	// measured coefficient itself (-493), not (0 + -493) / 2.
	prev = fbclock.DataV2{SysclockTimeNS: 1749167822494826022, PHCTimeNS: 1749167859494830869}
	cur = fbclock.DataV2{SysclockTimeNS: 1749167822504951677, PHCTimeNS: 1749167859504956519}
	c = d.meanCoeffPPB(&prev, &cur)
	require.Equal(t, int64(-493), c)
}

// TestPopulateDataV2 pins v2's PTP output: every field of the records clients read
// from data_v2, for a fixed last stored sample and fake clock readings.
func TestPopulateDataV2(t *testing.T) {
	// Readings are 2^20 ns apart on the base clock and the PHC gains (primary) or
	// loses (realtime) 16, then 48 ns of them: exact coefficients, unequal so the mean shows.
	const step = 1 << 20
	gain := []int64{0, 16, 64}
	stored := &fbclock.Data{
		IngressTimeNS:        1755000036500000000,
		ErrorBoundNS:         789,
		HoldoverMultiplierNS: 30.5,
		SmearingStartS:       1483228836,
		SmearingEndS:         1483291336,
		UTCOffsetPreS:        36,
		UTCOffsetPostS:       37,
	}
	primary := fbclock.DataV2{
		IngressTimeNS:        1755000036500000000,
		ErrorBoundNS:         814, // plus the 25 ns read delay
		HoldoverMultiplierNS: 30.5,
		SmearingStartS:       1483228836,
		UTCOffsetPreS:        36,
		UTCOffsetPostS:       37,
		ClockID:              unix.CLOCK_MONOTONIC_RAW,
		PHCTimeNS:            1755000037002097216,
		SysclockTimeNS:       5000002097152,
		CoefPPB:              30517, // the mean of 15258 and 45776
	}
	realtimePrimary := primary
	realtimePrimary.ClockID = unix.CLOCK_REALTIME
	testCases := []struct {
		name         string
		clockID      uint32
		wantPrimary  fbclock.DataV2
		wantRealtime *fbclock.DataV2 // nil: never written
	}{
		{
			name:        "MONOTONIC_RAW primary",
			clockID:     unix.CLOCK_MONOTONIC_RAW,
			wantPrimary: primary,
			wantRealtime: &fbclock.DataV2{
				IngressTimeNS:        1755000036500000000,
				ErrorBoundNS:         829, // plus its own 40 ns read delay
				HoldoverMultiplierNS: 30.5,
				SmearingStartS:       1483228836,
				UTCOffsetPreS:        36,
				UTCOffsetPostS:       37,
				ClockID:              unix.CLOCK_REALTIME,
				PHCTimeNS:            1755000037002098088,
				SysclockTimeNS:       1755000000002098152,
				CoefPPB:              -45776, // the last measurement, not averaged
			},
		},
		{
			name:        "CLOCK_REALTIME primary",
			clockID:     unix.CLOCK_REALTIME,
			wantPrimary: realtimePrimary,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestDaemon(&Config{RingSize: 30, Interval: time.Second}, stats.NewStats())
			s.state.setLastStoredData(stored)
			// populateDataV2 never returns, so the reads of its fourth tick end its goroutine
			const ticks = 3
			done := make(chan struct{})
			var reads, realtimeReads int64
			s.getPHCAndSysTime = func() (time.Time, time.Time, uint32, time.Duration, error) {
				if reads == ticks {
					close(done)
					runtime.Goexit()
				}
				phc, sys := 1755000037000000000+reads*step+gain[reads], 5000000000000+reads*step
				reads++
				return time.Unix(0, phc), time.Unix(0, sys), tc.clockID, 25 * time.Nanosecond, nil
			}
			s.getPHCAndSysTimeRealtime = func() (time.Time, time.Time, time.Duration, error) {
				phc, sys := 1755000037000001000+realtimeReads*step-gain[realtimeReads], 1755000000000001000+realtimeReads*step
				realtimeReads++
				return time.Unix(0, phc), time.Unix(0, sys), 40 * time.Nanosecond, nil
			}
			shm, err := fbclock.OpenFBClockShmCustomVer(filepath.Join(t.TempDir(), "data_v2"), 2)
			require.NoError(t, err)
			defer shm.Close()

			go s.populateDataV2(shm)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "populateDataV2 didn't reach its fourth tick")
			}

			shmp, err := fbclock.MmapShmpDataV2(shm.File.Fd())
			require.NoError(t, err)
			got, err := fbclock.ReadFBClockDataV2(shmp)
			require.NoError(t, err)
			require.Equal(t, tc.wantPrimary, *got)
			got, err = fbclock.ReadFBClockDataRealtime(shmp)
			if tc.wantRealtime == nil {
				require.ErrorContains(t, err, "no data from daemon")
				require.Zero(t, realtimeReads)
				return
			}
			require.NoError(t, err)
			require.Equal(t, *tc.wantRealtime, *got)
		})
	}
}

// v3Clock is how a stored record reads back.
func v3Clock(c fbclock.DataClock) *fbclock.DataClockSnapshot {
	return &fbclock.DataClockSnapshot{
		Reserved:              &[12]uint8{},
		ClockID:               c.ClockID,
		RefTimeNS:             new(c.RefTimeNS),
		SysclockTimeNS:        c.SysclockTimeNS,
		CoefPPB:               c.CoefPPB,
		IngressTimeNS:         c.IngressTimeNS,
		ErrorBoundNS:          c.ErrorBoundNS,
		HoldoverMultiplierRaw: fbclock.FloatAsUint32(c.HoldoverMultiplierNS),
		HoldoverMultiplierNS:  c.HoldoverMultiplierNS,
	}
}

// dataSections reads a v3 file's sections by name: ptp, prtc and ntp.
func dataSections(path string) (*fbclock.DataFileSnapshot, map[string]fbclock.DataSectionSnapshot, error) {
	file, err := fbclock.ReadFBClockDataFile(path, false)
	if err != nil {
		return nil, nil, err
	}
	sections := map[string]fbclock.DataSectionSnapshot{}
	for _, section := range file.Sections {
		sections[section.Name] = section
	}
	return file, sections, nil
}

func TestOpenDataV3(t *testing.T) {
	const ptpHost = fbclock.SourcePTP | fbclock.SourcePRTC | fbclock.SourceNTP
	testCases := []struct {
		name        string
		sources     fbclock.Sources
		v2, v3      bool
		failOpen    bool   // a directory where the file goes
		wantSources uint32 // 0: no file
	}{
		{name: "PTP host: ptp, prtc and ntp", sources: ptpHost, v2: true, v3: true, wantSources: 0x7},
		{name: "PTP host, -v2 off: still all three", sources: ptpHost, v3: true, wantSources: 0x7},
		{name: "no PTP: ntp only", sources: fbclock.SourceNTP, v2: true, v3: true, wantSources: 0x4},
		{name: "no file with -v3 off", sources: ptpHost, v2: true},
		{name: "nil when the open fails", sources: ptpHost, v2: true, v3: true, failOpen: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestDaemon(&Config{RingSize: 1, Interval: time.Second, EnableDataV2: tc.v2, EnableDataV3: tc.v3}, stats.NewStats())
			s.sources = tc.sources
			path := filepath.Join(t.TempDir(), "data")
			if tc.failOpen {
				require.NoError(t, os.Mkdir(path, 0o755))
			}
			v3 := s.openDataV3(path)
			if tc.wantSources == 0 {
				require.Nil(t, v3)
				if !tc.failOpen {
					require.NoFileExists(t, path)
				}
				return
			}
			require.NotNil(t, v3)
			t.Cleanup(func() { v3.Close() })
			file, _, err := dataSections(path)
			require.NoError(t, err)
			require.Equal(t, tc.wantSources, file.Header.Sources)
		})
	}
}

func TestPopulatePTPDataV3(t *testing.T) {
	// TestPopulateDataV2's snapshot and readings, so the records are the ones it pins for v2
	const step = 1 << 20
	gain := []int64{0, 16, 64}
	stored := &fbclock.Data{IngressTimeNS: 1755000036500000000, ErrorBoundNS: 789, HoldoverMultiplierNS: 30.5}
	ptp := fbclock.DataClock{
		ClockID:              unix.CLOCK_MONOTONIC_RAW,
		RefTimeNS:            1755000037002097216,
		SysclockTimeNS:       5000002097152,
		CoefPPB:              30517, // the mean of 15258 and 45776
		IngressTimeNS:        1755000036500000000,
		ErrorBoundNS:         814, // plus the 25 ns read delay
		HoldoverMultiplierNS: 30.5,
	}
	realtimePTP := ptp
	realtimePTP.ClockID = unix.CLOCK_REALTIME
	prtc := fbclock.DataClock{
		ClockID:              unix.CLOCK_REALTIME,
		RefTimeNS:            1755000037002098088,
		SysclockTimeNS:       1755000000002098152,
		CoefPPB:              -45776, // the last measurement, not averaged
		IngressTimeNS:        1755000036500000000,
		ErrorBoundNS:         829, // plus its own 40 ns read delay
		HoldoverMultiplierNS: 30.5,
	}
	// a failed read skips tick 1, so tick 2's coefficient spans ticks 0 to 2; for ptp that
	// comes to the same 30517
	prtcAfterGap := prtc
	prtcAfterGap.CoefPPB = -30517
	// phc falling back to CLOCK_REALTIME from tick 1: tick 2's coefficient only spans ticks 1 to 2
	fellBack := ptp
	fellBack.ClockID = unix.CLOCK_REALTIME
	fellBack.SysclockTimeNS = 1755000000002097152
	fellBack.CoefPPB = 45776
	testCases := []struct {
		name                  string
		clockID               uint32
		primaryFails          bool // the primary read of tick 1 fails
		realtimeFails         bool // the realtime read of tick 1 fails
		negativeDelay         bool // the failing read returns a negative delay instead of an error
		fallBack              bool // the primary reads against CLOCK_REALTIME from tick 1 on
		ntp                   bool
		wantPTP               fbclock.DataClock
		wantPRTC              fbclock.DataClock
		ptpWrites, prtcWrites uint64
	}{
		{name: "MONOTONIC_RAW primary", clockID: unix.CLOCK_MONOTONIC_RAW, wantPTP: ptp, wantPRTC: prtc, ptpWrites: 3, prtcWrites: 3},
		{name: "MONOTONIC_RAW primary, primary read fails once", clockID: unix.CLOCK_MONOTONIC_RAW, primaryFails: true, wantPTP: ptp, wantPRTC: prtcAfterGap, ptpWrites: 2, prtcWrites: 2},
		{name: "MONOTONIC_RAW primary, primary read with a negative delay once", clockID: unix.CLOCK_MONOTONIC_RAW, primaryFails: true, negativeDelay: true, wantPTP: ptp, wantPRTC: prtcAfterGap, ptpWrites: 2, prtcWrites: 2},
		// ntp's writes run next to the PTP loop's and must leave them alone
		{name: "MONOTONIC_RAW primary, ntp loop alongside", clockID: unix.CLOCK_MONOTONIC_RAW, ntp: true, wantPTP: ptp, wantPRTC: prtc, ptpWrites: 3, prtcWrites: 3},
		{name: "MONOTONIC_RAW primary, realtime read fails once", clockID: unix.CLOCK_MONOTONIC_RAW, realtimeFails: true, wantPTP: ptp, wantPRTC: prtcAfterGap, ptpWrites: 3, prtcWrites: 2},
		{name: "MONOTONIC_RAW primary, realtime read with a negative delay once", clockID: unix.CLOCK_MONOTONIC_RAW, realtimeFails: true, negativeDelay: true, wantPTP: ptp, wantPRTC: prtcAfterGap, ptpWrites: 3, prtcWrites: 2},
		{name: "MONOTONIC_RAW primary falling back to CLOCK_REALTIME", clockID: unix.CLOCK_MONOTONIC_RAW, fallBack: true, wantPTP: fellBack, wantPRTC: fellBack, ptpWrites: 3, prtcWrites: 3},
		// a REALTIME primary already is the realtime anchor
		{name: "CLOCK_REALTIME primary", clockID: unix.CLOCK_REALTIME, wantPTP: realtimePTP, wantPRTC: realtimePTP, ptpWrites: 3, prtcWrites: 3},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestDaemon(&Config{RingSize: 30, Interval: time.Second, MaxDriftRate: 50, EnableDataV3: true}, stats.NewStats())
			s.state.setLastStoredData(stored)
			path := filepath.Join(t.TempDir(), "data")
			v3 := s.openDataV3(path)
			require.NotNil(t, v3)
			t.Cleanup(func() { v3.Close() })
			if tc.ntp {
				startNTP(t, s, fakeChronyd(t, trackingReply), v3)
			}
			// populatePTPDataV3 never returns, so the reads of its fourth tick end its goroutine
			const ticks = 3
			done := make(chan struct{})
			var reads, realtimeReads int64
			s.getPHCAndSysTime = func() (time.Time, time.Time, uint32, time.Duration, error) {
				if reads == ticks {
					close(done)
					runtime.Goexit()
				}
				tick := reads
				reads++
				phc, sys := 1755000037000000000+tick*step+gain[tick], 5000000000000+tick*step
				clockID := tc.clockID
				if tc.fallBack && tick >= 1 {
					clockID, sys = unix.CLOCK_REALTIME, 1755000000000000000+tick*step
				}
				delay := 25 * time.Nanosecond
				if tc.primaryFails && tick == 1 {
					if !tc.negativeDelay {
						return time.Time{}, time.Time{}, 0, 0, errors.New("primary read failed")
					}
					delay = -5 * time.Nanosecond
				}
				return time.Unix(0, phc), time.Unix(0, sys), clockID, delay, nil
			}
			s.getPHCAndSysTimeRealtime = func() (time.Time, time.Time, time.Duration, error) {
				realtimeReads++
				tick := reads - 1
				phc, sys := 1755000037000001000+tick*step-gain[tick], 1755000000000001000+tick*step
				delay := 40 * time.Nanosecond
				if tc.realtimeFails && tick == 1 {
					if !tc.negativeDelay {
						return time.Time{}, time.Time{}, 0, errors.New("realtime read failed")
					}
					delay = -5 * time.Nanosecond
				}
				return time.Unix(0, phc), time.Unix(0, sys), delay, nil
			}

			go s.populatePTPDataV3(v3)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "populatePTPDataV3 didn't reach its fourth tick")
			}

			file, sections, err := dataSections(path)
			require.NoError(t, err)
			require.Equal(t, uint32(fbclock.SourcePTP|fbclock.SourcePRTC|fbclock.SourceNTP), file.Header.Sources)
			require.Equal(t, v3Clock(tc.wantPTP), sections["ptp"].Clock)
			require.Equal(t, v3Clock(tc.wantPRTC), sections["prtc"].Clock)
			// each write moves a section's seq on by 2
			require.Equal(t, 2*tc.ptpWrites, sections["ptp"].Seq)
			require.Equal(t, 2*tc.prtcWrites, sections["prtc"].Seq)
			if tc.ntp {
				require.Equal(t, uint32(unix.CLOCK_REALTIME), ntpSection(t, path).Clock.ClockID)
			} else {
				// the PTP loop writes only ptp and prtc
				require.Zero(t, sections["ntp"].Seq)
			}
			if tc.clockID == unix.CLOCK_REALTIME {
				require.Zero(t, realtimeReads)
			}
		})
	}
}

// withPTPDevice points New and Run's re-check at path until t ends.
func withPTPDevice(t *testing.T, path string) {
	old := ptpDevicePath
	ptpDevicePath = path
	t.Cleanup(func() { ptpDevicePath = old })
}

func TestNewWithoutPTP(t *testing.T) {
	// noPHCIface doesn't exist, so it has no PHC
	const noPHCIface = "fbclock-test"
	dir := t.TempDir()
	present, absent := filepath.Join(dir, "ptp"), filepath.Join(dir, "none")
	require.NoError(t, os.WriteFile(present, nil, 0o600))
	for _, tc := range []struct {
		name    string
		device  string
		v3      bool
		wantErr string // none: New runs without PTP
	}{
		{name: "no device", device: absent, v3: true},
		{name: "device but no PHC", device: present, v3: true},
		// the error names what's missing, and the PHC is only looked for once the device is there
		{name: "no device, v3 off", device: absent, wantErr: "nothing to write without PTP and -v3: stat " + absent},
		{name: "no PHC, v3 off", device: present, wantErr: `nothing to write without PTP and -v3: finding PHC device for "fbclock-test"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Iface: noPHCIface, RingSize: 1, Interval: time.Second, EnableDataV2: true, EnableDataV3: tc.v3}
			st := stats.NewStats()
			withPTPDevice(t, tc.device)
			s, err := New(cfg, st, &testLogger{})
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, fbclock.SourceNTP, s.sources)
			// none of PTP's counters, such as m_ns, are seeded
			require.Empty(t, st.Get())
		})
	}
}

// withRecheckInterval shortens Run's re-check of PTP until t ends.
func withRecheckInterval(t *testing.T, d time.Duration) {
	old := ptpRecheckInterval
	ptpRecheckInterval = d
	t.Cleanup(func() { ptpRecheckInterval = old })
}

func TestWaitForPTP(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "ptp")
	require.NoError(t, os.WriteFile(present, nil, 0o600))
	for _, tc := range []struct {
		name   string
		device string
	}{
		{name: "no device", device: filepath.Join(dir, "none")},
		// the link is there but -iface has no PHC, so New still wouldn't run PTP
		{name: "device but no PHC", device: present},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPTPDevice(t, tc.device)
			withRecheckInterval(t, time.Millisecond)
			s := newTestDaemon(&Config{Iface: "fbclock-test", RingSize: 1, Interval: time.Second}, stats.NewStats())
			require.False(t, s.ptpAvailable())
			// it keeps checking and only returns, without an error, once ctx is done
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			require.NoError(t, s.waitForPTP(ctx))
			require.Error(t, ctx.Err(), "returned before ctx was done")
		})
	}
}
