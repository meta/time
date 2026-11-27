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

package protocol

import (
	"bytes"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/facebook/time/timestamp"
	"golang.org/x/sys/unix"

	"github.com/stretchr/testify/require"
)

var (
	// Unix
	usec  = int64(1585147599)
	unsec = int64(631495778)
	// NTP
	nsec  = uint32(3794136399)
	nfrac = uint32(2712253714)

	// Network Delays
	forwardDelay   = 10 * time.Millisecond
	returnDelay    = 20 * time.Millisecond
	symmetricDelay = 25 * time.Millisecond

	// roundTripDelay nanoseconds
	roundTripDelay = int64(30000000)

	// offset between local and remote clock
	offset = int64(-5_000_000)

	// Packet request. From ntpdate run
	ntpRequest = &Packet{
		Settings:       227,
		Stratum:        0,
		Poll:           3,
		Precision:      -6,
		RootDelay:      65536,
		RootDispersion: 65536,
		ReferenceID:    0,
		RefTimeSec:     0,
		RefTimeFrac:    0,
		OrigTimeSec:    0,
		OrigTimeFrac:   0,
		RxTimeSec:      0,
		RxTimeFrac:     0,
		TxTimeSec:      3794210679,
		TxTimeFrac:     2718216404,
	}

	// Same request as above in bytes
	ntpRequestBytes = []byte{227, 0, 3, 250, 0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 226, 39, 15, 119, 162, 4, 176, 212}

	// Packet response
	ntpResponse = &Packet{
		Settings:       36,
		Stratum:        1,
		Poll:           3,
		Precision:      -32,
		RootDelay:      0,
		RootDispersion: 10,
		ReferenceID:    1178738720,
		RefTimeSec:     3794209800,
		RefTimeFrac:    0,
		OrigTimeSec:    3794210679,
		OrigTimeFrac:   2718216404,
		RxTimeSec:      3794210679,
		RxTimeFrac:     2718375472,
		TxTimeSec:      3794210679,
		TxTimeFrac:     2719753478,
	}
	// Same response as above in bytes
	ntpResponseBytes = []byte{36, 1, 3, 224, 0, 0, 0, 0, 0, 0, 0, 10, 70, 66, 32, 32, 226, 39, 12, 8, 0, 0, 0, 0, 226, 39, 15, 119, 162, 4, 176, 212, 226, 39, 15, 119, 162, 7, 30, 48, 226, 39, 15, 119, 162, 28, 37, 6}

	ntpBadRequest = &Packet{Settings: 0}
)

// Testing conversion so if Packet structure changes we notice
func TestRequestConversion(t *testing.T) {
	bytes, err := ntpRequest.Bytes()
	require.NoError(t, err)
	require.Equal(t, ntpRequestBytes, bytes)
}

// Testing conversion so if Packet structure changes we notice
func TestResponseConversion(t *testing.T) {
	bytes, err := ntpResponse.Bytes()
	require.NoError(t, err)
	require.Equal(t, ntpResponseBytes, bytes)
}

// TestPacketHeaderRoundTrip checks Bytes -> UnmarshalBinary is an exact identity
// across every header field. The hand-rolled codec has no reflection safety net,
// so a new field left unwired, or a wrong byte offset, would silently drop or
// corrupt a value; the whole-struct compare catches it.
func TestPacketHeaderRoundTrip(t *testing.T) {
	p := &Packet{
		Settings: 0x23, Stratum: 2, Poll: 6, Precision: -25,
		RootDelay: 0x11111111, RootDispersion: 0x22222222, ReferenceID: 0x33333333,
		RefTimeSec: 0x44444444, RefTimeFrac: 0x55555555,
		OrigTimeSec: 0x66666666, OrigTimeFrac: 0x77777777,
		RxTimeSec: 0x88888888, RxTimeFrac: 0x99999999,
		TxTimeSec: 0xAAAAAAAA, TxTimeFrac: 0xBBBBBBBB,
	}
	b, err := p.Bytes()
	require.NoError(t, err)
	var got Packet
	require.NoError(t, got.UnmarshalBinary(b))
	require.Equal(t, *p, got)
}

