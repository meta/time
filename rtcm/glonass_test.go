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
	"math/bits"
	"testing"

	"github.com/stretchr/testify/require"
)

// gloR03 holds R03 L1OF strings 1-4 of one frame, received by time02.cln3.
var gloR03 = []string{
	"0603000c041f020075e036089796d8bb00b8b4f303008b0c",
	"0603000c041f02001f00b71007882c0200900eb103008b0c",
	"0603000c041f0200b835001caa864c450038d77903008b0c",
	"0603000c041f0200132f0f243013000000706b7c03008b0c",
}

// gloR03L2 holds the same strings received on L2OF.
var gloR03L2 = []string{
	"0603020c045c020075e036089796d8bb00b8b4f303008b0c",
	"0603020c045c02001f00b71007882c0200900eb103008b0c",
	"0603020c045c0200b835001caa864c450038d77903008b0c",
	"0603020c045c0200132f0f243013000000706b7c03008b0c",
}

// glo1020R03 is the 1020 of gloR03; RTKLIB decodes it to the same ephemeris as the strings.
const glo1020R03 = "d3002d3fc0d80db8b70eb77b5a5fcec203e045201ec432b708a91aa9e752800d03cbc4c1383e80000000000000000000ce24aa"

func TestEphCollectorEmits1020(t *testing.T) {
	c := NewEphCollector()
	for _, s := range gloR03[:3] {
		require.Nil(t, c.AddSFRBX(mustHex(t, s)), "no 1020 before string 4")
	}
	frame := c.AddSFRBX(mustHex(t, gloR03[3]))

	require.Equal(t, glo1020R03, hex.EncodeToString(frame))
	require.Nil(t, c.AddSFRBX(mustHex(t, gloR03[3])), "the same tb is not sent again")
	require.Equal(t, [][]byte{frame}, c.All())
}

func TestEphCollectorTakesGLONASSStringsFromBothFrequencies(t *testing.T) {
	c := NewEphCollector()
	var frames [][]byte
	for i := range gloR03 {
		for _, s := range []string{gloR03[i], gloR03L2[i]} {
			if f := c.AddSFRBX(mustHex(t, s)); f != nil {
				frames = append(frames, f)
			}
		}
	}

	require.Equal(t, [][]byte{mustHex(t, glo1020R03)}, frames)
}

func TestEphCollectorNeedsGLONASSStringsOfOneFrame(t *testing.T) {
	c := NewEphCollector()
	c.AddSFRBX(mustHex(t, gloR03[0]))
	c.AddSFRBX(mustHex(t, gloR03[1]))

	// Strings 3 and 4 of the next frame: same data, frame number 4 instead of 3.
	require.Nil(t, c.AddSFRBX(mustHex(t, "0603000c041f0200b835001caa864c450038d77904008b0c")))
	require.Nil(t, c.AddSFRBX(mustHex(t, "0603000c041f0200132f0f243013000000706b7c04008b0c")))
}

// gloStr returns the GLONASS string of an SFRBX payload, MSB first.
func gloStr(p []byte) [16]byte {
	var str [16]byte
	for i := range 4 {
		binary.BigEndian.PutUint32(str[4*i:], binary.LittleEndian.Uint32(p[8+4*i:]))
	}
	return str
}

// putGLOStr writes a GLONASS string back into an SFRBX payload.
func putGLOStr(p []byte, str [16]byte) {
	for i := range 4 {
		binary.LittleEndian.PutUint32(p[8+4*i:], binary.BigEndian.Uint32(str[4*i:]))
	}
}

// maskParity is the parity of the string bits under mask.
func maskParity(str []byte, mask [11]byte) uint32 {
	n := 0
	for i, m := range mask {
		n += bits.OnesCount8(str[i] & m)
	}
	return uint32(n & 1)
}

// setGLOHamming sets check bits 77-84 of a GLONASS string so that it passes gloStringOK.
func setGLOHamming(str []byte) {
	setbitu(str, 77, 8, 0)
	for i, mask := range gloHamming[:7] {
		setbitu(str, 84-i, 1, maskParity(str, mask))
	}
	setbitu(str, 77, 1, maskParity(str, gloHamming[7]))
}

func TestEphCollectorSendsANewDaysEphemerisWithTheSameTb(t *testing.T) {
	c := NewEphCollector()
	for _, s := range gloR03 {
		c.AddSFRBX(mustHex(t, s))
	}

	// The same strings in a later frame of the next day: NT (string 4 bits 59-69) one higher.
	var frame []byte
	var nt uint32
	for i, s := range gloR03 {
		p := mustHex(t, s)
		binary.LittleEndian.PutUint32(p[20:], 0x0c8c0001)
		if i == 3 {
			str := gloStr(p)
			nt = getbitu(str[:], 59, 11) + 1
			setbitu(str[:], 59, 11, nt)
			setGLOHamming(str[:])
			require.True(t, gloStringOK(str[:11]))
			putGLOStr(p, str)
		}
		frame = c.AddSFRBX(p)
	}

	require.NotNil(t, frame, "a new day with the same tb is a new ephemeris")
	require.Equal(t, nt, getbitu(frame[HeaderSize:], 268, 11), "DF129 NT")
}

func TestGLONASSStringHammingCheck(t *testing.T) {
	tests := map[string]struct {
		bit int // string bit to flip, -1 for none
		ok  bool
	}{
		"intact":            {-1, true},
		"flipped data bit":  {30, false},
		"flipped check bit": {84, true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := mustHex(t, gloR03[3])
			var str [16]byte
			for i := range 4 {
				binary.BigEndian.PutUint32(str[4*i:], binary.LittleEndian.Uint32(p[8+4*i:]))
			}
			if tt.bit >= 0 {
				str[tt.bit/8] ^= 1 << (7 - tt.bit%8)
			}

			require.Equal(t, tt.ok, gloStringOK(str[:11]))
		})
	}
}
