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
	"testing"

	"github.com/stretchr/testify/require"
)

// setbitu sets n bits (MSB-first) at bit offset pos.
func setbitu(buf []byte, pos, n int, val uint32) {
	for i := range n {
		idx := pos + i
		if (val>>uint(n-1-i))&1 == 1 {
			buf[idx/8] |= 1 << uint(7-idx%8)
		} else {
			buf[idx/8] &^= 1 << uint(7-idx%8)
		}
	}
}

// gpsSubframe builds a 30-byte GPS LNAV subframe (ten 24-bit words) with the
// given subframe ID encoded in the HOW word.
func gpsSubframe(id uint32) []byte {
	sf := make([]byte, 30)
	setbitu(sf, 24, 24, id<<2) // (word1>>2)&7 == id
	return sf
}

// buildEph builds GPS LNAV subframes 1-3 for a satellite with a consistent
// IODE/IODC. The optional fields callback sets additional ephemeris values.
func buildEph(iode uint32, fields func(s1, s2, s3 []byte)) (s1, s2, s3 []byte) {
	s1, s2, s3 = gpsSubframe(1), gpsSubframe(2), gpsSubframe(3)
	setbitu(s1, 70, 2, 1)     // IODC MSBs
	setbitu(s1, 168, 8, iode) // IODC LSBs -> iodc&0xFF == iode
	setbitu(s2, 48, 8, iode)  // IODE (subframe 2)
	setbitu(s3, 216, 8, iode) // IODE (subframe 3)
	if fields != nil {
		fields(s1, s2, s3)
	}
	return
}

// sfrbx wraps a subframe in a UBX-RXM-SFRBX payload (reverses the >>6 word
// extraction the decoder performs).
func sfrbx(gnss, prn uint8, sf []byte) []byte {
	p := make([]byte, 8+10*4)
	p[0] = gnss
	p[1] = prn
	p[4] = 10
	for i := range 10 {
		binary.LittleEndian.PutUint32(p[8+i*4:12+i*4], getbitu(sf, i*24, 24)<<6)
	}
	return p
}

type decoded1019 struct {
	prn, week, iode, iodc, toe uint32
	m0                         int32
	ecc, sqrtA                 uint32
}

// decode1019 reads an RTCM 1019 frame field by field in spec order (no hardcoded
// offsets), validating framing, CRC, and that the fields exactly fill the payload.
func decode1019(t *testing.T, frame []byte) decoded1019 {
	t.Helper()
	require.Equal(t, Preamble, frame[0])
	pl := int(frame[1]&0x03)<<8 | int(frame[2])
	require.Equal(t, HeaderSize+pl+CRCSize, len(frame))
	stored := uint32(frame[len(frame)-3])<<16 | uint32(frame[len(frame)-2])<<8 | uint32(frame[len(frame)-1])
	require.Equal(t, CRC24Q(frame[:HeaderSize+pl]), stored)

	r := NewBitReader(frame[HeaderSize : HeaderSize+pl])
	require.Equal(t, uint32(1019), r.ReadBits(12))

	var d decoded1019
	d.prn = r.ReadBits(6)
	d.week = r.ReadBits(10)
	r.ReadBits(4)        // SV accuracy
	r.ReadBits(2)        // code on L2
	r.ReadSignedBits(14) // IDOT
	d.iode = r.ReadBits(8)
	r.ReadBits(16)       // toc
	r.ReadSignedBits(8)  // af2
	r.ReadSignedBits(16) // af1
	r.ReadSignedBits(22) // af0
	d.iodc = r.ReadBits(10)
	r.ReadSignedBits(16) // crs
	r.ReadSignedBits(16) // delta-n
	d.m0 = r.ReadSignedBits(32)
	r.ReadSignedBits(16) // cuc
	d.ecc = r.ReadBits(32)
	r.ReadSignedBits(16) // cus
	d.sqrtA = r.ReadBits(32)
	d.toe = r.ReadBits(16)
	r.ReadSignedBits(16) // cic
	r.ReadSignedBits(32) // OMEGA0
	r.ReadSignedBits(16) // cis
	r.ReadSignedBits(32) // i0
	r.ReadSignedBits(16) // crc
	r.ReadSignedBits(32) // omega
	r.ReadSignedBits(24) // OMEGADOT
	r.ReadSignedBits(8)  // tgd
	r.ReadBits(6)        // SV health
	r.ReadBits(1)        // L2 P data flag
	r.ReadBits(1)        // fit interval

	require.LessOrEqual(t, r.Pos(), pl*8)
	require.Less(t, pl*8-r.Pos(), 8, "1019 fields exactly fill the payload")
	return d
}

