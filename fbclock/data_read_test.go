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

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// dataReadSection is a v3 section a fixture writes through the writer.
type dataReadSection struct {
	config DataSectionConfig
	clock  DataClock
}

// dataReadOffset is where source's section starts: bit k is line k + 1.
func dataReadOffset(source Sources) int {
	return (bits.TrailingZeros32(uint32(source)) + 1) * 64
}

func dataReadV3Fixture(t *testing.T, sections map[Sources]dataReadSection) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	configs := make(map[Sources]DataSectionConfig, len(sections))
	for source, fixture := range sections {
		configs[source] = fixture.config
	}
	file, err := OpenFBClockDataCustom(path, configs)
	require.NoError(t, err)
	for source, fixture := range sections {
		require.NoError(t, file.Store(source, fixture.clock))
	}
	require.NoError(t, file.Close())
	return path
}

func dataReadV2Fixture(t *testing.T, primary, realtime *DataV2) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data_v2")
	file, err := OpenFBClockShmCustomVer(path, 2)
	require.NoError(t, err)
	t.Cleanup(func() { _ = file.Close() })
	if primary != nil {
		require.NoError(t, StoreFBClockDataV2(file.File.Fd(), *primary))
	}
	if realtime != nil {
		require.NoError(t, StoreFBClockDataRealtime(file.File.Fd(), *realtime))
	}
	require.NoError(t, file.Close())
	return path
}

