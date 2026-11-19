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
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	// Announce carrying an ALTERNATE_TIME_OFFSET_INDICATOR TLV with displayName "PTP"
	announceWithATOI = []uint8("\x0b\x12\x00\x5a\x00\x00\x04\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x01\x00\x00\x05\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x25\x00\x80\xf8\xfe\xff\xff\x80\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x00\xa0\x00\x09\x00\x16\x01\x00\x00\x00\x25\x00\x00\x00\x01\x00\x00\x62\xc2\xfd\xb6\x03\x50\x54\x50\x00\x00\x00")
	// the same Announce with an empty displayName, so the TLV lengthField is 16
	announceWithATOIEmptyName = []uint8("\x0b\x12\x00\x54\x00\x00\x04\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x01\x00\x00\x05\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x25\x00\x80\xf8\xfe\xff\xff\x80\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x00\xa0\x00\x09\x00\x10\x01\x00\x00\x00\x25\x00\x00\x00\x01\x00\x00\x62\xc2\xfd\xb6\x00\x00\x00")
)

func TestBytesTo(t *testing.T) {
	packet := &SyncDelayReq{
		Header: Header{
			SdoIDAndMsgType:     NewSdoIDAndMsgType(MessageSync, 1),
			Version:             MajorVersion,
			MessageLength:       44,
			DomainNumber:        0,
			MinorSdoID:          0,
			FlagField:           0,
			CorrectionField:     0,
			MessageTypeSpecific: 0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			SequenceID:         116,
			ControlField:       0,
			LogMessageInterval: 0,
		},
		SyncDelayReqBody: SyncDelayReqBody{
			OriginTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5a},
				Nanoseconds: 174389936,
			},
		},
	}

	b, err := Bytes(packet)
	require.NoError(t, err)
	t.Run("buffer too small", func(t *testing.T) {
		buf := make([]byte, 10)
		_, err := BytesTo(packet, buf)
		require.Error(t, err)
	})
	t.Run("just enough buffer", func(t *testing.T) {
		buf := make([]byte, len(b))
		l, err := BytesTo(packet, buf)
		require.NoError(t, err)
		require.Equal(t, len(b), l)
		require.Equal(t, b, buf)
	})
	t.Run("very big buffer", func(t *testing.T) {
		buf := make([]byte, len(b)+1000)
		l, err := BytesTo(packet, buf)
		require.NoError(t, err)
		require.Equal(t, len(b), l)
		require.Equal(t, b, buf[:l])
	})
}

func TestSetDomainNumber(t *testing.T) {
	packet := &SyncDelayReq{
		Header: Header{
			SdoIDAndMsgType:     NewSdoIDAndMsgType(MessageSync, 1),
			Version:             MajorVersion,
			MessageLength:       44,
			DomainNumber:        0,
			MinorSdoID:          0,
			FlagField:           0,
			CorrectionField:     0,
			MessageTypeSpecific: 0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			SequenceID:         116,
			ControlField:       0,
			LogMessageInterval: 0,
		},
		SyncDelayReqBody: SyncDelayReqBody{
			OriginTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5a},
				Nanoseconds: 174389936,
			},
		},
	}

	require.Equal(t, uint8(0), packet.DomainNumber)

	var testDomain uint8 = 42
	packet.SetDomainNumber(testDomain)
	require.Equal(t, testDomain, packet.DomainNumber)

	b, err := Bytes(packet)
	require.NoError(t, err)
	require.Equal(t, testDomain, b[4])
}

func TestParseSync(t *testing.T) {
	raw := []uint8{
		0x10, 0x02, 0x00, 0x2c, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x63, 0xff,
		0xff, 0x00, 0x09, 0xba, 0x00, 0x01, 0x00, 0x74,
		0x00, 0x00, 0x00, 0x00, 0x45, 0xb1, 0x11, 0x5a,
		0x0a, 0x64, 0xfa, 0xb0, 0x00, 0x00,
	}
	packet := new(SyncDelayReq)
	err := FromBytes(raw, packet)
	require.Nil(t, err)
	want := SyncDelayReq{
		Header: Header{
			SdoIDAndMsgType:     NewSdoIDAndMsgType(MessageSync, 1),
			Version:             MajorVersion,
			MessageLength:       44,
			DomainNumber:        0,
			MinorSdoID:          0,
			FlagField:           0,
			CorrectionField:     0,
			MessageTypeSpecific: 0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			SequenceID:         116,
			ControlField:       0,
			LogMessageInterval: 0,
		},
		SyncDelayReqBody: SyncDelayReqBody{
			OriginTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5a},
				Nanoseconds: 174389936,
			},
		},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	assert.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(raw)
	require.Nil(t, err)
	assert.Equal(t, &want, pp)
}

