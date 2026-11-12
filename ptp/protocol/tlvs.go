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
	"encoding/binary"
	"fmt"
)

// TLV abstracts away any TLV
type TLV interface {
	Type() TLVType
}

const tlvHeadSize = 4

// TLVHead is a common part of all TLVs
type TLVHead struct {
	TLVType     TLVType
	LengthField uint16 // The length of all TLVs shall be an even number of octets
}

// Type implements TLV interface
func (t TLVHead) Type() TLVType {
	return t.TLVType
}

// tlvMinSize maps TLV types to their minimum total marshal size (header + payload)
var tlvMinSize = map[TLVType]int{
	TLVRequestUnicastTransmission:           tlvHeadSize + 6,
	TLVGrantUnicastTransmission:             tlvHeadSize + 8,
	TLVCancelUnicastTransmission:            tlvHeadSize + 2,
	TLVAcknowledgeCancelUnicastTransmission: tlvHeadSize + 2,
	TLVAlternateTimeOffsetIndicator:         tlvHeadSize + 15,
	TLVAlternateResponsePort:                tlvHeadSize + 2,
}

// checkTLVBufferSize checks if the buffer is large enough to marshal a TLV of the given type
func checkTLVBufferSize(tlvType TLVType, b []byte) error {
	minSize, ok := tlvMinSize[tlvType]
	if !ok {
		return fmt.Errorf("unknown TLV type %s (%d)", tlvType, tlvType)
	}
	if len(b) < minSize {
		return fmt.Errorf("not enough buffer to marshal TLV type %s (%d), need %d, have %d", tlvType, tlvType, minSize, len(b))
	}
	return nil
}

func tlvHeadMarshalBinaryTo(t *TLVHead, b []byte) {
	binary.BigEndian.PutUint16(b, uint16(t.TLVType))
	binary.BigEndian.PutUint16(b[2:], t.LengthField)
}

func unmarshalTLVHeader(p *TLVHead, b []byte) error {
	if len(b) < tlvHeadSize {
		return fmt.Errorf("not enough data to decode PTP header")
	}
	p.TLVType = TLVType(binary.BigEndian.Uint16(b[0:]))
	p.LengthField = binary.BigEndian.Uint16(b[2:])
	return nil
}

func checkTLVLength(p *TLVHead, l, want int, strict bool) error {
	if strict && int(p.LengthField) != want {
		return fmt.Errorf("expected TLV of type %s (%d) to have length of %d, got %d in the header", p.TLVType, p.TLVType, want, p.LengthField)
	}

	if int(p.LengthField) < want {
		return fmt.Errorf("expected TLV of type %s (%d) to have length of at least %d, got %d in the header", p.TLVType, p.TLVType, want, p.LengthField)
	}
	if tlvHeadSize+int(p.LengthField) > l {
		return fmt.Errorf("cannot decode TLV of length %d from %d bytes", tlvHeadSize+int(p.LengthField), l)
	}
	return nil
}

func writeTLVs(tlvs []TLV, b []byte) (int, error) {
	pos := 0
	for _, tlv := range tlvs {
		if ttlv, ok := tlv.(BinaryMarshalerTo); ok {
			nn, err := ttlv.MarshalBinaryTo(b[pos:])
			if err != nil {
				return 0, err
			}
			pos += nn
			continue
		}
		// very inefficient path for TLVs that don't support MarshalBinaryTo
		buf := new(bytes.Buffer)
		if err := binary.Write(buf, binary.BigEndian, tlv); err != nil {
			return 0, err
		}
		bbytes := buf.Bytes()
		copy(b[pos:], bbytes)
		pos += len(bbytes)
	}
	return pos, nil
}

