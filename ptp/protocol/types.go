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
	"math"
	"net"
	"time"
)

// 2 ** 16
const twoPow16 = 65536

// MessageType is type for Message Types
type MessageType uint8

// As per Table 36 Values of messageType field
const (
	MessageSync               MessageType = 0x0
	MessageDelayReq           MessageType = 0x1
	MessagePDelayReq          MessageType = 0x2
	MessagePDelayResp         MessageType = 0x3
	MessageFollowUp           MessageType = 0x8
	MessageDelayResp          MessageType = 0x9
	MessagePDelayRespFollowUp MessageType = 0xA
	MessageAnnounce           MessageType = 0xB
	MessageSignaling          MessageType = 0xC
	MessageManagement         MessageType = 0xD
)

// Table 36 splits message types into two classes by the 0x8 bit
const (
	EventMsgClass   MessageType = 0x0
	GeneralMsgClass MessageType = 0x8
)

// IsGeneral reports whether this is a general message. Only event ones are hardware stamped.
func (m MessageType) IsGeneral() bool {
	return m&GeneralMsgClass != 0
}

// IsPDelay reports whether this is a peer delay message, which is sent to the
// multicast group and so is not hardware stamped
func (m MessageType) IsPDelay() bool {
	switch m {
	case MessagePDelayReq, MessagePDelayResp, MessagePDelayRespFollowUp:
		return true
	}
	return false
}

// IsEvent reports whether this is an event message, the only class a NIC stamps
func (m MessageType) IsEvent() bool {
	return m&GeneralMsgClass == EventMsgClass
}

// MessageTypeToString is a map from MessageType to string
var MessageTypeToString = map[MessageType]string{
	MessageSync:               "SYNC",
	MessageDelayReq:           "DELAY_REQ",
	MessagePDelayReq:          "PDELAY_REQ",
	MessagePDelayResp:         "PDELAY_RES",
	MessageFollowUp:           "FOLLOW_UP",
	MessageDelayResp:          "DELAY_RESP",
	MessagePDelayRespFollowUp: "PDELAY_RESP_FOLLOW_UP",
	MessageAnnounce:           "ANNOUNCE",
	MessageSignaling:          "SIGNALING",
	MessageManagement:         "MANAGEMENT",
}

func (m MessageType) String() string {
	return MessageTypeToString[m]
}

// SdoIDAndMsgType is a uint8 where first 4 bites contain SdoID and last 4 bits MessageType
type SdoIDAndMsgType uint8

// MsgType extracts MessageType from SdoIDAndMsgType
func (m SdoIDAndMsgType) MsgType() MessageType {
	return MessageType(m & 0xf) // last 4 bits
}

// NewSdoIDAndMsgType builds new SdoIDAndMsgType from MessageType and flags
func NewSdoIDAndMsgType(msgType MessageType, sdoID uint8) SdoIDAndMsgType {
	return SdoIDAndMsgType(sdoID<<4 | uint8(msgType))
}

// ProbeMsgType reads first 8 bits of data and tries to decode it to SdoIDAndMsgType, then return MessageType
func ProbeMsgType(data []byte) (msg MessageType, err error) {
	if len(data) < 1 {
		return 0, fmt.Errorf("not enough data to probe MsgType")
	}
	return SdoIDAndMsgType(data[0]).MsgType(), nil
}

// TLVType is type for TLV types
type TLVType uint16

// As per Table 52 tlvType values
const (
	TLVManagement                           TLVType = 0x0001
	TLVManagementErrorStatus                TLVType = 0x0002
	TLVOrganizationExtension                TLVType = 0x0003
	TLVRequestUnicastTransmission           TLVType = 0x0004
	TLVGrantUnicastTransmission             TLVType = 0x0005
	TLVCancelUnicastTransmission            TLVType = 0x0006
	TLVAcknowledgeCancelUnicastTransmission TLVType = 0x0007
	TLVPathTrace                            TLVType = 0x0008
	TLVAlternateTimeOffsetIndicator         TLVType = 0x0009
	TLVAlternateResponsePort                TLVType = 0x2007
	// Remaining 52 tlvType TLVs not implemented
)