func TestParseFollowup(t *testing.T) {
	raw := []uint8{
		0x8, 0x2, 0x0, 0x2c, 0x0, 0x0, 0x4, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x80, 0x63, 0xff, 0xff, 0x0,
		0x9, 0xba, 0x0, 0x1, 0x0, 0x0, 0x2, 0x0, 0x0,
		0x0, 0x45, 0xb1, 0x11, 0x5e, 0x4, 0x5d, 0xd2, 0x6e, 0x0, 0x0,
	}
	packet := new(FollowUp)
	err := FromBytes(raw, packet)
	require.Nil(t, err)
	want := FollowUp{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageFollowUp, 0),
			Version:         MajorVersion,
			MessageLength:   uint16(binary.Size(FollowUp{})),
			DomainNumber:    0,
			FlagField:       FlagUnicast,
			SequenceID:      0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			LogMessageInterval: 0,
			ControlField:       2,
		},
		FollowUpBody: FollowUpBody{
			PreciseOriginTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5e},
				Nanoseconds: 73257582,
			},
		},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	assert.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(raw)
	require.Nil(t, err)
	assert.Equal(t, &want, pp)
}

func TestParsePDelayReq(t *testing.T) {
	raw := []uint8{
		0x12, 0x02, 0x00, 0x36, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x80, 0x63, 0xff,
		0xff, 0x00, 0x09, 0xba, 0x00, 0x01, 0x9e, 0x57,
		0x05, 0x0f, 0x00, 0x00, 0x45, 0xb1, 0x11, 0x5e,
		0x04, 0x5d, 0xd2, 0x6e, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	packet := new(PDelayReq)
	err := FromBytes(raw, packet)
	require.Nil(t, err)
	want := PDelayReq{
		Header: Header{
			SdoIDAndMsgType:     NewSdoIDAndMsgType(MessagePDelayReq, 1),
			Version:             MajorVersion,
			MessageLength:       54,
			DomainNumber:        0,
			MinorSdoID:          0,
			FlagField:           0,
			CorrectionField:     0,
			MessageTypeSpecific: 0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			SequenceID:         40535,
			ControlField:       5,
			LogMessageInterval: 15,
		},
		PDelayReqBody: PDelayReqBody{
			OriginTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5e},
				Nanoseconds: 73257582,
			},
		},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	assert.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(raw)
	require.Nil(t, err)
	assert.Equal(t, &want, pp)
}

func TestParseAnnounce(t *testing.T) {
	raw := []uint8{
		0xb, 0x2, 0x0, 0x40, 0x0, 0x0, 0x4, 0x8, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x80, 0x63, 0xff, 0xff, 0x0,
		0x9, 0xba, 0x0, 0x1, 0x0, 0x0, 0x5, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x80, 0x6, 0x21, 0x59, 0xe0,
		0x80, 0x0, 0x80, 0x63, 0xff, 0xff, 0x0,
		0x9, 0xba, 0x0, 0x0, 0x20, 0x0, 0x0,
	}
	packet := new(Announce)
	err := FromBytes(raw, packet)
	require.Nil(t, err)
	want := Announce{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageAnnounce, 0),
			Version:         MajorVersion,
			MessageLength:   64,
			DomainNumber:    0,
			FlagField:       FlagUnicast | FlagPTPTimescale,
			SequenceID:      0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			LogMessageInterval: 0,
			ControlField:       5,
		},
		AnnounceBody: AnnounceBody{
			CurrentUTCOffset:     0,
			Reserved:             0,
			GrandmasterPriority1: 128,
			GrandmasterClockQuality: ClockQuality{
				ClockClass:              6,
				ClockAccuracy:           33, // 0x21 - Time Accurate within 100ns
				OffsetScaledLogVariance: 23008,
			},
			GrandmasterPriority2: 128,
			GrandmasterIdentity:  36138748164966842,
			StepsRemoved:         0,
			TimeSource:           TimeSourceGNSS,
		},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	assert.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(raw)
	require.Nil(t, err)
	assert.Equal(t, &want, pp)
}

