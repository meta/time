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
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTLVHeadType(t *testing.T) {
	head := &TLVHead{
		TLVType:     TLVRequestUnicastTransmission,
		LengthField: 10,
	}
	require.Equal(t, TLVRequestUnicastTransmission, head.Type())
}

func pathTraceBytes(t *testing.T, pathSequence []ClockIdentity) []byte {
	t.Helper()
	tlv := &PathTraceTLV{
		TLVHead:      TLVHead{TLVType: TLVPathTrace, LengthField: uint16(len(pathSequence) * 8)},
		PathSequence: pathSequence,
	}
	raw := make([]byte, tlvHeadSize+len(pathSequence)*8)
	n, err := tlv.MarshalBinaryTo(raw)
	require.NoError(t, err)
	require.Equal(t, len(raw), n)
	return raw
}

func TestParsePathTraceTLV(t *testing.T) {
	pathSequence := []ClockIdentity{630763432548989518, 123479299994292777, 342422224531222222}

	t.Run("LastInBuffer", func(t *testing.T) {
		parsed := &PathTraceTLV{}
		require.NoError(t, parsed.UnmarshalBinary(pathTraceBytes(t, pathSequence)))
		require.Equal(t, pathSequence, parsed.PathSequence)
	})

	t.Run("FollowedByTLV", func(t *testing.T) {
		buf := slices.Concat(pathTraceBytes(t, pathSequence), pathTraceBytes(t, []ClockIdentity{42}))
		parsed := &PathTraceTLV{}
		require.NoError(t, parsed.UnmarshalBinary(buf))
		require.Equal(t, pathSequence, parsed.PathSequence)
	})

	t.Run("LengthNotMultipleOf8", func(t *testing.T) {
		malformed := []byte{0x00, 0x08, 0x00, 0x0c, 1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2}
		buf := slices.Concat(malformed, pathTraceBytes(t, []ClockIdentity{42}))
		tlvs, err := readTLVs(nil, len(buf), buf)
		require.NoError(t, err)
		require.Len(t, tlvs, 2)
		require.Equal(t, []ClockIdentity{0x0101010101010101}, tlvs[0].(*PathTraceTLV).PathSequence)
		require.Equal(t, []ClockIdentity{42}, tlvs[1].(*PathTraceTLV).PathSequence)
	})
}

func TestParseAnnounceWithPathTraceDatagram(t *testing.T) {
	pathSequence := []ClockIdentity{630763432548989518, 123479299994292777, 342422224531222222}
	packet := &Announce{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageAnnounce, 0),
			Version:         Version,
			MessageLength:   uint16(headerSize + 30 + tlvHeadSize + len(pathSequence)*8),
			FlagField:       FlagUnicast | FlagPTPTimescale,
		},
		TLVs: []TLV{&PathTraceTLV{
			TLVHead:      TLVHead{TLVType: TLVPathTrace, LengthField: uint16(len(pathSequence) * 8)},
			PathSequence: pathSequence,
		}},
	}
	b, err := packet.MarshalBinary()
	require.NoError(t, err)
	require.Len(t, b, int(packet.MessageLength))

	got := &Announce{}
	require.NoError(t, FromBytes(b, got))
	require.Equal(t, packet, got)
}

func TestParseAnnounceWithPathTrace(t *testing.T) {
	raw := []uint8("\x0b\x12\x00\x4c\x00\x00\x04\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x01\x00\x00\x05\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x25\x00\x80\xf8\xfe\xff\xff\x80\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x00\xa0\x00\x08\x00\x18\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x01\xb6\xaf\xc4\xe5\x46\x12\x29\x04\xc0\x87\x32\xf0\x61\xee\xce\x00\x00")
	packet := new(Announce)
	err := FromBytes(raw, packet)
	require.Nil(t, err)
	want := Announce{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageAnnounce, 0),
			Version:         Version,
			MessageLength:   76,
			DomainNumber:    0,
			FlagField:       FlagUnicast | FlagPTPTimescale,
			SequenceID:      0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 630763432548989518,
			},
			LogMessageInterval: 1,
			ControlField:       5,
		},
		AnnounceBody: AnnounceBody{
			CurrentUTCOffset:     37,
			Reserved:             0,
			GrandmasterPriority1: 128,
			GrandmasterClockQuality: ClockQuality{
				ClockClass:              248,
				ClockAccuracy:           254,
				OffsetScaledLogVariance: 65535,
			},
			GrandmasterPriority2: 128,
			GrandmasterIdentity:  630763432548989518,
			StepsRemoved:         0,
			TimeSource:           TimeSourceInternalOscillator,
		},
		TLVs: []TLV{
			&PathTraceTLV{
				TLVHead: TLVHead{
					TLVType:     TLVPathTrace,
					LengthField: 24,
				},
				PathSequence: []ClockIdentity{
					630763432548989518,
					123479299994292777,
					342422224531222222,
				},
			},
		},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	require.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(b)
	require.Nil(t, err)
	require.Equal(t, &want, pp)
}

