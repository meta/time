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

package test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	lib "github.com/facebook/time/fbclock"

	"github.com/stretchr/testify/require"
)

// dataOffset is where source's section starts: bit k is line k + 1.
func dataOffset(source lib.Sources) int {
	return (bits.TrailingZeros32(uint32(source)) + 1) * 64
}

func dataSnapshot(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, raw, lib.DataSize)
	return raw
}

func requireDataHeader(t *testing.T, raw []byte, sources lib.Sources) {
	t.Helper()
	require.Equal(t, []byte{'F', 'B', 'C', 'L', 'O', 'C', 'K', 0}, raw[:8])
	require.Equal(t, byte(3), raw[8])
	require.Equal(t, byte(0), raw[9])
	require.Equal(t, make([]byte, 2), raw[10:12])
	require.Equal(t, uint32(sources), binary.NativeEndian.Uint32(raw[12:16]))
	require.Equal(t, make([]byte, 48), raw[16:64])
}

func dataSnapshotSeq(raw []byte, source lib.Sources) uint64 {
	offset := dataOffset(source)
	return binary.NativeEndian.Uint64(raw[offset : offset+8])
}

func dataInt64(t *testing.T, raw []byte) int64 {
	t.Helper()
	var value int64
	_, err := binary.Decode(raw, binary.NativeEndian, &value)
	require.NoError(t, err)
	return value
}

func dataClockFromBytes(t *testing.T, raw []byte, source lib.Sources) lib.DataClock {
	t.Helper()
	offset := dataOffset(source)
	section := raw[offset : offset+64]
	return lib.DataClock{
		RefTimeNS:            dataInt64(t, section[8:16]),
		SysclockTimeNS:       dataInt64(t, section[16:24]),
		CoefPPB:              dataInt64(t, section[24:32]),
		ClockID:              binary.NativeEndian.Uint32(section[32:36]),
		ErrorBoundNS:         uint64(binary.NativeEndian.Uint32(section[36:40])),
		IngressTimeNS:        dataInt64(t, section[40:48]),
		HoldoverMultiplierNS: lib.Uint32AsFloat(binary.NativeEndian.Uint32(section[48:52])),
	}
}

func requireDataReserved(t *testing.T, raw []byte, source lib.Sources) {
	t.Helper()
	offset := dataOffset(source)
	require.Equal(t, make([]byte, 12), raw[offset+52:offset+64])
}

func sampleDataClock() lib.DataClock {
	return lib.DataClock{
		ClockID:              4,
		RefTimeNS:            1700000037000000200,
		SysclockTimeNS:       1000000000200,
		CoefPPB:              -493,
		IngressTimeNS:        1700000036000000000,
		ErrorBoundNS:         17,
		HoldoverMultiplierNS: 64.5,
	}
}

// A new file offers the configured sources that aren't observe-only, and every
// section stays empty, so readers get NO_DATA, until its first store.
func TestFBClockDataFresh(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources map[lib.Sources]lib.DataSectionConfig
		offered lib.Sources
	}{
		{name: "none"},
		{
			name: "ptp_and_ntp",
			sources: map[lib.Sources]lib.DataSectionConfig{
				lib.SourcePTP:  {},
				lib.SourcePRTC: {},
				lib.SourceNTP:  {ObserveOnly: true},
			},
			offered: lib.SourcePTP | lib.SourcePRTC,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			file, err := lib.OpenFBClockDataCustom(path, tc.sources)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			require.Equal(t, path, file.Path)
			raw := dataSnapshot(t, path)
			requireDataHeader(t, raw, tc.offered)
			require.Equal(t, make([]byte, lib.DataSize-64), raw[64:])
		})
	}
}

func TestFBClockDataReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	file, err := lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{lib.SourcePTP: {}})
	require.NoError(t, err)
	clock := sampleDataClock()
	require.NoError(t, file.Store(lib.SourcePTP, clock))
	require.NoError(t, file.Close())
	beforeInfo, err := os.Stat(path)
	require.NoError(t, err)
	before := dataSnapshot(t, path)
	before[9] = 42
	before[10] = 0xaa
	for _, source := range []lib.Sources{lib.SourcePTP, lib.SourceNTP} {
		offset := dataOffset(source)
		copy(before[offset+52:offset+64], bytes.Repeat([]byte{0xcc}, 12))
	}
	require.NoError(t, os.WriteFile(path, before, 0o600))

	file, err = lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{
		lib.SourcePTP: {},
		lib.SourceNTP: {ObserveOnly: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	afterInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, os.SameFile(beforeInfo, afterInfo))
	after := dataSnapshot(t, path)
	requireDataHeader(t, after, lib.SourcePTP)
	require.Equal(t, clock, dataClockFromBytes(t, after, lib.SourcePTP))
	require.Greater(t, dataSnapshotSeq(after, lib.SourcePTP), dataSnapshotSeq(before, lib.SourcePTP))
	require.Zero(t, dataSnapshotSeq(after, lib.SourcePTP)&1)
	requireDataReserved(t, after, lib.SourcePTP)
	ntp := dataOffset(lib.SourceNTP)
	require.Equal(t, make([]byte, 64), after[ntp:ntp+64])
}

func TestFBClockDataChangedSetup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	sections := map[lib.Sources]lib.DataSectionConfig{
		lib.SourcePTP:  {},
		lib.SourcePRTC: {},
		lib.SourceNTP:  {},
	}
	file, err := lib.OpenFBClockDataCustom(path, sections)
	require.NoError(t, err)
	for source := range sections {
		require.NoError(t, file.Store(source, sampleDataClock()))
	}
	require.NoError(t, file.Close())
	before := dataSnapshot(t, path)
	requireDataHeader(t, before, lib.SourcePTP|lib.SourcePRTC|lib.SourceNTP)

	file, err = lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{
		lib.SourceNTP: {ObserveOnly: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	after := dataSnapshot(t, path)
	requireDataHeader(t, after, 0)
	for _, source := range []lib.Sources{lib.SourcePTP, lib.SourcePRTC} {
		offset := dataOffset(source)
		require.Equal(t, make([]byte, 56), after[offset+8:offset+64])
		require.Greater(t, dataSnapshotSeq(after, source), dataSnapshotSeq(before, source))
		require.Zero(t, dataSnapshotSeq(after, source)&1)
	}
	// Observe-only keeps NTP's anchor and only takes it out of the header.
	require.Equal(t, sampleDataClock(), dataClockFromBytes(t, after, lib.SourceNTP))
	spare := dataOffset(lib.SourceNTP << 1)
	require.Equal(t, make([]byte, 64), after[spare:spare+64])

	// Another start with the same setup leaves the cleared sections alone.
	require.NoError(t, file.Close())
	file, err = lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{
		lib.SourceNTP: {ObserveOnly: true},
	})
	require.NoError(t, err)
	again := dataSnapshot(t, path)
	for _, source := range []lib.Sources{lib.SourcePTP, lib.SourcePRTC} {
		offset := dataOffset(source)
		require.Equal(t, after[offset:offset+64], again[offset:offset+64])
	}
}

func TestFBClockDataMappedReaderAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	sources := map[lib.Sources]lib.DataSectionConfig{lib.SourcePTP: {}}
	file, err := lib.OpenFBClockDataCustom(path, sources)
	require.NoError(t, err)
	clock := sampleDataClock()
	require.NoError(t, file.Store(lib.SourcePTP, clock))
	reader, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	mapping, err := syscall.Mmap(int(reader.Fd()), 0, lib.DataSize, syscall.PROT_READ, syscall.MAP_SHARED)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syscall.Munmap(mapping)) })
	require.Equal(t, clock, dataClockFromBytes(t, mapping, lib.SourcePTP))
	require.NoError(t, file.Close())

	file, err = lib.OpenFBClockDataCustom(path, sources)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	// Until the restarted writer stores, readers keep serving the last anchor.
	require.Equal(t, clock, dataClockFromBytes(t, mapping, lib.SourcePTP))
	clock.RefTimeNS += 1234
	clock.SysclockTimeNS += 5678
	clock.CoefPPB = -491
	clock.IngressTimeNS += 1000000000
	clock.ErrorBoundNS = 50
	clock.HoldoverMultiplierNS = 32.25
	require.NoError(t, file.Store(lib.SourcePTP, clock))
	require.Equal(t, clock, dataClockFromBytes(t, mapping, lib.SourcePTP))
}

