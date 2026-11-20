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

import "encoding/binary"

// galWord is the size of an I/NAV word in bytes.
const galWord = 16

// galInav holds I/NAV word types 1-5 of a satellite, word type t at byte t*galWord as in RTKLIB.
type galInav [6 * galWord]byte

// gal1046 lists the RTCM 1046 fields after IODnav as bit runs in galInav (offsets from RTKLIB rcvraw.c).
var gal1046 = [][]part{
	{{3*128 + 120, 8}},  // SISA
	{{2*128 + 112, 14}}, // IDOT
	{{4*128 + 54, 14}},  // toc
	{{4*128 + 120, 6}},  // af2
	{{4*128 + 99, 21}},  // af1
	{{4*128 + 68, 31}},  // af0
	{{3*128 + 104, 16}}, // Crs
	{{3*128 + 40, 16}},  // delta n
	{{1*128 + 30, 32}},  // M0
	{{3*128 + 56, 16}},  // Cuc
	{{1*128 + 62, 32}},  // e
	{{3*128 + 72, 16}},  // Cus
	{{1*128 + 94, 32}},  // sqrt(A)
	{{1*128 + 16, 14}},  // toe
	{{4*128 + 22, 16}},  // Cic
	{{2*128 + 16, 32}},  // OMEGA0
	{{4*128 + 38, 16}},  // Cis
	{{2*128 + 48, 32}},  // i0
	{{3*128 + 88, 16}},  // Crc
	{{2*128 + 80, 32}},  // omega
	{{3*128 + 16, 24}},  // OMEGADOT
	{{5*128 + 47, 10}},  // BGD E5a/E1
	{{5*128 + 57, 10}},  // BGD E5b/E1
	{{5*128 + 67, 2}},   // E5b signal health
	{{5*128 + 71, 1}},   // E5b data validity
	{{5*128 + 69, 2}},   // E1-B signal health
	{{5*128 + 72, 1}},   // E1-B data validity
}

// addGalileo stores the I/NAV word of an E1-B or E5b-I page pair and returns RTCM 1046 when
// word type 5 completes an ephemeris.
func (c *EphCollector) addGalileo(sv uint8, words []byte) []byte {
	if len(words) < 8*4 {
		return nil
	}
	var page [32]byte
	for i := range 8 {
		binary.BigEndian.PutUint32(page[4*i:], binary.LittleEndian.Uint32(words[4*i:]))
	}
	// An even then an odd nominal page, and the CRC over both.
	if getbitu(page[:], 0, 2) != 0 || getbitu(page[:], 128, 2) != 2 {
		return nil
	}
	crc := NewBitWriter(25)
	crc.WriteBits(0, 4)
	writeParts(crc, page[:], part{0, 114}, part{128, 82})
	if CRC24Q(crc.Bytes()) != getbitu(page[:], 210, 24) {
		return nil
	}
	typ := int(getbitu(page[:], 2, 6))
	if typ < 1 || typ > 5 {
		return nil
	}
	nav := c.gal[sv]
	if nav == nil {
		nav = &galInav{}
		c.gal[sv] = nav
	}
	w := NewBitWriter(galWord)
	writeParts(w, page[:], part{2, 112}, part{130, 16})
	copy(nav[typ*galWord:], w.Bytes())
	if typ != 5 {
		return nil
	}
	return encode1046(sv, nav[:])
}

// encode1046 builds RTCM 1046 from I/NAV word types 1-5 that share one IODnav, copying fields
// bit for bit; RTCM uses the I/NAV scale factors.
func encode1046(sv uint8, nav []byte) []byte {
	iod := getbitu(nav, 128+6, 10)
	for t := 1; t <= 5; t++ {
		if getbitu(nav, t*128, 6) != uint32(t) || t < 5 && getbitu(nav, t*128+6, 10) != iod {
			return nil
		}
	}
	if getbitu(nav, 4*128+16, 6) != uint32(sv) {
		return nil
	}
	// RTCM carries the week of toe, which is not the week of transmission around a rollover.
	week, tow, toe := getbitu(nav, 5*128+73, 12), int(getbitu(nav, 5*128+85, 20)), int(getbitu(nav, 128+16, 14))
	switch {
	case toe*60-tow > 302400:
		week = (week + 4095) % 4096
	case toe*60-tow < -302400:
		week = (week + 1) % 4096
	}
	w := NewBitWriter(63)
	w.WriteBits(1046, 12)
	w.WriteBits(uint32(sv), 6)
	w.WriteBits(week, 12)
	w.WriteBits(iod, 10)
	for _, f := range gal1046 {
		writeParts(w, nav, f...)
	}
	w.WriteBits(0, 2)
	return frameRTCM(w.Bytes())
}
