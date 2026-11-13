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
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync/atomic"
)

const speedOfLight = 299792458.0 // m/s

// MSM7 bit layout, used to size a frame against the 10-bit RTCM3 length field.
const (
	msmHeaderBits      = 73
	msmSatMaskBits     = 64
	msmSigMaskBits     = 32
	msmPerSatBits      = 36
	msmPerCellBits     = 80
	msmMaxSatellite    = 64
	msmMaxCellMaskBits = 64
)

// The 1/1024 ms grid DF397 and DF398 share. DF397 = 255 means unavailable.
const (
	maxRoughUnits     = 254*1024 + 1023
	invalidRoughUnits = 255 << 10
)

// Values RTCM 10403.x reserves to mark a signal's fine fields unavailable.
const (
	invalidFinePR    = -1 << 19 // DF405
	invalidFinePhase = -1 << 23 // DF406
)

// The largest magnitude each fine field can carry. One step further is the
// unavailable marker, so a residual past this has to be marked, not clamped:
// clamping ships the extreme as if it were a real correction.
const (
	maxFinePR    = 1<<19 - 1
	maxFinePhase = 1<<23 - 1
	maxFineRate  = 1<<14 - 1
)

// DF399 spans -8192..8191; -8192 is the unavailable marker, so 8191 is the
// largest magnitude it can carry. DF404 is a residual off DF399.
const (
	maxRoughRateMps  = 8191
	invalidRoughRate = -1 << 13 // DF399
	invalidFineRate  = -1 << 14 // DF404
)

// DF419 carries the GLONASS frequency channel as k+7; 15 means unknown.
const (
	glonassFreqMax     = 13
	glonassFreqUnknown = 15
)

// GNSS signal wavelengths in meters, keyed by u-blox sigID.
var signalWavelength = map[uint8]map[uint8]float64{
	GnssGPS: {0: speedOfLight / 1575.42e6, 3: speedOfLight / 1227.60e6, 4: speedOfLight / 1227.60e6, 6: speedOfLight / 1176.45e6, 7: speedOfLight / 1176.45e6},
	GnssGalileo: {
		0: speedOfLight / 1575.42e6, 1: speedOfLight / 1575.42e6, // E1
		3: speedOfLight / 1176.45e6, 4: speedOfLight / 1176.45e6, // E5a
		5: speedOfLight / 1207.14e6, 6: speedOfLight / 1207.14e6, // E5b
		8: speedOfLight / 1278.75e6, 9: speedOfLight / 1278.75e6, // E6
	},
	GnssGLONASS: {0: speedOfLight / 1602.0e6, 2: speedOfLight / 1246.0e6},
	GnssBeiDou: {
		0: speedOfLight / 1561.098e6, 1: speedOfLight / 1561.098e6, // B1I
		2: speedOfLight / 1207.14e6, 3: speedOfLight / 1207.14e6, // B2I
		4: speedOfLight / 1268.52e6, 10: speedOfLight / 1268.52e6, // B3I
		7: speedOfLight / 1176.45e6, 8: speedOfLight / 1176.45e6, // B2a
	},
	GnssQZSS: {
		0: speedOfLight / 1575.42e6,                              // L1C/A
		4: speedOfLight / 1227.60e6, 5: speedOfLight / 1227.60e6, // L2C
		8: speedOfLight / 1176.45e6, 9: speedOfLight / 1176.45e6, // L5
	},
	GnssNavIC: {0: speedOfLight / 1176.45e6},
}

// MSM7 message types per constellation.
var msm7MsgType = map[uint8]uint16{
	GnssGPS:     1077,
	GnssGLONASS: 1087,
	GnssGalileo: 1097,
	GnssQZSS:    1117,
	GnssBeiDou:  1127,
	GnssNavIC:   1137,
}