// TLVTypeToString is a map from TLVType to string
var TLVTypeToString = map[TLVType]string{
	TLVManagement:                           "MANAGEMENT",
	TLVManagementErrorStatus:                "MANAGEMENT_ERROR_STATUS",
	TLVOrganizationExtension:                "ORGANIZATION_EXTENSION",
	TLVRequestUnicastTransmission:           "REQUEST_UNICAST_TRANSMISSION",
	TLVGrantUnicastTransmission:             "GRANT_UNICAST_TRANSMISSION",
	TLVCancelUnicastTransmission:            "CANCEL_UNICAST_TRANSMISSION",
	TLVAcknowledgeCancelUnicastTransmission: "ACKNOWLEDGE_CANCEL_UNICAST_TRANSMISSION",
	TLVPathTrace:                            "PATH_TRACE",
	TLVAlternateTimeOffsetIndicator:         "ALTERNATE_TIME_OFFSET_INDICATOR",
	TLVAlternateResponsePort:                "ALTERNATE_RESPONSE_PORT",
}

func (t TLVType) String() string {
	return TLVTypeToString[t]
}

// IntFloat is a float64 stored in int64
type IntFloat int64

// Value decodes IntFloat to float64
func (t IntFloat) Value() float64 {
	return float64(t) / twoPow16
}

/*
TimeInterval is the time interval expressed in nanoseconds, multiplied by 2**16.
Positive or negative time intervals outside the maximum range of this data type shall be encoded as the largest
positive and negative values of the data type, respectively.
For example, 2.5 ns is expressed as 0000 0000 0002 8000 base 16
*/
type TimeInterval IntFloat

// Nanoseconds decodes TimeInterval to human-understandable nanoseconds
func (t TimeInterval) Nanoseconds() float64 {
	return IntFloat(t).Value()
}

func (t TimeInterval) String() string {
	return fmt.Sprintf("TimeInterval(%.3fns)", t.Nanoseconds())
}

// NewTimeInterval returns TimeInterval built from Nanoseconds
func NewTimeInterval(ns float64) TimeInterval {
	return TimeInterval(ns * twoPow16)
}

/*
Correction is the value of the correction measured in nanoseconds and multiplied by 2**16.
For example, 2.5 ns is represented as 0000 0000 0002 8000 base 16
A value of one in all bits, except the most significant, of the field shall indicate that the correction is too big to be represented.
*/
type Correction IntFloat

// Nanoseconds decodes Correction to human-understandable nanoseconds
func (t Correction) Nanoseconds() float64 {
	if t.TooBig() {
		return math.Inf(1)
	}
	return IntFloat(t).Value()
}

// Duration converts PTP CorrectionField to time.Duration, ignoring
// case where correction is too big, and dropping fractions of nanoseconds
func (t Correction) Duration() time.Duration {
	if !t.TooBig() {
		return time.Duration(t.Nanoseconds())
	}
	return 0
}

func (t Correction) String() string {
	if t.TooBig() {
		return "Correction(Too big)"
	}
	return fmt.Sprintf("Correction(%.3fns)", t.Nanoseconds())
}

// TooBig means correction is too big to be represented.
func (t Correction) TooBig() bool {
	return t == 0x7fffffffffffffff // one in all bits, except the most significant
}

// NewCorrection returns Correction built from Nanoseconds
func NewCorrection(ns float64) Correction {
	t := ns * twoPow16
	if t > 0x7fffffffffffffff {
		return Correction(0x7fffffffffffffff)
	}
	return Correction(ns * twoPow16)
}

// The ClockIdentity type identifies unique entities within a PTP Network, e.g. a PTP Instance or an entity of a common service.
type ClockIdentity uint64

// String formats ClockIdentity same way ptp4l pmc client does
func (c ClockIdentity) String() string {
	ptr := make([]byte, 8)
	binary.BigEndian.PutUint64(ptr, uint64(c))
	return fmt.Sprintf("%02x%02x%02x.%02x%02x.%02x%02x%02x",
		ptr[0], ptr[1], ptr[2], ptr[3],
		ptr[4], ptr[5], ptr[6], ptr[7],
	)
}