func TestFBClockDataForeignFile(t *testing.T) {
	for _, tc := range []struct {
		name         string
		size         int
		major        byte
		corruptMagic bool
	}{
		{name: "magic", size: 4096, major: 3, corruptMagic: true},
		{name: "major", size: 4096, major: 4},
		{name: "short", size: 256, major: 3},
		{name: "large", size: 8192, major: 3},
		{name: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			raw := bytes.Repeat([]byte{0xa5}, tc.size)
			copy(raw, []byte{'F', 'B', 'C', 'L', 'O', 'C', 'K', 0})
			if len(raw) > 8 {
				raw[8] = tc.major
			}
			if tc.corruptMagic {
				raw[7] = 'X'
			}
			for offset := 64; offset+64 <= len(raw); offset += 64 {
				binary.NativeEndian.PutUint64(raw[offset:offset+8], 8)
			}
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			beforeInfo, err := os.Stat(path)
			require.NoError(t, err)
			file, err := lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{lib.SourceNTP: {}})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			afterInfo, err := os.Stat(path)
			require.NoError(t, err)
			require.True(t, os.SameFile(beforeInfo, afterInfo))
			raw = dataSnapshot(t, path)
			requireDataHeader(t, raw, lib.SourceNTP)
			require.Equal(t, make([]byte, lib.DataSize-64), raw[64:])
		})
	}
}

func TestFBClockDataOddSequenceRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	sources := map[lib.Sources]lib.DataSectionConfig{lib.SourcePTP: {}}
	file, err := lib.OpenFBClockDataCustom(path, sources)
	require.NoError(t, err)
	clock := sampleDataClock()
	require.NoError(t, file.Store(lib.SourcePTP, clock))
	require.NoError(t, file.Close())
	raw := dataSnapshot(t, path)
	oddSeq := dataSnapshotSeq(raw, lib.SourcePTP) | 1
	ptp := dataOffset(lib.SourcePTP)
	binary.NativeEndian.PutUint64(raw[ptp:ptp+8], oddSeq)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	file, err = lib.OpenFBClockDataCustom(path, sources)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	// Opening alone settles the section the writer died in, before any store.
	raw = dataSnapshot(t, path)
	requireDataHeader(t, raw, lib.SourcePTP)
	require.Greater(t, dataSnapshotSeq(raw, lib.SourcePTP), oddSeq)
	require.Zero(t, dataSnapshotSeq(raw, lib.SourcePTP)&1)
	require.Equal(t, lib.DataClock{}, dataClockFromBytes(t, raw, lib.SourcePTP))
	clock.RefTimeNS += 1234
	require.NoError(t, file.Store(lib.SourcePTP, clock))
	require.Equal(t, clock, dataClockFromBytes(t, dataSnapshot(t, path), lib.SourcePTP))
}

// An observe-only section is written but not offered, and a store to a source
// that isn't configured changes nothing.
func TestFBClockDataStoreOnlyWritesConfiguredSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	file, err := lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{
		lib.SourceNTP: {ObserveOnly: true},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	clock := sampleDataClock()
	require.NoError(t, file.Store(lib.SourceNTP, clock))
	before := dataSnapshot(t, path)
	requireDataHeader(t, before, 0)
	require.Equal(t, clock, dataClockFromBytes(t, before, lib.SourceNTP))
	for _, source := range []lib.Sources{0, lib.SourcePTP, lib.SourcePTP | lib.SourceNTP, lib.SourceNTP << 1, lib.SourceAny} {
		require.Error(t, file.Store(source, clock))
	}
	require.Equal(t, before, dataSnapshot(t, path))
}

// A rate the bound can't grow with is written as UINT32_MAX, which readers refuse.
func TestFBClockDataUnusableHoldoverIsTooBig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	file, err := lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{lib.SourcePTP: {}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	holdover := dataOffset(lib.SourcePTP) + 48
	for rate, want := range map[float64]uint32{
		0:            0,
		0.25:         16384,
		65535.99:     4294966640,
		65536:        math.MaxUint32,
		math.Inf(1):  math.MaxUint32,
		-0.25:        math.MaxUint32,
		math.Inf(-1): math.MaxUint32,
		math.NaN():   math.MaxUint32,
	} {
		clock := sampleDataClock()
		clock.HoldoverMultiplierNS = rate
		require.NoError(t, file.Store(lib.SourcePTP, clock))
		raw := dataSnapshot(t, path)
		require.Equal(t, want, binary.NativeEndian.Uint32(raw[holdover:holdover+4]), "rate %v", rate)
	}
}