// dataReadPatch overwrites a fixture file's bytes at offset, as a newer or
// broken writer could leave them.
func dataReadPatch(t *testing.T, path string, offset int, patch ...byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteAt(patch, int64(offset))
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

func TestReadFBClockDataV3Raw(t *testing.T) {
	path := dataReadV3Fixture(t, map[Sources]dataReadSection{
		SourcePTP: {
			clock: DataClock{
				ClockID: 4, RefTimeNS: 1000, SysclockTimeNS: 100, CoefPPB: -500000000, IngressTimeNS: 700,
				ErrorBoundNS: math.MaxUint64, HoldoverMultiplierNS: 100000,
			},
		},
		SourcePRTC: {
			config: DataSectionConfig{ObserveOnly: true},
			clock: DataClock{
				ClockID: 0, RefTimeNS: 2000, SysclockTimeNS: 500, CoefPPB: 500000000, IngressTimeNS: 1900,
				ErrorBoundNS: 50, HoldoverMultiplierNS: 0.3125,
			},
		},
		SourceNTP: {
			config: DataSectionConfig{ObserveOnly: true},
			clock: DataClock{
				ClockID: 0, RefTimeNS: 3000, SysclockTimeNS: 350, IngressTimeNS: 2800,
				ErrorBoundNS: 300000, HoldoverMultiplierNS: 0.25,
			},
		},
	})
	reserved := [12]uint8{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	dataReadPatch(t, path, 9, 6)
	// The header offers bit 5 too, which this reader doesn't know.
	dataReadPatch(t, path, 12, binary.NativeEndian.AppendUint32(nil, uint32(SourcePTP)|1<<5)...)
	dataReadPatch(t, path, dataReadOffset(SourcePTP)+52, reserved[:]...)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	snapshot, err := ReadFBClockDataFile(path, false)
	require.NoError(t, err)
	require.Equal(t, path, snapshot.Path)
	require.Equal(t, 3, snapshot.Version)
	require.Equal(t, int64(4096), snapshot.Size)
	require.Equal(t, &DataHeaderSnapshot{Magic: "FBCLOCK\x00", Major: 3, Minor: 6, Sources: uint32(SourcePTP) | 1<<5}, snapshot.Header)
	require.Len(t, snapshot.Sections, 4)
	for i, source := range []Sources{SourcePTP, SourcePRTC, SourceNTP} {
		require.Equal(t, DataSectionName(source), snapshot.Sections[i].Name)
		require.Equal(t, dataReadOffset(source), snapshot.Sections[i].Offset)
		require.Equal(t, uint64(2), snapshot.Sections[i].Seq)
		require.Empty(t, snapshot.Sections[i].ReadError)
		require.Empty(t, snapshot.Sections[i].AgeError)
		require.NotNil(t, snapshot.Sections[i].AnchorAgeNS)
		require.NotNil(t, snapshot.Sections[i].SinceIngressNS)
	}
	require.Equal(t, []string{"ptp", "prtc", "ntp"}, []string{snapshot.Sections[0].Name, snapshot.Sections[1].Name, snapshot.Sections[2].Name})
	// bit 5 is offered but never written
	require.Equal(t, "bit_5", snapshot.Sections[3].Name)
	require.Zero(t, snapshot.Sections[3].Seq)
	require.NotEmpty(t, snapshot.Sections[3].ReadError)
	require.Equal(t, &DataClockSnapshot{
		Reserved: &reserved, ClockID: 4,
		RefTimeNS: new(int64(1000)), SysclockTimeNS: 100,
		CoefPPB: -500000000, IngressTimeNS: 700, ErrorBoundNS: math.MaxUint32,
		HoldoverMultiplierRaw: math.MaxUint32, HoldoverMultiplierNS: float64(math.MaxUint32) / 65536,
	}, snapshot.Sections[0].Clock)
	require.Equal(t, &DataClockSnapshot{
		Reserved: &[12]uint8{}, ClockID: 0,
		RefTimeNS: new(int64(2000)), SysclockTimeNS: 500,
		CoefPPB: 500000000, IngressTimeNS: 1900, ErrorBoundNS: 50,
		HoldoverMultiplierRaw: 20480, HoldoverMultiplierNS: 0.3125,
	}, snapshot.Sections[1].Clock)
	require.Equal(t, &DataClockSnapshot{
		Reserved: &[12]uint8{}, ClockID: 0,
		RefTimeNS: new(int64(3000)), SysclockTimeNS: 350,
		IngressTimeNS: 2800, ErrorBoundNS: 300000,
		HoldoverMultiplierRaw: 16384, HoldoverMultiplierNS: 0.25,
	}, snapshot.Sections[2].Clock)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestReadFBClockDataV2Raw(t *testing.T) {
	path := dataReadV2Fixture(t, &DataV2{
		ClockID: 4, PHCTimeNS: 1000, SysclockTimeNS: 100, CoefPPB: -500000000, IngressTimeNS: 700,
		ErrorBoundNS: math.MaxUint64, HoldoverMultiplierNS: 64.5,
		SmearingStartS: 1483228836, UTCOffsetPreS: math.MinInt16, UTCOffsetPostS: math.MaxInt16,
	}, &DataV2{
		ClockID: 0, PHCTimeNS: 2000, SysclockTimeNS: 500, CoefPPB: 500000000, IngressTimeNS: 1900,
		ErrorBoundNS: 50, HoldoverMultiplierNS: 0.3125,
	})
	snapshot, err := ReadFBClockDataFile(path, true)
	require.NoError(t, err)
	require.Equal(t, path, snapshot.Path)
	require.Equal(t, 2, snapshot.Version)
	require.Equal(t, int64(128), snapshot.Size)
	require.Nil(t, snapshot.Header)
	require.Len(t, snapshot.Sections, 2)
	for i, name := range []string{"primary", "realtime"} {
		require.Equal(t, name, snapshot.Sections[i].Name)
		require.Equal(t, i*64, snapshot.Sections[i].Offset)
		require.Equal(t, uint64(2), snapshot.Sections[i].Seq)
		require.Empty(t, snapshot.Sections[i].ReadError)
		require.Empty(t, snapshot.Sections[i].AgeError)
		require.NotNil(t, snapshot.Sections[i].AnchorAgeNS)
		require.NotNil(t, snapshot.Sections[i].SinceIngressNS)
	}
	require.Equal(t, &DataClockSnapshot{
		ClockID: 4, PHCTimeNS: new(int64(1000)), SysclockTimeNS: 100,
		CoefPPB: -500000000, IngressTimeNS: 700, ErrorBoundNS: math.MaxUint32,
		HoldoverMultiplierRaw: 4227072, HoldoverMultiplierNS: 64.5,
		SmearingStartS: new(uint64(1483228836)),
		UTCOffsetPreS:  new(int16(math.MinInt16)), UTCOffsetPostS: new(int16(math.MaxInt16)),
	}, snapshot.Sections[0].Clock)
	require.Equal(t, &DataClockSnapshot{
		ClockID: 0, PHCTimeNS: new(int64(2000)), SysclockTimeNS: 500,
		CoefPPB: 500000000, IngressTimeNS: 1900, ErrorBoundNS: 50,
		HoldoverMultiplierRaw: 20480, HoldoverMultiplierNS: 0.3125,
		SmearingStartS: new(uint64(0)), UTCOffsetPreS: new(int16(0)), UTCOffsetPostS: new(int16(0)),
	}, snapshot.Sections[1].Clock)
}

func TestReadFBClockDataSpareSections(t *testing.T) {
	path := dataReadV3Fixture(t, map[Sources]dataReadSection{
		SourceNTP: {
			config: DataSectionConfig{ObserveOnly: true},
			clock:  DataClock{ClockID: 0, RefTimeNS: 3000, SysclockTimeNS: 350},
		},
	})
	// Sections of bits this reader doesn't know, as a newer daemon could write
	// them: NTP copies, a busy one, a cleared one and bit 6, offered but unwritten.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	ntpLine := raw[dataReadOffset(SourceNTP) : dataReadOffset(SourceNTP)+64]
	dataReadPatch(t, path, dataReadOffset(1<<3), ntpLine...)
	dataReadPatch(t, path, dataReadOffset(1<<31), ntpLine...)
	dataReadPatch(t, path, 40*64, ntpLine...) // line 40 has no bit, so it's no section
	dataReadPatch(t, path, dataReadOffset(1<<4), binary.NativeEndian.AppendUint64(nil, 9)...)
	dataReadPatch(t, path, dataReadOffset(1<<5), binary.NativeEndian.AppendUint64(nil, 2)...)
	dataReadPatch(t, path, 12, binary.NativeEndian.AppendUint32(nil, 1<<6)...)
	snapshot, err := ReadFBClockDataFile(path, false)
	require.NoError(t, err)
	names := make([]string, 0, len(snapshot.Sections))
	for _, section := range snapshot.Sections {
		names = append(names, section.Name)
	}
	require.Equal(t, []string{"ptp", "prtc", "ntp", "bit_3", "bit_4", "bit_5", "bit_6", "bit_31"}, names)
	for _, section := range snapshot.Sections[:2] {
		require.Zero(t, section.Seq)
		require.Nil(t, section.Clock)
		require.NotEmpty(t, section.ReadError)
	}
	require.NotNil(t, snapshot.Sections[2].Clock)
	require.Equal(t, dataReadOffset(1<<3), snapshot.Sections[3].Offset)
	require.Equal(t, snapshot.Sections[2].Clock, snapshot.Sections[3].Clock)
	require.Equal(t, dataReadOffset(1<<4), snapshot.Sections[4].Offset)
	require.Equal(t, uint64(9), snapshot.Sections[4].Seq)
	require.Nil(t, snapshot.Sections[4].Clock)
	require.NotEmpty(t, snapshot.Sections[4].ReadError)
	require.Equal(t, dataReadOffset(1<<5), snapshot.Sections[5].Offset)
	require.Equal(t, uint64(2), snapshot.Sections[5].Seq)
	require.Equal(t, &DataClockSnapshot{Reserved: &[12]uint8{}, RefTimeNS: new(int64(0))}, snapshot.Sections[5].Clock)
	require.Equal(t, dataReadOffset(1<<6), snapshot.Sections[6].Offset)
	require.Zero(t, snapshot.Sections[6].Seq)
	require.Nil(t, snapshot.Sections[6].Clock)
	require.NotEmpty(t, snapshot.Sections[6].ReadError)
	require.Equal(t, dataReadOffset(1<<31), snapshot.Sections[7].Offset)
	require.Equal(t, snapshot.Sections[2].Clock, snapshot.Sections[7].Clock)
}

func TestReadFBClockDataUnreadableSections(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2_%t", v2), func(t *testing.T) {
			var path string
			offset := dataReadOffset(SourcePRTC)
			if v2 {
				path = dataReadV2Fixture(t, nil, nil)
				offset = 64
			} else {
				path = dataReadV3Fixture(t, nil)
			}
			dataReadPatch(t, path, offset, binary.NativeEndian.AppendUint64(nil, 7)...)
			snapshot, err := ReadFBClockDataFile(path, v2)
			require.NoError(t, err)
			require.Zero(t, snapshot.Sections[0].Seq)
			require.Equal(t, uint64(7), snapshot.Sections[1].Seq)
			for _, section := range snapshot.Sections {
				require.Nil(t, section.Clock)
				require.NotEmpty(t, section.ReadError)
				require.Nil(t, section.AnchorAgeNS)
				require.Nil(t, section.SinceIngressNS)
				require.Empty(t, section.AgeError)
			}
		})
	}
}

