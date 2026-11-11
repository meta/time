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
	"encoding/binary"
	"math"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/facebook/time/fbclock"
	"github.com/facebook/time/fbclock/stats"
	"github.com/facebook/time/leapsectz"
	ntp "github.com/facebook/time/ntp/protocol"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// trackingReply is a real reply captured from chronyd, the same fixture the
// chrony package decodes in its own tests.
var trackingReply = []uint8{
	0x06, 0x02, 0x00, 0x00, 0x00, 0x21, 0x00, 0x05, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xe6, 0x25,
	0xc6, 0x6e, 0x24, 0x01, 0xdb, 0x00, 0x31, 0x10, 0x21, 0x32,
	0xfa, 0xce, 0x00, 0x00, 0x00, 0x8e, 0x00, 0x00, 0x00, 0x02,
	0x00, 0x00, 0x00, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x61, 0x38, 0xe1, 0x81, 0x36, 0x94, 0x8d, 0xd5, 0xdf, 0x19,
	0x2d, 0xb7, 0xdf, 0x42, 0x83, 0xf5, 0xe2, 0xeb, 0xca, 0x12,
	0x05, 0x39, 0xe1, 0x11, 0xeb, 0x7b, 0x3e, 0x5d, 0xf4, 0xb0,
	0x75, 0x12, 0xea, 0xe7, 0x5b, 0x0c, 0xf0, 0x88, 0x1d, 0x4e,
	0x16, 0x82, 0x1f, 0x69,
}

// ntpTestFloorPPM isn't -max-drift-rate's default, so a test can tell the flag is used.
const ntpTestFloorPPM = 20

// ntpTestSysTime is a fixed system clock reading for the NTP tests.
var ntpTestSysTime = time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)

// fakeChronyd answers the i-th request on a loopback UDP port with replies[i] and
// the rest with nothing; a nil reply leaves its request unanswered.
func fakeChronyd(t *testing.T, replies ...[]uint8) (address string) {
	t.Helper()
	return slowChronyd(t, 0, replies...)
}

// slowChronyd is fakeChronyd answering each request delay late.
func slowChronyd(t *testing.T, delay time.Duration, replies ...[]uint8) (address string) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]uint8, 1024)
		for _, reply := range replies {
			_, client, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if reply != nil {
				time.Sleep(delay)
				_, _ = conn.WriteToUDP(reply, client)
			}
		}
	}()
	return conn.LocalAddr().String()
}

func TestFetchTracking(t *testing.T) {
	cfg := &Config{Interval: 2 * time.Second}

	tracking, err := (&ChronyFetcher{address: fakeChronyd(t, trackingReply)}).FetchTracking(cfg)
	require.NoError(t, err)
	require.Equal(t, uint16(3), tracking.Stratum)
	require.Equal(t, time.Unix(0, 1631117697915705301), tracking.RefTime)
	require.Equal(t, 0.0010384710039943457, tracking.RootDispersion)
}