// u-blox sigID to DF395 mask bit (bit 0 = RTCM signal 1): the RTCM signal ID minus
// one, from the RTCM 10403.3 MSM signal tables, as in RTKLIB's msm_sig_* arrays.
var signalMaskBit = map[uint8]map[uint8]int{
	GnssGPS:     {0: 1, 3: 15, 4: 14, 6: 21, 7: 22},                    // 1C, 2L, 2S, 5I, 5Q
	GnssGalileo: {0: 1, 1: 3, 3: 21, 4: 22, 5: 13, 6: 14, 8: 9, 9: 7},  // 1C, 1B, 5I, 5Q, 7I, 7Q, 6B, 6C
	GnssGLONASS: {0: 1, 2: 7},                                          // 1C, 2C
	GnssBeiDou:  {0: 1, 1: 1, 2: 13, 3: 13, 4: 7, 7: 22, 8: 21, 10: 7}, // 2I, 7I, 6I, 5P, 5D; D1/D2 share a signal
	GnssQZSS:    {0: 1, 4: 14, 5: 15, 8: 21, 9: 22},                    // 1C, 2S, 2L, 5I, 5Q
	GnssNavIC:   {0: 21},                                               // 5A
}

// satSig keys an observation by satellite and signal mask bit.
type satSig struct {
	sv  uint8
	bit int
}

var (
	// droppedSats counts satellites discarded to keep a frame within the RTCM3
	// length field and the 64-bit cell mask. Non-zero means the caster is
	// receiving fewer observations than the receiver tracked.
	droppedSats atomic.Uint64

	// nonFinitePseudoranges counts observations dropped before encoding because
	// the receiver reported a NaN or Inf pseudorange. Unlike a ghost, which is a
	// satellite the receiver never acquired, this is corrupt data.
	nonFinitePseudoranges atomic.Uint64

	// weakSignals counts observations dropped by the minimum CNO threshold.
	weakSignals atomic.Uint64

	// unmappedSignals counts observations of a signal with no RTCM mapping here:
	// tracked by the receiver, never sent to the caster.
	unmappedSignals atomic.Uint64

	voidedRoughRanges     atomic.Uint64
	voidedRoughRates      atomic.Uint64
	voidedFinePRs         atomic.Uint64
	voidedFinePhases      atomic.Uint64
	voidedFineRates       atomic.Uint64
	receiverFlaggedPhases atomic.Uint64
)

// minCNO is the carrier-to-noise floor in dB-Hz; observations below it are not
// encoded. Zero disables the filter.
var minCNO uint

// SetMinCNO sets the carrier-to-noise floor in dB-Hz. Zero disables filtering.
func SetMinCNO(cno uint) {
	minCNO = cno
}

// WeakSignals returns the cumulative number of observations dropped for
// falling below the minimum CNO.
func WeakSignals() uint64 {
	return weakSignals.Load()
}

// UnmappedSignals returns the cumulative number of observations dropped because
// their signal has no RTCM MSM mapping.
func UnmappedSignals() uint64 {
	return unmappedSignals.Load()
}

// DroppedSats returns the cumulative number of satellites dropped to fit the
// RTCM3 length field and the 64-bit cell mask.
func DroppedSats() uint64 {
	return droppedSats.Load()
}

// NonFinitePseudoranges returns the cumulative number of observations dropped
// because the receiver reported a NaN or Inf pseudorange. The satellite leaves
// the frame entirely, so no voided-field counter can report it.
func NonFinitePseudoranges() uint64 {
	return nonFinitePseudoranges.Load()
}

// VoidedRoughRanges returns the cumulative number of satellites whose DF397
// rough range was marked unavailable.
func VoidedRoughRanges() uint64 {
	return voidedRoughRanges.Load()
}

// VoidedRoughRates returns the cumulative number of satellites whose DF399
// rough phase range rate was marked unavailable.
func VoidedRoughRates() uint64 {
	return voidedRoughRates.Load()
}