func TestDataAges(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		sys, now, coef       int64
		ref, ingress         int64
		wantAge, wantIngress *int64
	}{
		{name: "positive_fraction", sys: 10, now: 13, coef: 500000000, ref: 100, ingress: 90, wantAge: new(int64(3)), wantIngress: new(int64(14))},
		{name: "negative_age_fraction", sys: 10, now: 7, coef: 500000000, ref: 100, ingress: 90, wantAge: new(int64(-3)), wantIngress: new(int64(6))},
		{name: "negative_coef_fraction", sys: 10, now: 13, coef: -500000000, ref: 100, ingress: 90, wantAge: new(int64(3)), wantIngress: new(int64(12))},
		{name: "negative_age_and_coef", sys: 10, now: 7, coef: -500000000, ref: 100, ingress: 90, wantAge: new(int64(-3)), wantIngress: new(int64(8))},
		{name: "product_exceeds_int64", now: 4000000000, coef: 4000000000, ref: 100, ingress: 90, wantAge: new(int64(4000000000)), wantIngress: new(int64(20000000010))},
		{name: "sum_cancels", now: 1, ref: math.MaxInt64, ingress: math.MaxInt64, wantAge: new(int64(1)), wantIngress: new(int64(1))},
		{name: "above_float_precision", sys: 9007199254740993, now: 9007199254740994, ref: 9007199254740995, ingress: 9007199254740994, wantAge: new(int64(1)), wantIngress: new(int64(2))},
		{name: "future_ingress", ref: 100, ingress: 101, wantAge: new(int64(0)), wantIngress: new(int64(-1))},
		{name: "anchor_overflow", sys: math.MinInt64, now: math.MaxInt64},
		{name: "anchor_underflow", sys: math.MaxInt64, now: math.MinInt64},
		{name: "anchor_overflow_ingress_fits", sys: math.MinInt64, now: math.MaxInt64, ref: math.MinInt64, ingress: math.MaxInt64, wantIngress: new(int64(0))},
		{name: "ingress_overflow_anchor_fits", now: 1, ref: math.MaxInt64, wantAge: new(int64(1))},
		{name: "ingress_underflow_anchor_fits", now: -1, ref: math.MinInt64, wantAge: new(int64(-1))},
		{name: "scaled_ingress_overflow", now: math.MaxInt64, coef: math.MaxInt64, wantAge: new(int64(math.MaxInt64))},
		{name: "ingress_subtraction_overflow", ref: math.MaxInt64, ingress: math.MinInt64, wantAge: new(int64(0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			age, sinceIngress := dataAges(tc.now, tc.sys, tc.coef, tc.ref, tc.ingress)
			require.Equal(t, tc.wantAge, age)
			require.Equal(t, tc.wantIngress, sinceIngress)
		})
	}
}

