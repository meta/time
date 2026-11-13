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

package rtcm

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// decodedMSM7 holds the fields of an MSM7 message decoded for testing.
type decodedMSM7 struct {
	msgType, stationID uint16
	epoch              uint32
	multiple           bool
	sats               []int // 1-based satellite IDs from the satellite mask
	sigs               []int // 1-based signal IDs from the signal mask
	cells              []bool
	roughInt, roughMod []uint32
	extInfo            []uint32
	roughRate          []int32
	finePR, finePhase  []int32
	lock, half, cnr    []uint32
	fineRate           []int32
}

// decodeMSM7 fully decodes an MSM7 frame by reading every field sequentially in
// spec order (no hardcoded bit offsets), validating framing and CRC along the
// way. It is the inverse of EncodeMSM7 and is used to verify encoder output.
func decodeMSM7(t *testing.T, frame []byte) decodedMSM7 {
	t.Helper()
	require.NotNil(t, frame)
	require.Equal(t, Preamble, frame[0])

	pl := int(frame[1]&0x03)<<8 | int(frame[2])
	require.Equal(t, HeaderSize+pl+CRCSize, len(frame), "declared length matches frame size")
	stored := uint32(frame[len(frame)-3])<<16 | uint32(frame[len(frame)-2])<<8 | uint32(frame[len(frame)-1])
	require.Equal(t, CRC24Q(frame[:HeaderSize+pl]), stored, "CRC-24Q")

	r := NewBitReader(frame[HeaderSize : HeaderSize+pl])
	var d decodedMSM7
	// Masks are no-ops for a 12-bit read; they satisfy gosec G115 without a lint suppression.
	d.msgType = uint16(r.ReadBits(12) & 0xFFF)
	d.stationID = uint16(r.ReadBits(12) & 0xFFF)
	d.epoch = r.ReadBits(30)
	d.multiple = r.ReadBits(1) == 1
	r.ReadBits(3) // IODS
	r.ReadBits(7) // reserved
	r.ReadBits(2) // clock steering
	r.ReadBits(2) // external clock
	r.ReadBits(1) // smoothing indicator
	r.ReadBits(3) // smoothing interval

	for i := range 64 {
		if r.ReadBits(1) == 1 {
			d.sats = append(d.sats, i+1)
		}
	}
	for i := range 32 {
		if r.ReadBits(1) == 1 {
			d.sigs = append(d.sigs, i+1)
		}
	}
	nsat, nsig := len(d.sats), len(d.sigs)
	ncell := 0
	for range nsat * nsig {
		on := r.ReadBits(1) == 1
		d.cells = append(d.cells, on)
		if on {
			ncell++
		}
	}

	// Satellite data.
	for range nsat {
		d.roughInt = append(d.roughInt, r.ReadBits(8))
	}
	for range nsat {
		d.extInfo = append(d.extInfo, r.ReadBits(4))
	}
	for range nsat {
		d.roughMod = append(d.roughMod, r.ReadBits(10))
	}
	for range nsat {
		d.roughRate = append(d.roughRate, r.ReadSignedBits(14))
	}

	// Signal data.
	for range ncell {
		d.finePR = append(d.finePR, r.ReadSignedBits(20))
	}
	for range ncell {
		d.finePhase = append(d.finePhase, r.ReadSignedBits(24))
	}
	for range ncell {
		d.lock = append(d.lock, r.ReadBits(10))
	}
	for range ncell {
		d.half = append(d.half, r.ReadBits(1))
	}
	for range ncell {
		d.cnr = append(d.cnr, r.ReadBits(10))
	}
	for range ncell {
		d.fineRate = append(d.fineRate, r.ReadSignedBits(15))
	}

	// Everything after the last field is byte padding (< 8 bits).
	require.LessOrEqual(t, r.Pos(), pl*8)
	require.Less(t, pl*8-r.Pos(), 8, "field layout exactly fills the payload")
	return d
}

func gpsObs(sv uint8, pr, cp float64, dop float32, cno uint8) RawxObservation {
	return RawxObservation{
		PrMes: pr, CpMes: cp, DoMes: dop, GnssID: GnssGPS, SvID: sv, SigID: 0,
		Locktime: 63000, CNO: cno, PrValid: true, CpValid: true, HalfCyc: true,
	}
}

func carrierPhaseCycles(gnss, sigID, freqID uint8, rangeM float64) float64 {
	return rangeM / getWavelength(gnss, sigID, freqID)
}

func ghostObs(sv uint8) RawxObservation {
	return RawxObservation{GnssID: GnssGPS, SvID: sv, SigID: 0, PrValid: true}
}

func TestEncodeMSM7ValidFrame(t *testing.T) {
	const stationID, epochMs = uint16(7), uint32(288093000)
	obs := []RawxObservation{
		gpsObs(4, 20960834.6, carrierPhaseCycles(GnssGPS, 0, 0, 20960834.6), -3722.4, 43),
		gpsObs(8, 18864369.0, carrierPhaseCycles(GnssGPS, 0, 0, 18864369.0), -1825.4, 49),
		gpsObs(9, 19008414.2, carrierPhaseCycles(GnssGPS, 0, 0, 19008414.2), -2474.4, 45),
		gpsObs(21, 23242403.8, carrierPhaseCycles(GnssGPS, 0, 0, 23242403.8), -1072.4, 40),
		gpsObs(27, 21312585.2, carrierPhaseCycles(GnssGPS, 0, 0, 21312585.2), -3452.4, 42),
		ghostObs(1), // timing-only track, must be excluded
	}

	frame, err := EncodeMSM7(stationID, GnssGPS, epochMs, obs)
	require.NoError(t, err)

	d := decodeMSM7(t, frame)
	require.Equal(t, TypeGPSMSM7, d.msgType)
	require.Equal(t, stationID, d.stationID)
	require.Equal(t, epochMs, d.epoch)
	require.False(t, d.multiple, "encoder leaves the multiple-message bit clear")
	require.Equal(t, []int{4, 8, 9, 21, 27}, d.sats, "ghost SV1 excluded")
	require.Equal(t, []int{2}, d.sigs, "GPS L1 C/A is RTCM signal 2")
	require.Len(t, d.cells, 5)
	for _, finePhase := range d.finePhase {
		require.NotEqual(t, int32(invalidFinePhase), finePhase)
	}
	require.NotContains(t, d.sats, 1)
}

func TestEncodeMSM7NoObservationsForConstellation(t *testing.T) {
	obs := []RawxObservation{gpsObs(5, 20000000, 105000000, -1000, 40)}
	frame, err := EncodeMSM7(1, GnssGalileo, 100000, obs) // ask for Galileo
	require.ErrorIs(t, err, ErrNoObservations)
	require.Nil(t, frame)
}