// withTrailer returns ntpRequestBytes followed by trailer.
func withTrailer(trailer ...[]byte) []byte {
	b := bytes.Clone(ntpRequestBytes)
	for _, t := range trailer {
		b = append(b, t...)
	}
	return b
}

// legacyMAC returns an unframed MAC: a key identifier no extension-field parser
// can read as a {type, length}, plus a digest of the given length.
func legacyMAC(digestLen int) []byte {
	keyID := make([]byte, 4, 4+digestLen)
	if digestLen > 0 {
		keyID[3] = 1
	}
	return append(keyID, bytes.Repeat([]byte{0xAB}, digestLen)...)
}

// TestUnmarshalBinaryLegacyMAC pins the split: an unframed trailer at one of the
// three RFC 5905 MAC lengths lands in Packet.MAC instead of failing the parse. Any
// other length is still an error.
func TestUnmarshalBinaryLegacyMAC(t *testing.T) {
	tests := []struct {
		name      string
		digestLen int
		want      bool
	}{
		{"CryptoNAK", 0, true},
		{"MD5", 16, true},
		{"SHA1", 20, true},
		// No RFC 5905 digest is this long, so these are malformed, not authenticated.
		{"ShorterThanAnyDigest", 4, false},
		{"BetweenMD5AndSHA1", 18, false},
		{"SHA256", 32, false},
		{"SHA512", 64, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mac := legacyMAC(tc.digestLen)
			b := withTrailer(mac)
			var p Packet
			err := p.UnmarshalBinary(b)
			if !tc.want {
				require.Error(t, err, "only an RFC 5905 MAC length earns a reply")
				require.Nil(t, p.MAC)
				return
			}
			require.NoError(t, err)
			require.Equal(t, mac, p.MAC)
			require.Empty(t, p.ExtensionFields)
		})
	}
	t.Run("NonzeroKeyIDWithoutDigest", func(t *testing.T) {
		var p Packet
		require.Error(t, p.UnmarshalBinary(withTrailer([]byte{0x00, 0x00, 0x00, 0x01})))
		require.Nil(t, p.MAC)
	})
}

// TestUnmarshalBinaryCopiesMAC pins that the MAC does not alias the caller's
// buffer, which the responder reuses across reads.
func TestUnmarshalBinaryCopiesMAC(t *testing.T) {
	mac := legacyMAC(16)
	b := withTrailer(mac)
	var p Packet
	require.NoError(t, p.UnmarshalBinary(b))
	clear(b)
	require.Equal(t, mac, p.MAC)
}

// TestUnmarshalBinaryReadsAMACBehindExtensionFields covers the other shape RFC 5905
// allows: extension fields, then a MAC. The fields frame, so the MAC starts where
// framing stopped and no length guess is needed. Totals here avoid 4, 20 and 24
// octets, where the trailer is a MAC whole and
// TestUnmarshalBinaryReadsAShortTrailerWholeEvenBehindAField applies instead.
func TestUnmarshalBinaryReadsAMACBehindExtensionFields(t *testing.T) {
	// The shortest field RFC 7822 allows ahead of another, written out so the
	// octets a client would put on the wire are visible.
	minimalField := slices.Concat([]byte{0x05, 0x00, 0x00, 0x10}, bytes.Repeat([]byte{0xCD}, 12))
	tests := []struct {
		name   string
		fields []byte
		want   []ExtensionField
		mac    []byte
	}{
		{"MinimumFieldThenMD5", minimalField,
			[]ExtensionField{{Type: 0x0500, Body: bytes.Repeat([]byte{0xCD}, 12)}}, legacyMAC(16)},
		{"MinimumFieldThenSHA1", minimalField,
			[]ExtensionField{{Type: 0x0500, Body: bytes.Repeat([]byte{0xCD}, 12)}}, legacyMAC(20)},
		{"TwoFieldsThenMD5", slices.Concat(minimalField, minimalField),
			[]ExtensionField{
				{Type: 0x0500, Body: bytes.Repeat([]byte{0xCD}, 12)},
				{Type: 0x0500, Body: bytes.Repeat([]byte{0xCD}, 12)},
			}, legacyMAC(16)},
		{"LongFieldThenCryptoNAK", slices.Concat([]byte{0x05, 0x00, 0x00, 0x1C}, make([]byte, 24)),
			[]ExtensionField{{Type: 0x0500, Body: make([]byte, 24)}}, legacyMAC(0)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, isLegacyMAC(slices.Concat(tc.fields, tc.mac)), "trailer must not be a MAC whole")
			var p Packet
			require.NoError(t, p.UnmarshalBinary(withTrailer(tc.fields, tc.mac)))
			require.Equal(t, tc.want, p.ExtensionFields)
			require.Equal(t, tc.mac, p.MAC)
		})
	}
}