func TestReadFBClockDataUnsupportedBaseClock(t *testing.T) {
	path := dataReadV3Fixture(t, map[Sources]dataReadSection{
		SourcePTP: {
			config: DataSectionConfig{ObserveOnly: true},
			clock:  DataClock{ClockID: 123, RefTimeNS: 456, ErrorBoundNS: 789},
		},
	})
	snapshot, err := ReadFBClockDataFile(path, false)
	require.NoError(t, err)
	section := snapshot.Sections[0]
	require.NotNil(t, section.Clock)
	require.Equal(t, uint32(123), section.Clock.ClockID)
	require.Equal(t, new(int64(456)), section.Clock.RefTimeNS)
	require.Equal(t, uint64(789), section.Clock.ErrorBoundNS)
	require.Empty(t, section.ReadError)
	require.Equal(t, "unsupported base clock 123", section.AgeError)
	require.Nil(t, section.AnchorAgeNS)
	require.Nil(t, section.SinceIngressNS)
}

func TestReadFBClockDataMinorVersions(t *testing.T) {
	for _, minor := range []uint8{0, 255} {
		t.Run(fmt.Sprintf("minor_%d", minor), func(t *testing.T) {
			path := dataReadV3Fixture(t, map[Sources]dataReadSection{
				SourceNTP: {},
			})
			dataReadPatch(t, path, 9, minor)
			snapshot, err := ReadFBClockDataFile(path, false)
			require.NoError(t, err)
			require.NotNil(t, snapshot.Header)
			require.Equal(t, minor, snapshot.Header.Minor)
			require.NotNil(t, snapshot.Sections[2].Clock)
		})
	}
}