func TestEncodeMSM7ExcludesGhostCells(t *testing.T) {
	obs := []RawxObservation{
		gpsObs(4, 20960834.6, 110135576.123, -3722.4, 43),
		ghostObs(1),
		gpsObs(8, 18864369.0, 99121456.789, -1825.4, 49),
		ghostObs(14),
		gpsObs(9, 19008414.2, 99878123.456, -2474.4, 45),
		// Nonzero pseudorange but flagged invalid — also a ghost.
		{PrMes: 21000000, GnssID: GnssGPS, SvID: 30, SigID: 0, PrValid: false},
	}
	frame, err := EncodeMSM7(1, GnssGPS, 288093000, obs)
	require.NoError(t, err)

	d := decodeMSM7(t, frame)
	require.Equal(t, []int{4, 8, 9}, d.sats)
	require.Len(t, d.cells, 3)
	for _, c := range d.cells {
		require.True(t, c, "single-signal frame has a cell for every satellite")
	}
	// No emitted cell may carry an "untracked" value.
	for i, ri := range d.roughInt {
		require.NotEqual(t, uint32(0), ri, "sat %d rough range is real", i)
		require.NotEqual(t, uint32(255), ri, "sat %d rough range not the invalid marker", i)
	}
	for i, cnr := range d.cnr {
		require.NotEqual(t, uint32(0), cnr, "cell %d carries real signal strength", i)
	}
}

func TestEncodeMSM7AllGhostsYieldNoObservations(t *testing.T) {
	obs := []RawxObservation{ghostObs(1), ghostObs(14)}
	frame, err := EncodeMSM7(1, GnssGPS, 288093000, obs)
	require.ErrorIs(t, err, ErrNoObservations)
	require.Nil(t, frame)
}

func TestEncodeMSM7UnsupportedConstellation(t *testing.T) {
	_, err := EncodeMSM7(1, GnssSBAS, 100000, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoObservations)
}

func TestEncodeMSM7PhaseRangeRate(t *testing.T) {
	// DF399 must carry the satellite range rate in m/s (-Doppler*wavelength),
	// not the raw Doppler in Hz. Encode a known Doppler and reconstruct it from
	// the rough (DF399) + fine (DF404) phase range rate.
	const doppler float32 = -519.0 // Hz, satellite receding
	obs := []RawxObservation{gpsObs(5, 20000000, 105000000, doppler, 45)}

	frame, err := EncodeMSM7(1, GnssGPS, 100000, obs)
	require.NoError(t, err)
	d := decodeMSM7(t, frame)
	require.Len(t, d.roughRate, 1)
	require.Len(t, d.fineRate, 1)

	wavelength := speedOfLight / 1575.42e6
	totalRate := float64(d.roughRate[0]) + float64(d.fineRate[0])*0.0001 // m/s
	gotDoppler := -totalRate / wavelength
	require.InDelta(t, float64(doppler), gotDoppler, 1.0)
	require.Positive(t, d.roughRate[0], "receding satellite has a positive range rate")
}

func TestEncodeMSM7HalfCycleInverted(t *testing.T) {
	// DF420 (half-cycle ambiguity) is the inverse of the u-blox "half cycle
	// valid" flag: resolved -> 0, unresolved -> 1.
	resolved := gpsObs(5, 20000000, 105000000, -100, 45)
	resolved.HalfCyc = true
	d := decodeMSM7(t, mustEncode(t, resolved))
	require.Equal(t, uint32(0), d.half[0], "resolved half cycle -> DF420 0")

	unresolved := gpsObs(6, 20000000, 105000000, -100, 45)
	unresolved.HalfCyc = false
	d = decodeMSM7(t, mustEncode(t, unresolved))
	require.Equal(t, uint32(1), d.half[0], "unresolved half cycle -> DF420 1")
}

func TestEncodeMSM7GLONASS(t *testing.T) {
	// GLONASS is FDMA: the wavelength depends on the frequency channel. The
	// encoder must still produce a valid frame with real measurements.
	obs := []RawxObservation{
		{PrMes: 19500000, CpMes: carrierPhaseCycles(GnssGLONASS, 0, 5, 19500000), DoMes: 1500, GnssID: GnssGLONASS, SvID: 3, SigID: 0, FreqID: 5, CNO: 44, PrValid: true, CpValid: true},
		{PrMes: 21000000, CpMes: carrierPhaseCycles(GnssGLONASS, 0, 1, 21000000), DoMes: -2000, GnssID: GnssGLONASS, SvID: 7, SigID: 0, FreqID: 1, CNO: 41, PrValid: true, CpValid: true},
	}
	frame, err := EncodeMSM7(1, GnssGLONASS, 100000, obs)
	require.NoError(t, err)
	d := decodeMSM7(t, frame)
	require.Equal(t, TypeGLONASSMSM7, d.msgType)
	require.Equal(t, []int{3, 7}, d.sats)
	for _, cnr := range d.cnr {
		require.NotZero(t, cnr)
	}
	for _, finePhase := range d.finePhase {
		require.NotEqual(t, int32(invalidFinePhase), finePhase)
	}
}

func TestEncodeMSM7FiltersByConstellation(t *testing.T) {
	obs := []RawxObservation{
		gpsObs(4, 20000000, 105000000, -100, 45),
		{PrMes: 22000000, CpMes: 115000000, DoMes: -200, GnssID: GnssGalileo, SvID: 11, SigID: 0, CNO: 40, PrValid: true, CpValid: true},
		gpsObs(9, 21000000, 110000000, -300, 43),
	}
	d := decodeMSM7(t, mustEncodeAll(t, obs))
	require.Equal(t, []int{4, 9}, d.sats, "only GPS satellites encoded")
}

func TestEncodeMSM7Deterministic(t *testing.T) {
	obs := []RawxObservation{
		gpsObs(10, 22000000, 115600000, -2000, 45),
		gpsObs(15, 24000000, 126100000, -1500, 42),
	}
	a, err := EncodeMSM7(1, GnssGPS, 200000000, obs)
	require.NoError(t, err)
	b, err := EncodeMSM7(1, GnssGPS, 200000000, obs)
	require.NoError(t, err)
	require.Equal(t, a, b, "encoding is deterministic")
}

func mustEncode(t *testing.T, obs RawxObservation) []byte {
	t.Helper()
	frame, err := EncodeMSM7(1, GnssGPS, 100000, []RawxObservation{obs})
	require.NoError(t, err)
	require.NotNil(t, frame)
	return frame
}

func mustEncodeAll(t *testing.T, obs []RawxObservation) []byte {
	t.Helper()
	frame, err := EncodeMSM7(1, GnssGPS, 100000, obs)
	require.NoError(t, err)
	require.NotNil(t, frame)
	return frame
}

func sigObs(gnss, sv, sigID uint8, pr, cp float64) RawxObservation {
	return RawxObservation{
		PrMes: pr, CpMes: cp, DoMes: -1500, GnssID: gnss, SvID: sv, SigID: sigID,
		Locktime: 63000, CNO: 44, PrValid: true, CpValid: true, HalfCyc: true,
	}
}

// A dual-band receiver reports one satellite on two signals.
func TestEncodeMSM7DualBandGPS(t *testing.T) {
	obs := []RawxObservation{
		sigObs(GnssGPS, 4, 0, 20960834.6, carrierPhaseCycles(GnssGPS, 0, 0, 20960834.6)), // L1C/A
		sigObs(GnssGPS, 4, 3, 20960834.9, carrierPhaseCycles(GnssGPS, 3, 0, 20960834.9)), // L2CL
		sigObs(GnssGPS, 8, 0, 18864369.0, carrierPhaseCycles(GnssGPS, 0, 0, 18864369.0)), // L1C/A
	}

	d := decodeMSM7(t, mustEncodeAll(t, obs))
	require.Equal(t, []int{4, 8}, d.sats)
	require.Equal(t, []int{2, 16}, d.sigs, "GPS L1C/A is signal 2, L2CL is signal 16")
	require.Equal(t, []bool{true, true, true, false}, d.cells, "SV8 has no L2")
	require.Len(t, d.finePR, 3)
	for _, finePhase := range d.finePhase {
		require.NotEqual(t, int32(invalidFinePhase), finePhase)
	}
	require.Len(t, d.cnr, 3)
}