func TestParseDelayResp(t *testing.T) {
	raw := []uint8{
		0x9, 0x2, 0x0, 0x36, 0x0, 0x0, 0x4, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x80, 0x63, 0xff, 0xff, 0x0,
		0x9, 0xba, 0x0, 0x1, 0x0, 0xa, 0x3, 0x7f,
		0x0, 0x0, 0x45, 0xb1, 0x11, 0x5e, 0x4, 0x5d,
		0xd2, 0x6e, 0xb8, 0x59, 0x9f, 0xff, 0xfe,
		0x55, 0xaf, 0x4e, 0x0, 0x1, 0x0, 0x0,
	}
	packet := new(DelayResp)
	err := FromBytes(raw, packet)
	require.Nil(t, err)
	want := DelayResp{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageDelayResp, 0),
			Version:         MajorVersion,
			MessageLength:   uint16(binary.Size(DelayResp{})),
			DomainNumber:    0,
			FlagField:       FlagUnicast,
			SequenceID:      10,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			LogMessageInterval: 0x7f,
			ControlField:       3,
			CorrectionField:    0,
		},
		DelayRespBody: DelayRespBody{
			ReceiveTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5e},
				Nanoseconds: 73257582,
			},
			RequestingPortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 13283824497738493774,
			},
		},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	assert.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(raw)
	require.Nil(t, err)
	assert.Equal(t, &want, pp)
}

func TestFoundFuzzResults(t *testing.T) {
	allBytes := [][]byte{
		[]byte("00\x0000000000000000000000000000000000000000000\x00\x04\x00\x06000000"),
		[]byte("00\x00A0000000000000000000000000000000000000000\x00\x04\x00\x06000000\x00\x04\x00\x06000000000"),
	}
	for _, b := range allBytes {
		packet, err := DecodePacket(b)
		require.NoError(t, err)
		bb, err := Bytes(packet)
		require.NoError(t, err)
		// ignore last 2 bytes as they are only for ipv6 checksums
		l := len(bb)
		require.Equal(t, b[:l-2], bb[:l-2], "we expect binary form of packet %v %+v to be equal to original", packet.MessageType(), packet)
	}
}

func BenchmarkReadSyncDelay(b *testing.B) {
	raw := []byte{1, 18, 0, 50, 0, 0, 36, 0, 0, 0, 0, 0, 6, 32, 0, 2, 0, 0, 0, 0, 184, 206, 246, 255, 254, 68, 148, 144, 0, 1, 149, 17, 0, 127, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 32, 7, 0, 2, 16, 146, 0, 0}
	p := &SyncDelayReq{}
	for n := 0; n < b.N; n++ {
		_ = p.UnmarshalBinary(raw)
	}
}

func BenchmarkWriteSync(b *testing.B) {
	p := &SyncDelayReq{
		Header: Header{
			SdoIDAndMsgType:     NewSdoIDAndMsgType(MessageSync, 1),
			Version:             MajorVersion,
			MessageLength:       44,
			DomainNumber:        0,
			MinorSdoID:          0,
			FlagField:           0,
			CorrectionField:     0,
			MessageTypeSpecific: 0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			SequenceID:         116,
			ControlField:       0,
			LogMessageInterval: 0,
		},
		SyncDelayReqBody: SyncDelayReqBody{
			OriginTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5a},
				Nanoseconds: 174389936,
			},
		},
	}
	buf := make([]byte, 64)
	for n := 0; n < b.N; n++ {
		_, _ = BytesTo(p, buf)
	}
}