// TestUnmarshalBinaryRejectsATailBehindExtensionFields pins the limits on that
// split. A tail no RFC 5905 digest length explains is still a parse error; a tail
// that would pass for a MAC is refused once anything names NTS, since answering
// that in plain NTP would downgrade it; and a key identifier that frames is only
// recovered when the trailer is a MAC whole, which a field ahead of it rules out.
func TestUnmarshalBinaryRejectsATailBehindExtensionFields(t *testing.T) {
	tests := []struct {
		name    string
		trailer []byte
	}{
		{"TailIsNoDigestLength", slices.Concat(
			[]byte{0x05, 0x00, 0x00, 0x10}, bytes.Repeat([]byte{0xCD}, 12), legacyMAC(8))},
		// A 4-octet cookie frames cleanly, so only the parsed fields give NTS away.
		// The trailer is 24 octets, a MAC length, so nothing else would stop it.
		{"MACLengthTailBehindAnNTSField", slices.Concat(
			[]byte{0x02, 0x04, 0x00, 0x04}, legacyMAC(16))},
		// Key identifier 0x00000004 frames as an empty field, so parsing stops 4
		// octets late and the tail is 16, not 20. Alone this trailer is recovered
		// whole (TestUnmarshalBinaryRecoversMACWhoseKeyIDFrames/EmptyFieldThenMD5);
		// behind a field it is not, and separating the two needs a search this
		// deliberately does not do.
		{"FramingKeyIDBehindAField", slices.Concat(
			[]byte{0x05, 0x00, 0x00, 0x10}, bytes.Repeat([]byte{0xCD}, 12),
			[]byte{0x00, 0x00, 0x00, 0x04}, bytes.Repeat([]byte{0xAB}, 16))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p Packet
			require.Error(t, p.UnmarshalBinary(withTrailer(tc.trailer)))
			require.Nil(t, p.MAC)
			require.Nil(t, p.ExtensionFields)
		})
	}
}

// TestUnmarshalBinaryReadsFramedKeyIDAsExtensionField pins a residual ambiguity: a
// key identifier whose low 16 bits are a valid field length frames the whole MAC as
// one field, so the parse succeeds and MAC stays nil -- master reads these the same
// way. Preferring the MAC would misread a well-framed 20- or 24-octet field.
func TestUnmarshalBinaryReadsFramedKeyIDAsExtensionField(t *testing.T) {
	tests := []struct {
		name      string
		keyID     []byte
		digestLen int
	}{
		{"MD5LengthKeyID", []byte{0x00, 0x00, 0x00, 0x14}, 16},
		{"SHA1LengthKeyID", []byte{0x00, 0x00, 0x00, 0x18}, 20},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trailer := slices.Concat(tc.keyID, bytes.Repeat([]byte{0xAB}, tc.digestLen))
			var p Packet
			require.NoError(t, p.UnmarshalBinary(withTrailer(trailer)))
			require.Nil(t, p.MAC)
			require.Equal(t, []ExtensionField{{Type: 0x0000, Body: trailer[ExtensionHeaderSize:]}}, p.ExtensionFields)
		})
	}
}

