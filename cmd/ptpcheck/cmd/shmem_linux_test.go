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

package cmd

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/facebook/time/fbclock"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// The header shmemTestV3File leaves: PTP, plus bit 7, which only a newer
// daemon offers.
const shmemTestSources = uint32(fbclock.SourcePTP) | 1<<7

func shmemTestV3File(t *testing.T, clock fbclock.DataClock) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	file, err := fbclock.OpenFBClockDataCustom(path, map[fbclock.Sources]fbclock.DataSectionConfig{
		fbclock.SourcePTP: {},
	})
	require.NoError(t, err)
	require.NoError(t, file.Store(fbclock.SourcePTP, clock))
	require.NoError(t, file.Close())
	fixture, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = fixture.WriteAt(binary.NativeEndian.AppendUint32(nil, shmemTestSources), 12)
	require.NoError(t, err)
	require.NoError(t, fixture.Close())
	return path
}

func shmemTestRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	command := newShmemCommand()
	command.SetOut(&out)
	command.SetErr(&stderr)
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

func TestShmemFlagDefaults(t *testing.T) {
	command := newShmemCommand()
	path := command.Flags().Lookup("path")
	require.NotNil(t, path)
	require.Equal(t, fbclock.DataPath, path.DefValue)
	jsonFlag := command.Flags().ShorthandLookup("j")
	require.NotNil(t, jsonFlag)
	require.Equal(t, "json", jsonFlag.Name)
	require.Equal(t, "false", jsonFlag.DefValue)
	v2 := command.Flags().Lookup("v2")
	require.NotNil(t, v2)
	require.Equal(t, "false", v2.DefValue)
}

func TestShmemJSONRawFields(t *testing.T) {
	clock := fbclock.DataClock{
		ClockID:              math.MaxUint32,
		RefTimeNS:            1790000000000000000,
		SysclockTimeNS:       12345,
		CoefPPB:              -123,
		IngressTimeNS:        1790000000000000001,
		ErrorBoundNS:         math.MaxUint32 + 1,
		HoldoverMultiplierNS: 65536,
	}
	path := shmemTestV3File(t, clock)
	out, err := shmemTestRun(t, "--path", path, "-j")
	require.NoError(t, err)
	var data fbclock.DataFileSnapshot
	require.NoError(t, json.Unmarshal([]byte(out), &data))
	require.Equal(t, path, data.Path)
	require.Equal(t, 3, data.Version)
	require.Equal(t, int64(fbclock.DataSize), data.Size)
	require.NotNil(t, data.Header)
	require.Equal(t, "FBCLOCK\x00", data.Header.Magic)
	require.Equal(t, uint8(3), data.Header.Major)
	require.Equal(t, uint8(0), data.Header.Minor)
	require.Equal(t, shmemTestSources, data.Header.Sources)
	require.Len(t, data.Sections, 4)
	require.Equal(t, "bit_7", data.Sections[3].Name)
	require.Zero(t, data.Sections[3].Seq)
	section := data.Sections[0]
	require.Equal(t, "ptp", section.Name)
	require.Equal(t, 64, section.Offset)
	require.NotZero(t, section.Seq)
	require.Zero(t, section.Seq%2)
	require.Empty(t, section.ReadError)
	raw := section.Clock
	require.NotNil(t, raw)
	require.Equal(t, clock.ClockID, raw.ClockID)
	require.NotNil(t, raw.RefTimeNS)
	require.Equal(t, clock.RefTimeNS, *raw.RefTimeNS)
	require.Nil(t, raw.PHCTimeNS)
	require.Equal(t, clock.SysclockTimeNS, raw.SysclockTimeNS)
	require.Equal(t, clock.CoefPPB, raw.CoefPPB)
	require.Equal(t, clock.IngressTimeNS, raw.IngressTimeNS)
	require.Equal(t, uint64(math.MaxUint32), raw.ErrorBoundNS)
	require.Equal(t, uint32(math.MaxUint32), raw.HoldoverMultiplierRaw)
	require.Equal(t, fbclock.Uint32AsFloat(math.MaxUint32), raw.HoldoverMultiplierNS)
	require.NotNil(t, raw.Reserved)
	require.Equal(t, [12]uint8{}, *raw.Reserved)
	require.Nil(t, raw.SmearingStartS)
	require.Nil(t, raw.UTCOffsetPreS)
	require.Nil(t, raw.UTCOffsetPostS)
	require.Equal(t, "unsupported base clock 4294967295", section.AgeError)
	require.Nil(t, section.AnchorAgeNS)
	require.Nil(t, section.SinceIngressNS)
}