// VoidedFinePRs returns the cumulative number of cells whose DF405 fine
// pseudorange was marked unavailable.
func VoidedFinePRs() uint64 {
	return voidedFinePRs.Load()
}

// VoidedFinePhases returns the cumulative number of cells whose DF406 fine
// phase range this encoder could not represent. A phase the receiver merely
// flagged is counted by ReceiverFlaggedPhases instead; one that is also
// unusable is counted here, since no encoder could have shipped it.
func VoidedFinePhases() uint64 {
	return voidedFinePhases.Load()
}

// ReceiverFlaggedPhases returns the cumulative number of cells whose DF406 was
// marked unavailable because the receiver reported the carrier phase invalid.
// Routine on healthy epochs; a sustained run means no caster can resolve phase.
func ReceiverFlaggedPhases() uint64 {
	return receiverFlaggedPhases.Load()
}

// VoidedFineRates returns the cumulative number of cells whose DF404 fine
// phase range rate was marked unavailable.
func VoidedFineRates() uint64 {
	return voidedFineRates.Load()
}

// ErrNoObservations indicates a constellation had no usable observations to
// encode for the requested epoch. It is an expected outcome (e.g. a
// constellation not currently in view), not a programming error.
var ErrNoObservations = errors.New("no usable observations for constellation")