// MAC turns ClockIdentity into the MAC address it was based upon. EUI-48 is assumed.
func (c ClockIdentity) MAC() net.HardwareAddr {
	mac := make(net.HardwareAddr, 6)
	mac[0] = byte(c >> 56)
	mac[1] = byte(c >> 48)
	mac[2] = byte(c >> 40)
	mac[3] = byte(c >> 16)
	mac[4] = byte(c >> 8)
	mac[5] = byte(c)
	return mac
}

// NewClockIdentity creates new ClockIdentity from MAC address
func NewClockIdentity(mac net.HardwareAddr) (ClockIdentity, error) {
	b := [8]byte{}
	macLen := len(mac)
	switch macLen {
	case 6: // EUI-48
		b[0] = mac[0]
		b[1] = mac[1]
		b[2] = mac[2]
		b[3] = 0xFF
		b[4] = 0xFE
		b[5] = mac[3]
		b[6] = mac[4]
		b[7] = mac[5]
	case 8: // EUI-64
		copy(b[:], mac)
	default:
		return 0, fmt.Errorf("unsupported MAC %v, must be either EUI48 or EUI64", mac)
	}
	return ClockIdentity(binary.BigEndian.Uint64(b[:])), nil
}

// The PortIdentity type identifies a PTP Port or a Link Port
type PortIdentity struct {
	ClockIdentity ClockIdentity
	PortNumber    uint16
}

// String formats PortIdentity same way ptp4l pmc client does
func (p PortIdentity) String() string {
	return fmt.Sprintf("%s-%d", p.ClockIdentity, p.PortNumber)
}

// Compare returns an integer comparing two port identities. The result will be 0 if p == q, -1 if p < q, and +1 if p > q.
// The definition of "less than" is the same as the Less method.
func (p PortIdentity) Compare(q PortIdentity) int {
	cl1, cl2 := p.ClockIdentity, q.ClockIdentity
	switch {
	case cl1 < cl2:
		return -1
	case cl1 > cl2:
		return 1
	}
	// cl1 == cl2
	pn1, pn2 := p.PortNumber, q.PortNumber
	switch {
	case pn1 < pn2:
		return -1
	case pn1 > pn2:
		return 1
	}
	// pn1 == pn2
	return 0
}

// Less reports whether p sorts before q. Port identities sort first by clock identity, then their port numbers.
func (p PortIdentity) Less(q PortIdentity) bool { return p.Compare(q) == -1 }

// PTPSeconds type representing seconds
type PTPSeconds [6]uint8 // uint48

// Empty returns 0 seconds
func (s PTPSeconds) Empty() bool {
	return s == [6]uint8{0, 0, 0, 0, 0, 0}
}

// Seconds returns number of seconds as uint64
func (s PTPSeconds) Seconds() uint64 {
	return uint64(s[5]) | uint64(s[4])<<8 | uint64(s[3])<<16 | uint64(s[2])<<24 |
		uint64(s[1])<<32 | uint64(s[0])<<40
}

// Time returns number of seconds in as Time
func (s PTPSeconds) Time() time.Time {
	if s.Empty() {
		return time.Time{}
	}
	return time.Unix(int64(s.Seconds()), 0)
}

// String returns number of seconds in as String
func (s PTPSeconds) String() string {
	if s.Empty() {
		return "PTPSeconds(empty)"
	}
	return fmt.Sprintf("PTPSeconds(%s)", s.Time())
}

// NewPTPSeconds creates a new instance of PTPSeconds
func NewPTPSeconds(t time.Time) PTPSeconds {
	if t.IsZero() {
		return PTPSeconds{}
	}
	v := uint64(t.Unix())
	s := PTPSeconds{}
	s[0] = byte(v >> 40)
	s[1] = byte(v >> 32)
	s[2] = byte(v >> 24)
	s[3] = byte(v >> 16)
	s[4] = byte(v >> 8)
	s[5] = byte(v)
	return s
}