// TestUnmarshalBinaryRecoversMACWhoseKeyIDFrames covers the case between the two
// tests above: a key identifier whose low 16 bits frame a field ending inside the
// digest rather than at the end of the trailer. Taking the trailer whole keeps the
// key identifier attached to its digest.
func TestUnmarshalBinaryRecoversMACWhoseKeyIDFrames(t *testing.T) {
	tests := []struct {
		name      string
		keyID     []byte
		digestLen int
	}{
		// low 16 bits = 4: an empty field, leaving 20 octets that are a MAC length.
		{"EmptyFieldThenSHA1", []byte{0x00, 0x00, 0x00, 0x04}, 20},
		// The type octets are arbitrary too, so this is not a zero-key-id quirk.
		{"NonZeroTypeOctets", []byte{0x12, 0x34, 0x00, 0x04}, 20},
		// low 16 bits = 4 with an MD5 digest: 16 left over, not a MAC length, so
		// before this fix the whole packet failed to parse instead.
		{"EmptyFieldThenMD5", []byte{0x00, 0x00, 0x00, 0x04}, 16},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mac := slices.Concat(tc.keyID, bytes.Repeat([]byte{0xAB}, tc.digestLen))
			require.True(t, isLegacyMAC(mac), "test fixture must be a MAC")
			var p Packet
			require.NoError(t, p.UnmarshalBinary(withTrailer(mac)))
			require.Equal(t, mac, p.MAC)
			require.Empty(t, p.ExtensionFields, "the key identifier is not a field")
		})
	}
}

// TestUnmarshalBinaryReadsAShortTrailerWholeEvenBehindAField pins the other side of
// that decision. When a well-framed field and the octets after it together come to a
// MAC length, the trailer is taken whole and the field goes with it. That favours
// what real traffic sends -- a plain authenticated packet carrying no fields at all.
// Either reading answers the client the same: neither trailer names NTS, so the
// responder replies in plain NTP.
func TestUnmarshalBinaryReadsAShortTrailerWholeEvenBehindAField(t *testing.T) {
	tests := []struct {
		name    string
		trailer []byte
	}{
		// Not a crypto-NAK: those are key identifier zero, and taken alone these four
		// octets are rejected by TestUnmarshalBinaryLegacyMAC/NonzeroKeyIDWithoutDigest.
		{"FieldThenTrailingKeyID", slices.Concat(
			[]byte{0x05, 0x00, 0x00, 0x14}, bytes.Repeat([]byte{0xAB}, 16), []byte{0x00, 0x00, 0x00, 0x01})},
		{"EmptyFieldThenMD5", slices.Concat(
			[]byte{0x05, 0x00, 0x00, 0x04}, []byte{0x00, 0x00, 0x00, 0x01}, bytes.Repeat([]byte{0xAB}, 16))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, isLegacyMAC(tc.trailer), "test fixture must be a MAC")
			var p Packet
			require.NoError(t, p.UnmarshalBinary(withTrailer(tc.trailer)))
			require.Equal(t, tc.trailer, p.MAC)
			require.Empty(t, p.ExtensionFields)
		})
	}
}