func TestEncodeMSM7SignalMaskPerConstellation(t *testing.T) {
	tests := []struct {
		name string
		gnss uint8
		sigs []uint8
		want []int
	}{
		{"GPSL1L2", GnssGPS, []uint8{0, 3, 4}, []int{2, 15, 16}},
		{"GPSL5", GnssGPS, []uint8{0, 6, 7}, []int{2, 22, 23}},
		{"GalileoE1E5b", GnssGalileo, []uint8{0, 1, 5, 6}, []int{2, 4, 14, 15}},
		{"GalileoE5a", GnssGalileo, []uint8{3, 4}, []int{22, 23}},
		{"GlonassL1L2", GnssGLONASS, []uint8{0, 2}, []int{2, 8}},
		{"BeidouB1B2", GnssBeiDou, []uint8{0, 2}, []int{2, 14}},
		{"GalileoE6", GnssGalileo, []uint8{8, 9}, []int{8, 10}},
		{"BeidouB3IB2a", GnssBeiDou, []uint8{4, 7, 8}, []int{8, 22, 23}},
		{"BeidouB3ID2", GnssBeiDou, []uint8{10}, []int{8}},
		{"QZSSL1L2L5", GnssQZSS, []uint8{0, 4, 5, 8, 9}, []int{2, 15, 16, 22, 23}},
		{"NavICL5", GnssNavIC, []uint8{0}, []int{22}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var obs []RawxObservation
			for i, sig := range tt.sigs {
				obs = append(obs, sigObs(tt.gnss, uint8(i+1), sig, 21000000, 110000000))
			}
			frame, err := EncodeMSM7(1, tt.gnss, 100000, obs)
			require.NoError(t, err)
			require.Equal(t, tt.want, decodeMSM7(t, frame).sigs)
		})
	}
}

// BeiDou D1/D2 are separate u-blox sigIDs for one RTCM signal.
func TestEncodeMSM7BeidouD1D2ShareSignal(t *testing.T) {
	obs := []RawxObservation{
		sigObs(GnssBeiDou, 6, 0, 22000000, 114000000),  // B1I D1
		sigObs(GnssBeiDou, 20, 1, 23000000, 119000000), // B1I D2
	}

	d := decodeMSM7(t, mustEncodeAllGNSS(t, GnssBeiDou, obs))
	require.Equal(t, []int{2}, d.sigs, "D1 and D2 share RTCM signal 2")
	require.Equal(t, []int{6, 20}, d.sats)
	require.Equal(t, []bool{true, true}, d.cells)
}

func TestEncodeMSM7UnmappedSignalSkipped(t *testing.T) {
	obs := []RawxObservation{
		sigObs(GnssGPS, 4, 0, 20960834.6, 110135576.123),
		sigObs(GnssGPS, 4, 9, 20960834.9, 85820011.456), // not a mapped signal
	}
	before := UnmappedSignals()

	d := decodeMSM7(t, mustEncodeAll(t, obs))
	require.Equal(t, []int{2}, d.sigs)
	require.Len(t, d.finePR, 1)
	require.Equal(t, before+1, UnmappedSignals())
}

func TestEncodeMSM7QZSSAndNavICMessageTypes(t *testing.T) {
	tests := []struct {
		name string
		gnss uint8
		want uint16
	}{
		{"QZSS", GnssQZSS, 1117},
		{"NavIC", GnssNavIC, 1137},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := mustEncodeAllGNSS(t, tt.gnss, []RawxObservation{sigObs(tt.gnss, 1, 0, 21000000, 110000000)})
			require.Equal(t, tt.want, decodeMSM7(t, frame).msgType)
		})
	}
}

// GLONASS rows carry a u-blox freqID (slot k plus 7) and expect FDMA spacing of
// 562.5 kHz on L1 and 437.5 kHz on L2.
func TestSignalWavelengths(t *testing.T) {
	tests := []struct {
		name    string
		gnss    uint8
		sigID   uint8
		freqID  uint8
		freqMHz float64
	}{
		{"GPSL1", GnssGPS, 0, 0, 1575.42},
		{"GPSL2CL", GnssGPS, 3, 0, 1227.60},
		{"GPSL5", GnssGPS, 6, 0, 1176.45},
		{"GalileoE1", GnssGalileo, 0, 0, 1575.42},
		{"GalileoE5aI", GnssGalileo, 3, 0, 1176.45},
		{"GalileoE5bI", GnssGalileo, 5, 0, 1207.14},
		{"GalileoE5bQ", GnssGalileo, 6, 0, 1207.14},
		{"BeidouB1ID1", GnssBeiDou, 0, 0, 1561.098},
		{"BeidouB2ID2", GnssBeiDou, 3, 0, 1207.14},
		{"GalileoE6B", GnssGalileo, 8, 0, 1278.75},
		{"GalileoE6C", GnssGalileo, 9, 0, 1278.75},
		{"BeidouB3ID1", GnssBeiDou, 4, 0, 1268.52},
		{"BeidouB3ID2", GnssBeiDou, 10, 0, 1268.52},
		{"BeidouB2aPilot", GnssBeiDou, 7, 0, 1176.45},
		{"BeidouB2aData", GnssBeiDou, 8, 0, 1176.45},
		{"QZSSL2CM", GnssQZSS, 4, 0, 1227.60},
		{"QZSSL2CL", GnssQZSS, 5, 0, 1227.60},
		{"QZSSL5I", GnssQZSS, 8, 0, 1176.45},
		{"QZSSL5Q", GnssQZSS, 9, 0, 1176.45},
		{"NavICL5A", GnssNavIC, 0, 0, 1176.45},
		{"GlonassL1Slot0", GnssGLONASS, 0, 7, 1602.0},
		{"GlonassL1SlotMinus7", GnssGLONASS, 0, 0, 1598.0625},
		{"GlonassL1SlotPlus6", GnssGLONASS, 0, 13, 1605.375},
		{"GlonassL2Slot0", GnssGLONASS, 2, 7, 1246.0},
		{"GlonassL2SlotMinus7", GnssGLONASS, 2, 0, 1242.9375},
		{"GlonassL2SlotPlus6", GnssGLONASS, 2, 13, 1248.625},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.InDelta(t, speedOfLight/(tt.freqMHz*1e6), getWavelength(tt.gnss, tt.sigID, tt.freqID), 1e-12)
		})
	}
}

func mustEncodeAllGNSS(t *testing.T, gnss uint8, obs []RawxObservation) []byte {
	t.Helper()
	frame, err := EncodeMSM7(1, gnss, 100000, obs)
	require.NoError(t, err)
	require.NotNil(t, frame)
	return frame
}