/*
Timestamp type represents a positive time with respect to the epoch.
The secondsField member is the integer portion of the timestamp in units of seconds.
The nanosecondsField member is the fractional portion of the timestamp in units of nanoseconds.
The nanosecondsField member is always less than 10**9 .
For example:
+2.000000001 seconds is represented by secondsField = 0000 0000 0002 base 16 and nanosecondsField= 0000 0001 base 16.
*/
type Timestamp struct {
	Seconds     PTPSeconds
	Nanoseconds uint32
}

// Time turns Timestamp into normal Go time.Time
func (t Timestamp) Time() time.Time {
	if t.Empty() {
		return time.Time{}
	}
	return time.Unix(int64(t.Seconds.Seconds()), int64(t.Nanoseconds))
}

// Empty timestamp
func (t Timestamp) Empty() bool {
	return t.Nanoseconds == 0 && t.Seconds.Empty()
}

// String representation of the timestamp
func (t Timestamp) String() string {
	if t.Empty() {
		return "Timestamp(empty)"
	}
	return fmt.Sprintf("Timestamp(%s)", t.Time())
}

// NewTimestamp allows to create Timestamp from time.Time
func NewTimestamp(t time.Time) Timestamp {
	if t.IsZero() {
		return Timestamp{}
	}
	ts := Timestamp{
		Nanoseconds: uint32(t.Nanosecond()),
	}
	v := uint64(t.Unix())
	ts.Seconds[0] = byte(v >> 40)
	ts.Seconds[1] = byte(v >> 32)
	ts.Seconds[2] = byte(v >> 24)
	ts.Seconds[3] = byte(v >> 16)
	ts.Seconds[4] = byte(v >> 8)
	ts.Seconds[5] = byte(v)
	return ts
}

// ClockClass represents a PTP clock class
type ClockClass uint8

// Available Clock Classes
// https://datatracker.ietf.org/doc/html/rfc8173#section-7.6.2.4
const (
	ClockClass6         ClockClass = 6
	ClockClass7         ClockClass = 7
	ClockClass13        ClockClass = 13
	ClockClass14        ClockClass = 14
	ClockClass52        ClockClass = 52
	ClockClass58        ClockClass = 58
	ClockClassSlaveOnly ClockClass = 255
)

// ClockAccuracy represents a PTP clock accuracy
type ClockAccuracy uint8

// Available Clock Accuracy
// https://datatracker.ietf.org/doc/html/rfc8173#section-7.6.2.5
const (
	ClockAccuracyNanosecond25       ClockAccuracy = 0x20
	ClockAccuracyNanosecond100      ClockAccuracy = 0x21
	ClockAccuracyNanosecond250      ClockAccuracy = 0x22
	ClockAccuracyMicrosecond1       ClockAccuracy = 0x23
	ClockAccuracyMicrosecond2point5 ClockAccuracy = 0x24
	ClockAccuracyMicrosecond10      ClockAccuracy = 0x25
	ClockAccuracyMicrosecond25      ClockAccuracy = 0x26
	ClockAccuracyMicrosecond100     ClockAccuracy = 0x27
	ClockAccuracyMicrosecond250     ClockAccuracy = 0x28
	ClockAccuracyMillisecond1       ClockAccuracy = 0x29
	ClockAccuracyMillisecond2point5 ClockAccuracy = 0x2A
	ClockAccuracyMillisecond10      ClockAccuracy = 0x2B
	ClockAccuracyMillisecond25      ClockAccuracy = 0x2C
	ClockAccuracyMillisecond100     ClockAccuracy = 0x2D
	ClockAccuracyMillisecond250     ClockAccuracy = 0x2E
	ClockAccuracySecond1            ClockAccuracy = 0x2F
	ClockAccuracySecond10           ClockAccuracy = 0x30
	ClockAccuracySecondGreater10    ClockAccuracy = 0x31
	ClockAccuracyUnknown            ClockAccuracy = 0xFE
)