// TestUnmarshalBinaryRejectsNTSTrailerAsMAC pins that a trailer naming an NTS field
// type is not read as a MAC: answering it in plain NTP would downgrade a request
// that named NTS. Cases split by where the name sits -- at the header that failed to
// frame, or in a field ahead of it -- which is what the guard reads. Every case is a
// legal MAC length, so that guard is all that stands between it and a reply.
func TestUnmarshalBinaryRejectsNTSTrailerAsMAC(t *testing.T) {
	// A 4-octet non-NTS field, a well-framed 4-octet NTS field, then 16 octets that
	// do not frame: 24 in all, with the failure past the NTS field.
	framedNTSThenGarbage := func(ntsHeader []byte) []byte {
		b := append([]byte{0x00, 0x00, 0x00, 0x04}, ntsHeader...)
		b = append(b, 0x00, 0x00, 0x00, 0x03)
		return append(b, make([]byte, 12)...)
	}
	tests := []struct {
		name    string
		trailer []byte
	}{
		// The NTS field is the one that failed to frame, so it sits exactly where a
		// MAC would start.
		{"UniqueIdentifier", append([]byte{0x01, 0x04, 0x00, 0x24}, make([]byte, 20)...)},
		{"Cookie", append([]byte{0x02, 0x04, 0x00, 0x40}, make([]byte, 16)...)},
		{"Authenticator", append([]byte{0x04, 0x04, 0x00, 0x40}, make([]byte, 16)...)},
		{"UnalignedLength", append([]byte{0x03, 0x04, 0x00, 0x06}, make([]byte, 16)...)},
		{"BehindAFramedKeyID",
			append([]byte{0x00, 0x00, 0x00, 0x04, 0x02, 0x04, 0x00, 0x40}, make([]byte, 16)...)},
		// The NTS field framed cleanly, so it is behind where framing stopped and
		// only the parsed fields give it away.
		{"FramedAuthenticator", framedNTSThenGarbage([]byte{0x04, 0x04, 0x00, 0x04})},
		{"FramedCookie", framedNTSThenGarbage([]byte{0x02, 0x04, 0x00, 0x04})},
		{"FramedUniqueIdentifier", framedNTSThenGarbage([]byte{0x01, 0x04, 0x00, 0x04})},
		{"FramedCookiePlaceholder", framedNTSThenGarbage([]byte{0x03, 0x04, 0x00, 0x04})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p Packet
			require.Error(t, p.UnmarshalBinary(withTrailer(tc.trailer)))
			require.Nil(t, p.MAC)
			require.Nil(t, p.ExtensionFields)
		})
	}
}

func TestBytesToPacket(t *testing.T) {
	packet, err := BytesToPacket(ntpResponseBytes)
	require.NoError(t, err)
	require.Equal(t, ntpResponse, packet)
}

func TestBytesToPacketError(t *testing.T) {
	bytes := []byte{}
	packet, err := BytesToPacket(bytes)
	require.NotNil(t, err)
	require.Equal(t, &Packet{}, packet)
}

// Testing conversion so if Packet structure changes we notice
func TestPacketConversionFailure(t *testing.T) {
	bytes, err := ntpRequest.Bytes()
	require.NoError(t, err)
	require.Equal(t, ntpRequestBytes, bytes)
}

func TestRequestSize(t *testing.T) {
	require.Equal(t, PacketSizeBytes, len(ntpRequestBytes))
}

func TestResponseSize(t *testing.T) {
	require.Equal(t, PacketSizeBytes, len(ntpResponseBytes))
}

func TestValidSettingsFormat(t *testing.T) {
	require.True(t, ntpRequest.ValidSettingsFormat())
}

func TestInvalidSettingsFormat(t *testing.T) {
	require.False(t, ntpBadRequest.ValidSettingsFormat())
}

func TestTime(t *testing.T) {
	testtime := time.Unix(usec, unsec)
	sec, frac := Time(testtime)

	require.Equal(t, nsec, sec)
	require.Equal(t, nfrac, frac)
}

func TestUnix(t *testing.T) {
	testtime := Unix(nsec, nfrac)

	require.Equal(t, usec, testtime.Unix())
	require.Equal(t, unsec, int64(testtime.Nanosecond()))
}