// An error bound past 32 bits is written as UINT32_MAX, which readers refuse.
func TestFBClockDataOversizedErrorBoundIsTooBig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	file, err := lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{lib.SourcePTP: {}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	bound := dataOffset(lib.SourcePTP) + 36
	for errorBound, want := range map[uint64]uint32{
		math.MaxUint32 - 1: math.MaxUint32 - 1,
		math.MaxUint32:     math.MaxUint32,
		math.MaxUint32 + 1: math.MaxUint32,
		math.MaxUint64:     math.MaxUint32,
	} {
		clock := sampleDataClock()
		clock.ErrorBoundNS = errorBound
		require.NoError(t, file.Store(lib.SourcePTP, clock))
		raw := dataSnapshot(t, path)
		require.Equal(t, want, binary.NativeEndian.Uint32(raw[bound:bound+4]), "bound %d", errorBound)
	}
}

// A section the host stops serving is cleared even if a write was cut off in it.
func TestFBClockDataClearsAnUnservedSectionLeftMidWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	file, err := lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{lib.SourceNTP: {}})
	require.NoError(t, err)
	require.NoError(t, file.Store(lib.SourceNTP, sampleDataClock()))
	require.NoError(t, file.Close())
	raw := dataSnapshot(t, path)
	oddSeq := dataSnapshotSeq(raw, lib.SourceNTP) | 1
	ntp := dataOffset(lib.SourceNTP)
	binary.NativeEndian.PutUint64(raw[ntp:ntp+8], oddSeq)
	require.NoError(t, os.WriteFile(path, raw, 0o600))

	file, err = lib.OpenFBClockDataCustom(path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	raw = dataSnapshot(t, path)
	requireDataHeader(t, raw, 0)
	require.Greater(t, dataSnapshotSeq(raw, lib.SourceNTP), oddSeq)
	require.Zero(t, dataSnapshotSeq(raw, lib.SourceNTP)&1)
	require.Equal(t, make([]byte, 56), raw[ntp+8:ntp+64])
}

func TestFBClockDataRefusesNonRegularPaths(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			require.NoError(t, os.WriteFile(target, []byte("not fbclock"), 0o600))
			path := filepath.Join(dir, "data")
			if kind == "symlink" {
				require.NoError(t, os.Symlink(target, path))
			} else {
				require.NoError(t, syscall.Mkfifo(path, 0o600))
			}
			file, err := lib.OpenFBClockDataCustom(path, nil)
			require.Error(t, err)
			require.Nil(t, file)
			raw, err := os.ReadFile(target)
			require.NoError(t, err)
			require.Equal(t, []byte("not fbclock"), raw)
		})
	}
}

// The file is 0644 for readers even if it was left behind with another mode or
// is created under a tight umask.
func TestFBClockDataRestoresReaderMode(t *testing.T) {
	for _, how := range []string{"left behind", "tight umask"} {
		t.Run(how, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			if how == "left behind" {
				file, err := lib.OpenFBClockDataCustom(path, nil)
				require.NoError(t, err)
				require.NoError(t, file.Close())
				require.NoError(t, os.Chmod(path, 0o600))
			} else {
				defer syscall.Umask(syscall.Umask(0o077))
			}
			file, err := lib.OpenFBClockDataCustom(path, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, file.Close()) })
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
		})
	}
}

// Only one source bit names a section.
func TestFBClockDataClosedAndInvalidSection(t *testing.T) {
	dir := t.TempDir()
	for _, source := range []lib.Sources{0, lib.SourcePTP | lib.SourceNTP, lib.SourceAny} {
		path := filepath.Join(dir, fmt.Sprintf("invalid_%x", uint32(source)))
		invalid, err := lib.OpenFBClockDataCustom(path, map[lib.Sources]lib.DataSectionConfig{source: {}})
		require.Error(t, err)
		require.Nil(t, invalid)
		require.NoFileExists(t, path)
	}
	file, err := lib.OpenFBClockDataCustom(filepath.Join(dir, "data"), map[lib.Sources]lib.DataSectionConfig{
		lib.SourcePTP: {},
	})
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.NoError(t, file.Close())
	require.Error(t, file.Store(lib.SourcePTP, sampleDataClock()))
}
