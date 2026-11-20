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
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

// galE07 holds E07 E1-B page pairs with I/NAV word types 1-5, IODnav 21, received by time02.cln3.
var galE07 = []string{
	"0207010009050200965e0501703dff051099200000c004aac6fb939e2a95f1b29352aaaa00c08ad371425a03",
	"020701000905020098470502e2494d4488c6684100c0bd0d8b647581aa9edf397b72aaaa0000812d71425a03",
	"0207010009050200ef7f05037fa949d4c73d82d400803fdafaf8daafaa7cf8f73472aaaa00c08b5b71425a03",
	"0207010009050200004705047aeaff3f3622ff5b00c0ffcf29588091aa17dafa3b7caaaa00c04aa571425a03",
	"02070100090502007fdf32058001c0fee51a0b700080eae3ae8daaaaead3c2532151aaaa0000814571425a03",
}

// gal1046E07 is the 1046 of galE07; RTKLIB decodes it to the same ephemeris as the pages.
const gal1046E07 = "d3003f4161d634156b05d5e9603ffe8dff911b67fafc9a9417fcf5c3fd480082644023dea8137a4de96003f1e611135ffea278905a31f681a2036f7ffbf510300e0072821c"

func mustHex(t *testing.T, s string) []byte {
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// galE07Nav returns a copy of the I/NAV words of galE07 as the collector stores them.
func galE07Nav(t *testing.T) []byte {
	c := NewEphCollector()
	for _, p := range galE07 {
		c.AddSFRBX(mustHex(t, p))
	}
	nav := *c.gal[7]
	return nav[:]
}

func TestEphCollectorEmits1046(t *testing.T) {
	c := NewEphCollector()
	for _, p := range galE07[:4] {
		require.Nil(t, c.AddSFRBX(mustHex(t, p)), "no 1046 before word type 5")
	}
	frame := c.AddSFRBX(mustHex(t, galE07[4]))

	require.Equal(t, gal1046E07, hex.EncodeToString(frame))
	require.Nil(t, c.AddSFRBX(mustHex(t, galE07[4])), "an unchanged ephemeris is not sent again")
	require.Equal(t, [][]byte{frame}, c.All())
}

func TestEphCollectorDropsGalileoPageWithBadCRC(t *testing.T) {
	c := NewEphCollector()
	for _, p := range galE07[:4] {
		c.AddSFRBX(mustHex(t, p))
	}
	bad := mustHex(t, galE07[4])
	bad[9] ^= 0x01 // a data bit of the even page

	require.Nil(t, c.AddSFRBX(bad))
	require.NotNil(t, c.AddSFRBX(mustHex(t, galE07[4])))
}

func TestEncode1046NeedsOneIODnavAndSVID(t *testing.T) {
	tests := map[string]struct {
		pos, n int
		val    uint32
	}{
		"IODnav of word type 3 differs": {3*128 + 6, 10, 22},
		"SVID of word type 4 differs":   {4*128 + 16, 6, 8},
		"word type 2 missing":           {2 * 128, 6, 0},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			nav := galE07Nav(t)
			require.NotNil(t, encode1046(7, nav))
			setbitu(nav, tt.pos, tt.n, tt.val)
			require.Nil(t, encode1046(7, nav))
		})
	}
}

func TestEncode1046WeekOfToe(t *testing.T) {
	tests := map[string]struct {
		week, tow, toe uint32 // week and s of week of transmission, min of week of toe
		want           uint32
	}{
		"same week":                     {1400, 300000, 5000, 1400},
		"toe in the next week":          {1400, 604700, 10, 1401},
		"toe in the previous week":      {1400, 100, 10070, 1399},
		"previous week across rollover": {0, 100, 10070, 4095},
		"next week across rollover":     {4095, 604700, 10, 0},
		"half a week ahead stays":       {1400, 0, 5040, 1400},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			nav := galE07Nav(t)
			setbitu(nav, 5*128+73, 12, tt.week)
			setbitu(nav, 5*128+85, 20, tt.tow)
			setbitu(nav, 128+16, 14, tt.toe)

			frame := encode1046(7, nav)

			require.Equal(t, tt.want, getbitu(frame[HeaderSize:], 18, 12))
		})
	}
}