// EncodeMSM7 generates an RTCM MSM7 frame for one constellation from RAWX
// observations. Only observations matching gnssID are included. stationID and
// epochMs are encoded into the MSM header. It returns ErrNoObservations if the
// constellation has no usable observations this epoch, or an error if gnssID is
// not a supported MSM constellation.
func EncodeMSM7(stationID uint16, gnssID uint8, epochMs uint32, obs []RawxObservation) ([]byte, error) {
	msgType, ok := msm7MsgType[gnssID]
	if !ok {
		return nil, fmt.Errorf("unsupported constellation: %d", gnssID)
	}

	// Filter observations for this constellation with valid signal mapping.
	bits, ok := signalMaskBit[gnssID]
	if !ok {
		return nil, fmt.Errorf("no signal mapping for constellation: %d", gnssID)
	}
	var filtered []RawxObservation
	for i := range obs {
		if obs[i].GnssID != gnssID {
			continue
		}
		// Skip ghost observations. u-blox receivers report satellites they
		// track for timing without RF acquisition as zero-valued measurements
		// (PrMes=0, CpMes=0, CNO=0). Encoding them yields all-zero MSM cells
		// that prevent a caster from computing a position. A correct MSM
		// encoder never emits cells for untracked signals. This encoder is the
		// single output chokepoint, so it rejects them regardless of the caller.
		if !obs[i].PrValid || obs[i].CNO == 0 {
			continue
		}
		// -Inf satisfies PrMes <= 0, so the finite check has to precede the
		// magnitude check or corrupt data is discarded as a ghost, uncounted.
		if !isFinite(obs[i].PrMes) {
			nonFinitePseudoranges.Add(1)
			continue
		}
		// Below this the receiver still reports a pseudorange but rarely a
		// usable carrier phase, and casters score those cells as cycle slips.
		// Kept after the finite check so corrupt data stays counted as corrupt.
		if minCNO > 0 && uint(obs[i].CNO) < minCNO {
			weakSignals.Add(1)
			continue
		}
		if obs[i].PrMes <= 0 {
			continue
		}
		// Only include observations with a known signal mask bit mapping.
		if _, ok := bits[obs[i].SigID]; !ok {
			unmappedSignals.Add(1)
			continue
		}
		o := obs[i]
		// A corrupt phase costs the cell only DF406; its pseudorange still ships.
		if !isFinite(o.CpMes) {
			o.CpValid = false
		}
		filtered = append(filtered, o)
	}
	if len(filtered) == 0 {
		return nil, ErrNoObservations
	}

	// Keyed by mask bit so sigIDs sharing one RTCM signal collapse.
	satSet := map[uint8]bool{}
	sigSet := map[int]bool{}
	obsMap := map[satSig]RawxObservation{}
	for _, o := range filtered {
		bit := bits[o.SigID]
		satSet[o.SvID] = true
		sigSet[bit] = true
		obsMap[satSig{o.SvID, bit}] = o
	}

	sats := sortedKeys(satSet)
	sigs := slices.Sorted(maps.Keys(sigSet))

	// A dense multi-band epoch can overflow the 10-bit length field or the
	// 64-bit cell mask; drop the weakest satellites rather than the whole
	// constellation.
	if over := len(sats) - maxSats(len(sigs)); over > 0 {
		sats = dropWeakestSats(sats, sigs, obsMap, over)
		maps.DeleteFunc(obsMap, func(k satSig, _ RawxObservation) bool {
			return !slices.Contains(sats, k.sv)
		})
		droppedSats.Add(uint64(over))
	}

	// Build satellite mask (64 bits).
	var satMaskHi, satMaskLo uint32
	for _, sv := range sats {
		bit := int(sv) - 1 // SVs are 1-based
		if bit < 32 {
			satMaskHi |= 1 << (31 - bit)
		} else if bit < 64 {
			satMaskLo |= 1 << (63 - bit)
		}
	}

	// Build signal mask (32 bits).
	var sigMask uint32
	for _, bit := range sigs {
		sigMask |= 1 << (31 - bit)
	}

	// Build cell mask and cell list.
	numSat := len(sats)
	type cell struct {
		satIdx int
		sigIdx int
		obs    RawxObservation
	}
	var cells []cell
	var cellMaskBits []bool

	satIdx := map[uint8]int{}
	for i, sv := range sats {
		satIdx[sv] = i
	}
	sigIdx := map[int]int{}
	for i, sig := range sigs {
		sigIdx[sig] = i
	}

	// Build cell mask row by row (sat-major)
	for _, sv := range sats {
		for _, sig := range sigs {
			if o, exists := obsMap[satSig{sv, sig}]; exists {
				cellMaskBits = append(cellMaskBits, true)
				cells = append(cells, cell{satIdx: satIdx[sv], sigIdx: sigIdx[sig], obs: o})
			} else {
				cellMaskBits = append(cellMaskBits, false)
			}
		}
	}

	// Compute per-satellite rough range and rough phase range rate (m/s).
	type satInfo struct {
		roughRateMps   float64
		roughRateValid bool
		// DF397, DF398 and the fine-range reference must all derive from this
		// one value, or they can disagree by a 1/1024 ms quantum: 292.8 m.
		roughUnits uint32
		set        bool
		freqID     uint8
	}
	satData := make([]satInfo, numSat)
	for i := range satData {
		satData[i].roughUnits = invalidRoughUnits
		satData[i].freqID = glonassFreqUnknown
	}
	for _, c := range cells {
		if gnssID == GnssGLONASS && c.obs.FreqID <= glonassFreqMax {
			satData[c.satIdx].freqID = c.obs.FreqID
		}
		if !satData[c.satIdx].set {
			rangeMs := c.obs.PrMes / speedOfLight * 1000.0 // convert m to light-ms
			if roughUnits := quantizeRoughRange(rangeMs); roughUnits != invalidRoughUnits {
				satData[c.satIdx].roughUnits = roughUnits
				satData[c.satIdx].set = true
			}
		}
		if !satData[c.satIdx].roughRateValid && isFinite(float64(c.obs.DoMes)) {
			// Rough phase range rate (DF399) is the satellite range rate in
			// integer m/s: -Doppler[Hz] * wavelength[m]. The raw Doppler in Hz
			// is NOT the range rate; encoding it directly produces a nonsensical
			// phase range rate that makes casters reject the measurements.
			wavelength := getWavelength(gnssID, c.obs.SigID, c.obs.FreqID)
			// DF404 is a residual off this value, so a rate DF399 cannot carry
			// leaves the pair unavailable rather than disagreeing on the wire.
			rate := math.Round(-float64(c.obs.DoMes) * wavelength)
			if math.Abs(rate) <= maxRoughRateMps {
				satData[c.satIdx].roughRateMps = rate
				satData[c.satIdx].roughRateValid = true
			}
		}
	}

	// --- Encode MSM7 ---
	w := NewBitWriter(512)

	// Header
	w.WriteBits(uint32(msgType), 12)
	w.WriteBits(uint32(stationID), 12)
	w.WriteBits(epochMs, 30)
	w.WriteBits(0, 1) // multiple message: no
	w.WriteBits(0, 3) // IODS
	w.WriteBits(0, 7) // reserved
	w.WriteBits(0, 2) // clock steering
	w.WriteBits(0, 2) // ext clock
	w.WriteBits(0, 1) // smoothing
	w.WriteBits(0, 3) // smoothing interval
	w.WriteBits(satMaskHi, 32)
	w.WriteBits(satMaskLo, 32)
	w.WriteBits(sigMask, 32)

	// Cell mask
	for _, bit := range cellMaskBits {
		if bit {
			w.WriteBits(1, 1)
		} else {
			w.WriteBits(0, 1)
		}
	}

	// Satellite data: rough range integer ms (8 bits per sat)
	for i := range numSat {
		if satData[i].roughUnits == invalidRoughUnits {
			voidedRoughRanges.Add(1)
		}
		w.WriteBits(satData[i].roughUnits>>10, 8)
	}

	// Satellite data: extended info (4 bits per sat). DF419 for GLONASS,
	// reserved elsewhere.
	for i := range numSat {
		if gnssID == GnssGLONASS {
			w.WriteBits(uint32(satData[i].freqID), 4)
			continue
		}
		w.WriteBits(0, 4)
	}

	// Satellite data: rough range modulo (10 bits per sat)
	for i := range numSat {
		w.WriteBits(satData[i].roughUnits&1023, 10)
	}

	// Satellite data: rough phase range rate (14 bits signed per sat, m/s)
	for i := range numSat {
		if !satData[i].roughRateValid {
			voidedRoughRates.Add(1)
			w.WriteSignedBits(invalidRoughRate, 14)
			continue
		}
		w.WriteSignedBits(int32(satData[i].roughRateMps), 14)
	}

	// Signal data: fine pseudorange (20 bits signed per cell)
	for _, c := range cells {
		// With no rough range to refine, the residual is the whole measurement.
		if satData[c.satIdx].roughUnits == invalidRoughUnits {
			voidedFinePRs.Add(1)
			w.WriteSignedBits(invalidFinePR, 20)
			continue
		}
		// The rough range comes from the satellite's first cell, so this cell's
		// own range can sit arbitrarily far from it.
		rangeMs := c.obs.PrMes / speedOfLight * 1000.0
		// Scale: 2^-29 ms -> value = fineMs / 2^-29 = fineMs * 2^29
		val := math.Round((rangeMs - roughRangeMs(satData[c.satIdx].roughUnits)) * (1 << 29))
		if math.Abs(val) > maxFinePR {
			voidedFinePRs.Add(1)
			w.WriteSignedBits(invalidFinePR, 20)
			continue
		}
		w.WriteSignedBits(int32(val), 20)
	}

	// Signal data: fine phase range (24 bits signed per cell)
	for _, c := range cells {
		// DF406 refines the same rough range DF405 does, over a span four times
		// wider, so it is voided by an unusable rough range but not by a
		// pseudorange residual that only DF405 cannot carry.
		roughUnusable := satData[c.satIdx].roughUnits == invalidRoughUnits
		if !c.obs.CpValid || roughUnusable {
			// Routine trkStat state would swamp the refusals oncall watches
			// for, so the two get their own counters. Every marker written
			// here increments exactly one of them.
			switch {
			case roughUnusable || !isFinite(c.obs.CpMes):
				voidedFinePhases.Add(1)
			default:
				receiverFlaggedPhases.Add(1)
			}
			w.WriteSignedBits(invalidFinePhase, 24)
			continue
		}
		wavelength := getWavelength(gnssID, c.obs.SigID, c.obs.FreqID)
		// Phase in ms: cpMes * wavelength / speedOfLight * 1000
		phaseMs := c.obs.CpMes * wavelength / speedOfLight * 1000.0
		// Scale: 2^-31 ms. Measured off the rough range directly: folding whole
		// milliseconds away would turn a phase 1 ms out into a plausible
		// small correction, hiding a 299.8 km error.
		val := math.Round((phaseMs - roughRangeMs(satData[c.satIdx].roughUnits)) * (1 << 31))
		if math.Abs(val) > maxFinePhase {
			voidedFinePhases.Add(1)
			w.WriteSignedBits(invalidFinePhase, 24)
			continue
		}
		w.WriteSignedBits(int32(val), 24)
	}

	// Signal data: lock time indicator (10 bits per cell)
	for _, c := range cells {
		w.WriteBits(uint32(lockTimeMsToExt(c.obs.Locktime)), 10)
	}

	// Signal data: half-cycle ambiguity indicator (DF420, 1 bit per cell).
	// DF420 = 1 means a half-cycle ambiguity is present, i.e. the carrier
	// phase is NOT fully resolved. The u-blox HalfCyc flag is the opposite
	// ("half cycle valid"), so it must be inverted here. Writing it directly
	// flags every observation as ambiguous, making casters discard all
	// carrier phase ("lack usable measurements").
	for _, c := range cells {
		if c.obs.HalfCyc {
			w.WriteBits(0, 1)
		} else {
			w.WriteBits(1, 1)
		}
	}

	// Signal data: CNR (10 bits per cell, 0.0625 dB-Hz units)
	for _, c := range cells {
		val := min(
			// convert 1 dB-Hz to 0.0625 dB-Hz
			uint32(c.obs.CNO)*16, 1023)
		w.WriteBits(val, 10)
	}

	// Signal data: fine phase range rate (15 bits signed per cell)
	for _, c := range cells {
		if !satData[c.satIdx].roughRateValid || !isFinite(float64(c.obs.DoMes)) {
			voidedFineRates.Add(1)
			w.WriteSignedBits(invalidFineRate, 15)
			continue
		}
		// Doppler in Hz -> phase range rate = -doppler * wavelength (m/s)
		// Fine rate = total rate - rough rate, in 0.0001 m/s units
		wavelength := getWavelength(gnssID, c.obs.SigID, c.obs.FreqID)
		totalRate := -float64(c.obs.DoMes) * wavelength
		fineRate := totalRate - satData[c.satIdx].roughRateMps
		// Scale: 0.0001 m/s
		val := math.Round(fineRate * 10000.0)
		if math.Abs(val) > maxFineRate {
			voidedFineRates.Add(1)
			w.WriteSignedBits(invalidFineRate, 15)
			continue
		}
		w.WriteSignedBits(int32(val), 15)
	}

	// Build complete frame.
	payload := w.Bytes()
	if len(payload) > MaxPayloadLen {
		return nil, fmt.Errorf("%w: %d > %d (%d sats x %d signals)",
			ErrPayloadTooLarge, len(payload), MaxPayloadLen, numSat, len(sigs))
	}
	frameLen := HeaderSize + len(payload) + CRCSize
	frame := make([]byte, frameLen)
	frame[0] = Preamble
	frame[1] = byte((len(payload) >> 8) & 0x03)
	frame[2] = byte(len(payload) & 0xFF)
	copy(frame[HeaderSize:], payload)

	putCRC(frame)

	return frame, nil
}