func BenchmarkReadAnnounce(b *testing.B) {
	raw := []uint8{
		0xb, 0x2, 0x0, 0x40, 0x0, 0x0, 0x4, 0x8, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x80, 0x63, 0xff, 0xff, 0x0,
		0x9, 0xba, 0x0, 0x1, 0x0, 0x0, 0x5, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x80, 0x6, 0x21, 0x59, 0xe0,
		0x80, 0x0, 0x80, 0x63, 0xff, 0xff, 0x0,
		0x9, 0xba, 0x0, 0x0, 0x20, 0x0, 0x0,
	}
	p := &Announce{}
	for n := 0; n < b.N; n++ {
		_ = p.UnmarshalBinary(raw)
	}
}

func BenchmarkReadAnnouncePathTrace(b *testing.B) {
	raw := []uint8("\x0b\x12\x00\x4c\x00\x00\x04\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x01\x00\x00\x05\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x25\x00\x80\xf8\xfe\xff\xff\x80\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x00\xa0\x00\x08\x00\x08\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x00")
	p := &Announce{}
	for n := 0; n < b.N; n++ {
		_ = p.UnmarshalBinary(raw)
	}
}

func BenchmarkWriteAnnounce(b *testing.B) {
	p := &Announce{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageAnnounce, 0),
			Version:         MajorVersion,
			MessageLength:   64,
			DomainNumber:    0,
			FlagField:       FlagUnicast | FlagPTPTimescale,
			SequenceID:      0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			LogMessageInterval: 0,
			ControlField:       5,
		},
		AnnounceBody: AnnounceBody{
			CurrentUTCOffset:     0,
			Reserved:             0,
			GrandmasterPriority1: 128,
			GrandmasterClockQuality: ClockQuality{
				ClockClass:              6,
				ClockAccuracy:           33, // 0x21 - Time Accurate within 100ns
				OffsetScaledLogVariance: 23008,
			},
			GrandmasterPriority2: 128,
			GrandmasterIdentity:  36138748164966842,
			StepsRemoved:         0,
			TimeSource:           TimeSourceGNSS,
		},
	}
	buf := make([]byte, 66)
	for n := 0; n < b.N; n++ {
		_, _ = BytesTo(p, buf)
	}
}

func BenchmarkWriteFollowup(b *testing.B) {
	p := &FollowUp{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageFollowUp, 0),
			Version:         MajorVersion,
			MessageLength:   uint16(binary.Size(FollowUp{})),
			DomainNumber:    0,
			FlagField:       FlagUnicast,
			SequenceID:      0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 36138748164966842,
			},
			LogMessageInterval: 0,
			ControlField:       2,
		},
		FollowUpBody: FollowUpBody{
			PreciseOriginTimestamp: Timestamp{
				Seconds:     [6]byte{0x0, 0x00, 0x45, 0xb1, 0x11, 0x5e},
				Nanoseconds: 73257582,
			},
		},
	}
	buf := make([]byte, 64)
	for n := 0; n < b.N; n++ {
		_, _ = BytesTo(p, buf)
	}
}

func TestReqPDelayMarshalUnmarshal(t *testing.T) {
	clockID := ClockIdentity(0x123456789abcdef0)
	portID := uint16(1)
	seq := uint16(42)

	req := ReqPDelay(clockID, portID, seq)
	require.NotNil(t, req)
	require.Equal(t, uint16(54), req.MessageLength)

	b, err := Bytes(req)
	require.NoError(t, err)
	// Bytes() appends 2 zero bytes for IPv6 checksums
	require.Len(t, b, 56)

	decoded := new(PDelayReq)
	err = FromBytes(b, decoded)
	require.NoError(t, err)
	require.Equal(t, MessagePDelayReq, decoded.MessageType())
	require.Equal(t, seq, decoded.SequenceID)
	require.Equal(t, clockID, decoded.SourcePortIdentity.ClockIdentity)
	require.Equal(t, portID, decoded.SourcePortIdentity.PortNumber)
	require.Equal(t, LogInterval(0x7f), decoded.LogMessageInterval)

	pp, err := DecodePacket(b)
	require.NoError(t, err)
	require.Equal(t, req, pp)
}