// readTLVs reads TLVs from the bytes.
// tlvs is passed to save on allocations and it's user's task to ensure it's empty
func readTLVs(tlvs []TLV, maxLength int, b []byte) ([]TLV, error) {
	pos := 0
	var tlvType TLVType
	for pos+tlvHeadSize <= maxLength {
		tlvType = TLVType(binary.BigEndian.Uint16(b[pos:]))

		switch tlvType {
		case TLVAcknowledgeCancelUnicastTransmission:
			tlv := &AcknowledgeCancelUnicastTransmissionTLV{}
			if err := tlv.UnmarshalBinary(b[pos:]); err != nil {
				return tlvs, err
			}
			tlvs = append(tlvs, tlv)
			pos += tlvHeadSize + int(tlv.LengthField)
		case TLVGrantUnicastTransmission:
			tlv := &GrantUnicastTransmissionTLV{}
			if err := tlv.UnmarshalBinary(b[pos:]); err != nil {
				return tlvs, err
			}
			tlvs = append(tlvs, tlv)
			pos += tlvHeadSize + int(tlv.LengthField)
		case TLVRequestUnicastTransmission:
			tlv := &RequestUnicastTransmissionTLV{}
			if err := tlv.UnmarshalBinary(b[pos:]); err != nil {
				return tlvs, err
			}
			tlvs = append(tlvs, tlv)
			pos += tlvHeadSize + int(tlv.LengthField)
		case TLVCancelUnicastTransmission:
			tlv := &CancelUnicastTransmissionTLV{}
			if err := tlv.UnmarshalBinary(b[pos:]); err != nil {
				return tlvs, err
			}
			tlvs = append(tlvs, tlv)
			pos += tlvHeadSize + int(tlv.LengthField)
		case TLVPathTrace:
			tlv := &PathTraceTLV{}
			if err := tlv.UnmarshalBinary(b[pos:]); err != nil {
				return tlvs, err
			}
			tlvs = append(tlvs, tlv)
			pos += tlvHeadSize + int(tlv.LengthField)
		case TLVAlternateTimeOffsetIndicator:
			tlv := &AlternateTimeOffsetIndicatorTLV{}
			if err := tlv.UnmarshalBinary(b[pos:]); err != nil {
				return tlvs, err
			}
			tlvs = append(tlvs, tlv)
			pos += tlvHeadSize + int(tlv.LengthField)
		case TLVAlternateResponsePort:
			tlv := &AlternateResponsePortTLV{}
			if err := tlv.UnmarshalBinary(b[pos:]); err != nil {
				return tlvs, err
			}
			tlvs = append(tlvs, tlv)
			pos += tlvHeadSize + int(tlv.LengthField)
		default:
			return tlvs, fmt.Errorf("reading TLV %s (%d) is not yet implemented", tlvType, tlvType)
		}
	}
	return tlvs, nil
}

// Unicast TLVs

// RequestUnicastTransmissionTLV Table 110 REQUEST_UNICAST_TRANSMISSION TLV format
type RequestUnicastTransmissionTLV struct {
	TLVHead
	MsgTypeAndReserved    UnicastMsgTypeAndFlags // first 4 bits only, same enums as with normal message type
	LogInterMessagePeriod LogInterval
	DurationField         uint32
}

// MarshalBinaryTo marshals bytes to RequestUnicastTransmissionTLV
func (t *RequestUnicastTransmissionTLV) MarshalBinaryTo(b []byte) (int, error) {
	if err := checkTLVBufferSize(TLVRequestUnicastTransmission, b); err != nil {
		return 0, err
	}
	tlvHeadMarshalBinaryTo(&t.TLVHead, b)
	b[tlvHeadSize] = byte(t.MsgTypeAndReserved)
	b[tlvHeadSize+1] = byte(t.LogInterMessagePeriod)
	binary.BigEndian.PutUint32(b[tlvHeadSize+2:], t.DurationField)
	return tlvHeadSize + 6, nil
}

// UnmarshalBinary parses []byte and populates struct fields
func (t *RequestUnicastTransmissionTLV) UnmarshalBinary(b []byte) error {
	if err := unmarshalTLVHeader(&t.TLVHead, b); err != nil {
		return err
	}
	if err := checkTLVLength(&t.TLVHead, len(b), 6, true); err != nil {
		return err
	}
	t.MsgTypeAndReserved = UnicastMsgTypeAndFlags(b[4])
	t.LogInterMessagePeriod = LogInterval(b[5])
	t.DurationField = binary.BigEndian.Uint32(b[6:])
	return nil
}

// GrantUnicastTransmissionTLV Table 111 GRANT_UNICAST_TRANSMISSION TLV format
type GrantUnicastTransmissionTLV struct {
	TLVHead
	MsgTypeAndReserved    UnicastMsgTypeAndFlags // first 4 bits only, same enums as with normal message type
	LogInterMessagePeriod LogInterval
	DurationField         uint32
	Reserved              uint8
	Renewal               uint8
}

// MarshalBinaryTo marshals bytes to GrantUnicastTransmissionTLV
func (t *GrantUnicastTransmissionTLV) MarshalBinaryTo(b []byte) (int, error) {
	if err := checkTLVBufferSize(TLVGrantUnicastTransmission, b); err != nil {
		return 0, err
	}
	tlvHeadMarshalBinaryTo(&t.TLVHead, b)
	b[tlvHeadSize] = byte(t.MsgTypeAndReserved)
	b[tlvHeadSize+1] = byte(t.LogInterMessagePeriod)
	binary.BigEndian.PutUint32(b[tlvHeadSize+2:], t.DurationField)
	b[tlvHeadSize+6] = t.Reserved
	b[tlvHeadSize+7] = t.Renewal
	return tlvHeadSize + 8, nil
}

