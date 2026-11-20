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

// bdsFrameBytes holds one 300-bit B1I subframe or page with its parity bits.
const bdsFrameBytes = 38

// bdsNav holds a satellite's D1 subframes 1-3 or D2 subframe 1 pages 1-10, frame n at byte
// (n-1)*bdsFrameBytes as in RTKLIB.
type bdsNav [10 * bdsFrameBytes]byte

// bdsLayout locates the RTCM 1042 inputs in a bdsNav (offsets from RTKLIB rcvraw.c).
type bdsLayout struct {
	week     part
	toc, toe []part
	fields   [][]part // the RTCM 1042 fields after the week
	frames   []uint32 // subframe or page numbers that carry the ephemeris
	period   uint32   // seconds between consecutive subframes or pages
	pages    bool     // frames are D2 page numbers
}

var (
	bdsD1Toc = []part{{bdsBit(1, 73), 9}, {bdsBit(1, 90), 8}}
	bdsD1Toe = []part{{bdsBit(2, 290), 2}, {bdsBit(3, 42), 10}, {bdsBit(3, 60), 5}}
	bdsD2Toc = []part{{bdsBit(1, 77), 5}, {bdsBit(1, 90), 12}}
	bdsD2Toe = []part{{bdsBit(7, 80), 2}, {bdsBit(7, 90), 15}}

	// bdsD1 is the D1 layout of IGSO and MEO satellites.
	bdsD1 = bdsLayout{
		week: part{bdsBit(1, 60), 13},
		toc:  bdsD1Toc,
		toe:  bdsD1Toe,
		fields: [][]part{
			{{bdsBit(1, 48), 4}},                        // URAI
			{{bdsBit(3, 189), 13}, {bdsBit(3, 210), 1}}, // IDOT
			{{bdsBit(1, 287), 5}},                       // AODE
			bdsD1Toc,
			{{bdsBit(1, 214), 11}},                       // a2
			{{bdsBit(1, 257), 5}, {bdsBit(1, 270), 17}},  // a1
			{{bdsBit(1, 225), 7}, {bdsBit(1, 240), 17}},  // a0
			{{bdsBit(1, 43), 5}},                         // AODC
			{{bdsBit(2, 224), 8}, {bdsBit(2, 240), 10}},  // Crs
			{{bdsBit(2, 42), 10}, {bdsBit(2, 60), 6}},    // delta n
			{{bdsBit(2, 92), 20}, {bdsBit(2, 120), 12}},  // M0
			{{bdsBit(2, 66), 16}, {bdsBit(2, 90), 2}},    // Cuc
			{{bdsBit(2, 132), 10}, {bdsBit(2, 150), 22}}, // e
			{{bdsBit(2, 180), 18}},                       // Cus
			{{bdsBit(2, 250), 12}, {bdsBit(2, 270), 20}}, // sqrt(A)
			bdsD1Toe,
			{{bdsBit(3, 105), 7}, {bdsBit(3, 120), 11}},  // Cic
			{{bdsBit(3, 211), 21}, {bdsBit(3, 240), 11}}, // OMEGA0
			{{bdsBit(3, 163), 9}, {bdsBit(3, 180), 9}},   // Cis
			{{bdsBit(3, 65), 17}, {bdsBit(3, 90), 15}},   // i0
			{{bdsBit(2, 198), 4}, {bdsBit(2, 210), 14}},  // Crc
			{{bdsBit(3, 251), 11}, {bdsBit(3, 270), 21}}, // omega
			{{bdsBit(3, 131), 11}, {bdsBit(3, 150), 13}}, // OMEGADOT
			{{bdsBit(1, 98), 10}},                        // TGD1
			{{bdsBit(1, 108), 4}, {bdsBit(1, 120), 6}},   // TGD2
			{{bdsBit(1, 42), 1}},                         // SatH1
		},
		frames: []uint32{1, 2, 3},
		period: 6,
	}

	// bdsD2 is the D2 layout of GEO satellites.
	bdsD2 = bdsLayout{
		week: part{bdsBit(1, 64), 13},
		toc:  bdsD2Toc,
		toe:  bdsD2Toe,
		fields: [][]part{
			{{bdsBit(1, 60), 4}},                        // URAI
			{{bdsBit(10, 51), 1}, {bdsBit(10, 60), 13}}, // IDOT
			{{bdsBit(4, 91), 5}},                        // AODE
			bdsD2Toc,
			{{bdsBit(4, 72), 10}, {bdsBit(4, 90), 1}}, // a2
			{{bdsBit(3, 132), 4}, {bdsBit(4, 46), 6}, {bdsBit(4, 60), 12}}, // a1
			{{bdsBit(3, 100), 12}, {bdsBit(3, 120), 12}},                   // a0
			{{bdsBit(1, 47), 5}},  // AODC
			{{bdsBit(8, 91), 18}}, // Crs
			{{bdsBit(4, 96), 16}}, // delta n
			{{bdsBit(5, 50), 2}, {bdsBit(5, 60), 22}, {bdsBit(5, 90), 8}},   // M0
			{{bdsBit(4, 120), 14}, {bdsBit(5, 46), 4}},                      // Cuc
			{{bdsBit(5, 124), 10}, {bdsBit(6, 46), 6}, {bdsBit(6, 60), 16}}, // e
			{{bdsBit(5, 98), 14}, {bdsBit(5, 120), 4}},                      // Cus
			{{bdsBit(6, 76), 6}, {bdsBit(6, 90), 22}, {bdsBit(6, 120), 4}},  // sqrt(A)
			bdsD2Toe,
			{{bdsBit(6, 124), 10}, {bdsBit(7, 46), 6}, {bdsBit(7, 60), 2}}, // Cic
			{{bdsBit(9, 51), 1}, {bdsBit(9, 60), 22}, {bdsBit(9, 90), 9}},  // OMEGA0
			{{bdsBit(7, 62), 18}}, // Cis
			{{bdsBit(7, 105), 7}, {bdsBit(7, 120), 14}, {bdsBit(8, 46), 6}, {bdsBit(8, 60), 5}}, // i0
			{{bdsBit(8, 65), 17}, {bdsBit(8, 90), 1}},                                           // Crc
			{{bdsBit(9, 99), 13}, {bdsBit(9, 120), 14}, {bdsBit(10, 46), 5}},                    // omega
			{{bdsBit(8, 109), 3}, {bdsBit(8, 120), 16}, {bdsBit(9, 46), 5}},                     // OMEGADOT
			{{bdsBit(1, 102), 10}}, // TGD1
			{{bdsBit(1, 120), 10}}, // TGD2
			{{bdsBit(1, 46), 1}},   // SatH1
		},
		frames: []uint32{1, 3, 4, 5, 6, 7, 8, 9, 10},
		period: 3,
		pages:  true,
	}
)