// ClockAccuracyFromOffset returns PTP Clock Accuracy covering the time.Duration
func ClockAccuracyFromOffset(offset time.Duration) ClockAccuracy {
	if offset < 0 {
		offset *= -1
	}

	// https://datatracker.ietf.org/doc/html/rfc8173#section-7.6.2.4
	if offset <= 25*time.Nanosecond {
		return ClockAccuracyNanosecond25
	} else if offset <= 100*time.Nanosecond {
		return ClockAccuracyNanosecond100
	} else if offset <= 250*time.Nanosecond {
		return ClockAccuracyNanosecond250
	} else if offset <= time.Microsecond {
		return ClockAccuracyMicrosecond1
	} else if offset <= 2500*time.Nanosecond {
		return ClockAccuracyMicrosecond2point5
	} else if offset <= 10*time.Microsecond {
		return ClockAccuracyMicrosecond10
	} else if offset <= 25*time.Microsecond {
		return ClockAccuracyMicrosecond25
	} else if offset <= 100*time.Microsecond {
		return ClockAccuracyMicrosecond100
	} else if offset <= 250*time.Microsecond {
		return ClockAccuracyMicrosecond250
	} else if offset <= time.Millisecond {
		return ClockAccuracyMillisecond1
	} else if offset <= 2500*time.Microsecond {
		return ClockAccuracyMillisecond2point5
	} else if offset <= 10*time.Millisecond {
		return ClockAccuracyMillisecond10
	} else if offset <= 25*time.Millisecond {
		return ClockAccuracyMillisecond25
	} else if offset <= 100*time.Millisecond {
		return ClockAccuracyMillisecond100
	} else if offset <= 250*time.Millisecond {
		return ClockAccuracyMillisecond250
	} else if offset <= time.Second {
		return ClockAccuracySecond1
	} else if offset <= 10*time.Second {
		return ClockAccuracySecond10
	}

	return ClockAccuracySecondGreater10
}

// Duration returns matching time.Duration of PTP Clock Accuracy
func (c ClockAccuracy) Duration() time.Duration {
	switch c {
	case ClockAccuracyNanosecond25:
		return 25 * time.Nanosecond
	case ClockAccuracyNanosecond100:
		return 100 * time.Nanosecond
	case ClockAccuracyNanosecond250:
		return 250 * time.Nanosecond
	case ClockAccuracyMicrosecond1:
		return 1000 * time.Nanosecond
	case ClockAccuracyMicrosecond2point5:
		return 2500 * time.Nanosecond
	case ClockAccuracyMicrosecond10:
		return 10 * time.Microsecond
	case ClockAccuracyMicrosecond25:
		return 25 * time.Microsecond
	case ClockAccuracyMicrosecond100:
		return 100 * time.Microsecond
	case ClockAccuracyMicrosecond250:
		return 250 * time.Microsecond
	case ClockAccuracyMillisecond1:
		return 1 * time.Millisecond
	case ClockAccuracyMillisecond2point5:
		return 2500 * time.Microsecond
	case ClockAccuracyMillisecond10:
		return 10 * time.Millisecond
	case ClockAccuracyMillisecond25:
		return 25 * time.Millisecond
	case ClockAccuracyMillisecond100:
		return 100 * time.Millisecond
	case ClockAccuracyMillisecond250:
		return 250 * time.Millisecond
	case ClockAccuracySecond1:
		return 1 * time.Second
	case ClockAccuracySecond10:
		return 10 * time.Second
	}
	return 25 * time.Second
}

// ClockQuality represents the quality of a clock.
type ClockQuality struct {
	ClockClass              ClockClass    `json:"clock_class"`
	ClockAccuracy           ClockAccuracy `json:"clock_accuracy"`
	OffsetScaledLogVariance uint16        `json:"offset_scaled_log_variance"`
}

// TimeSource indicates the immediate source of time used by the Grandmaster PTP Instance
type TimeSource uint8

// TimeSource values, Table 6 timeSource enumeration
const (
	TimeSourceAtomicClock        TimeSource = 0x10
	TimeSourceGNSS               TimeSource = 0x20
	TimeSourceTerrestrialRadio   TimeSource = 0x30
	TimeSourceSerialTimeCode     TimeSource = 0x39
	TimeSourcePTP                TimeSource = 0x40
	TimeSourceNTP                TimeSource = 0x50
	TimeSourceHandSet            TimeSource = 0x60
	TimeSourceOther              TimeSource = 0x90
	TimeSourceInternalOscillator TimeSource = 0xa0
)