func TestEphCollectorEmits1019(t *testing.T) {
	const prn = uint8(5)
	const iode, week = uint32(0x42), uint32(0x2AB)
	m0 := int32(-123456)
	s1, s2, s3 := buildEph(iode, func(s1, s2, s3 []byte) {
		setbitu(s1, 48, 10, week)
		setbitu(s2, 88, 32, uint32(m0))  // M0
		setbitu(s2, 136, 32, 0x0A000000) // eccentricity
		setbitu(s2, 184, 32, 0x51234567) // sqrtA
		setbitu(s2, 216, 16, 450)        // toe (raw 16-bit)
	})

	c := NewEphCollector()
	require.Nil(t, c.AddSFRBX(sfrbx(GnssGPS, prn, s1)), "no emit before subframe 3")
	require.Nil(t, c.AddSFRBX(sfrbx(GnssGPS, prn, s2)), "no emit before subframe 3")
	frame := c.AddSFRBX(sfrbx(GnssGPS, prn, s3))
	require.NotNil(t, frame)

	d := decode1019(t, frame)
	require.Equal(t, uint32(prn), d.prn)
	require.Equal(t, week, d.week)
	require.Equal(t, iode, d.iode)
	require.Equal(t, iode, d.iodc&0xFF)
	require.Equal(t, m0, d.m0)
	require.Equal(t, uint32(0x0A000000), d.ecc)
	require.Equal(t, uint32(0x51234567), d.sqrtA)
	require.Equal(t, uint32(450), d.toe)
}

func TestEphCollectorEmitsOnlyOnIODEChange(t *testing.T) {
	c := NewEphCollector()
	feed := func(iode uint32) bool {
		s1, s2, s3 := buildEph(iode, nil)
		emitted := false
		for _, s := range [][]byte{s1, s2, s3} {
			if c.AddSFRBX(sfrbx(GnssGPS, 9, s)) != nil {
				emitted = true
			}
		}
		return emitted
	}
	require.True(t, feed(0x10), "first ephemeris emitted")
	require.False(t, feed(0x10), "unchanged ephemeris not re-emitted")
	require.True(t, feed(0x20), "new IODE emitted")
}

func TestEphCollectorAllPerSatellite(t *testing.T) {
	c := NewEphCollector()
	for _, prn := range []uint8{3, 11} {
		s1, s2, s3 := buildEph(0x30, nil)
		c.AddSFRBX(sfrbx(GnssGPS, prn, s1))
		c.AddSFRBX(sfrbx(GnssGPS, prn, s2))
		require.NotNil(t, c.AddSFRBX(sfrbx(GnssGPS, prn, s3)))
	}
	all := c.All()
	require.Len(t, all, 2)

	prns := map[uint32]bool{}
	for _, f := range all {
		prns[decode1019(t, f).prn] = true
	}
	require.True(t, prns[3] && prns[11], "one cached 1019 per satellite")
}

func TestEphCollectorIncompleteNoEmit(t *testing.T) {
	c := NewEphCollector()
	s1, s2, _ := buildEph(0x10, nil)
	require.Nil(t, c.AddSFRBX(sfrbx(GnssGPS, 5, s1)))
	require.Nil(t, c.AddSFRBX(sfrbx(GnssGPS, 5, s2)))
	require.Empty(t, c.All())
}

func TestEphCollectorIODEMismatchDropped(t *testing.T) {
	c := NewEphCollector()
	s1, s2, s3 := buildEph(0x10, nil)
	setbitu(s3, 216, 8, 0x99) // subframe 3 IODE disagrees
	c.AddSFRBX(sfrbx(GnssGPS, 7, s1))
	c.AddSFRBX(sfrbx(GnssGPS, 7, s2))
	require.Nil(t, c.AddSFRBX(sfrbx(GnssGPS, 7, s3)))
	require.Empty(t, c.All())
}

// An L2C CNAV message whose words happen to parse as LNAV subframes 1-3 must not become a 1019.
func TestEphCollectorIgnoresL2CNAV(t *testing.T) {
	c := NewEphCollector()
	s1, s2, s3 := buildEph(0x42, nil)
	for _, s := range [][]byte{s1, s2, s3} {
		p := sfrbx(GnssGPS, 5, s)
		p[11] = 0x8B // CNAV preamble in the top byte of the first word
		require.Nil(t, c.AddSFRBX(p))
	}
	require.Empty(t, c.All())
}

func TestEphCollectorNonGPSIgnored(t *testing.T) {
	c := NewEphCollector()
	require.Nil(t, c.AddSFRBX(sfrbx(GnssGalileo, 1, gpsSubframe(3))))
	require.Empty(t, c.All())
}

func TestEphCollectorShortPayloadIgnored(t *testing.T) {
	c := NewEphCollector()
	require.Nil(t, c.AddSFRBX([]byte{0x00, 0x01}))
}
