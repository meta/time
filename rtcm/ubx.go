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
	"fmt"
	"math"
)

// UBX frame constants.
const (
	UBXSync1 byte = 0xB5
	UBXSync2 byte = 0x62

	UBXClassRXM byte = 0x02
	UBXMsgRAWX  byte = 0x15
	UBXMsgSFRBX byte = 0x13

	UBXHeaderSize  = 6 // sync(2) + class(1) + id(1) + len(2)
	UBXChecksumLen = 2

	// MaxUBXPayloadLen is the largest payload the 16-bit UBX length field can express.
	MaxUBXPayloadLen = 65535
	// MaxUBXFrameSize is the largest possible UBX frame.
	MaxUBXFrameSize = UBXHeaderSize + MaxUBXPayloadLen + UBXChecksumLen
)

// GNSS system identifiers from UBX protocol.
const (
	GnssGPS     uint8 = 0
	GnssSBAS    uint8 = 1
	GnssGalileo uint8 = 2
	GnssBeiDou  uint8 = 3
	GnssQZSS    uint8 = 5
	GnssGLONASS uint8 = 6
	GnssNavIC   uint8 = 7
)

// RawxObservation holds one satellite/signal measurement from UBX-RXM-RAWX.
type RawxObservation struct {
	PrMes    float64 // Pseudorange measurement [m]
	CpMes    float64 // Carrier phase measurement [cycles]
	DoMes    float32 // Doppler measurement [Hz]
	GnssID   uint8   // GNSS identifier
	SvID     uint8   // Satellite identifier
	SigID    uint8   // Signal identifier
	FreqID   uint8   // GLONASS frequency slot
	Locktime uint16  // Carrier phase lock time [ms]
	CNO      uint8   // Carrier-to-noise ratio [dB-Hz]
	PrValid  bool    // Pseudorange valid
	CpValid  bool    // Carrier phase valid
	HalfCyc  bool    // Half cycle resolved
}

// RawxEpoch holds all observations from one UBX-RXM-RAWX message.
type RawxEpoch struct {
	Observations []RawxObservation // Per-satellite measurements
	RcvTow       float64           // Receiver time of week [s]
	Week         uint16            // GPS week number
	LeapS        int8              // GPS-UTC leap seconds
}

// ParseUBXFrame validates a UBX frame starting at data[0] and returns the
// payload and total frame length consumed. Returns an error if the frame
// is incomplete or checksum fails.
func ParseUBXFrame(data []byte) (payload []byte, frameLen int, err error) {
	if len(data) < UBXHeaderSize+UBXChecksumLen {
		return nil, 0, fmt.Errorf("UBX frame too short: %d bytes", len(data))
	}
	if data[0] != UBXSync1 || data[1] != UBXSync2 {
		return nil, 0, fmt.Errorf("invalid UBX sync bytes")
	}

	payloadLen := int(binary.LittleEndian.Uint16(data[4:6]))
	frameLen = UBXHeaderSize + payloadLen + UBXChecksumLen
	if len(data) < frameLen {
		return nil, 0, fmt.Errorf("UBX frame incomplete: need %d, have %d", frameLen, len(data))
	}

	// Verify Fletcher-8 checksum over class+id+len+payload.
	var ckA, ckB uint8
	for i := 2; i < UBXHeaderSize+payloadLen; i++ {
		ckA += data[i]
		ckB += ckA
	}
	if ckA != data[frameLen-2] || ckB != data[frameLen-1] {
		return nil, 0, fmt.Errorf("UBX checksum mismatch")
	}

	return data[UBXHeaderSize : UBXHeaderSize+payloadLen], frameLen, nil
}

// ParseRawx decodes a UBX-RXM-RAWX payload into an epoch with observations.
func ParseRawx(payload []byte) (RawxEpoch, error) {
	if len(payload) < 16 {
		return RawxEpoch{}, fmt.Errorf("RAWX payload too short: %d", len(payload))
	}

	var epoch RawxEpoch
	epoch.RcvTow = math.Float64frombits(binary.LittleEndian.Uint64(payload[0:8]))
	epoch.Week = binary.LittleEndian.Uint16(payload[8:10])
	epoch.LeapS = int8(payload[10] & 0x7F) // GPS leap seconds are small and non-negative
	numMeas := int(payload[11])

	if len(payload) < 16+numMeas*32 {
		return RawxEpoch{}, fmt.Errorf("RAWX payload truncated: need %d, have %d",
			16+numMeas*32, len(payload))
	}

	epoch.Observations = make([]RawxObservation, 0, numMeas)
	for i := range numMeas {
		off := 16 + i*32
		obs := RawxObservation{
			PrMes:    math.Float64frombits(binary.LittleEndian.Uint64(payload[off : off+8])),
			CpMes:    math.Float64frombits(binary.LittleEndian.Uint64(payload[off+8 : off+16])),
			DoMes:    math.Float32frombits(binary.LittleEndian.Uint32(payload[off+16 : off+20])),
			GnssID:   payload[off+20],
			SvID:     payload[off+21],
			SigID:    payload[off+22],
			FreqID:   payload[off+23],
			Locktime: binary.LittleEndian.Uint16(payload[off+24 : off+26]),
			CNO:      payload[off+26],
		}
		trkStat := payload[off+30]
		obs.PrValid = trkStat&0x01 != 0
		obs.CpValid = trkStat&0x02 != 0
		obs.HalfCyc = trkStat&0x04 != 0

		// Only include observations with valid, nonzero pseudorange and signal.
		if obs.PrValid && obs.PrMes > 0 && obs.CNO > 0 {
			epoch.Observations = append(epoch.Observations, obs)
		}
	}

	return epoch, nil
}