// A dense multi-band epoch must stay within the 10-bit length field rather
// than wrapping it.
func TestEncodeMSM7CapsOversizedEpoch(t *testing.T) {
	var obs []RawxObservation
	for sv := uint8(1); sv <= 32; sv++ {
		for _, sig := range []uint8{0, 3, 4} {
			o := sigObs(GnssGPS, sv, sig, 21000000, 110000000)
			o.CNO = 20 + sv // SV1 weakest
			obs = append(obs, o)
		}
	}

	before := DroppedSats()
	frame, err := EncodeMSM7(1, GnssGPS, 100000, obs)
	require.NoError(t, err)
	require.LessOrEqual(t, len(frame)-FrameOverhead, MaxPayloadLen)
	require.Greater(t, DroppedSats(), before, "dropped satellites are counted")

	d := decodeMSM7(t, frame) // validates declared length and CRC
	require.Equal(t, []int{2, 15, 16}, d.sigs)
	require.NotContains(t, d.sats, 1, "weakest satellite dropped first")
	require.Contains(t, d.sats, 32, "strongest satellite retained")
}

// A frame carrying more than 64 MSM cells is rejected by compliant decoders, so
// the cap binds on cell count as well as payload length.
func TestEncodeMSM7CapsCellCount(t *testing.T) {
	tests := []struct {
		name string
		sigs []uint8
		sats int
	}{
		{"TwoSignals", []uint8{0, 3}, 32},
		{"ThreeSignals", []uint8{0, 3, 4}, 21},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var obs []RawxObservation
			for sv := uint8(1); sv <= 40; sv++ {
				for _, sig := range tt.sigs {
					o := sigObs(GnssGPS, sv, sig, 21000000, 110000000)
					o.CNO = 20 + sv
					obs = append(obs, o)
				}
			}

			d := decodeMSM7(t, mustEncodeAll(t, obs))
			require.Len(t, d.sats, tt.sats)
			require.Len(t, d.cells, tt.sats*len(tt.sigs))
			require.LessOrEqual(t, len(d.cells), msmMaxCellMaskBits)
		})
	}
}

func TestMaxSatsFitsLengthFieldAndCellCap(t *testing.T) {
	for nsig := 1; nsig <= 6; nsig++ {
		n := maxSats(nsig)
		require.True(t, msm7Fits(n, nsig), "nsig=%d n=%d", nsig, n)
		if n < msmMaxSatellite {
			require.False(t, msm7Fits(n+1, nsig), "nsig=%d is not tight", nsig)
		}
	}
}

func msm7Fits(nsat, nsig int) bool {
	return msm7PayloadBytes(nsat, nsig) <= MaxPayloadLen && nsat*nsig <= msmMaxCellMaskBits
}

func msm7PayloadBytes(nsat, nsig int) int {
	return (169 + nsat*nsig + nsat*36 + nsat*nsig*80 + 7) / 8
}

// decodedRangeM reconstructs the first cell's pseudorange in metres exactly as
// a caster does: DF397 + DF398/1024 for the rough range, plus DF405 at 2^-29 ms.
func decodedRangeM(d decodedMSM7) float64 {
	roughMs := float64(d.roughInt[0]) + float64(d.roughMod[0])/1024.0
	fineMs := float64(d.finePR[0]) / (1 << 29)
	return (roughMs + fineMs) * speedOfLight / 1000.0
}

// obsAtRoughMs builds a single-signal GPS observation whose light-time is
// exactly roughMs, with the carrier phase consistent with the pseudorange.
func obsAtRoughMs(roughMs float64) RawxObservation {
	pr := roughMs * speedOfLight / 1000.0
	return gpsObs(4, pr, pr/(speedOfLight/1575.42e6), -1000, 45)
}

// A fraction rounding up to 1024/1024 must carry into DF397, not clamp DF398.
func TestEncodeMSM7RoughRangeQuantumBoundary(t *testing.T) {
	const roughMs = 70.9996 // frac*1024 = 1023.59, rounds to 1024
	obs := obsAtRoughMs(roughMs)

	d := decodeMSM7(t, mustEncode(t, obs))

	require.Equal(t, uint32(71), d.roughInt[0], "DF397 carries into the next whole ms")
	require.Equal(t, uint32(0), d.roughMod[0], "DF398 wraps to 0 with the carry")
	require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001,
		"caster-reconstructed pseudorange must match the receiver measurement")
}

// Fractions either side of the rounding boundary must all round trip.
func TestEncodeMSM7PseudorangeRoundTripAcrossFractions(t *testing.T) {
	fracs := []float64{
		0, 0.25, 0.5, 0.75,
		0.99900, 0.99940, 0.99950, 0.99951, 0.99960, 0.99990, 0.99999,
	}
	for _, frac := range fracs {
		t.Run(strconv.FormatFloat(frac, 'f', -1, 64), func(t *testing.T) {
			obs := obsAtRoughMs(70 + frac)
			d := decodeMSM7(t, mustEncode(t, obs))

			require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001)
		})
	}
}

// DF405 refines the nearest grid point, so it never needs more than half a
// step. Each fraction sits half a step off, where the residual is largest.
func TestEncodeMSM7FinePseudorangeStaysWithinHalfQuantum(t *testing.T) {
	for i := range 1024 {
		frac := (float64(i) + 0.5) / 1024.0
		obs := obsAtRoughMs(70 + frac)
		d := decodeMSM7(t, mustEncode(t, obs))
		require.NotZero(t, d.finePR[0], "frac=%v must exercise a non-zero DF405", frac)
		require.LessOrEqual(t, max(d.finePR[0], -d.finePR[0]), int32(1<<18),
			"frac=%v DF405 exceeds half a DF398 quantum", frac)
		require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "frac=%v", frac)
	}
}

// Rounding past the grid top must pin, not carry into the 255 invalid marker.
func TestEncodeMSM7RoughRangeNearInvalidMarker(t *testing.T) {
	const roughMs = 254.9996 // rounds to 255*1024 units
	obs := obsAtRoughMs(roughMs)

	d := decodeMSM7(t, mustEncode(t, obs))

	require.Equal(t, uint32(254), d.roughInt[0], "DF397 stays below the invalid marker")
	require.Equal(t, uint32(1023), d.roughMod[0], "DF398 pins to the top of the grid")
	require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001)
}

func TestQuantizeRoughRange(t *testing.T) {
	tests := []struct {
		name    string
		rangeMs float64
		want    uint32
	}{
		{"zero", 0, 0},
		{"half ms", 0.5, 512},
		{"typical GPS", 70.5, 70*1024 + 512},
		{"on grid", 70 + 512.0/1024.0, 70*1024 + 512},
		{"below rounding boundary", 70 + 1023.49/1024.0, 70*1024 + 1023},
		{"carries to next ms", 70.9996, 71 * 1024},
		{"top of grid", 254 + 1023.0/1024.0, maxRoughUnits},
		{"rounds past top but still encodable", 254.9996, maxRoughUnits},
		// The pin is bounded by what DF405 encodes, so these step the residual
		// in DF405 units across its last representable value.
		{"pin at DF405's last exact step", (maxRoughUnits + float64(maxFinePR)/(1<<19)) / 1024.0, maxRoughUnits},
		{"pin within DF405's rounding reach", (maxRoughUnits + (maxFinePR+0.25)/(1<<19)) / 1024.0, maxRoughUnits},
		{"pin past DF405's rounding reach", (maxRoughUnits + (maxFinePR+0.75)/(1<<19)) / 1024.0, invalidRoughUnits},
		{"exactly one ms past the grid", 255.0, invalidRoughUnits},
		{"one ms too far", 255.5, invalidRoughUnits},
		{"absurdly large", 1e12, invalidRoughUnits},
		{"positive infinity", math.Inf(1), invalidRoughUnits},
		{"not a number", math.NaN(), invalidRoughUnits},
		{"negative", -1, invalidRoughUnits},
		{"negative infinity", math.Inf(-1), invalidRoughUnits},
		// math.Round takes these to negative zero, which is not less than zero.
		{"negative below half a grid step", -0.4 / 1024.0, invalidRoughUnits},
		{"negative zero", math.Copysign(0, -1), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, quantizeRoughRange(tt.rangeMs))
		})
	}
}

