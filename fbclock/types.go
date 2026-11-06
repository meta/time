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

package fbclock

import (
	"maps"
	"math"
	"os"
	"sync"
	"time"
)

// TrueTime is a time interval we are confident the clock is right now
type TrueTime struct {
	Earliest time.Time
	Latest   time.Time
}

// Sources is a set of fbclock.h's FBCLOCK_SOURCE_* flags, combined with |. A
// host with a PTP device serves PTP; one without serves NTP.
type Sources uint32

// Options are caller options applied at init (see fbclock_options)
type Options struct {
	// Sources the caller accepts; 0 means SourcePTP. NTP needs NewFBClockV3, and
	// its windows are orders of magnitude wider than PTP's.
	Sources Sources
}

// Data is a Go equivalent of what we want to store in shared memory for fbclock to use
type Data struct {
	IngressTimeNS        int64
	ErrorBoundNS         uint64
	HoldoverMultiplierNS float64
	SmearingStartS       uint64
	SmearingEndS         uint64
	UTCOffsetPreS        int32
	UTCOffsetPostS       int32
}

// DataV2 is a Go equivalent of what we want to store in shared memory for fbclock to use
type DataV2 struct {
	IngressTimeNS        int64
	ErrorBoundNS         uint64
	HoldoverMultiplierNS float64
	SmearingStartS       uint64
	UTCOffsetPreS        int16
	UTCOffsetPostS       int16
	ClockID              uint32
	PHCTimeNS            int64
	SysclockTimeNS       int64
	CoefPPB              int64
}

// Shm is POSIX shared memory
type Shm struct {
	Path    string
	File    *os.File
	Version int
}

// Stats aggregate stats for fbclock GetTime results
type Stats struct {
	Requests    int64
	Errors      int64
	WOUAvg      int64
	WOUMax      int64
	WOUlt10us   int64
	WOUlt100us  int64
	WOUlt1000us int64
	WOUge1000us int64
}

// StatsCollector collects stats based on GetTime results
type StatsCollector struct {
	mu        sync.Mutex
	stats     Stats
	wouSum    int64
	errCauses map[string]int64
}

// Stats returns collected stats
func (s *StatsCollector) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// The tally is capped so it cannot grow with the request count: a caller's
// error text may vary per call. Overflow pools under otherErrorCauses.
const (
	maxErrorCauses   = 16
	otherErrorCauses = "(other causes)"
)

// ErrorCauses breaks Stats().Errors down by message, which the count alone
// cannot do across fbclock's ten failure modes.
func (s *StatsCollector) ErrorCauses() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.errCauses)
}

// Close cleans up Shm resources
func (s *Shm) Close() error {
	if s.File != nil {
		return s.File.Close()
	}
	return nil
}

const pow2_16 = float64(1 << 16)

// FloatAsUint32 stores float as multiplier of 2**16
func FloatAsUint32(val float64) uint32 {
	valAsUint := pow2_16 * val
	if valAsUint > math.MaxUint32 {
		valAsUint = math.MaxUint32
	}
	return uint32(valAsUint)
}

// Uint32AsFloat restores float that was stored as a multiplier of 2**16
func Uint32AsFloat(val uint32) float64 {
	return float64(val) / pow2_16
}

// Uint64ToUint32 converts uint64 to uint32, capping at MaxUint32
func Uint64ToUint32(val uint64) uint32 {
	if val > math.MaxUint32 {
		val = math.MaxUint32
	}
	return uint32(val)
}