func TestRespPDelayMarshalUnmarshal(t *testing.T) {
	reqClockID := ClockIdentity(0xfedcba9876543210)
	reqPortID := uint16(2)
	seq := uint16(42)
	req := ReqPDelay(reqClockID, reqPortID, seq)
	req.CorrectionField = NewCorrection(968)

	clockID := ClockIdentity(0x123456789abcdef0)
	portID := uint16(1)
	reqReceiptTS := NewTimestamp(time.Unix(1000, 500000))

	resp := RespPDelay(clockID, portID, reqReceiptTS, req)
	require.NotNil(t, resp)
	require.Equal(t, uint16(54), resp.MessageLength)

	b, err := Bytes(resp)
	require.NoError(t, err)
	// Bytes() appends 2 zero bytes for IPv6 checksums
	require.Len(t, b, 56)

	decoded := new(PDelayResp)
	err = FromBytes(b, decoded)
	require.NoError(t, err)
	require.Equal(t, MessagePDelayResp, decoded.MessageType())
	require.Equal(t, seq, decoded.SequenceID)
	require.Equal(t, clockID, decoded.SourcePortIdentity.ClockIdentity)
	require.Equal(t, portID, decoded.SourcePortIdentity.PortNumber)
	require.Equal(t, req.SourcePortIdentity, decoded.RequestingPortIdentity)
	require.Equal(t, FlagTwoStep, decoded.FlagField)
	require.Equal(t, reqReceiptTS, decoded.RequestReceiptTimestamp)
	// The request path goes in the follow-up; this field is left to the response path.
	require.Zero(t, decoded.CorrectionField)

	pp, err := DecodePacket(b)
	require.NoError(t, err)
	require.Equal(t, resp, pp)
}

func TestRespFollowUpPDelayMarshalUnmarshal(t *testing.T) {
	reqClockID := ClockIdentity(0xfedcba9876543210)
	reqPortID := uint16(2)
	seq := uint16(42)
	req := ReqPDelay(reqClockID, reqPortID, seq)
	cf := NewCorrection(968)
	req.CorrectionField = cf

	clockID := ClockIdentity(0x123456789abcdef0)
	portID := uint16(1)
	respOriginTS := NewTimestamp(time.Unix(1000, 1000000))

	followUp := RespFollowUpPDelay(clockID, portID, respOriginTS, req)
	require.NotNil(t, followUp)
	require.Equal(t, uint16(54), followUp.MessageLength)

	b, err := Bytes(followUp)
	require.NoError(t, err)
	// Bytes() appends 2 zero bytes for IPv6 checksums
	require.Len(t, b, 56)

	decoded := new(PDelayRespFollowUp)
	err = FromBytes(b, decoded)
	require.NoError(t, err)
	require.Equal(t, MessagePDelayRespFollowUp, decoded.MessageType())
	require.Equal(t, seq, decoded.SequenceID)
	require.Equal(t, clockID, decoded.SourcePortIdentity.ClockIdentity)
	require.Equal(t, portID, decoded.SourcePortIdentity.PortNumber)
	require.Equal(t, req.SourcePortIdentity, decoded.RequestingPortIdentity)
	require.Equal(t, respOriginTS, decoded.ResponseOriginTimestamp)
	require.Equal(t, cf, decoded.CorrectionField)

	pp, err := DecodePacket(b)
	require.NoError(t, err)
	require.Equal(t, followUp, pp)
}