// UnmarshalBinary parses []byte and populates struct fields
func (t *GrantUnicastTransmissionTLV) UnmarshalBinary(b []byte) error {
	if err := unmarshalTLVHeader(&t.TLVHead, b); err != nil {
		return err
	}
	if err := checkTLVLength(&t.TLVHead, len(b), 8, true); err != nil {
		return err
	}
	t.MsgTypeAndReserved = UnicastMsgTypeAndFlags(b[4])
	t.LogInterMessagePeriod = LogInterval(b[5])
	t.DurationField = binary.BigEndian.Uint32(b[6:])
	t.Reserved = b[10]
	t.Renewal = b[11]
	return nil
}

// CancelUnicastTransmissionTLV Table 112 CANCEL_UNICAST_TRANSMISSION TLV format
type CancelUnicastTransmissionTLV struct {
	TLVHead
	MsgTypeAndFlags UnicastMsgTypeAndFlags // first 4 bits is msg type, then flags R and/or G
	Reserved        uint8
}

// MarshalBinaryTo marshals bytes to CancelUnicastTransmissionTLV
func (t *CancelUnicastTransmissionTLV) MarshalBinaryTo(b []byte) (int, error) {
	if err := checkTLVBufferSize(TLVCancelUnicastTransmission, b); err != nil {
		return 0, err
	}
	tlvHeadMarshalBinaryTo(&t.TLVHead, b)
	b[tlvHeadSize] = byte(t.MsgTypeAndFlags)
	b[tlvHeadSize+1] = t.Reserved
	return tlvHeadSize + 2, nil
}

// UnmarshalBinary parses []byte and populates struct fields
func (t *CancelUnicastTransmissionTLV) UnmarshalBinary(b []byte) error {
	if err := unmarshalTLVHeader(&t.TLVHead, b); err != nil {
		return err
	}
	if err := checkTLVLength(&t.TLVHead, len(b), 2, true); err != nil {
		return err
	}
	t.MsgTypeAndFlags = UnicastMsgTypeAndFlags(b[4])
	t.Reserved = b[5]
	return nil
}

// AcknowledgeCancelUnicastTransmissionTLV Table 113 ACKNOWLEDGE_CANCEL_UNICAST_TRANSMISSION TLV format
type AcknowledgeCancelUnicastTransmissionTLV struct {
	TLVHead
	MsgTypeAndFlags UnicastMsgTypeAndFlags // first 4 bits is msg type, then flags R and/or G
	Reserved        uint8
}

// MarshalBinaryTo marshals bytes to AcknowledgeCancelUnicastTransmissionTLV
func (t *AcknowledgeCancelUnicastTransmissionTLV) MarshalBinaryTo(b []byte) (int, error) {
	if err := checkTLVBufferSize(TLVAcknowledgeCancelUnicastTransmission, b); err != nil {
		return 0, err
	}
	tlvHeadMarshalBinaryTo(&t.TLVHead, b)
	b[tlvHeadSize] = byte(t.MsgTypeAndFlags)
	b[tlvHeadSize+1] = t.Reserved
	return tlvHeadSize + 2, nil
}

// UnmarshalBinary parses []byte and populates struct fields
func (t *AcknowledgeCancelUnicastTransmissionTLV) UnmarshalBinary(b []byte) error {
	if err := unmarshalTLVHeader(&t.TLVHead, b); err != nil {
		return err
	}
	if err := checkTLVLength(&t.TLVHead, len(b), 2, true); err != nil {
		return err
	}
	t.MsgTypeAndFlags = UnicastMsgTypeAndFlags(b[4])
	t.Reserved = b[5]
	return nil
}

// other TLVs

// PathTraceTLV Table 115 PATH_TRACE TLV format
type PathTraceTLV struct {
	TLVHead
	// The value of the lengthField is 8N.
	PathSequence []ClockIdentity // N
}

// MarshalBinaryTo marshals bytes to PathTraceTLV
func (t *PathTraceTLV) MarshalBinaryTo(b []byte) (int, error) {
	size := tlvHeadSize + len(t.PathSequence)*8
	if len(b) < size {
		return 0, fmt.Errorf("not enough buffer to marshal TLV type %s (%d), need %d, have %d", TLVPathTrace, TLVPathTrace, size, len(b))
	}
	tlvHeadMarshalBinaryTo(&t.TLVHead, b)
	pos := tlvHeadSize
	for _, ps := range t.PathSequence {
		binary.BigEndian.PutUint64(b[pos:pos+8], uint64(ps))
		pos += 8
	}
	return pos, nil
}

