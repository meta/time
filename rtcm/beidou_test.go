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
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// bdsC20 holds C20 B1I D1 subframes 1-3 of one frame, SOW 470760-470772, received by time02.cln3.
var bdsC20 = []string{
	"031400000a18020021179038f510a03ba2e4820830710721b6ff23374153453d73bf700ebd4600380140fd376b411100",
	"031400000a18020023279038a196b83b589b021a7378a022ea01fc06521e35081ab02f04a701ae35ec511a1edb055e14",
	"031400000a1802002c379038e424d33b24d950082200e335c9fd47023900142f0952620271da58209b089833824e4632",
}

// bds1042C20 is the 1042 of bdsC20; RTKLIB decodes it to the same ephemeris as the subframes.
const bds1042C20 = "d3004041250882025285c9080000008a8dbfea080bc24b34540f037e053700241a8f085f74a2a2f02e484001200b1b59cc0013286cebc60d6b804648c9ffede28771dc333d31"

// bdsC01 holds C01 D2 subframe 1 pages 1-10 with random bits apart from page numbers, SOW and toe.
var bdsC01 = []string{
	"030100000a000200e693570841498803a38789008d42f836ad7fcc0e68ad88378f96e531a23b261c61bbb7093ad03b16",
	"030100000a000200e0138431a4b094034b1f1c379879441d18f83a2eb69e6c3b36eb9635eff671134617b808e3279200",
	"030100000a000200eb93bd10a4cda0035507ae35885992224e255a25ee6a753e6d1fb42bab9bfd07b76c8a2cae683f05",
	"030100000a000200ed13a4242d2aad031149f2137b101f085effa836c18c65319894a705c46d4b0582c7b71a20dca939",
	"030100000a000200e5932d18cf5eb903f3041e2d014f093f62e6c6042f238f10712aa1210ab027039495b73e3e52cf05",
	"030100000a000200ed1318372a96c503ba627613f99e1a0f51e0333c8b2bea1635bdf31c8542fa2f80156313c9af962c",
	"030100000a000200e713011420d2d10355808a3eea2ddf3e11eda61659d0c73bb4d23f18405ef60525131d28d5129539",
	"030100000a000200e713d40b3404de036b802533f806d00508ef6b248a03f814f510351f6fad2f130686ac29252f9529",
	"030100000a000200ec13352b527fea03f65a512c91e23726724ed11c7f88e0113b43c1142e93d616f3d551375c413a2d",
	"030100000a000200e81396274ca8f603d915652f4a6aa218025ae41a7392b61ec7eb050cfc9fe115fce39b2073dbce3f",
}

// bds1042C01 is the 1042 of bdsC01; RTKLIB decodes it to the same ephemeris as the pages.
const bds1042C01 = "d30040412048980bd920fb7d24a2a4fc2599564974003e215a3c09f9b54398cb26ec929e311e353de7dbe0cd2f628ad4c7a2a05ab5309992c02fc4e68d348d7fc423b066ec37"

func TestEphCollectorEmits1042(t *testing.T) {
	tests := map[string]struct {
		frames []string
		want   string
	}{
		"D1 subframes 1-3 of a MEO": {bdsC20, bds1042C20},
		"D2 pages 1-10 of a GEO":    {bdsC01, bds1042C01},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := NewEphCollector()
			last := len(tt.frames) - 1
			for _, f := range tt.frames[:last] {
				require.Nil(t, c.AddSFRBX(mustHex(t, f)), "no 1042 before the last frame")
			}
			frame := c.AddSFRBX(mustHex(t, tt.frames[last]))

			require.Equal(t, tt.want, hex.EncodeToString(frame))
			require.Nil(t, c.AddSFRBX(mustHex(t, tt.frames[last])), "an unchanged ephemeris is not sent again")
		})
	}
}

func TestEphCollectorNeedsBeiDouFramesOfOneTransmission(t *testing.T) {
	c := NewEphCollector()
	c.AddSFRBX(mustHex(t, bdsC20[0]))
	c.AddSFRBX(mustHex(t, bdsC20[1]))
	// Subframe 3 of the next frame, SOW 470802 instead of 470772.
	next := "031400000a1802002c379038d4244b3c24d950082200e335c9fd47023900142f0952620271da58209b089833824e4632"

	require.Nil(t, c.AddSFRBX(mustHex(t, next)))
	require.NotNil(t, c.AddSFRBX(mustHex(t, bdsC20[2])))
}

// flipBDSBit flips bit pos of the 300-bit frame in a BeiDou SFRBX payload.
func flipBDSBit(p []byte, pos int) {
	off := 8 + 4*(pos/30)
	binary.LittleEndian.PutUint32(p[off:], binary.LittleEndian.Uint32(p[off:])^1<<(29-pos%30))
}

func TestEphCollectorRejectsBeiDouFramesThatDoNotFit(t *testing.T) {
	tests := map[string]struct {
		frames []string
		frame  int // frame with a flipped bit, -1 for none
		bit    int
		sig    byte // u-blox signal ID of every frame
	}{
		"D2 page 10 of another transmission": {bdsC01, 9, 41, 0}, // lowest SOW bit
		"D1 toc differs from toe":            {bdsC20, 0, 97, 0}, // lowest toc bit
		"B2a navigation message":             {bdsC20, -1, 0, 7},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := NewEphCollector()
			var frame []byte
			for i, f := range tt.frames {
				p := mustHex(t, f)
				p[2] = tt.sig
				if i == tt.frame {
					flipBDSBit(p, tt.bit)
				}
				frame = c.AddSFRBX(p)
			}

			require.Nil(t, frame)
			require.Empty(t, c.All())
		})
	}
}

func TestEncode1042WeekOfToe(t *testing.T) {
	tests := map[string]struct {
		week, sow, toe uint32 // BDT week and s of week of subframe 1, toe in units of 8 s
		want           uint32
	}{
		"same week":                {900, 300000, 37500, 900},
		"toe in the next week":     {900, 604700, 10, 901},
		"toe in the previous week": {900, 100, 75500, 899},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := NewEphCollector()
			for _, f := range bdsC20 {
				c.AddSFRBX(mustHex(t, f))
			}
			nav := *c.bds[20]
			setbitu(nav[:], bdsD1.week.pos, bdsD1.week.n, tt.week)
			for i, n := range []int{1, 2, 3} {
				sow := tt.sow + uint32(6*i)
				setbitu(nav[:], bdsBit(n, 18), 8, sow>>12)
				setbitu(nav[:], bdsBit(n, 30), 12, sow&0xFFF)
			}
			for _, field := range [][]part{bdsD1.toc, bdsD1.toe} {
				v, left := tt.toe, 17
				for _, p := range field {
					left -= p.n
					setbitu(nav[:], p.pos, p.n, v>>left&(1<<p.n-1))
				}
			}

			frame := encode1042(20, nav[:], &bdsD1)

			require.Equal(t, tt.want, getbitu(frame[HeaderSize:], 18, 13))
		})
	}
}