func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}

// roughRangeMs converts DF397/DF398 grid units back to milliseconds. DF405 and
// DF406 both refine this value, so both must read it the same way.
func roughRangeMs(units uint32) float64 {
	return float64(units) / 1024.0
}

// quantizeRoughRange rounds a rough range onto the DF397/DF398 grid, pinning a
// range that rounds just past the top only while DF405 can carry the residual.
// Sign is tested before rounding: math.Round takes a small negative to -0.
func quantizeRoughRange(rangeMs float64) uint32 {
	if math.IsNaN(rangeMs) || rangeMs < 0 {
		return invalidRoughUnits
	}
	units := math.Round(rangeMs * 1024.0)
	switch {
	case units <= maxRoughUnits:
		return uint32(units)
	case math.Round((rangeMs*1024.0-maxRoughUnits)*(1<<19)) <= maxFinePR:
		return maxRoughUnits
	default:
		return invalidRoughUnits
	}
}

// maxSats returns the largest satellite count that fits both the 10-bit length
// field and the cell mask. DF396 is nsat*nsig bits wide however sparse the
// epoch, so sparsity does not relax the bound.
func maxSats(nsig int) int {
	fixed := msmHeaderBits + msmSatMaskBits + msmSigMaskBits
	// Each satellite costs its own bits plus, per signal, one cell-mask bit and
	// (when the cell is present) a full cell.
	perSat := msmPerSatBits + nsig*(1+msmPerCellBits)
	return min((MaxPayloadLen*8-fixed)/perSat, msmMaxCellMaskBits/nsig, msmMaxSatellite)
}