func TestShmemTextRawFields(t *testing.T) {
	clock := fbclock.DataClock{
		ClockID:              math.MaxUint32,
		RefTimeNS:            1790000000000000000,
		SysclockTimeNS:       12345,
		CoefPPB:              -123,
		IngressTimeNS:        1790000000000000001,
		ErrorBoundNS:         math.MaxUint32,
		HoldoverMultiplierNS: 65536,
	}
	path := shmemTestV3File(t, clock)
	out, err := shmemTestRun(t, "--path", path)
	require.NoError(t, err)
	// The base clock is unknown, so there are no ages and the output is exact.
	want := "Path: " + path + `
Version: 3
Size: 4096 bytes
Header:
  magic: FBCLOCK
  major: 3
  minor: 0
  sources: 0x00000081 (ptp|bit_7)

ptp (offset 64):
  seq: 2
  clock_id: 4294967295 (unknown)
  ref_time_ns: 1790000000000000000
  sysclock_time_ns: 12345
  coef_ppb: -123
  ingress_time_ns: 1790000000000000001
  error_bound_ns: 4294967295
  holdover_multiplier_raw: 4294967295
  holdover_multiplier_ns: 65535.99998474121 ns/s
  reserved: [0 0 0 0 0 0 0 0 0 0 0 0]
  age_error: unsupported base clock 4294967295

prtc (offset 128):
  seq: 0
  read_error: no data from daemon error

ntp (offset 192):
  seq: 0
  read_error: no data from daemon error

bit_7 (offset 512):
  seq: 0
  read_error: no data from daemon error
`
	require.Equal(t, want, out)
}

func TestShmemPrintTextV2(t *testing.T) {
	data := &fbclock.DataFileSnapshot{
		Path: "/run/fbclock/data_v2", Version: 2, Size: 128,
		Sections: []fbclock.DataSectionSnapshot{
			{
				Name: "primary", Offset: 0, Seq: 6,
				Clock: &fbclock.DataClockSnapshot{
					ClockID: unix.CLOCK_MONOTONIC_RAW, PHCTimeNS: new(int64(1790000000000000000)),
					SysclockTimeNS: 1000, CoefPPB: -493, IngressTimeNS: 1789999999000000000,
					ErrorBoundNS: 17, HoldoverMultiplierRaw: 4227072, HoldoverMultiplierNS: 64.5,
					SmearingStartS: new(uint64(1483228836)), UTCOffsetPreS: new(int16(36)), UTCOffsetPostS: new(int16(37)),
				},
				AnchorAgeNS: new(int64(250)), SinceIngressNS: new(int64(1000000250)),
			},
			{Name: "realtime", Offset: 64, ReadError: "no data from daemon error"},
		},
	}
	var out strings.Builder
	require.NoError(t, shmemPrintText(&out, data))
	require.Equal(t, `Path: /run/fbclock/data_v2
Version: 2
Size: 128 bytes

primary (offset 0):
  seq: 6
  clock_id: 4 (CLOCK_MONOTONIC_RAW)
  phc_time_ns: 1790000000000000000
  sysclock_time_ns: 1000
  coef_ppb: -493
  ingress_time_ns: 1789999999000000000
  error_bound_ns: 17
  holdover_multiplier_raw: 4227072
  holdover_multiplier_ns: 64.5 ns/s
  clock_smearing_start_s: 1483228836
  utc_offset_pre_s: 36
  utc_offset_post_s: 37
  anchor_age_ns: 250
  since_ingress_ns: 1000000250

realtime (offset 64):
  seq: 0
  read_error: no data from daemon error
`, out.String())
}