func TestFetchTrackingTimeout(t *testing.T) {
	// UDP gives no connection-refused, so the read deadline ends the poll
	cfg := &Config{Interval: 20 * time.Millisecond}

	_, err := (&ChronyFetcher{address: fakeChronyd(t, nil)}).FetchTracking(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to get tracking from chronyd")
}

// leapAlarmReply is trackingReply with chronyd's leap status set to LeapAlarm: no usable source.
func leapAlarmReply() []uint8 {
	reply := slices.Clone(trackingReply)
	// after the 28-byte reply header, the reference ID (4), its address (20) and the stratum (2)
	binary.BigEndian.PutUint16(reply[54:], ntp.LeapAlarm)
	return reply
}

// refclockReply is trackingReply as chronyd sends it while it follows `refclock PHC`:
// reference ID "PHC0", no address and stratum 1.
func refclockReply() []uint8 {
	reply := slices.Clone(trackingReply)
	copy(reply[28:32], "PHC0")
	clear(reply[32:52])
	binary.BigEndian.PutUint16(reply[52:], 1)
	return reply
}

// trackingBound is the bound and holdover rate ChronyBound gives trackingReply.
func trackingBound(t *testing.T) (uint64, float64) {
	t.Helper()
	tracking, err := (&ChronyFetcher{address: fakeChronyd(t, trackingReply)}).FetchTracking(&Config{Interval: 2 * time.Second})
	require.NoError(t, err)
	boundNS, holdoverNS, err := ChronyBound(tracking, ntpTestFloorPPM)
	require.NoError(t, err)
	return boundNS, holdoverNS
}

// ntpTestDaemon opens a v3 file in the test's temp dir, as -v3 does, for the NTP path.
func ntpTestDaemon(t *testing.T, interval time.Duration) (*Daemon, *fbclock.DataFile, string) {
	t.Helper()
	cfg := &Config{RingSize: 1, Interval: interval, MaxDriftRate: ntpTestFloorPPM, EnableDataV2: true, EnableDataV3: true}
	s := newTestDaemon(cfg, stats.NewStats())
	path := filepath.Join(t.TempDir(), "data")
	v3 := s.openDataV3(path)
	require.NotNil(t, v3)
	t.Cleanup(func() { v3.Close() })
	return s, v3, path
}

// startNTP runs populateNTPDataV3 against the chronyd at address until the test ends.
func startNTP(t *testing.T, s *Daemon, address string, v3 *fbclock.DataFile) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.populateNTPDataV3(t.Context(), &ChronyFetcher{address: address}, v3)
	}()
	// cleanups run last first, so this one waits before an earlier one closes v3
	t.Cleanup(func() { <-done })
}

// ntpSection reads the v3 file's ntp section once it has been written.
func ntpSection(t *testing.T, path string) fbclock.DataSectionSnapshot {
	t.Helper()
	require.Eventually(t, func() bool {
		_, sections, err := dataSections(path)
		return err == nil && sections["ntp"].Clock != nil
	}, 5*time.Second, 10*time.Millisecond)
	_, sections, err := dataSections(path)
	require.NoError(t, err)
	return sections["ntp"]
}

// requireRefused waits for the ntp section to hold the bound readers refuse.
func requireRefused(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, sections, err := dataSections(path)
		return err == nil && sections["ntp"].Clock != nil && sections["ntp"].Clock.ErrorBoundNS == math.MaxUint32
	}, 5*time.Second, 10*time.Millisecond)
}

func TestPollChrony(t *testing.T) {
	s := newTestDaemon(&Config{RingSize: 1, Interval: 2 * time.Second, MaxDriftRate: ntpTestFloorPPM}, stats.NewStats())
	wantBoundNS, wantHoldoverNS := trackingBound(t)

	sample, err := s.pollChrony(&ChronyFetcher{address: fakeChronyd(t, trackingReply)}, ntpTestSysTime)
	require.NoError(t, err)
	require.Equal(t, wantBoundNS, sample.errorBoundNS)
	require.Equal(t, wantHoldoverNS, sample.holdoverMultiplierNS)
	// the system clock reading in TAI, not chronyd's RefTime (2021 in the fixture)
	require.Equal(t, ntpTestSysTime.Add(leapsectz.UTCOffset).UnixNano(), sample.ingressTimeNS)

	_, err = s.pollChrony(&ChronyFetcher{address: fakeChronyd(t, leapAlarmReply())}, ntpTestSysTime)
	require.ErrorIs(t, err, errCorrectness)
	require.ErrorContains(t, err, "chronyd is not synchronised")

	_, err = s.pollChrony(&ChronyFetcher{address: fakeChronyd(t, refclockReply())}, ntpTestSysTime)
	require.ErrorIs(t, err, errCorrectness)
	require.ErrorIs(t, err, errLocalReference)
	require.ErrorContains(t, err, "(refid 50484330)")
}