// UnmarshalBinary parses []byte and populates struct fields
func (t *PathTraceTLV) UnmarshalBinary(b []byte) error {
	if err := unmarshalTLVHeader(&t.TLVHead, b); err != nil {
		return err
	}
	if err := checkTLVLength(&t.TLVHead, len(b), 8, false); err != nil {
		return err
	}
	t.PathSequence = []ClockIdentity{}
	for i := range int(t.LengthField) / 8 {
		pos := tlvHeadSize + i*8
		identity := ClockIdentity(binary.BigEndian.Uint64(b[pos:]))
		t.PathSequence = append(t.PathSequence, identity)
	}
	return nil
}

// AlternateTimeOffsetIndicatorTLV is a Table 116 ALTERNATE_TIME_OFFSET_INDICATOR TLV format
type AlternateTimeOffsetIndicatorTLV struct {
	TLVHead
	KeyField       uint8
	CurrentOffset  int32
	JumpSeconds    int32
	TimeOfNextJump PTPSeconds // uint48
	DisplayName    PTPText
}

// MarshalBinaryTo marshals bytes to AlternateTimeOffsetIndicatorTLV
func (t *AlternateTimeOffsetIndicatorTLV) MarshalBinaryTo(b []byte) (int, error) {
	if err := checkTLVBufferSize(TLVAlternateTimeOffsetIndicator, b); err != nil {
		return 0, err
	}
	tlvHeadMarshalBinaryTo(&t.TLVHead, b)
	b[tlvHeadSize] = t.KeyField
	binary.BigEndian.PutUint32(b[tlvHeadSize+1:], uint32(t.CurrentOffset))
	binary.BigEndian.PutUint32(b[tlvHeadSize+5:], uint32(t.JumpSeconds))
	copy(b[tlvHeadSize+9:], t.TimeOfNextJump[:]) //uint48
	size := tlvHeadSize + 15
	// DisplayName is always present on the wire, an empty one is a single zero length octet
	dd, err := t.DisplayName.MarshalBinary()
	if err != nil {
		return 0, fmt.Errorf("writing AlternateTimeOffsetIndicatorTLV DisplayName: %w", err)
	}
	if len(b) < size+len(dd) {
		return 0, fmt.Errorf("not enough buffer to marshal AlternateTimeOffsetIndicatorTLV with DisplayName, need %d, have %d", size+len(dd), len(b))
	}
	copy(b[tlvHeadSize+15:], dd)
	size += len(dd)
	return size, nil
}

// UnmarshalBinary parses []byte and populates struct fields
func (t *AlternateTimeOffsetIndicatorTLV) UnmarshalBinary(b []byte) error {
	if err := unmarshalTLVHeader(&t.TLVHead, b); err != nil {
		return err
	}
	// 15 octets of fixed fields followed by DisplayName, which is at least its length octet
	if err := checkTLVLength(&t.TLVHead, len(b), 16, false); err != nil {
		return err
	}
	t.KeyField = b[tlvHeadSize]
	t.CurrentOffset = int32(binary.BigEndian.Uint32(b[tlvHeadSize+1:]))
	t.JumpSeconds = int32(binary.BigEndian.Uint32(b[tlvHeadSize+5:]))
	copy(t.TimeOfNextJump[:], b[tlvHeadSize+9:]) // uint48
	if err := t.DisplayName.UnmarshalBinary(b[tlvHeadSize+15 : tlvHeadSize+int(t.LengthField)]); err != nil {
		return fmt.Errorf("reading AlternateTimeOffsetIndicatorTLV DisplayName: %w", err)
	}
	return nil
}

// AlternateResponsePortTLV is a CSPTP optional TLV to switch response source port of the server
// Offset flag indicates the number of the port steps, not the port number itself.
// Ex:
// 0 means no switch (use default port). For example 1234
// 1 means next port. For example 4567
// 2 means next next port. For example 6789
// etc
type AlternateResponsePortTLV struct {
	TLVHead
	Offset uint16
}

// MarshalBinaryTo marshals bytes to AlternateResponsePortTLV
func (a *AlternateResponsePortTLV) MarshalBinaryTo(b []byte) (int, error) {
	if err := checkTLVBufferSize(TLVAlternateResponsePort, b); err != nil {
		return 0, err
	}
	tlvHeadMarshalBinaryTo(&a.TLVHead, b)
	binary.BigEndian.PutUint16(b[tlvHeadSize:], a.Offset)
	return tlvHeadSize + 2, nil
}

// UnmarshalBinary parses []byte and populates struct fields
func (a *AlternateResponsePortTLV) UnmarshalBinary(b []byte) error {
	if err := unmarshalTLVHeader(&a.TLVHead, b); err != nil {
		return err
	}
	if err := checkTLVLength(&a.TLVHead, len(b), 2, true); err != nil {
		return err
	}
	a.Offset = binary.BigEndian.Uint16(b[tlvHeadSize:])
	return nil
}
