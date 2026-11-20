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
	"bytes"
	"encoding/binary"
)

// EphCollector assembles broadcast ephemerides from UBX-RXM-SFRBX navigation data and
// emits them as RTCM, so the caster can position the satellites of the MSM observations.
type EphCollector struct {
	subfrm   map[uint8]*[3][30]byte // PRN -> packed subframes 1,2,3 (24-bit words)
	have     map[uint8][3]bool
	lastIODE map[uint8]uint32 // PRN -> IODE of the last emitted ephemeris
	gal      map[uint8]*galInav
	latest   map[satID][]byte
}

// satID identifies a satellite across constellations.
type satID struct {
	gnss, sv uint8
}

// NewEphCollector creates an empty ephemeris collector.
func NewEphCollector() *EphCollector {
	return &EphCollector{
		subfrm:   map[uint8]*[3][30]byte{},
		have:     map[uint8][3]bool{},
		lastIODE: map[uint8]uint32{},
		gal:      map[uint8]*galInav{},
		latest:   map[satID][]byte{},
	}
}

// All returns the latest cached ephemeris frame for every satellite with a
// complete ephemeris, for sending on (re)connect and periodically.
func (c *EphCollector) All() [][]byte {
	out := make([][]byte, 0, len(c.latest))
	for _, f := range c.latest {
		out = append(out, f)
	}
	return out
}

// AddSFRBX consumes a UBX-RXM-SFRBX payload (without the UBX header/checksum) and returns
// the RTCM ephemeris frame it completes, or nil if it completes none or nothing changed.
func (c *EphCollector) AddSFRBX(payload []byte) []byte {
	if len(payload) < 8 {
		return nil
	}
	gnssID, svID, numWords := payload[0], payload[1], int(payload[4])
	if len(payload) < 8+numWords*4 {
		return nil
	}
	words := payload[8 : 8+numWords*4]
	switch gnssID {
	case GnssGPS:
		return c.addGPS(svID, words)
	case GnssGalileo:
		return c.emit(satID{GnssGalileo, svID}, c.addGalileo(svID, words))
	}
	return nil
}

// emit caches and returns frame unless it is nil or already the satellite's cached frame.
func (c *EphCollector) emit(sat satID, frame []byte) []byte {
	if frame == nil || bytes.Equal(frame, c.latest[sat]) {
		return nil
	}
	c.latest[sat] = frame
	return frame
}

// addGPS returns RTCM 1019 when LNAV subframes 1-3 complete an ephemeris with a new IODE.
func (c *EphCollector) addGPS(svID uint8, payload []byte) []byte {
	if len(payload) < 10*4 {
		return nil
	}
	// L2C CNAV messages start with their 0x8B preamble in the top byte; LNAV words are right-aligned.
	if binary.LittleEndian.Uint32(payload[0:4])>>24 == 0x8B {
		return nil
	}

	// Each 32-bit dword carries a 30-bit nav word; the top 24 bits are data.
	var words [10]uint32
	for i := range 10 {
		dw := binary.LittleEndian.Uint32(payload[i*4 : i*4+4])
		words[i] = (dw >> 6) & 0xFFFFFF
	}
	id := (words[1] >> 2) & 7 // subframe ID from the HOW word
	if id < 1 || id > 3 {
		return nil // only subframes 1-3 carry ephemeris
	}

	// Pack the 10 24-bit words into a 30-byte subframe buffer.
	bw := NewBitWriter(30)
	for i := range 10 {
		bw.WriteBits(words[i], 24)
	}
	var buf [30]byte
	copy(buf[:], bw.Bytes())

	sf := c.subfrm[svID]
	if sf == nil {
		sf = &[3][30]byte{}
		c.subfrm[svID] = sf
	}
	sf[id-1] = buf
	hv := c.have[svID]
	hv[id-1] = true
	c.have[svID] = hv

	if hv[0] && hv[1] && hv[2] {
		// Only emit when the broadcast ephemeris actually changes (new IODE);
		// otherwise the same ephemeris would be re-sent on every subframe.
		iode := getbitu(sf[1][:], 48, 8)
		if last, ok := c.lastIODE[svID]; ok && last == iode {
			return nil
		}
		frame := encode1019(svID, sf)
		if frame == nil {
			return nil
		}
		c.latest[satID{GnssGPS, svID}] = frame
		c.lastIODE[svID] = iode
		return frame
	}
	return nil
}

// getbitu reads n bits (MSB-first) at bit offset pos as an unsigned value.
func getbitu(buf []byte, pos, n int) uint32 {
	var v uint32
	for i := pos; i < pos+n; i++ {
		v = (v << 1) | uint32((buf[i/8]>>(7-i%8))&1)
	}
	return v
}