// An unusable range must produce (255, 0) and mark the fine fields that refine
// it unavailable too, rather than clamp DF405 to a real-looking +293 m.
func TestEncodeMSM7UnusableRoughRangeMarkedInvalid(t *testing.T) {
	for _, roughMs := range []float64{255.5, 300} {
		t.Run(strconv.FormatFloat(roughMs, 'f', -1, 64), func(t *testing.T) {
			d := decodeMSM7(t, mustEncode(t, obsAtRoughMs(roughMs)))

			require.Equal(t, uint32(255), d.roughInt[0], "DF397 invalid marker")
			require.Equal(t, uint32(0), d.roughMod[0], "DF398 zeroed alongside it")
			require.Equal(t, int32(invalidFinePR), d.finePR[0], "DF405 invalid marker")
			require.Equal(t, int32(invalidFinePhase), d.finePhase[0], "DF406 invalid marker")
		})
	}
}

// A corrupt UBX frame delivers NaN or Inf. DF397, DF398 and DF405 derive from
// PrMes, so a non-finite pseudorange leaves no cell to encode.
func TestEncodeMSM7RejectsNonFiniteMeasurements(t *testing.T) {
	tests := []struct {
		name string
		pr   float64
	}{
		{"pseudorange NaN", math.NaN()},
		{"pseudorange +Inf", math.Inf(1)},
		{"pseudorange -Inf", math.Inf(-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obs := sigObs(GnssGPS, 4, 0, tt.pr, 110135576.123)

			_, err := EncodeMSM7(1, GnssGPS, 100000, []RawxObservation{obs})

			require.ErrorIs(t, err, ErrNoObservations)
		})
	}
}

// A ghost and a corrupt pseudorange both leave the frame with no cell and both
// return ErrNoObservations, which ntripper treats as routine. Only the counter
// separates a satellite the receiver never acquired from corrupt UBX.
func TestEncodeMSM7CountsNonFinitePseudorangesNotGhosts(t *testing.T) {
	before := NonFinitePseudoranges()

	_, err := EncodeMSM7(1, GnssGPS, 100000, []RawxObservation{ghostObs(1)})
	require.ErrorIs(t, err, ErrNoObservations)
	require.Equal(t, before, NonFinitePseudoranges(), "a ghost is not corrupt data")

	// -Inf is the case the ghost filter's PrMes <= 0 test would swallow.
	for _, pr := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(strconv.FormatFloat(pr, 'g', -1, 64), func(t *testing.T) {
			before := NonFinitePseudoranges()

			corrupt := sigObs(GnssGPS, 4, 0, pr, 110135576.123)
			_, err := EncodeMSM7(1, GnssGPS, 100000, []RawxObservation{corrupt})

			require.ErrorIs(t, err, ErrNoObservations)
			require.Equal(t, before+1, NonFinitePseudoranges(), "a corrupt pseudorange is visible to oncall")
		})
	}
}

// Only DF399 and DF404 read Doppler, so a corrupt one must cost the cell its
// rates and nothing else.
func TestEncodeMSM7NonFiniteDopplerKeepsPseudorange(t *testing.T) {
	for _, do := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		t.Run(strconv.FormatFloat(float64(do), 'g', -1, 32), func(t *testing.T) {
			obs := obsAtRoughMs(70.5)
			obs.DoMes = do

			d := decodeMSM7(t, mustEncode(t, obs))

			require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange still ships")
			require.Equal(t, int32(invalidRoughRate), d.roughRate[0], "DF399 invalid marker")
			require.Equal(t, int32(invalidFineRate), d.fineRate[0], "DF404 invalid marker")
		})
	}
}

// Only DF406 reads the carrier phase, so a corrupt one must cost the cell its
// phase and nothing else.
func TestEncodeMSM7NonFiniteCarrierPhaseKeepsPseudorange(t *testing.T) {
	for _, cp := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run(strconv.FormatFloat(cp, 'f', -1, 64), func(t *testing.T) {
			obs := sigObs(GnssGPS, 4, 0, 20960834.6, cp)

			d := decodeMSM7(t, mustEncode(t, obs))

			require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange still ships")
			require.Equal(t, int32(invalidFinePhase), d.finePhase[0], "DF406 invalid marker")
		})
	}
}

// A phase already flagged invalid is never read, so it must not cost the cell
// its pseudorange.
func TestEncodeMSM7KeepsPseudorangeWhenCarrierPhaseIsFlaggedInvalid(t *testing.T) {
	obs := sigObs(GnssGPS, 4, 0, 20960834.6, math.NaN())
	obs.CpValid = false

	d := decodeMSM7(t, mustEncode(t, obs))

	require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange still ships")
	require.Equal(t, int32(invalidFinePhase), d.finePhase[0], "DF406 invalid marker")
}

// cpMesForPhaseMs returns the GPS L1 carrier phase whose light-time is phaseMs.
func cpMesForPhaseMs(phaseMs float64) float64 {
	return phaseMs * speedOfLight / 1000.0 / getWavelength(GnssGPS, 0, 0)
}

// Extreme finite phases must be marked unavailable.
func TestEncodeMSM7LargeFiniteCarrierPhaseMarkedInvalid(t *testing.T) {
	for _, phaseMs := range []float64{4194304, 8388608, 1e290} {
		t.Run(strconv.FormatFloat(phaseMs, 'g', -1, 64), func(t *testing.T) {
			obs := sigObs(GnssGPS, 4, 0, 20960834.6, cpMesForPhaseMs(phaseMs))

			d := decodeMSM7(t, mustEncode(t, obs))

			require.Equal(t, int32(invalidFinePhase), d.finePhase[0])
			require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange is unaffected")
		})
	}
}

// The guard must not cost a phase that agrees with the pseudorange its DF406.
func TestEncodeMSM7ConsistentCarrierPhaseEncodesFinePhase(t *testing.T) {
	obs := obsAtRoughMs(70.5)

	d := decodeMSM7(t, mustEncode(t, obs))

	require.NotEqual(t, int32(invalidFinePhase), d.finePhase[0])
	require.Less(t, max(d.finePhase[0], -d.finePhase[0]), int32(1<<16), "an agreeing phase needs a small DF406")
}

// A phase whose residual will not fit DF406 must be marked unavailable rather
// than saturated: +8388607 is a real -1171 m correction to a caster.
func TestEncodeMSM7UnrepresentableFinePhaseMarkedInvalid(t *testing.T) {
	// 0.01 ms past the pseudorange, well beyond DF406's +/-0.0039 ms span.
	obs := obsAtRoughMs(70.5)
	obs.CpMes += 0.01 * speedOfLight / 1000.0 / getWavelength(GnssGPS, 0, 0)

	d := decodeMSM7(t, mustEncode(t, obs))

	require.Equal(t, int32(invalidFinePhase), d.finePhase[0])
	require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange is unaffected")
}