func TestShmemV2ExplicitPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data_v2")
	file, err := fbclock.OpenFBClockShmCustomVer(path, 2)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	primary := fbclock.DataV2{
		IngressTimeNS:        1790000000000000001,
		ErrorBoundNS:         math.MaxUint32,
		HoldoverMultiplierNS: 65536,
		SmearingStartS:       1483228836,
		UTCOffsetPreS:        -36,
		UTCOffsetPostS:       37,
		ClockID:              unix.CLOCK_MONOTONIC_RAW,
		PHCTimeNS:            1790000000000000000,
		SysclockTimeNS:       1000,
		CoefPPB:              -493,
	}
	realtime := primary
	realtime.ClockID = unix.CLOCK_REALTIME
	realtime.CoefPPB = 491
	require.NoError(t, fbclock.StoreFBClockDataV2(file.File.Fd(), primary))
	require.NoError(t, fbclock.StoreFBClockDataRealtime(file.File.Fd(), realtime))
	out, err := shmemTestRun(t, "--v2", "--path", path, "--json")
	require.NoError(t, err)
	var data fbclock.DataFileSnapshot
	require.NoError(t, json.Unmarshal([]byte(out), &data))
	require.Equal(t, path, data.Path)
	require.Equal(t, 2, data.Version)
	require.Equal(t, int64(128), data.Size)
	require.Nil(t, data.Header)
	require.Len(t, data.Sections, 2)
	for i, expected := range []fbclock.DataV2{primary, realtime} {
		section := data.Sections[i]
		require.Empty(t, section.ReadError)
		require.NotZero(t, section.Seq)
		raw := section.Clock
		require.NotNil(t, raw)
		require.Nil(t, raw.Reserved)
		require.Nil(t, raw.RefTimeNS)
		require.NotNil(t, raw.PHCTimeNS)
		require.Equal(t, expected.PHCTimeNS, *raw.PHCTimeNS)
		require.Equal(t, expected.CoefPPB, raw.CoefPPB)
		require.NotNil(t, raw.SmearingStartS)
		require.Equal(t, expected.SmearingStartS, *raw.SmearingStartS)
		require.NotNil(t, raw.UTCOffsetPreS)
		require.Equal(t, expected.UTCOffsetPreS, *raw.UTCOffsetPreS)
		require.NotNil(t, raw.UTCOffsetPostS)
		require.Equal(t, expected.UTCOffsetPostS, *raw.UTCOffsetPostS)
	}
}

func TestShmemEmptyAndBusySections(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2=%t", v2), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			var args []string
			var offset int64
			if v2 {
				file, err := fbclock.OpenFBClockShmCustomVer(path, 2)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, file.Close()) })
				args = append(args, "--v2")
			} else {
				file, err := fbclock.OpenFBClockDataCustom(path, nil)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, file.Close()) })
				offset = 64
			}
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			require.NoError(t, err)
			var seq [8]byte
			binary.NativeEndian.PutUint64(seq[:], 1)
			_, err = file.WriteAt(seq[:], offset)
			require.NoError(t, err)
			require.NoError(t, file.Close())
			args = append(args, "--path", path, "-j")
			out, err := shmemTestRun(t, args...)
			require.NoError(t, err)
			var data fbclock.DataFileSnapshot
			require.NoError(t, json.Unmarshal([]byte(out), &data))
			require.GreaterOrEqual(t, len(data.Sections), 2)
			busy, empty := data.Sections[0], data.Sections[1]
			require.Equal(t, uint64(1), busy.Seq)
			require.NotEmpty(t, busy.ReadError)
			require.Nil(t, busy.Clock)
			require.Zero(t, empty.Seq)
			require.NotEmpty(t, empty.ReadError)
			require.Nil(t, empty.Clock)
			out, err = shmemTestRun(t, args[:len(args)-1]...)
			require.NoError(t, err)
			want := "Path: " + path + `
Version: 3
Size: 4096 bytes
Header:
  magic: FBCLOCK
  major: 3
  minor: 0
  sources: 0x00000000 (none)

ptp (offset 64):
  seq: 1
  read_error: CRC check failed all tries

prtc (offset 128):
  seq: 0
  read_error: no data from daemon error

ntp (offset 192):
  seq: 0
  read_error: no data from daemon error
`
			if v2 {
				want = "Path: " + path + `
Version: 2
Size: 128 bytes

primary (offset 0):
  seq: 1
  read_error: CRC check failed all tries

realtime (offset 64):
  seq: 0
  read_error: no data from daemon error
`
			}
			require.Equal(t, want, out)
		})
	}
}

func TestShmemFileErrors(t *testing.T) {
	for _, kind := range []string{"missing", "small", "magic", "major"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			if kind == "small" {
				require.NoError(t, os.WriteFile(path, make([]byte, 64), 0o600))
			} else if kind != "missing" {
				file, err := fbclock.OpenFBClockDataCustom(path, nil)
				require.NoError(t, err)
				require.NoError(t, file.Close())
				fileToCorrupt, err := os.OpenFile(path, os.O_WRONLY, 0)
				require.NoError(t, err)
				var offset int64
				value := byte('X')
				if kind == "major" {
					offset = 8
					value = 4
				}
				_, err = fileToCorrupt.WriteAt([]byte{value}, offset)
				require.NoError(t, err)
				require.NoError(t, fileToCorrupt.Close())
			}
			out, err := shmemTestRun(t, "--path", path, "-j")
			require.Error(t, err)
			require.Empty(t, out)
		})
	}
}