// getbits reads n bits at bit offset pos as a two's complement signed value.
func getbits(buf []byte, pos, n int) int32 {
	v := getbitu(buf, pos, n)
	if n < 32 && v&(1<<(n-1)) != 0 {
		v |= ^uint32(0) << n // sign extend
	}
	//nolint:gosec // intentional two's-complement reinterpret of an n-bit field
	return int32(v)
}

// part is a run of bits in a navigation data buffer; a field can span several.
type part struct{ pos, n int }

// writeParts appends the bit runs of a field to w, in order.
func writeParts(w *BitWriter, buf []byte, parts ...part) {
	for _, p := range parts {
		for done := 0; done < p.n; done += 32 {
			n := min(32, p.n-done)
			w.WriteBits(getbitu(buf, p.pos+done, n), n)
		}
	}
}

// encode1019 builds an RTCM 1019 GPS ephemeris frame directly from the raw
// subframe bit fields. Every RTCM 1019 field shares the same scaling as the
// corresponding LNAV field, so the values are copied bit-for-bit with no
// physical-unit conversion.
func encode1019(prn uint8, sf *[3][30]byte) []byte {
	b1, b2, b3 := sf[0][:], sf[1][:], sf[2][:]

	// Subframe 1.
	week := getbitu(b1, 48, 10)
	code := getbitu(b1, 58, 2)
	sva := getbitu(b1, 60, 4)
	svh := getbitu(b1, 64, 6)
	iodc0 := getbitu(b1, 70, 2)
	flag := getbitu(b1, 72, 1)
	tgd := getbits(b1, 160, 8)
	iodc1 := getbitu(b1, 168, 8)
	toc := getbitu(b1, 176, 16)
	af2 := getbits(b1, 192, 8)
	af1 := getbits(b1, 200, 16)
	af0 := getbits(b1, 216, 22)
	iodc := (iodc0 << 8) | iodc1

	// Subframe 2.
	iode2 := getbitu(b2, 48, 8)
	crs := getbits(b2, 56, 16)
	deln := getbits(b2, 72, 16)
	m0 := getbits(b2, 88, 32)
	cuc := getbits(b2, 120, 16)
	ecc := getbitu(b2, 136, 32)
	cus := getbits(b2, 168, 16)
	sqrtA := getbitu(b2, 184, 32)
	toe := getbitu(b2, 216, 16)
	fit := getbitu(b2, 232, 1)

	// Subframe 3.
	cic := getbits(b3, 48, 16)
	omg0 := getbits(b3, 64, 32)
	cis := getbits(b3, 96, 16)
	i0 := getbits(b3, 112, 32)
	crc := getbits(b3, 144, 16)
	omg := getbits(b3, 160, 32)
	omgd := getbits(b3, 192, 24)
	iode3 := getbitu(b3, 216, 8)
	idot := getbits(b3, 224, 14)

	// Drop a partially-updated ephemeris (IODE/IODC must agree).
	if iode2 != iode3 || iode2 != (iodc&0xFF) {
		return nil
	}

	w := NewBitWriter(64)
	w.WriteBits(1019, 12)
	w.WriteBits(uint32(prn), 6)
	w.WriteBits(week, 10)
	w.WriteBits(sva, 4)
	w.WriteBits(code, 2)
	w.WriteSignedBits(idot, 14)
	w.WriteBits(iode2, 8)
	w.WriteBits(toc, 16)
	w.WriteSignedBits(af2, 8)
	w.WriteSignedBits(af1, 16)
	w.WriteSignedBits(af0, 22)
	w.WriteBits(iodc, 10)
	w.WriteSignedBits(crs, 16)
	w.WriteSignedBits(deln, 16)
	w.WriteSignedBits(m0, 32)
	w.WriteSignedBits(cuc, 16)
	w.WriteBits(ecc, 32)
	w.WriteSignedBits(cus, 16)
	w.WriteBits(sqrtA, 32)
	w.WriteBits(toe, 16)
	w.WriteSignedBits(cic, 16)
	w.WriteSignedBits(omg0, 32)
	w.WriteSignedBits(cis, 16)
	w.WriteSignedBits(i0, 32)
	w.WriteSignedBits(crc, 16)
	w.WriteSignedBits(omg, 32)
	w.WriteSignedBits(omgd, 24)
	w.WriteSignedBits(tgd, 8)
	w.WriteBits(svh, 6)
	w.WriteBits(flag, 1)
	w.WriteBits(fit, 1)

	return frameRTCM(w.Bytes())
}

// frameRTCM wraps an RTCM3 payload with the 3-byte header and CRC-24Q.
func frameRTCM(payload []byte) []byte {
	frameLen := HeaderSize + len(payload) + CRCSize
	frame := make([]byte, frameLen)
	frame[0] = Preamble
	frame[1] = byte((len(payload) >> 8) & 0x03)
	frame[2] = byte(len(payload) & 0xFF)
	copy(frame[HeaderSize:], payload)
	putCRC(frame)
	return frame
}