// decodedPhaseM reconstructs a cell's carrier phase in metres as a caster does:
// the satellite's rough range plus DF406 at 2^-31 ms.
func decodedPhaseM(d decodedMSM7, cell int) float64 {
	roughMs := float64(d.roughInt[0]) + float64(d.roughMod[0])/1024.0
	return (roughMs + float64(d.finePhase[cell])/(1<<31)) * speedOfLight / 1000.0
}

// DF406 spans four DF405s, and carrier phase survives the code multipath that
// ruins a pseudorange, so the signal a caster most needs must not be voided
// just because DF405 could not carry its cell's range.
func TestEncodeMSM7FinePhaseSurvivesUnrepresentableFineRange(t *testing.T) {
	const roughMs = 70.5 // 72192 units exactly
	roughM := roughMs * speedOfLight / 1000.0
	l2Phase := roughM + 5
	obs := []RawxObservation{
		obsAtRoughMs(roughMs),
		sigObs(GnssGPS, 4, 3, roughM+400, l2Phase/getWavelength(GnssGPS, 3, 0)),
	}

	d := decodeMSM7(t, mustEncodeAll(t, obs))

	require.Len(t, d.finePhase, 2)
	require.Equal(t, int32(invalidFinePR), d.finePR[1], "400 m is past DF405")
	require.NotEqual(t, int32(invalidFinePhase), d.finePhase[1], "5 m fits DF406")
	require.InDelta(t, l2Phase, decodedPhaseM(d, 1), 0.001, "caster reconstructs the phase")
}

// The only DF406 case here with an off-grid rough range: DF406 must carry a
// receiver's code-carrier divergence on top of the worst rounding residual,
// and still reconstruct the phase the receiver reported.
func TestEncodeMSM7FinePhaseCarriesCodeCarrierDivergence(t *testing.T) {
	// Rounds up onto the next grid point, the largest residual rounding leaves.
	const roughMs = 70.5 + 0.5/1024

	// Metres of divergence, carrier early: twice a storm-level L1 ionospheric
	// delay, down to sub-metre multipath.
	for _, divergenceM := range []float64{-100, -60, -12.5, -0.7, 0.4, 3} {
		t.Run(strconv.FormatFloat(divergenceM, 'f', -1, 64), func(t *testing.T) {
			obs := obsAtRoughMs(roughMs)
			phaseM := obs.PrMes + divergenceM
			obs.CpMes = phaseM / getWavelength(GnssGPS, 0, 0)

			d := decodeMSM7(t, mustEncode(t, obs))

			require.Equal(t, uint32(70), d.roughInt[0])
			require.Equal(t, uint32(513), d.roughMod[0], "half a quantum past the range")
			require.NotEqual(t, int32(invalidFinePhase), d.finePhase[0])
			require.InDelta(t, phaseM, decodedPhaseM(d, 0), 0.001, "caster reconstructs the phase")
			require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange is unaffected")
		})
	}
}

// A whole-cycle offset past DF406's span is refused, not folded away: this
// encoder holds no per-signal offset across epochs to fold it against.
func TestEncodeMSM7IntegerCycleAmbiguityRefused(t *testing.T) {
	before := VoidedFinePhases()
	obs := obsAtRoughMs(70.5)
	obs.CpMes += 7000 // whole cycles, 1332 m at GPS L1, just past DF406

	d := decodeMSM7(t, mustEncode(t, obs))

	require.Equal(t, int32(invalidFinePhase), d.finePhase[0])
	require.Equal(t, before+1, VoidedFinePhases(), "the refusal is counted")
	require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange is unaffected")
}

// Folding whole milliseconds out of the residual would encode a phase 1 ms from
// the rough range as a plausible +53 m correction, hiding a 299.8 km error.
func TestEncodeMSM7FinePhaseOneMillisecondOutMarkedInvalid(t *testing.T) {
	obs := obsAtRoughMs(70.5)
	obs.CpMes = 71.500177 * speedOfLight / 1000.0 / getWavelength(GnssGPS, 0, 0)

	d := decodeMSM7(t, mustEncode(t, obs))

	require.Equal(t, int32(invalidFinePhase), d.finePhase[0], "DF406 invalid marker")
	require.InDelta(t, obs.PrMes, decodedRangeM(d), 0.001, "pseudorange is unaffected")
}

// The rough range comes from a satellite's first cell, so a per-satellite guard
// would leave its other signals unchecked.
func TestEncodeMSM7RejectsNonFiniteSecondSignal(t *testing.T) {
	obs := []RawxObservation{
		sigObs(GnssGPS, 4, 0, 20960834.6, 110135576.123),
		sigObs(GnssGPS, 4, 3, math.Inf(1), 110135576.123),
	}

	d := decodeMSM7(t, mustEncodeAll(t, obs))

	require.Equal(t, []int{4}, d.sats)
	require.Equal(t, []int{2}, d.sigs, "only L1C/A survives")
	require.Len(t, d.finePR, 1)
	require.InDelta(t, obs[0].PrMes, decodedRangeM(d), 0.001)
}

// The rough range is quantized from the satellite's first cell, so a second
// signal whose own range disagrees has no rough range DF405 can refine.
func TestEncodeMSM7SecondSignalFarFromRoughRangeMarkedInvalid(t *testing.T) {
	obs := []RawxObservation{
		obsAtRoughMs(70.5),
		sigObs(
			GnssGPS,
			4,
			3,
			72.5*speedOfLight/1000.0,
			71.5*speedOfLight/1000.0/getWavelength(GnssGPS, 3, 0),
		),
	}

	d := decodeMSM7(t, mustEncodeAll(t, obs))

	require.Len(t, d.finePR, 2)
	require.Len(t, d.finePhase, 2)
	require.InDelta(t, obs[0].PrMes, decodedRangeM(d), 0.001, "L1 is unaffected")
	require.Equal(t, int32(invalidFinePR), d.finePR[1], "L2 DF405 invalid marker")
	require.Equal(t, int32(invalidFinePhase), d.finePhase[1], "L2 DF406 invalid marker")
}

// One whole DF398 quantum is the first residual DF405 cannot encode: it scales
// to 2^19, and only 2^19-1 fits.
func TestEncodeMSM7FineRangeOneQuantumOutMarkedInvalid(t *testing.T) {
	const roughMs = 70.5 // 72192 units exactly, so the residual below is exact
	pr := (roughMs + 1.0/1024.0) * speedOfLight / 1000.0
	obs := []RawxObservation{
		obsAtRoughMs(roughMs),
		sigObs(GnssGPS, 4, 3, pr, pr/getWavelength(GnssGPS, 3, 0)),
	}

	d := decodeMSM7(t, mustEncodeAll(t, obs))

	require.Len(t, d.finePR, 2)
	require.InDelta(t, obs[0].PrMes, decodedRangeM(d), 0.001, "L1 is unaffected")
	require.Equal(t, int32(invalidFinePR), d.finePR[1], "L2 DF405 invalid marker")
	require.Equal(t, int32(1<<21), d.finePhase[1], "one quantum still fits DF406")
}