// TimeSourceToString is a map from TimeSource to string
var TimeSourceToString = map[TimeSource]string{
	TimeSourceAtomicClock:        "ATOMIC_CLOCK",
	TimeSourceGNSS:               "GNSS",
	TimeSourceTerrestrialRadio:   "TERRESTRIAL_RADIO",
	TimeSourceSerialTimeCode:     "SERIAL_TIME_CODE",
	TimeSourcePTP:                "PTP",
	TimeSourceNTP:                "NTP",
	TimeSourceHandSet:            "HAND_SET",
	TimeSourceOther:              "OTHER",
	TimeSourceInternalOscillator: "INTERNAL_OSCILLATOR",
}

func (t TimeSource) String() string {
	return TimeSourceToString[t]
}

// LogInterval shall be the logarithm, to base 2, of the requested period in seconds.
// In layman's terms, it's specified as a power of two in seconds.
type LogInterval int8

// Duration returns LogInterval as time.Duration
func (i LogInterval) Duration() time.Duration {
	secs := math.Pow(2, float64(i))
	return time.Duration(secs * float64(time.Second))
}

// NewLogInterval returns new LogInterval from time.Duration.
// The values of these logarithmic attributes shall be selected from integers in the range -128 to 127 subject to
// further limits established in the applicable PTP Profile.
func NewLogInterval(d time.Duration) (LogInterval, error) {
	li := int(math.Log2(d.Seconds()))
	if li > 127 {
		return 0, fmt.Errorf("logInterval %d is too big", li)
	}
	if li < -128 {
		return 0, fmt.Errorf("logInterval %d is too small", li)
	}
	return LogInterval(li), nil
}

/*
PTPText data type is used to represent textual material in PTP messages.
TextField is encoded as UTF-8.
The most significant byte of the leading text symbol shall be the element of the array with index 0.
UTF-8 encoding has variable length, thus LengthField can be larger than number of characters.

	type PTPText struct {
		LengthField uint8
		TextField   []byte
	}
*/
type PTPText string

// UnmarshalBinary populates ptptext from bytes
func (p *PTPText) UnmarshalBinary(rawBytes []byte) error {
	var length uint8
	reader := bytes.NewReader(rawBytes)
	if err := binary.Read(reader, binary.BigEndian, &length); err != nil {
		return fmt.Errorf("reading PTPText LengthField: %w", err)
	}
	if length == 0 {
		// can be zero len, just empty string
		return nil
	}

	if len(rawBytes) < int(length)+1 {
		return fmt.Errorf("text field is too short, need %d got %d", int(length)+1, len(rawBytes))
	}
	text := make([]byte, length)
	if err := binary.Read(reader, binary.BigEndian, text); err != nil {
		return fmt.Errorf("reading PTPText TextField of len=%d: %w", length, err)
	}
	*p = PTPText(text)
	return nil
}

// MarshalBinary converts ptptext to []bytes
func (p *PTPText) MarshalBinary() ([]byte, error) {
	rawText := []byte(*p)
	if len(rawText) > 255 {
		return nil, fmt.Errorf("text is too long")
	}
	length := uint8(len(rawText))
	var bytes bytes.Buffer
	if err := binary.Write(&bytes, binary.BigEndian, length); err != nil {
		return nil, err
	}
	if err := binary.Write(&bytes, binary.BigEndian, rawText); err != nil {
		return nil, err
	}
	// padding to make sure packet length is even
	if length%2 != 0 {
		if err := bytes.WriteByte(0); err != nil {
			return nil, err
		}
	}
	return bytes.Bytes(), nil
}

// PortState is a enum describing one of possible states of port state machines
type PortState uint8

// Table 20 PTP state enumeration
const (
	PortStateInitializing PortState = iota + 1
	PortStateFaulty
	PortStateDisabled
	PortStateListening
	PortStatePreMaster
	PortStateMaster
	PortStatePassive
	PortStateUncalibrated
	PortStateSlave
	PortStateGrandMaster /*non-standard extension*/
)