func TestRoundTripDelay(t *testing.T) {
	// Time on server is = of time on client

	originTime := time.Now()
	// Network delay client -> server 10ms
	serverReceiveTime := originTime.Add(forwardDelay)
	// OS delay server 10us
	serverTransmitTime := serverReceiveTime.Add(10 * time.Microsecond)
	// Network delay client -> server 20ms
	clientReceiveTime := serverTransmitTime.Add(returnDelay)

	actualRoundTripDelay := RoundTripDelay(originTime, serverReceiveTime, serverTransmitTime, clientReceiveTime)
	require.Equal(t, roundTripDelay, actualRoundTripDelay)
}

func TestRoundTripDelayPositive(t *testing.T) {
	// Assuming time on client is > of time on server
	clientServerTsDelta := 50 * time.Millisecond

	originTime := time.Now()
	// Network delay client -> server 10ms
	serverReceiveTime := originTime.Add(forwardDelay)
	// OS delay server 10us
	serverTransmitTime := serverReceiveTime.Add(10 * time.Microsecond)
	// Network delay client -> server 20ms
	clientReceiveTime := serverTransmitTime.Add(returnDelay)

	actualRoundTripDelay := RoundTripDelay(originTime.Add(clientServerTsDelta), serverReceiveTime, serverTransmitTime, clientReceiveTime.Add(clientServerTsDelta))
	require.Equal(t, roundTripDelay, actualRoundTripDelay)
}

func TestRoundTripDelayNegative(t *testing.T) {
	// Assuming time on client is < of time on server
	clientServerTsDelta := -50 * time.Millisecond

	originTime := time.Now()
	// Network delay client -> server 10ms
	serverReceiveTime := originTime.Add(forwardDelay)
	// OS delay server 10us
	serverTransmitTime := serverReceiveTime.Add(10 * time.Microsecond)
	// Network delay client -> server 20ms
	clientReceiveTime := serverTransmitTime.Add(returnDelay)

	actualRoundTripDelay := RoundTripDelay(originTime.Add(clientServerTsDelta), serverReceiveTime, serverTransmitTime, clientReceiveTime.Add(clientServerTsDelta))
	require.Equal(t, roundTripDelay, actualRoundTripDelay)
}

// NTP on-wire protocol
// offset = [(T2 - T1) + (T3 - T4)] / 2
// delay = (T4 - T1) - (T3 - T2).
// T1 the client timestamp on the request packet (clientTransmitTime)
// T2 the server timestamp upon arrival (serverReceiveTime)
// T3 the server timestamp on departure of the reply packet (serverTransmitTime)
// T4 the client timestamp upon arrival (clientReceiveTime)

func TestOffsetSymmetricNetwork(t *testing.T) {
	// Assuming time on client is = time on server
	// Symmetric network delay

	originTime := time.Now()
	// Network delay client -> server 10ms
	serverReceiveTime := originTime.Add(symmetricDelay)
	// OS delay server 10us
	serverTransmitTime := serverReceiveTime.Add(10 * time.Microsecond)
	// Network delay client -> server 20ms
	clientReceiveTime := serverTransmitTime.Add(symmetricDelay)

	actualOffset := Offset(originTime, serverReceiveTime, serverTransmitTime, clientReceiveTime)
	require.Equal(t, int64(0), actualOffset)
}

func TestOffsetAsymmetricNetwork(t *testing.T) {
	// Assuming time on client is = time on server
	// Asymmetric network latency (one way delay in not the same in both directions)

	originTime := time.Now()
	// Network delay client -> server 10ms
	serverReceiveTime := originTime.Add(forwardDelay)
	// OS delay server 10us
	serverTransmitTime := serverReceiveTime.Add(10 * time.Microsecond)
	// Network delay client -> server 20ms
	clientReceiveTime := serverTransmitTime.Add(returnDelay)

	actualOffset := Offset(originTime, serverReceiveTime, serverTransmitTime, clientReceiveTime)
	require.Equal(t, offset, actualOffset)
}