func TestEncodeMSM7RoughRangeFallsBackToLaterSignal(t *testing.T) {
	l1 := sigObs(GnssGPS, 4, 0, 300*speedOfLight/1000.0, 110135576.123)
	l2 := obsAtRoughMs(70.5)
	l2.SigID = 3
	l2.CpMes = carrierPhaseCycles(GnssGPS, 3, 0, l2.PrMes)

	d := decodeMSM7(t, mustEncodeAll(t, []RawxObservation{l1, l2}))

	require.Len(t, d.finePR, 2)
	require.Equal(t, uint32(70), d.roughInt[0], "DF397 comes from L2")
	require.Equal(t, uint32(512), d.roughMod[0], "DF398 comes from L2")
	require.Equal(t, int32(invalidFinePR), d.finePR[0], "L1 DF405 invalid marker")
	require.NotEqual(t, int32(invalidFinePR), d.finePR[1], "L2 DF405 still encodes")
	require.NotEqual(t, int32(invalidFinePhase), d.finePhase[1], "L2 DF406 still encodes")
}

func TestEncodeMSM7CountsVoidedFields(t *testing.T) {
	beforeRoughRanges := VoidedRoughRanges()
	beforeRoughRates := VoidedRoughRates()
	beforeFinePRs := VoidedFinePRs()
	beforeFinePhases := VoidedFinePhases()
	beforeFineRates := VoidedFineRates()

	obs := obsAtRoughMs(300)
	obs.DoMes = float32(math.NaN())
	d := decodeMSM7(t, mustEncode(t, obs))
	require.Equal(t, uint32(255), d.roughInt[0])

	require.Equal(t, beforeRoughRanges+1, VoidedRoughRanges())
	require.Equal(t, beforeRoughRates+1, VoidedRoughRates())
	require.Equal(t, beforeFinePRs+1, VoidedFinePRs())
	require.Equal(t, beforeFinePhases+1, VoidedFinePhases())
	require.Equal(t, beforeFineRates+1, VoidedFineRates())
}

// The DF406 counter is what oncall reads, so it must track phases this encoder
// refused. trkStat clears CpValid on healthy epochs; counting that would swamp
// the fail-closed cases.
func TestEncodeMSM7CountsRefusedPhasesNotReceiverFlaggedOnes(t *testing.T) {
	flagged := sigObs(GnssGPS, 4, 0, 20960834.6, 110135576.123)
	flagged.CpValid = false

	beforeVoided, beforeFlagged := VoidedFinePhases(), ReceiverFlaggedPhases()
	d := decodeMSM7(t, mustEncode(t, flagged))

	require.Equal(t, int32(invalidFinePhase), d.finePhase[0], "DF406 invalid marker")
	require.Equal(t, beforeVoided, VoidedFinePhases(), "receiver-flagged phase is not an encoder refusal")
	require.Equal(t, beforeFlagged+1, ReceiverFlaggedPhases(), "receiver-flagged phase stays visible")

	decodeMSM7(t, mustEncode(t, sigObs(GnssGPS, 4, 0, 20960834.6, math.NaN())))
	require.Equal(t, beforeVoided+1, VoidedFinePhases(), "unrepresentable phase is an encoder refusal")
	require.Equal(t, beforeFlagged+1, ReceiverFlaggedPhases(), "an unusable phase is not merely flagged")
}

// A rough range pinned to the top of the grid must leave a residual DF405 can
// carry; past that the cell has no fine correction and the rough range would
// ship uncorrected.
func TestEncodeMSM7RoughRangeBeyondDF405ReachMarkedInvalid(t *testing.T) {
	obs := obsAtRoughMs((maxRoughUnits + 0.9999995) / 1024.0)

	d := decodeMSM7(t, mustEncode(t, obs))

	require.Equal(t, uint32(255), d.roughInt[0], "DF397 unavailable marker")
	require.Equal(t, int32(invalidFinePR), d.finePR[0], "DF405 invalid marker")
}

// DF404 carries a residual off the satellite's integer rough rate, so a second
// signal whose Doppler disagrees can exceed the field.
func TestEncodeMSM7SecondSignalRateOutOfRangeMarkedInvalid(t *testing.T) {
	l1 := sigObs(GnssGPS, 4, 0, 20960834.6, 110135576.123)
	l2 := sigObs(GnssGPS, 4, 3, l1.PrMes, l1.PrMes/getWavelength(GnssGPS, 3, 0))
	// L1 sets the rough rate at -DoMes*wavelength = 285 m/s; L2's own Doppler
	// puts its rate 10 m/s away, which is 100000 units in DF404's 0.0001 m/s.
	l2.DoMes = -float32((285.0 + 10.0) / getWavelength(GnssGPS, 3, 0))

	d := decodeMSM7(t, mustEncodeAll(t, []RawxObservation{l1, l2}))

	require.Len(t, d.fineRate, 2)
	require.Equal(t, int32(285), d.roughRate[0])
	require.NotEqual(t, int32(invalidFineRate), d.fineRate[0], "L1 rate still encodes")
	require.Equal(t, int32(invalidFineRate), d.fineRate[1], "L2 DF404 invalid marker")
}

// DF399 is one rate for the whole satellite, so a later signal may supply it
// when the first cell's Doppler cannot. The unusable cell still loses its own
// DF404, and the satellite keeps a rate it really measured.
func TestEncodeMSM7RoughRateFallsBackToLaterSignal(t *testing.T) {
	// L2's -DoMes*wavelength, the rate DF399 must fall back to.
	const l2Rate = 366

	tests := []struct {
		name    string
		l1DoMes float32
	}{
		{"first signal Doppler not finite", float32(math.NaN())},
		{"first signal rate past DF399", -float32(10000 / getWavelength(GnssGPS, 0, 0))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l1 := sigObs(GnssGPS, 4, 0, 20960834.6, 110135576.123)
			l1.DoMes = tt.l1DoMes
			l2 := sigObs(GnssGPS, 4, 3, l1.PrMes, l1.PrMes/getWavelength(GnssGPS, 3, 0))

			d := decodeMSM7(t, mustEncodeAll(t, []RawxObservation{l1, l2}))

			require.Len(t, d.fineRate, 2)
			require.Equal(t, int32(l2Rate), d.roughRate[0], "DF399 comes from L2")
			require.Equal(t, int32(invalidFineRate), d.fineRate[0], "L1 DF404 invalid marker")
			require.NotEqual(t, int32(invalidFineRate), d.fineRate[1], "L2 DF404 still encodes")
		})
	}
}

// A rate DF399 cannot carry must void the pair: DF404 is a residual off DF399,
// so a caster reconstructing the two must not get a rate nobody measured.
func TestEncodeMSM7RoughRateOutOfRangeMarksPairInvalid(t *testing.T) {
	for _, do := range []float32{-1e30, 1e30, -50000, 50000} {
		t.Run(strconv.FormatFloat(float64(do), 'g', -1, 32), func(t *testing.T) {
			obs := sigObs(GnssGPS, 4, 0, 20960834.6, 110135576.123)
			obs.DoMes = do

			d := decodeMSM7(t, mustEncode(t, obs))

			require.Equal(t, int32(invalidRoughRate), d.roughRate[0], "DF399 invalid marker")
			require.Equal(t, int32(invalidFineRate), d.fineRate[0], "DF404 follows DF399")
		})
	}
}