func TestPopulateNTPDataV3(t *testing.T) {
	t.Run("writes chronyd's bound on the system clock", func(t *testing.T) {
		wantBoundNS, wantHoldoverNS := trackingBound(t)
		s, v3, path := ntpTestDaemon(t, time.Second)
		before := time.Now()
		startNTP(t, s, fakeChronyd(t, trackingReply), v3)
		clock := ntpSection(t, path).Clock
		after := time.Now()

		require.Equal(t, uint32(unix.CLOCK_REALTIME), clock.ClockID)
		require.Zero(t, clock.CoefPPB)
		// the system clock is the base, and in TAI the anchor and the poll's ingress
		require.WithinRange(t, time.Unix(0, clock.SysclockTimeNS), before, after)
		require.Equal(t, time.Unix(0, clock.SysclockTimeNS).Add(leapsectz.UTCOffset).UnixNano(), *clock.RefTimeNS)
		require.WithinRange(t, time.Unix(0, clock.IngressTimeNS).Add(-leapsectz.UTCOffset), before, after)
		require.Equal(t, wantBoundNS, clock.ErrorBoundNS)
		require.Equal(t, fbclock.FloatAsUint32(wantHoldoverNS), clock.HoldoverMultiplierRaw)
		// the NTP path writes only ntp
		_, sections, err := dataSections(path)
		require.NoError(t, err)
		require.Zero(t, sections["ptp"].Seq)
		require.Zero(t, sections["prtc"].Seq)
	})

	t.Run("reads the clock before polling", func(t *testing.T) {
		// so a slow reply ages the bound rather than narrowing it
		s, v3, path := ntpTestDaemon(t, time.Second)
		before := time.Now()
		startNTP(t, s, slowChronyd(t, 200*time.Millisecond, trackingReply), v3)
		clock := ntpSection(t, path).Clock
		require.Less(t, time.Unix(0, clock.IngressTimeNS).Add(-leapsectz.UTCOffset).Sub(before), 100*time.Millisecond)
	})

	t.Run("holds over while chronyd is down", func(t *testing.T) {
		// the fake answers the first poll only; the rest time out after half of -i
		const interval = 100 * time.Millisecond
		s, v3, path := ntpTestDaemon(t, interval)
		startNTP(t, s, fakeChronyd(t, trackingReply), v3)
		first := ntpSection(t, path)

		require.Eventually(t, func() bool {
			_, sections, err := dataSections(path)
			later := sections["ntp"].Clock
			return err == nil && later != nil && later.SysclockTimeNS > first.Clock.SysclockTimeNS+int64(3*interval)
		}, 5*time.Second, 10*time.Millisecond)
		later := ntpSection(t, path)
		// over 3 intervals of 10 ms writes, not one per (timed out) poll
		require.GreaterOrEqual(t, later.Seq-first.Seq, uint64(2*10))
		require.Equal(t, first.Clock.IngressTimeNS, later.Clock.IngressTimeNS)
		require.Equal(t, first.Clock.ErrorBoundNS, later.Clock.ErrorBoundNS)
		require.Equal(t, first.Clock.HoldoverMultiplierRaw, later.Clock.HoldoverMultiplierRaw)
	})

	t.Run("refuses its bound once chronyd follows the PHC", func(t *testing.T) {
		const interval = 100 * time.Millisecond
		s, v3, path := ntpTestDaemon(t, interval)
		// one good poll, then chronyd steers the clock to the PHC
		startNTP(t, s, fakeChronyd(t, trackingReply, refclockReply(), refclockReply(), refclockReply(), refclockReply(), refclockReply()), v3)
		ntpSection(t, path)

		requireRefused(t, path)
		refused := ntpSection(t, path)
		time.Sleep(3 * interval)
		later := ntpSection(t, path)
		// still rewritten every 10 ms, with the bound readers refuse
		require.Greater(t, later.Seq, refused.Seq)
		require.Equal(t, uint64(math.MaxUint32), later.Clock.ErrorBoundNS)
	})

	t.Run("refuses a previous run's record while chronyd follows the PHC", func(t *testing.T) {
		s, v3, path := ntpTestDaemon(t, 100*time.Millisecond)
		taiNS := ntpTestSysTime.Add(leapsectz.UTCOffset).UnixNano()
		require.NoError(t, v3.Store(fbclock.SourceNTP, fbclock.DataClock{
			ClockID: unix.CLOCK_REALTIME, RefTimeNS: taiNS, SysclockTimeNS: ntpTestSysTime.UnixNano(),
			IngressTimeNS: taiNS, ErrorBoundNS: 5000, HoldoverMultiplierNS: 50000,
		}))
		startNTP(t, s, fakeChronyd(t, refclockReply()), v3)

		requireRefused(t, path)
	})

	t.Run("retries after a failed poll", func(t *testing.T) {
		const interval = 100 * time.Millisecond
		s, v3, path := ntpTestDaemon(t, interval)
		before := time.Now()
		startNTP(t, s, fakeChronyd(t, nil, trackingReply), v3)
		clock := ntpSection(t, path).Clock
		// the second poll's ingress: the first one timed out and wrote nothing
		require.WithinRange(t, time.Unix(0, clock.IngressTimeNS).Add(-leapsectz.UTCOffset), before.Add(interval), time.Now())
	})

	t.Run("logs a failure when it starts or changes, and the recovery", func(t *testing.T) {
		const interval = 100 * time.Millisecond
		s, v3, _ := ntpTestDaemon(t, interval)
		hooks := log.StandardLogger().ReplaceHooks(make(log.LevelHooks))
		t.Cleanup(func() { log.StandardLogger().ReplaceHooks(hooks) })
		hook := logtest.NewGlobal()
		// the poller's lines as "level: message", not what other tests' goroutines log
		pollLines := func() []string {
			var lines []string
			for _, e := range hook.AllEntries() {
				if strings.HasPrefix(e.Message, "polling chronyd for fbclock data v3") {
					lines = append(lines, e.Level.String()+": "+e.Message)
				}
			}
			return lines
		}
		// a good first poll, which logs nothing; after the last answer every poll times out
		startNTP(t, s, fakeChronyd(t, trackingReply, leapAlarmReply(), leapAlarmReply(), refclockReply(), refclockReply(),
			trackingReply, trackingReply, leapAlarmReply(), trackingReply), v3)
		want := []string{
			`^error: .*chronyd is not synchronised$`,
			`^error: .*chronyd follows a local reference`,
			`^info: .* succeeded$`,
			`^error: .*chronyd is not synchronised$`,
			`^info: .* succeeded$`,
			// once, though each timeout names its own socket's port
			`^error: .*i/o timeout$`,
		}

		require.Eventually(t, func() bool { return len(pollLines()) >= len(want) }, 5*time.Second, 10*time.Millisecond)
		// three more timeouts, none of them logged
		time.Sleep(3 * interval)
		lines := pollLines()
		require.Len(t, lines, len(want))
		for i, w := range want {
			require.Regexp(t, w, lines[i])
		}
	})

	for _, tc := range []struct {
		name  string
		reply []uint8
	}{
		{name: "writes nothing before the first good poll"},
		{name: "writes nothing from bad tracking", reply: leapAlarmReply()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const interval = 100 * time.Millisecond
			s, v3, path := ntpTestDaemon(t, interval)
			startNTP(t, s, fakeChronyd(t, tc.reply), v3)
			time.Sleep(3 * interval)

			_, sections, err := dataSections(path)
			require.NoError(t, err)
			require.Zero(t, sections["ntp"].Seq)
			require.Equal(t, "no data from daemon error", sections["ntp"].ReadError)
		})
	}
}