func TestUnixTimeRoundTrip(t *testing.T) {
	for _, tm := range []time.Time{
		time.Date(2026, 11, 27, 0, 0, 0, 123456789, time.UTC),
		time.Date(2036, 2, 7, 6, 28, 15, 999999999, time.UTC),
		// The 32-bit NTP seconds field wraps here (NTP era 1)
		time.Date(2036, 2, 7, 6, 28, 16, 0, time.UTC),
		time.Date(2040, 1, 1, 0, 0, 0, 500000000, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 1, time.UTC),
	} {
		sec, frac := Time(tm)
		require.True(t, tm.Equal(Unix(sec, frac)), "%v round-tripped to %v", tm, Unix(sec, frac).UTC())
	}
}

func TestCorrectTime(t *testing.T) {
	clientReceiveTime := time.Now()
	currentRealTime := CorrectTime(clientReceiveTime, offset)
	require.Equal(t, clientReceiveTime.Add(time.Duration(offset)), currentRealTime)
}

func TestReadNTPPacket(t *testing.T) {
	// listen to incoming udp packets
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.NoError(t, err)
	defer conn.Close()

	// Send a client request
	cconn, err := net.Dial("udp", conn.LocalAddr().String())
	require.NoError(t, err)
	defer cconn.Close()
	_, err = cconn.Write(ntpRequestBytes)
	require.NoError(t, err)

	request, returnaddr, err := ReadNTPPacket(conn)
	require.Equal(t, ntpRequest, request, "We should have the same request arriving on the server")
	require.Equal(t, cconn.LocalAddr().String(), returnaddr.String())
	require.NoError(t, err)
}

func Benchmark_PacketToBytesConversion(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = ntpResponse.Bytes()
	}
}

func Benchmark_BytesToPacketConversion(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = BytesToPacket(ntpResponseBytes)
	}
}

// TestPacketUnmarshalZeroAlloc locks the header-only unmarshal at zero allocs.
func TestPacketUnmarshalZeroAlloc(t *testing.T) {
	p := &Packet{}
	allocs := testing.AllocsPerRun(100, func() {
		_ = p.UnmarshalBinary(ntpResponseBytes)
	})
	require.Zero(t, allocs, "UnmarshalBinary must not allocate for a header-only packet")
}

/*
Benchmark_ServerWithoutKernelTimestamps is a benchmark to determine speed of
reading NTP packets without kernel timestamps
Usually numbers look like:

~/go/src/github.com/facebook/time/ntp/protocol/ntp go test -bench=ServerWithoutKernelTimestamps
goos: linux
goarch: amd64
pkg: github.com/facebook/time/ntp/protocol/ntp
Benchmark_ServerWithoutKernelTimestamps-24    	  204441	      4997 ns/op
PASS
ok  	github.com/facebook/time/ntp/protocol/ntp	1.094s
*/
func Benchmark_ServerWithoutKernelTimestamps(b *testing.B) {
	// Server
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.Nil(b, err)
	defer conn.Close()

	// Client
	addr, err := net.ResolveUDPAddr("udp", conn.LocalAddr().String())
	require.Nil(b, err)
	cconn, err := net.DialUDP("udp", nil, addr)
	require.Nil(b, err)
	defer cconn.Close()

	for i := 0; i < b.N; i++ {
		_, _ = cconn.Write(ntpRequestBytes)
		_, _, _ = ReadNTPPacket(conn)
	}
}

func Benchmark_ServerWithKernelTimestamps(b *testing.B) {
	// Server
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.Nil(b, err)
	defer conn.Close()

	// get connection file descriptor
	connFd, err := timestamp.ConnFd(conn)
	require.NoError(b, err)

	// Allow reading of kernel timestamps via socket
	err = timestamp.EnableSWTimestampsRx(connFd)
	require.NoError(b, err)

	err = unix.SetNonblock(connFd, false)
	require.NoError(b, err)

	// Client
	addr, err := net.ResolveUDPAddr("udp", conn.LocalAddr().String())
	require.Nil(b, err)
	cconn, err := net.DialUDP("udp", nil, addr)
	require.Nil(b, err)
	defer cconn.Close()

	for i := 0; i < b.N; i++ {
		_, _ = cconn.Write(ntpRequestBytes)
		_, _, _ = ReadNTPPacket(conn)
	}
}