// bdsBit is the position in a bdsNav of bit pos of subframe or page n.
func bdsBit(n, pos int) int {
	return (n-1)*bdsFrameBytes*8 + pos
}

// addBeiDou stores a D1 subframe or D2 page and returns RTCM 1042 when the last frame of an
// ephemeris arrives.
func (c *EphCollector) addBeiDou(sv, sig uint8, words []byte) []byte {
	// D1 and D2 ride on B1I, B2I and B3I (u-blox signals 0-4 and 10); B1C and B2a use other formats.
	if len(words) < 10*4 || sig > 4 && sig != 10 {
		return nil
	}
	w := NewBitWriter(bdsFrameBytes)
	for i := range 10 {
		w.WriteBits(binary.LittleEndian.Uint32(words[4*i:])&0x3FFFFFFF, 30)
	}
	frame := w.Bytes()
	layout, n := &bdsD1, getbitu(frame, 15, 3)
	if sv <= 5 || sv >= 59 {
		if n != 1 {
			return nil
		}
		layout, n = &bdsD2, getbitu(frame, 42, 4)
	}
	last := layout.frames[len(layout.frames)-1]
	if n < 1 || n > last {
		return nil
	}
	nav := c.bds[sv]
	if nav == nil {
		nav = &bdsNav{}
		c.bds[sv] = nav
	}
	copy(nav[(n-1)*bdsFrameBytes:], frame)
	if n != last {
		return nil
	}
	return encode1042(sv, nav[:], layout)
}

// encode1042 builds RTCM 1042 from the frames of one D1 or D2 transmission, copying fields bit
// for bit; RTCM uses the BeiDou scale factors.
func encode1042(sv uint8, nav []byte, l *bdsLayout) []byte {
	var sow0 uint32
	for i, n := range l.frames {
		at := bdsBit(int(n), 0)
		num, sow := getbitu(nav, at+15, 3), readParts(nav, part{at + 18, 8}, part{at + 30, 12})
		if l.pages {
			num = getbitu(nav, at+42, 4)
		}
		if i == 0 {
			sow0 = sow
		}
		if num != n || sow != sow0+l.period*(n-l.frames[0]) {
			return nil
		}
	}
	toe := readParts(nav, l.toe...)
	if readParts(nav, l.toc...) != toe {
		return nil
	}
	// RTCM carries the week of toe, which is not the week of transmission around a rollover.
	week := readParts(nav, l.week)
	switch {
	case int(toe)*8 > int(sow0)+302400:
		week = (week + 8191) % 8192
	case int(toe)*8 < int(sow0)-302400:
		week = (week + 1) % 8192
	}
	w := NewBitWriter(64)
	w.WriteBits(1042, 12)
	w.WriteBits(uint32(sv), 6)
	w.WriteBits(week, 13)
	for _, f := range l.fields {
		writeParts(w, nav, f...)
	}
	return frameRTCM(w.Bytes())
}