// dropWeakestSats removes n satellites, lowest total CNO first.
func dropWeakestSats(sats []uint8, sigs []int, obsMap map[satSig]RawxObservation, n int) []uint8 {
	cno := make(map[uint8]int, len(sats))
	for _, sv := range sats {
		for _, sig := range sigs {
			if o, ok := obsMap[satSig{sv, sig}]; ok {
				cno[sv] += int(o.CNO)
			}
		}
	}
	ranked := slices.Clone(sats)
	slices.SortFunc(ranked, func(a, b uint8) int {
		if cno[a] != cno[b] {
			return cmp.Compare(cno[a], cno[b])
		}
		return cmp.Compare(b, a)
	})
	keep := ranked[n:]
	slices.Sort(keep)
	return keep
}

// getWavelength returns the signal wavelength for a given GNSS/signal combination.
func getWavelength(gnssID, sigID, freqID uint8) float64 {
	if gnssID == GnssGLONASS {
		// GLONASS FDMA: frequency depends on slot
		k := int(freqID) - 7 // slot offset
		if sigID == 0 {
			return speedOfLight / (1602.0e6 + float64(k)*562.5e3)
		}
		return speedOfLight / (1246.0e6 + float64(k)*437.5e3)
	}
	if sigs, ok := signalWavelength[gnssID]; ok {
		if wl, ok := sigs[sigID]; ok {
			return wl
		}
	}
	return speedOfLight / 1575.42e6 // default GPS L1
}

// lockTimeMsToExt converts lock time in ms to the 10-bit extended indicator.
func lockTimeMsToExt(ms uint16) uint16 {
	switch {
	case ms < 64:
		return ms
	case ms < 128:
		return 64 + (ms-64)/2
	case ms < 256:
		return 96 + (ms-128)/4
	case ms < 512:
		return 128 + (ms-256)/8
	case ms < 1024:
		return 160 + (ms-512)/16
	case ms < 2048:
		return 192 + (ms-1024)/32
	case ms < 4096:
		return 224 + (ms-2048)/64
	case ms < 8192:
		return 256 + (ms-4096)/128
	case ms < 16384:
		return 288 + (ms-8192)/256
	case ms < 32768:
		return 320 + (ms-16384)/512
	default:
		return 352 + (ms-32768)/1024
	}
}

func sortedKeys(m map[uint8]bool) []uint8 {
	keys := make([]uint8, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