func TestParseAnnounceWithAlternateTimeOffsetIndicator(t *testing.T) {
	raw := []uint8("\x0b\x12\x00\x5a\x00\x00\x04\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x01\x00\x00\x05\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x25\x00\x80\xf8\xfe\xff\xff\x80\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x00\xa0\x00\x09\x00\x16\x01\x00\x00\x00\x25\x00\x00\x00\x01\x00\x00\x62\xc2\xfd\xb6\x03\x50\x54\x50\x00\x00\x00")
	packet := new(Announce)
	err := FromBytes(raw, packet)
	require.Nil(t, err)
	want := Announce{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageAnnounce, 0),
			Version:         Version,
			MessageLength:   90,
			DomainNumber:    0,
			FlagField:       FlagUnicast | FlagPTPTimescale,
			SequenceID:      0,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 630763432548989518,
			},
			LogMessageInterval: 1,
			ControlField:       5,
		},
		AnnounceBody: AnnounceBody{
			CurrentUTCOffset:     37,
			Reserved:             0,
			GrandmasterPriority1: 128,
			GrandmasterClockQuality: ClockQuality{
				ClockClass:              248,
				ClockAccuracy:           254,
				OffsetScaledLogVariance: 65535,
			},
			GrandmasterPriority2: 128,
			GrandmasterIdentity:  630763432548989518,
			StepsRemoved:         0,
			TimeSource:           TimeSourceInternalOscillator,
		},
		TLVs: []TLV{
			&AlternateTimeOffsetIndicatorTLV{
				TLVHead: TLVHead{
					TLVType:     TLVAlternateTimeOffsetIndicator,
					LengthField: 22,
				},
				KeyField:       0x01,
				CurrentOffset:  37,
				JumpSeconds:    1,
				TimeOfNextJump: NewPTPSeconds(time.Unix(1656946102, 0)),
				DisplayName:    PTPText("PTP"),
			},
		},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	require.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(raw)
	require.Nil(t, err)
	require.Equal(t, &want, pp)
}

func TestParseAnnounceWithAlternateTimeOffsetIndicatorEmptyDisplayName(t *testing.T) {
	// same Announce as above, but the ALTERNATE_TIME_OFFSET_INDICATOR TLV carries an empty displayName,
	// so its lengthField is 16: 15 octets of fixed fields plus the zero displayName length octet
	raw := []uint8("\x0b\x12\x00\x54\x00\x00\x04\x08\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x01\x00\x00\x05\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x25\x00\x80\xf8\xfe\xff\xff\x80\x08\xc0\xeb\xff\xfe\x63\x7a\x4e\x00\x00\xa0\x00\x09\x00\x10\x01\x00\x00\x00\x25\x00\x00\x00\x01\x00\x00\x62\xc2\xfd\xb6\x00\x00\x00")
	packet := new(Announce)
	err := FromBytes(raw, packet)
	require.NoError(t, err)
	want := []TLV{
		&AlternateTimeOffsetIndicatorTLV{
			TLVHead: TLVHead{
				TLVType:     TLVAlternateTimeOffsetIndicator,
				LengthField: 16,
			},
			KeyField:       0x01,
			CurrentOffset:  37,
			JumpSeconds:    1,
			TimeOfNextJump: NewPTPSeconds(time.Unix(1656946102, 0)),
		},
	}
	require.Equal(t, want, packet.TLVs)
	b, err := Bytes(packet)
	require.NoError(t, err)
	require.Equal(t, raw, b)
}

func TestParseAlternateTimeOffsetIndicatorDisplayNameOverrun(t *testing.T) {
	// lengthField is 20, but the displayName length octet claims 8 octets,
	// which would run into the ALTERNATE_RESPONSE_PORT TLV that follows
	raw := []uint8("\x00\x09\x00\x14\x01\x00\x00\x00\x25\x00\x00\x00\x01\x00\x00\x62\xc2\xfd\xb6\x08PTP\x00\x80\x01\x00\x02\x00\x05")
	tlv := &AlternateTimeOffsetIndicatorTLV{}
	err := tlv.UnmarshalBinary(raw)
	require.ErrorContains(t, err, "DisplayName")
}

func TestAlternateTimeOffsetIndicatorOneCharDisplayName(t *testing.T) {
	// 15 octets of fixed fields, the displayName length octet, one character and one pad octet
	raw := []uint8("\x00\x09\x00\x12\x01\x00\x00\x00\x25\x00\x00\x00\x01\x00\x00\x62\xc2\xfd\xb6\x01X\x00")
	tlv := &AlternateTimeOffsetIndicatorTLV{}
	err := tlv.UnmarshalBinary(raw)
	require.NoError(t, err)
	require.Equal(t, PTPText("X"), tlv.DisplayName)
	b := make([]byte, len(raw))
	n, err := tlv.MarshalBinaryTo(b)
	require.NoError(t, err)
	require.Equal(t, raw, b[:n])
}

