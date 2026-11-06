//go:build linux

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

/*
#cgo LDFLAGS: -lrt
#cgo amd64 CFLAGS: -msse4.2

// Scoped to this preamble on purpose: a #cgo CFLAGS define would also reach
// fbclock.c, which needs the real atomics. See FBCLOCK_CGO in fbclock.h.
#define FBCLOCK_CGO 1

#include "fbclock.h" // @oss-only
// @fb-only: #include "time/fbclock/fbclock.h"

#include <stdlib.h>   // for free()
*/
import "C"

import (
	"fmt"
	"strings"
	"time"
	"unsafe"
)

// Values for Options.Sources, from fbclock.h
const (
	SourcePTP Sources = C.FBCLOCK_SOURCE_PTP
	// SourcePRTC is PTP on the RTC (CLOCK_REALTIME), read only by gettime_past
	// for network monitoring. If you just want the time, use SourcePTP.
	SourcePRTC Sources = C.FBCLOCK_SOURCE_PRTC
	SourceNTP  Sources = C.FBCLOCK_SOURCE_NTP
	// SourceAny accepts every source, including ones added later
	SourceAny Sources = C.FBCLOCK_SOURCE_ANY
)

var sourceNames = map[string]Sources{"ptp": SourcePTP, "ntp": SourceNTP, "any": SourceAny}

// String implements pflag.Value
func (s Sources) String() string {
	for name, v := range sourceNames {
		if v == s {
			return name
		}
	}
	return fmt.Sprintf("0x%02x", uint32(s))
}

// Set implements pflag.Value
func (s *Sources) Set(name string) error {
	v, ok := sourceNames[strings.ToLower(name)]
	if !ok {
		return fmt.Errorf("use one of %s", s.Type())
	}
	*s = v
	return nil
}

// Type implements pflag.Value
func (s *Sources) Type() string { return "{ptp|ntp|any}" }

func strerror(errCode C.int) string {
	cStr := C.fbclock_strerror(errCode)
	return C.GoString(cStr)
}

// FBClock wraps around fbclock C lib
type FBClock struct {
	cFBClock *C.fbclock_lib
}

// NewFBClockCustom returns new FBClock wrapper with custom path
func NewFBClockCustom(path string) (*FBClock, error) {
	return newFBClock(path, nil)
}

// NewFBClock returns new FBClock wrapper
func NewFBClock() (*FBClock, error) {
	return NewFBClockCustom(C.FBCLOCK_PATH)
}

// NewFBClockV2 returns new FBClock wrapper using v2 data structure
func NewFBClockV2() (*FBClock, error) {
	return NewFBClockCustom(C.FBCLOCK_PATH)
}

// NewFBClockV2WithOptions is NewFBClockV2 with caller options
func NewFBClockV2WithOptions(opts Options) (*FBClock, error) {
	cOpts := C.fbclock_options{sources: C.uint32_t(opts.Sources)}
	return newFBClock(C.FBCLOCK_PATH, &cOpts)
}

// NewFBClockV3 returns new FBClock wrapper reading fbclock data v3, with caller options
func NewFBClockV3(opts Options) (*FBClock, error) {
	cOpts := C.fbclock_options{sources: C.uint32_t(opts.Sources), version: C.FBCLOCK_V3}
	return newFBClock(C.FBCLOCK_DATA_PATH, &cOpts)
}

// opts == nil selects the defaults, as fbclock_init does
func newFBClock(path string, opts *C.fbclock_options) (*FBClock, error) {
	cFBClock := &C.fbclock_lib{}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	errCode := C.fbclock_init_with_options(cFBClock, cPath, opts)
	if errCode != 0 {
		return nil, fmt.Errorf("initializing FBClock: %s", strerror(errCode))
	}
	return &FBClock{cFBClock: cFBClock}, nil
}

// Close destroys fbclock wrapper
func (f *FBClock) Close() error {
	errCode := C.fbclock_destroy(f.cFBClock)
	if errCode != 0 {
		return fmt.Errorf("destroying FBClock: %s", strerror(errCode))
	}
	return nil
}

// GetTime returns TrueTime
func (f *FBClock) GetTime() (*TrueTime, error) {
	tt := &C.fbclock_truetime{}
	errCode := C.fbclock_gettime(f.cFBClock, tt)
	if errCode != 0 {
		return nil, fmt.Errorf("reading FBClock TrueTime: %s", strerror(errCode))
	}

	earliest := time.Unix(0, int64(tt.earliest_ns))
	latest := time.Unix(0, int64(tt.latest_ns))

	return &TrueTime{Earliest: earliest, Latest: latest}, nil
}

// GetTimeUTC returns TrueTime in UTC
func (f *FBClock) GetTimeUTC() (*TrueTime, error) {
	tt := &C.fbclock_truetime{}
	errCode := C.fbclock_gettime_utc(f.cFBClock, tt)
	if errCode != 0 {
		return nil, fmt.Errorf("reading FBClock TrueTime UTC: %s", strerror(errCode))
	}

	earliest := time.Unix(0, int64(tt.earliest_ns))
	latest := time.Unix(0, int64(tt.latest_ns))

	return &TrueTime{Earliest: earliest, Latest: latest}, nil
}

// GetTimePast returns the [earliest, latest] PHC-domain window corresponding
// to a past CLOCK_REALTIME timestamp (e.g. a kernel SO_TIMESTAMPING software
// TX timestamp). ts must come from the same host. v2-only.
//
// If ts is older than the last GM sync, the window is error_bound only (no
// holdover term), so it can look surprisingly tight for an old ts.
func (f *FBClock) GetTimePast(ts time.Time) (*TrueTime, error) {
	tt := &C.fbclock_truetime{}
	errCode := C.fbclock_gettime_past(f.cFBClock, C.int64_t(ts.UnixNano()), tt)
	if errCode != 0 {
		return nil, fmt.Errorf("reading FBClock TrueTime past: %s", strerror(errCode))
	}
	return &TrueTime{
		Earliest: time.Unix(0, int64(tt.earliest_ns)),
		Latest:   time.Unix(0, int64(tt.latest_ns)),
	}, nil
}

// GetTimePastUTC is GetTimePast but returns UTC-adjusted values.
func (f *FBClock) GetTimePastUTC(ts time.Time) (*TrueTime, error) {
	tt := &C.fbclock_truetime{}
	errCode := C.fbclock_gettime_past_utc(f.cFBClock, C.int64_t(ts.UnixNano()), tt)
	if errCode != 0 {
		return nil, fmt.Errorf("reading FBClock TrueTime past UTC: %s", strerror(errCode))
	}
	return &TrueTime{
		Earliest: time.Unix(0, int64(tt.earliest_ns)),
		Latest:   time.Unix(0, int64(tt.latest_ns)),
	}, nil
}