func FuzzDecodePacket(f *testing.F) {
	delayResp := []uint8{
		0x9, 0x2, 0x0, 0x36, 0x0, 0x0, 0x4, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0,
		0x0, 0x0, 0x0, 0x80, 0x63, 0xff, 0xff, 0x0,
		0x9, 0xba, 0x0, 0x1, 0x0, 0xa, 0x3, 0x7f,
		0x0, 0x0, 0x45, 0xb1, 0x11, 0x5e, 0x4, 0x5d,
		0xd2, 0x6e, 0xb8, 0x59, 0x9f, 0xff, 0xfe,
		0x55, 0xaf, 0x4e, 0x0, 0x1, 0x0, 0x0,
	}
	managementDefaultDataSet := []uint8{
		0x0d, 0x12, 0x00, 0x4a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x48, 0x57, 0xdd, 0xff, 0xfe, 0x0e, 0x91, 0xda, 0x00, 0x00,
		0x00, 0x00, 0x04, 0x7f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xb7, 0x5f, 0x00, 0x00, 0x02, 0x00, 0x00, 0x01,
		0x00, 0x16, 0x20, 0x00, 0x03, 0x00, 0x00, 0x01, 0x80, 0xff,
		0xfe, 0xff, 0xff, 0x80, 0x48, 0x57, 0xdd, 0xff, 0xfe, 0x0e,
		0x91, 0xda, 0x00, 0x00, 0x00, 0x00,
	}
	signalingGrantUnicast := []uint8{0x0c, 0x02, 0x00, 0x38, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xe4, 0x1d, 0x2d, 0xff, 0xfe, 0xbb, 0x64, 0x60, 0x00,
		0x01, 0x1d, 0xc4, 0x05, 0x7f, 0x48, 0x57, 0xdd, 0xff,
		0xfe, 0x08, 0x64, 0x88, 0x00, 0x01, 0x00, 0x05, 0x00,
		0x08, 0xb0, 0x01, 0x00, 0x00, 0x00, 0x3c, 0x00, 0x01,
		0x00, 0x00,
	}
	managementErrorStatus := []uint8{0x0d, 0x02, 0x00, 0x3c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x48, 0x57, 0xdd, 0xff, 0xfe, 0x08, 0x64, 0x88, 0x00, 0x00,
		0x00, 0x01, 0x04, 0x7f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xdc, 0x6c, 0x00, 0x00, 0x02, 0x00, 0x00, 0x02,
		0x00, 0x08, 0x00, 0x06, 0x20, 0x01, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00,
	}
	for _, seed := range [][]byte{
		{}, {0}, {9}, delayResp, managementDefaultDataSet, signalingGrantUnicast, managementErrorStatus,
		announceWithATOI, announceWithATOIEmptyName,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		packet, err := DecodePacket(b)
		// check that marshalling works
		if err == nil {
			// special case for TLVs that have variable length
			// which may have padding and thus marshalling produces different results
			switch packet.MessageType() {
			case MessageManagement:
				// ignore ManagementMsgErrorStatus
				_, ok := packet.(*ManagementMsgErrorStatus)
				if ok {
					// skip extra checks
					return
				}
				m := packet.(*Management)
				// ignore Management with PortPropertiesNPT TLV
				_, ok = m.TLV.(*PortPropertiesNPTLV)
				if ok {
					return
				}
				// ignore Management with UnicastMasterTableNP TLV
				_, ok = m.TLV.(*UnicastMasterTableNPTLV)
				if ok {
					return
				}
			case MessageSignaling:
				m := packet.(*Signaling)
				// ignore Signaling with PathTrace or AlternateTimeOffsetIndicator TLVs
				for _, tlv := range m.TLVs {
					if tlv.Type() == TLVPathTrace {
						return
					}
					if tlv.Type() == TLVAlternateTimeOffsetIndicator {
						return
					}
				}
			// Announce msg can have TracePath TLV which also has PTPText
			case MessageAnnounce:
				m := packet.(*Announce)
				// ignore Announce with PathTrace or AlternateTimeOffsetIndicator TLVs
				for _, tlv := range m.TLVs {
					if tlv.Type() == TLVPathTrace {
						return
					}
					if tlv.Type() == TLVAlternateTimeOffsetIndicator {
						return
					}
				}
			case MessageSync, MessageDelayReq:
				m := packet.(*SyncDelayReq)
				// ignore Sync/DelayReq with GrantUnicastTransmissionTLV, TLVPathTrace or TLVAlternateTimeOffsetIndicator TLVs
				for _, tlv := range m.TLVs {
					if tlv.Type() == TLVGrantUnicastTransmission {
						return
					}
					if tlv.Type() == TLVAlternateTimeOffsetIndicator {
						return
					}
					if tlv.Type() == TLVPathTrace {
						return
					}
				}
			}
			bb, err := Bytes(packet)
			require.NoError(t, err)
			// ignore last 2 bytes as they are only for ipv6 checksums
			l := len(bb)
			require.Equal(t, b[:l-2], bb[:l-2], "we expect binary form of packet %v %+v to be equal to original", packet.MessageType(), packet)
		} else {
			require.Nil(t, packet)
		}
	})
}
