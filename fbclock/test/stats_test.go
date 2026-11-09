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

package test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	lib "github.com/facebook/time/fbclock"

	"github.com/stretchr/testify/require"
)

func TestStatsUpdate(t *testing.T) {
	type in struct {
		tt  *lib.TrueTime
		err error
	}
	testCases := []struct {
		name   string
		inputs []in
		want   lib.Stats
	}{
		{
			name: "empty",
			want: lib.Stats{},
		},
		{
			name: "single error",
			inputs: []in{
				{
					tt:  nil,
					err: fmt.Errorf("oh no"),
				},
			},
			want: lib.Stats{
				Requests: 1,
				Errors:   1,
			},
		},
		{
			name: "single value",
			inputs: []in{
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1648137249050666302), Latest: time.Unix(0, 1648137249050666313)},
					err: nil,
				},
			},
			want: lib.Stats{
				Requests:  1,
				Errors:    0,
				WOUAvg:    11,
				WOUMax:    11,
				WOUlt10us: 1,
				WOUlt1ms:  1,
			},
		},
		{
			name: "mixed",
			inputs: []in{
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1648137249050666302), Latest: time.Unix(0, 1648137249050666313)},
					err: nil,
				},
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1648137249050666902), Latest: time.Unix(0, 1648137249050667333)},
					err: nil,
				},
				{
					tt:  nil,
					err: fmt.Errorf("oh no"),
				},
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1648137249050667499), Latest: time.Unix(0, 1648137249050668300)},
					err: nil,
				},
				{
					tt:  nil,
					err: fmt.Errorf("whoops"),
				},
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1648137249050668999), Latest: time.Unix(0, 1648137249050699300)},
					err: nil,
				},
			},
			want: lib.Stats{
				Requests:   6,
				Errors:     2,
				WOUAvg:     7886,
				WOUMax:     30301,
				WOUlt10us:  3,
				WOUlt100us: 1,
				WOUlt1ms:   4,
			},
		},
		{
			name: "wou between 100us and 1ms",
			inputs: []in{
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1000500000)},
					err: nil,
				},
			},
			want: lib.Stats{
				Requests:    1,
				WOUAvg:      500000,
				WOUMax:      500000,
				WOUlt1000us: 1,
				WOUlt1ms:    1,
			},
		},
		{
			name: "wou ge 1ms",
			inputs: []in{
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1002000000)},
					err: nil,
				},
			},
			want: lib.Stats{
				Requests:    1,
				WOUAvg:      2000000,
				WOUMax:      2000000,
				WOUge1000us: 1,
				WOUlt10ms:   1,
			},
		},
		{
			name: "wou at the ms edges",
			inputs: []in{
				{tt: &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1000999999)}},
				{tt: &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1001000000)}},
				{tt: &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1009999999)}},
				{tt: &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1010000000)}},
				{tt: &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1099999999)}},
				{tt: &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1100000000)}},
			},
			want: lib.Stats{
				Requests:    6,
				WOUAvg:      36999999,
				WOUMax:      100000000,
				WOUlt1000us: 1,
				WOUge1000us: 5,
				WOUlt1ms:    1,
				WOUlt10ms:   2,
				WOUlt100ms:  2,
				WOUge100ms:  1,
			},
		},
		{
			name: "wou max tracking",
			inputs: []in{
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1000000100)},
					err: nil,
				},
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1000005000)},
					err: nil,
				},
				{
					tt:  &lib.TrueTime{Earliest: time.Unix(0, 1000000000), Latest: time.Unix(0, 1000000050)},
					err: nil,
				},
			},
			want: lib.Stats{
				Requests:  3,
				WOUAvg:    1716,
				WOUMax:    5000,
				WOUlt10us: 3,
				WOUlt1ms:  3,
			},
		},
		{
			name: "all errors",
			inputs: []in{
				{tt: nil, err: fmt.Errorf("e1")},
				{tt: nil, err: fmt.Errorf("e2")},
				{tt: nil, err: fmt.Errorf("e3")},
			},
			want: lib.Stats{
				Requests: 3,
				Errors:   3,
			},
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			s := &lib.StatsCollector{}
			for _, v := range tt.inputs {
				s.Update(v.tt, v.err)
			}
			require.Equal(t, tt.want, s.Stats())
		})
	}
}

// The two messages are fbclock_strerror's text for FBCLOCK_E_DATA_STALE and
// FBCLOCK_E_NO_DATA, the pair a daemon restart produces.
func TestStatsErrorCauses(t *testing.T) {
	stale := errors.New("reading FBClock TrueTime: shared memory data too old to extrapolate")
	noData := errors.New("reading FBClock TrueTime: no data from daemon error")

	s := &lib.StatsCollector{}
	require.Empty(t, s.ErrorCauses())

	s.Update(&lib.TrueTime{Earliest: time.Unix(0, 1000), Latest: time.Unix(0, 1100)}, nil)
	for range 3 {
		s.Update(nil, stale)
	}
	s.Update(nil, noData)

	require.Equal(t, int64(5), s.Stats().Requests)
	require.Equal(t, int64(4), s.Stats().Errors)
	require.Equal(t, map[string]int64{stale.Error(): 3, noData.Error(): 1}, s.ErrorCauses())

	var total int64
	for _, n := range s.ErrorCauses() {
		total += n
	}
	require.Equal(t, s.Stats().Errors, total)

	// The map is a copy: mutating it must not corrupt the tally.
	s.ErrorCauses()[stale.Error()] = 99
	require.Equal(t, int64(3), s.ErrorCauses()[stale.Error()])
}

// StatsCollector is exported, so a caller can pass error text that varies per
// call. fbclock's own messages are a fixed set of ten.
func TestStatsErrorCausesBounded(t *testing.T) {
	s := &lib.StatsCollector{}
	for i := range 10_000 {
		s.Update(nil, fmt.Errorf("attempt %d failed", i))
	}

	// Spelled out, not read back from the package, so a changed cap fails here.
	causes := s.ErrorCauses()
	require.Len(t, causes, 16)
	require.Equal(t, int64(10_000-15), causes["(other causes)"])

	var total int64
	for _, n := range causes {
		total += n
	}
	require.Equal(t, s.Stats().Errors, total)

	// A message already tallied keeps its entry after the cap is hit.
	s.Update(nil, errors.New("attempt 0 failed"))
	require.Equal(t, int64(2), s.ErrorCauses()["attempt 0 failed"])
	require.Len(t, s.ErrorCauses(), 16)
}

// StatsCollector is exported and holds a map, and the Go runtime fatals the
// whole process on a concurrent map write.
func TestStatsConcurrentUpdate(t *testing.T) {
	const writers, perWriter = 8, 500
	s := &lib.StatsCollector{}

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				s.Update(nil, fmt.Errorf("writer %d cause %d", w, i%4))
			}
		})
	}
	// ErrorCauses clones the map, which is itself a read of every key.
	for range writers {
		wg.Go(func() {
			for range perWriter {
				s.ErrorCauses()
				s.Stats()
			}
		})
	}
	wg.Wait()

	require.Equal(t, int64(writers*perWriter), s.Stats().Errors)
	var total int64
	for _, n := range s.ErrorCauses() {
		total += n
	}
	require.Equal(t, s.Stats().Errors, total, "every error landed in exactly one bucket")
}