func TestParseSyncDelayReqWithAlternateResponsePort(t *testing.T) {
	raw := []byte{1, 18, 0, 50, 0, 0, 36, 0, 0, 0, 0, 0, 6, 32, 0, 2, 0, 0, 0, 0, 184, 206, 246, 255, 254, 68, 148, 144, 0, 1, 149, 17, 0, 127, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 32, 7, 0, 2, 16, 146, 0, 0}
	packet := new(SyncDelayReq)
	err := FromBytes(raw, packet)
	require.Nil(t, err)

	want := SyncDelayReq{
		Header: Header{
			SdoIDAndMsgType: NewSdoIDAndMsgType(MessageDelayReq, 0),
			Version:         Version,
			SequenceID:      38161,
			MessageLength:   50,
			FlagField:       FlagUnicast | FlagProfileSpecific1,
			SourcePortIdentity: PortIdentity{
				PortNumber:    1,
				ClockIdentity: 13316852727524136080,
			},
			LogMessageInterval: 0x7f,
			CorrectionField:    102760450,
		},
		TLVs: []TLV{&AlternateResponsePortTLV{
			TLVHead: TLVHead{TLVType: TLVAlternateResponsePort, LengthField: uint16(2)},
			Offset:  uint16(4242),
		}},
	}
	require.Equal(t, want, *packet)
	b, err := Bytes(packet)
	require.Nil(t, err)
	require.Equal(t, raw, b)

	// test generic DecodePacket as well
	pp, err := DecodePacket(raw)
	require.Nil(t, err)
	require.Equal(t, &want, pp)
}

func TestCheckTLVBufferSize(t *testing.T) {
	tests := []struct {
		name    string
		tlvType TLVType
		bufLen  int
		wantErr bool
	}{
		{"RequestUnicast enough", TLVRequestUnicastTransmission, tlvHeadSize + 6, false},
		{"RequestUnicast short", TLVRequestUnicastTransmission, tlvHeadSize + 5, true},
		{"GrantUnicast enough", TLVGrantUnicastTransmission, tlvHeadSize + 8, false},
		{"GrantUnicast short", TLVGrantUnicastTransmission, tlvHeadSize, true},
		{"CancelUnicast enough", TLVCancelUnicastTransmission, tlvHeadSize + 2, false},
		{"CancelUnicast short", TLVCancelUnicastTransmission, tlvHeadSize + 1, true},
		{"AcknowledgeCancel enough", TLVAcknowledgeCancelUnicastTransmission, tlvHeadSize + 2, false},
		{"AcknowledgeCancel empty", TLVAcknowledgeCancelUnicastTransmission, 0, true},
		{"AlternateTimeOffset enough", TLVAlternateTimeOffsetIndicator, tlvHeadSize + 15, false},
		{"AlternateTimeOffset short", TLVAlternateTimeOffsetIndicator, tlvHeadSize + 14, true},
		{"AlternateResponsePort enough", TLVAlternateResponsePort, tlvHeadSize + 2, false},
		{"AlternateResponsePort short", TLVAlternateResponsePort, 3, true},
		{"unknown TLV type", TLVType(0x9999), 1024, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkTLVBufferSize(tt.tlvType, make([]byte, tt.bufLen))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestMarshalBinaryToShortBuffer(t *testing.T) {
	tests := []struct {
		name string
		tlv  BinaryMarshalerTo
	}{
		{"RequestUnicast", &RequestUnicastTransmissionTLV{}},
		{"GrantUnicast", &GrantUnicastTransmissionTLV{}},
		{"CancelUnicast", &CancelUnicastTransmissionTLV{}},
		{"AcknowledgeCancel", &AcknowledgeCancelUnicastTransmissionTLV{}},
		{"PathTrace", &PathTraceTLV{PathSequence: []ClockIdentity{1}}},
		{"AlternateTimeOffset", &AlternateTimeOffsetIndicatorTLV{}},
		{"AlternateResponsePort", &AlternateResponsePortTLV{}},
	}
	for _, tt := range tests {
		t.Run(tt.name+" empty", func(t *testing.T) {
			_, err := tt.tlv.MarshalBinaryTo([]byte{})
			require.Error(t, err)
		})
		t.Run(tt.name+" header only", func(t *testing.T) {
			_, err := tt.tlv.MarshalBinaryTo(make([]byte, tlvHeadSize))
			require.Error(t, err)
		})
	}
}