/*
Benchmark_ServerWithKernelTimestampsRead is a benchmark to determine speed of
reading NTP packets with kernel timestamps
Usually numbers look like:

~/go/src/github.com/facebook/time/ntp/protocol/ntp go test -bench=ServerWithKernelTimestampsRead
goos: linux
goarch: amd64
pkg: github.com/facebook/time/ntp/protocol/ntp
Benchmark_ServerWithKernelTimestampsRead-24    	  143074	      8084 ns/op
PASS
ok  	github.com/facebook/time/ntp/protocol/ntp	1.778s
*/
func Benchmark_ServerWithKernelTimestampsRead(b *testing.B) {
	request := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 42}
	// Server
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("localhost"), Port: 0})
	require.Nil(b, err)
	defer conn.Close()

	// get connection file descriptor
	connFd, err := timestamp.ConnFd(conn)
	require.NoError(b, err)

	// Allow reading of kernel timestamps via socket
	err = timestamp.EnableSWTimestampsRx(connFd)
	require.NoError(b, err)

	err = unix.SetNonblock(connFd, false)
	require.NoError(b, err)

	// Client
	addr, err := net.ResolveUDPAddr("udp", conn.LocalAddr().String())
	require.Nil(b, err)
	cconn, err := net.DialUDP("udp", nil, addr)
	require.Nil(b, err)
	defer cconn.Close()

	for i := 0; i < b.N; i++ {
		_, _ = cconn.Write(request)
		_, _, _, _ = timestamp.ReadPacketWithRXTimestamp(connFd)
	}
}

func FuzzBytesToPacket(f *testing.F) {
	for _, seed := range [][]byte{{}, {0}, {9}, ntpResponseBytes, ntpRequestBytes} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		packet, err := BytesToPacket(b)
		if err == nil {
			bb, err := packet.Bytes()
			if err != nil {
				return // Bytes() strictly rejects IANA-reserved EF types that parsing accepts (RFC 9748)
			}
			require.Equal(t, b[:len(bb)], bb)
		}
	})
}

// FuzzPacketAssociatedData exercises the exact NTS verification path (header via
// putHeader + first-n EFs via encodeExtensionFields) and asserts it equals the
// same prefix of the input, so the reconstructed authenticated bytes match the
// wire for every prefix.
func FuzzPacketAssociatedData(f *testing.F) {
	f.Add(ntpResponseBytes)
	f.Add(append(append([]byte{}, ntpRequestBytes...), 0x01, 0x04, 0x00, 0x08, 1, 2, 3, 4))
	// One field past the cap below, so normal `go test` covers the bounded branch.
	overCap := append([]byte{}, ntpResponseBytes...)
	for range maxUDPPacketSizeBytes/ExtensionMinSize + 1 {
		overCap = append(overCap, 0x01, 0x04, 0x00, 0x04)
	}
	f.Add(overCap)
	f.Fuzz(func(t *testing.T, b []byte) {
		var p Packet
		if err := p.UnmarshalBinary(b); err != nil {
			return
		}
		// AssociatedData is O(n) in the field count, so every prefix costs O(n^2):
		// 256KB of 4-octet fields needs 15s, past the 10s limit for a single exec.
		maxEFs := maxUDPPacketSizeBytes / ExtensionMinSize
		want := PacketSizeBytes
		for i := 0; i <= min(len(p.ExtensionFields), maxEFs); i++ {
			ad, err := p.AssociatedData(i)
			require.NoError(t, err)
			require.Equal(t, b[:want], ad)
			if i < len(p.ExtensionFields) {
				want += p.ExtensionFields[i].EncodedSize()
			}
		}
	})
}