func TestReadFBClockDataOversizedFiles(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v2_%t", v2), func(t *testing.T) {
			var path string
			wantSections := 3
			if v2 {
				path = dataReadV2Fixture(t, nil, nil)
				wantSections = 2
			} else {
				path = dataReadV3Fixture(t, nil)
			}
			info, err := os.Stat(path)
			require.NoError(t, err)
			dataReadPatch(t, path, int(info.Size()), bytes.Repeat([]byte{0xaa}, 77)...)
			snapshot, err := ReadFBClockDataFile(path, v2)
			require.NoError(t, err)
			require.Equal(t, info.Size()+77, snapshot.Size)
			require.Len(t, snapshot.Sections, wantSections)
		})
	}
}

func TestReadFBClockDataFileErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		v2      bool
		missing bool
		size    int
		magic   byte
		major   byte
	}{
		{name: "missing_v3", missing: true},
		{name: "missing_v2", v2: true, missing: true},
		{name: "small_v3", size: 4095, major: 3},
		{name: "small_v2", v2: true, size: 127},
		{name: "empty_v3"},
		{name: "empty_v2", v2: true},
		{name: "wrong_magic", size: 4096, magic: 'X', major: 3},
		{name: "wrong_major", size: 4096, major: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			if !tc.missing {
				raw := make([]byte, tc.size)
				copy(raw, []byte{'F', 'B', 'C', 'L', 'O', 'C', 'K', 0})
				if len(raw) > 8 {
					raw[7] = tc.magic
					raw[8] = tc.major
				}
				require.NoError(t, os.WriteFile(path, raw, 0o600))
			}
			snapshot, err := ReadFBClockDataFile(path, tc.v2)
			require.Error(t, err)
			require.Nil(t, snapshot)
		})
	}
}

func shmFirstByte(t *testing.T, path string) byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b[0]
}

// shmSeed leaves a file starting with b at path, as an earlier daemon would.
func shmSeed(t *testing.T, path string, b byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte{b}, 0o600))
}

func TestOpenFBClockShmLinked(t *testing.T) {
	for _, ver := range []int{1, 2} {
		for _, tc := range []struct {
			name    string
			restart bool // the daemon already ran once and wrote 1
			flat    byte // first byte of an existing flat file, 0 for none
			dir     byte // first byte of an existing /run/fbclock file, 0 for none
			kept    byte // first byte of the existing file both names keep, 0 for none
			// the kept file is the flat one, not the /run/fbclock one
			keepFlat bool
		}{
			{name: "fresh"},
			{name: "restart", restart: true, kept: 1},
			{name: "dir_only", dir: 2, kept: 2},
			{name: "flat_only", flat: 5, kept: 5, keepFlat: true},
			{name: "different_files", flat: 3, dir: 4, kept: 3, keepFlat: true},
		} {
			t.Run(fmt.Sprintf("v%d/%s", ver, tc.name), func(t *testing.T) {
				root := t.TempDir()
				dirPath := filepath.Join(root, "fbclock", fmt.Sprintf("data_v%d", ver))
				flatPath := filepath.Join(root, fmt.Sprintf("fbclock_data_v%d", ver))
				if tc.restart {
					shm, err := openFBClockShmLinked(dirPath, flatPath, ver)
					require.NoError(t, err)
					_, err = shm.File.WriteAt([]byte{1}, 0)
					require.NoError(t, err)
					require.NoError(t, shm.Close())
				}
				if tc.flat != 0 {
					shmSeed(t, flatPath, tc.flat)
				}
				if tc.dir != 0 {
					shmSeed(t, dirPath, tc.dir)
				}
				var before os.FileInfo
				if tc.kept != 0 {
					keptPath := dirPath
					if tc.keepFlat {
						keptPath = flatPath
					}
					var err error
					before, err = os.Stat(keptPath)
					require.NoError(t, err)
				}

				shm, err := openFBClockShmLinked(dirPath, flatPath, ver)
				require.NoError(t, err)
				defer shm.Close()

				require.Equal(t, dirPath, shm.Path)
				dir, err := os.Stat(dirPath)
				require.NoError(t, err)
				flat, err := os.Stat(flatPath)
				require.NoError(t, err)
				require.True(t, os.SameFile(dir, flat), "the two names aren't one file")
				if tc.kept != 0 {
					require.True(t, os.SameFile(before, dir), "moved off the existing file")
					require.Equal(t, tc.kept, shmFirstByte(t, flatPath))
				}
				_, err = shm.File.WriteAt([]byte{9}, 0)
				require.NoError(t, err)
				require.Equal(t, byte(9), shmFirstByte(t, dirPath))
				require.Equal(t, byte(9), shmFirstByte(t, flatPath))
			})
		}
	}
}