// PortStateToString is a map from PortState to string
var PortStateToString = map[PortState]string{
	PortStateInitializing: "INITIALIZING",
	PortStateFaulty:       "FAULTY",
	PortStateDisabled:     "DISABLED",
	PortStateListening:    "LISTENING",
	PortStatePreMaster:    "PRE_MASTER",
	PortStateMaster:       "MASTER",
	PortStatePassive:      "PASSIVE",
	PortStateUncalibrated: "UNCALIBRATED",
	PortStateSlave:        "SLAVE",
	PortStateGrandMaster:  "GRAND_MASTER",
}

func (ps PortState) String() string {
	return PortStateToString[ps]
}

// TransportType is a enum describing network transport protocol types
type TransportType uint16

// Table 3 networkProtocol enumeration
const (
	/* 0 is Reserved in spec. Use it for UDS */
	TransportTypeUDS TransportType = iota
	TransportTypeUDPIPV4
	TransportTypeUDPIPV6
	TransportTypeIEEE8023
	TransportTypeDeviceNet
	TransportTypeControlNet
	TransportTypePROFINET
)

// TransportTypeToString is a map from TransportType to string
var TransportTypeToString = map[TransportType]string{
	TransportTypeUDS:        "UDS",
	TransportTypeUDPIPV4:    "UDP_IPV4",
	TransportTypeUDPIPV6:    "UDP_IPV6",
	TransportTypeIEEE8023:   "IEEE_802_3",
	TransportTypeDeviceNet:  "DEVICENET",
	TransportTypeControlNet: "CONTROLNET",
	TransportTypePROFINET:   "PROFINET",
}

func (t TransportType) String() string {
	return TransportTypeToString[t]
}

// PortAddress see 5.3.6 PortAddress
type PortAddress struct {
	NetworkProtocol TransportType
	AddressLength   uint16
	AddressField    []byte
}

// UnmarshalBinary converts bytes to PortAddress
func (p *PortAddress) UnmarshalBinary(b []byte) error {
	if len(b) < 8 {
		return fmt.Errorf("not enough data to decode PortAddress")
	}
	p.NetworkProtocol = TransportType(binary.BigEndian.Uint16(b[0:]))
	p.AddressLength = binary.BigEndian.Uint16(b[2:])
	addrLen := int(p.AddressLength)
	if len(b) < 4+addrLen {
		return fmt.Errorf("not enough data to decode PortAddress address")
	}
	p.AddressField = make([]byte, addrLen)
	copy(p.AddressField, b[4:4+addrLen])
	return nil
}

// IP converts PortAddress to IP
func (p *PortAddress) IP() (net.IP, error) {
	if p.NetworkProtocol != TransportTypeUDPIPV4 && p.NetworkProtocol != TransportTypeUDPIPV6 {
		return nil, fmt.Errorf("unsupported network protocol %s (%d)", p.NetworkProtocol, p.NetworkProtocol)
	}
	if p.NetworkProtocol == TransportTypeUDPIPV4 && (p.AddressLength != 4 || len(p.AddressField) != 4) {
		return nil, fmt.Errorf("unexpected length of IPv4: %d", len(p.AddressField))
	}
	if p.NetworkProtocol == TransportTypeUDPIPV6 && (p.AddressLength != 16 || len(p.AddressField) != 16) {
		return nil, fmt.Errorf("unexpected length of IPv6: %d", len(p.AddressField))
	}
	return net.IP(p.AddressField), nil
}

// MarshalBinary converts PortAddress to []bytes
func (p *PortAddress) MarshalBinary() ([]byte, error) {
	var bytes bytes.Buffer
	if err := binary.Write(&bytes, binary.BigEndian, p.NetworkProtocol); err != nil {
		return nil, err
	}
	if err := binary.Write(&bytes, binary.BigEndian, p.AddressLength); err != nil {
		return nil, err
	}
	if err := binary.Write(&bytes, binary.BigEndian, p.AddressField); err != nil {
		return nil, err
	}
	return bytes.Bytes(), nil
}