// The representable side of each boundary: one unit further is the marker, so
// tightening any guard to >= would silently void real measurements.
func TestEncodeMSM7EncodesValuesAtFieldLimits(t *testing.T) {
	t.Run("DF399 at the maximum magnitude", func(t *testing.T) {
		for _, sign := range []float64{-1, 1} {
			obs := sigObs(GnssGPS, 4, 0, 20960834.6, 110135576.123)
			// DF399 is -Doppler*wavelength, so the encoded sign is the opposite.
			obs.DoMes = float32(-sign * maxRoughRateMps / getWavelength(GnssGPS, 0, 0))

			d := decodeMSM7(t, mustEncode(t, obs))

			require.Equal(t, int32(sign*maxRoughRateMps), d.roughRate[0])
			require.NotEqual(t, int32(invalidFineRate), d.fineRate[0], "DF404 still encodes")
		}
	})

	t.Run("DF406 at the maximum magnitude", func(t *testing.T) {
		for _, sign := range []float64{-1, 1} {
			roughMs := 70.5
			phaseMs := roughMs + sign*float64(maxFinePhase)/(1<<31)
			obs := obsAtRoughMs(roughMs)
			obs.CpMes = phaseMs * speedOfLight / 1000.0 / getWavelength(GnssGPS, 0, 0)

			d := decodeMSM7(t, mustEncode(t, obs))

			require.Equal(t, int32(sign*maxFinePhase), d.finePhase[0])
		}
	})

	t.Run("DF405 one unit inside the boundary", func(t *testing.T) {
		const roughMs = 70.5 // 72192 units exactly, so the residual below is exact
		pr := (roughMs + float64(maxFinePR)/(1<<29)) * speedOfLight / 1000.0
		obs := []RawxObservation{
			obsAtRoughMs(roughMs),
			sigObs(GnssGPS, 4, 3, pr, pr/getWavelength(GnssGPS, 3, 0)),
		}

		d := decodeMSM7(t, mustEncodeAll(t, obs))

		require.Equal(t, int32(maxFinePR), d.finePR[1], "DF405 still encodes")
	})
}

func TestEncodeMSM7MinCNOFiltersWeakSignals(t *testing.T) {
	t.Cleanup(func() { SetMinCNO(0) })

	const stationID, epochMs = uint16(7), uint32(288093000)
	const rangeM = 22000000.0
	obs := []RawxObservation{
		gpsObs(1, rangeM, carrierPhaseCycles(GnssGPS, 0, 0, rangeM), -500, 45),
		gpsObs(2, rangeM, carrierPhaseCycles(GnssGPS, 0, 0, rangeM), -500, 20),
	}

	SetMinCNO(0)
	frame, err := EncodeMSM7(stationID, GnssGPS, epochMs, obs)
	require.NoError(t, err)
	require.Len(t, decodeMSM7(t, frame).sats, 2, "filter disabled keeps the weak satellite")

	before := WeakSignals()
	SetMinCNO(30)
	frame, err = EncodeMSM7(stationID, GnssGPS, epochMs, obs)
	require.NoError(t, err)
	require.Len(t, decodeMSM7(t, frame).sats, 1, "CNO 20 is below the floor")
	require.Equal(t, before+1, WeakSignals())
}

func TestEncodeMSM7MinCNODropsEveryObservation(t *testing.T) {
	t.Cleanup(func() { SetMinCNO(0) })

	const rangeM = 22000000.0
	SetMinCNO(40)
	_, err := EncodeMSM7(7, GnssGPS, 288093000, []RawxObservation{
		gpsObs(1, rangeM, carrierPhaseCycles(GnssGPS, 0, 0, rangeM), -500, 20),
	})
	require.ErrorIs(t, err, ErrNoObservations)
}

// DF419 carries the GLONASS frequency channel the phase math was computed on;
// zeroing it makes the caster recompute phase on the wrong wavelength.
func TestEncodeMSM7EncodesGlonassFrequencyChannel(t *testing.T) {
	const rangeM = 19500000.0
	obs := []RawxObservation{
		{
			PrMes: rangeM, CpMes: carrierPhaseCycles(GnssGLONASS, 0, 5, rangeM), DoMes: 1500,
			GnssID: GnssGLONASS, SvID: 3, SigID: 0, FreqID: 5,
			Locktime: 63000, CNO: 44, PrValid: true, CpValid: true, HalfCyc: true,
		},
		{
			PrMes: rangeM, CpMes: carrierPhaseCycles(GnssGLONASS, 0, 1, rangeM), DoMes: -2000,
			GnssID: GnssGLONASS, SvID: 7, SigID: 0, FreqID: 1,
			Locktime: 63000, CNO: 41, PrValid: true, CpValid: true, HalfCyc: true,
		},
	}

	frame, err := EncodeMSM7(7, GnssGLONASS, 288093000, obs)
	require.NoError(t, err)
	d := decodeMSM7(t, frame)
	require.Equal(t, []int{3, 7}, d.sats)
	require.Equal(t, []uint32{5, 1}, d.extInfo, "DF419 carries each satellite's frequency channel")
}

func TestEncodeMSM7GlonassUnknownFrequencyChannel(t *testing.T) {
	const rangeM = 19500000.0
	obs := []RawxObservation{{
		PrMes: rangeM, CpMes: carrierPhaseCycles(GnssGLONASS, 0, 0, rangeM), DoMes: 1500,
		GnssID: GnssGLONASS, SvID: 3, SigID: 0, FreqID: 255,
		Locktime: 63000, CNO: 44, PrValid: true, CpValid: true, HalfCyc: true,
	}}

	frame, err := EncodeMSM7(7, GnssGLONASS, 288093000, obs)
	require.NoError(t, err)
	require.Equal(t, []uint32{glonassFreqUnknown}, decodeMSM7(t, frame).extInfo)
}

// Extended satellite info is reserved outside GLONASS.
func TestEncodeMSM7ExtendedInfoZeroForNonGlonass(t *testing.T) {
	const rangeM = 22000000.0
	frame, err := EncodeMSM7(7, GnssGPS, 288093000, []RawxObservation{
		gpsObs(1, rangeM, carrierPhaseCycles(GnssGPS, 0, 0, rangeM), -500, 45),
	})
	require.NoError(t, err)
	require.Equal(t, []uint32{0}, decodeMSM7(t, frame).extInfo)
}

// A corrupt pseudorange must stay visible as corrupt even when its CNO is weak.
func TestEncodeMSM7WeakCorruptPseudorangeCountsAsNonFinite(t *testing.T) {
	t.Cleanup(func() { SetMinCNO(0) })
	SetMinCNO(30)

	const rangeM = 22000000.0
	weakCorrupt := gpsObs(2, math.Inf(1), carrierPhaseCycles(GnssGPS, 0, 0, rangeM), -500, 20)
	beforeWeak, beforeCorrupt := WeakSignals(), NonFinitePseudoranges()

	_, err := EncodeMSM7(7, GnssGPS, 288093000, []RawxObservation{
		gpsObs(1, rangeM, carrierPhaseCycles(GnssGPS, 0, 0, rangeM), -500, 45),
		weakCorrupt,
	})
	require.NoError(t, err)
	require.Equal(t, beforeCorrupt+1, NonFinitePseudoranges())
	require.Equal(t, beforeWeak, WeakSignals(), "corrupt data is not charged to the CNO floor")
}
