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
	"encoding/binary"
	"math/bits"
)

// gloString is the size of a GLONASS string in gloStrings, in bytes.
const gloString = 10

// gloStrings holds strings 1-4 of one GLONASS frame, string m at byte (m-1)*gloString as in RTKLIB.
type gloStrings struct {
	buf   [4 * gloString]byte
	frame uint32 // superframe and frame number that u-blox reports with each string
	have  [4]bool
	day   uint32 // NT and tb of the last emitted ephemeris; tb alone repeats every day
	sent  bool
}

var (
	// glo1020 lists the RTCM 1020 fields after the almanac health as bit runs in gloStrings.buf
	// (offsets from RTKLIB rcvraw.c).
	glo1020 = [][]part{
		{{7, 2}},    // P1
		{{9, 12}},   // tk
		{{85, 1}},   // MSB of Bn
		{{88, 1}},   // P2
		{{89, 7}},   // tb
		{{21, 24}},  // xn'
		{{50, 27}},  // xn
		{{45, 5}},   // xn''
		{{101, 24}}, // yn'
		{{130, 27}}, // yn
		{{125, 5}},  // yn''
		{{181, 24}}, // zn'
		{{210, 27}}, // zn
		{{205, 5}},  // zn''
		{{165, 1}},  // P3
		{{166, 11}}, // gamma n
		{{178, 2}},  // P
		{{180, 1}},  // ln of string 3
		{{245, 22}}, // tau n
		{{267, 5}},  // delta tau n
		{{272, 5}},  // En
		{{291, 1}},  // P4
		{{292, 4}},  // FT
		{{299, 11}}, // NT
		{{315, 2}},  // M
	}

	// gloHamming masks the string bits that each of the 8 Hamming check bits covers (RTKLIB test_glostr).
	gloHamming = [8][11]byte{
		{0x55, 0x55, 0x5A, 0xAA, 0xAA, 0xAA, 0xB5, 0x55, 0x6A, 0xD8, 0x08},
		{0x66, 0x66, 0x6C, 0xCC, 0xCC, 0xCC, 0xD9, 0x99, 0xB3, 0x68, 0x10},
		{0x87, 0x87, 0x8F, 0x0F, 0x0F, 0x0F, 0x1E, 0x1E, 0x3C, 0x70, 0x20},
		{0x07, 0xF8, 0x0F, 0xF0, 0x0F, 0xF0, 0x1F, 0xE0, 0x3F, 0x80, 0x40},
		{0xF8, 0x00, 0x0F, 0xFF, 0xF0, 0x00, 0x1F, 0xFF, 0xC0, 0x00, 0x80},
		{0x00, 0x00, 0x0F, 0xFF, 0xFF, 0xFF, 0xE0, 0x00, 0x00, 0x01, 0x00},
		{0xFF, 0xFF, 0xF0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00},
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xF8},
	}
)

// gloStringOK reports whether a GLONASS string passes its Hamming check. Failing the overall
// check and exactly one other means a flipped check bit with intact data, as in RTKLIB.
func gloStringOK(str []byte) bool {
	failed, overall := 0, 0
	for _, mask := range gloHamming {
		parity := 0
		for i, m := range mask {
			parity ^= bits.OnesCount8(str[i] & m)
		}
		overall = parity & 1
		failed += overall
	}
	return failed == 0 || failed == 2 && overall == 1
}

// addGLONASS stores an L1OF or L2OF string and returns RTCM 1020 when strings 1-4 of one frame
// carry a new day (NT) or tb.
func (c *EphCollector) addGLONASS(sv, freqID uint8, words []byte) []byte {
	if len(words) < 4*4 || sv < 1 || sv > 24 {
		return nil
	}
	var str [16]byte
	for i := range 4 {
		binary.BigEndian.PutUint32(str[4*i:], binary.LittleEndian.Uint32(words[4*i:]))
	}
	m := getbitu(str[:], 1, 4)
	if !gloStringOK(str[:11]) || m < 1 || m > 4 {
		return nil
	}
	s := c.glo[sv]
	if s == nil {
		s = &gloStrings{}
		c.glo[sv] = s
	}
	if frame := binary.LittleEndian.Uint32(words[12:16]); frame != s.frame {
		s.frame, s.have = frame, [4]bool{}
	}
	copy(s.buf[(m-1)*gloString:], str[:gloString])
	s.have[m-1] = true
	if s.have != [4]bool{true, true, true, true} {
		return nil
	}
	frame := encode1020(sv, freqID, s.buf[:])
	day := getbitu(s.buf[:], 299, 11)<<7 | getbitu(s.buf[:], 89, 7)
	if frame == nil || s.sent && s.day == day {
		return nil
	}
	s.day, s.sent = day, true
	c.latest[satID{GnssGLONASS, sv}] = frame
	return frame
}

// encode1020 builds RTCM 1020 from strings 1-4 of one frame, copying fields bit for bit since
// both are sign-magnitude; the string 5 data is marked not available.
func encode1020(sv, freqID uint8, buf []byte) []byte {
	for m := range 4 {
		if getbitu(buf, m*gloString*8+1, 4) != uint32(m+1) {
			return nil
		}
	}
	if getbitu(buf, 310, 5) != uint32(sv) {
		return nil
	}
	w := NewBitWriter(45)
	w.WriteBits(1020, 12)
	w.WriteBits(uint32(sv), 6)
	w.WriteBits(uint32(freqID), 5)
	w.WriteBits(0, 2)
	for _, f := range glo1020 {
		writeParts(w, buf, f...)
	}
	w.WriteBits(0, 32)
	w.WriteBits(0, 32)
	w.WriteBits(0, 15)
	return frameRTCM(w.Bytes())
}
