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
	"errors"
	"fmt"
)

/*
NTP Extension Field framing per RFC 7822 §7.5.

	0                   1                   2                   3
	0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
  +-------------------------------+-------------------------------+
  |          Field Type           |            Length             |
  +-------------------------------+-------------------------------+
  |                                                               |
  .                            Value                              .
  .                                                               .
  |                                                               |
  +---------------------------------------------------------------+
  |                       Padding (as needed)                     |
  +---------------------------------------------------------------+

  - Field Type: 16-bit IANA-assigned Extension Field type
  - Length:     16-bit total length in octets of {type, length, value, padding},
                MUST be a multiple of 4 octets
  - Value:      variable-length payload
  - Padding:    zero octets so that the total length is a multiple of 4

For NTS (RFC 8915 §5.1) the minimum total length is 4 octets, which overrides
the 16-octet minimum from RFC 7822 §7.5.1.2. We follow the NTS profile.
*/

// ExtensionFieldType is the 16-bit IANA-assigned NTP extension field type
// (RFC 7822 §7.5 and the IANA "NTP Extension Field Types" registry).
type ExtensionFieldType uint16

// ExtensionField is a parsed NTP extension field. Body holds the value plus
// any padding bytes as they appeared on the wire; the caller is responsible
// for interpreting the value boundary based on the field type.
type ExtensionField struct {
	Type ExtensionFieldType
	Body []byte
}

const (
	// ExtensionHeaderSize is the size of the field-type+length prefix in octets.
	ExtensionHeaderSize = 4
	// ExtensionMinSize is the minimum total length of an extension field in octets,
	// per RFC 8915 §5.1 (NTS profile).
	ExtensionMinSize = 4
	// ExtensionAlignment is the field-length alignment in octets per RFC 7822 §7.5.
	ExtensionAlignment = 4
	// ExtensionMaxBodySize is the largest body that, after prepending the
	// 4-octet header and rounding up to a 4-octet alignment boundary, still
	// fits in the 16-bit length field. The largest aligned total that fits
	// in uint16 is 0xFFFC; subtracting the header gives the body bound.
	// Bodies larger than this would silently wrap the uint16 length on the
	// wire even though they pass a naïve `< 0xFFFF` check.
	ExtensionMaxBodySize = 0xFFFC - ExtensionHeaderSize

	// ExtensionTypeReservedLo is the lower bound (inclusive) of the range the
	// IANA "NTP Extension Field Types" registry marks "Reserved for Private or
	// Experimental Use" (RFC 9748). The range runs to 0xFFFF, the maximum
	// representable type, so no explicit upper bound is needed.
	ExtensionTypeReservedLo ExtensionFieldType = 0xF000
)

// NTS extension field types defined by RFC 8915 §5.1.1.
const (
	UniqueIdentifier     ExtensionFieldType = 0x0104
	NTSCookie            ExtensionFieldType = 0x0204
	NTSCookiePlaceholder ExtensionFieldType = 0x0304
	NTSAuthenticator     ExtensionFieldType = 0x0404
)

// AEADAlgorithm is a 16-bit IANA "AEAD Algorithms" registry identifier
// negotiated via NTS-KE and used to select the AEAD for the Authenticator
// extension field (RFC 8915 §5.1).
type AEADAlgorithm uint16

const (
	// AEADAESSIVCMAC256 is AEAD_AES_SIV_CMAC_256 (RFC 5297), the RFC 8915
	// §4.1.5 mandatory-to-implement algorithm. NTS carries a non-empty wire nonce
	// as an S2V component; this implementation generates 16 octets to match chrony.
	AEADAESSIVCMAC256 AEADAlgorithm = 15
	// AEADAESSIVCMAC512 is AEAD_AES_SIV_CMAC_512 (RFC 5297): a 64-octet key, nonce as for id 15.
	AEADAESSIVCMAC512 AEADAlgorithm = 17
	// AEADAES128GCMSIV is AEAD_AES_128_GCM_SIV (RFC 8452). It uses a 16-octet
	// key and a 12-octet nonce.
	AEADAES128GCMSIV AEADAlgorithm = 30
)

// Sentinel errors returned by extension-field parsing.
var (
	ErrExtensionTruncated     = errors.New("extension field truncated")
	ErrExtensionLengthInvalid = errors.New("extension field length invalid")
	ErrExtensionBodyTooLarge  = errors.New("extension field body too large")
	ErrExtensionTypeReserved  = errors.New("extension field type is IANA-reserved")
)

// padTo returns the smallest multiple of align >= n. align must be a power of two.
func padTo(n, align int) int {
	return (n + align - 1) &^ (align - 1)
}

// EncodedSize returns the wire-format size in octets, including padding, that
// MarshalExtensionFields would emit for this field.
func (ef ExtensionField) EncodedSize() int {
	total := padTo(ExtensionHeaderSize+len(ef.Body), ExtensionAlignment)
	return max(total, ExtensionMinSize)
}

// Encode returns the wire-format bytes of this extension field, padded per RFC 7822.
func (ef ExtensionField) Encode() ([]byte, error) {
	return MarshalExtensionFields([]ExtensionField{ef})
}

// String returns the IANA-registered name for known NTP extension field
// types and "Unknown(0xXXXX)" otherwise. See RFC 8915 §5 and the IANA "NTP Extension
// Field Types" registry.
func (t ExtensionFieldType) String() string {
	switch t {
	case UniqueIdentifier:
		return "Unique Identifier"
	case NTSCookie:
		return "NTS Cookie"
	case NTSCookiePlaceholder:
		return "NTS Cookie Placeholder"
	case NTSAuthenticator:
		return "NTS Authenticator and Encrypted Extension Fields"
	default:
		return fmt.Sprintf("Unknown(0x%04x)", uint16(t))
	}
}

// isNTS reports whether t is one of the extension field types RFC 8915 §5.1.1
// defines for NTS.
func (t ExtensionFieldType) isNTS() bool {
	switch t {
	case UniqueIdentifier, NTSCookie, NTSCookiePlaceholder, NTSAuthenticator:
		return true
	}
	return false
}

// isReserved reports whether t falls in the IANA-reserved range
// 0xF000–0xFFFF ("Reserved for Private or Experimental Use", RFC 9748).
func (t ExtensionFieldType) isReserved() bool {
	return t >= ExtensionTypeReservedLo
}

// checkBodySize guards against a body large enough to wrap the 16-bit wire
// length field once the 4-octet header and padding are added. It applies to
// every encode path, including faithful reconstruction of received packets.
func (ef ExtensionField) checkBodySize() error {
	if len(ef.Body) > ExtensionMaxBodySize {
		return fmt.Errorf("%w: type=%#x body=%d max=%d",
			ErrExtensionBodyTooLarge, ef.Type, len(ef.Body), ExtensionMaxBodySize)
	}
	return nil
}

// validate reports whether the extension field can be safely marshalled: the
// body must not exceed ExtensionMaxBodySize and Type must not fall in the
// IANA-reserved range (0xF000–0xFFFF, RFC 9748). It is the single policy source
// for extension fields we originate (see MarshalExtensionFields).
func (ef ExtensionField) validate() error {
	if err := ef.checkBodySize(); err != nil {
		return err
	}
	if ef.Type.isReserved() {
		return fmt.Errorf("%w: type=%#x", ErrExtensionTypeReserved, ef.Type)
	}
	return nil
}

// MarshalExtensionFields is the strict, outbound-only encoder: every field must
// pass Validate (rejecting IANA-reserved types we must never originate) before
// encoding. Use this for any extension fields we generate. Each field is padded
// with zeros to a multiple of 4 octets and to at least ExtensionMinSize (RFC 7822 §7.5).
func MarshalExtensionFields(efs []ExtensionField) ([]byte, error) {
	for _, ef := range efs {
		if err := ef.validate(); err != nil {
			return nil, err
		}
	}
	return encodeExtensionFields(efs)
}

// writeExtensionFields writes efs into dst (sized by the caller via EncodedSize)
// and returns bytes written, with no validation. It clear()s each field's
// padding so output is correct on a reused buffer (RFC 8915 §5.6: padding MUST
// be zero; stale padding in an authenticator's AD would break verification).
func writeExtensionFields(dst []byte, efs []ExtensionField) int {
	offset := 0
	for _, ef := range efs {
		flen := ef.EncodedSize()
		binary.BigEndian.PutUint16(dst[offset:offset+2], uint16(ef.Type))
		binary.BigEndian.PutUint16(dst[offset+2:offset+4], uint16(flen)) // #nosec G115 -- bounded by checkBodySize
		bodyEnd := offset + ExtensionHeaderSize + len(ef.Body)
		copy(dst[offset+ExtensionHeaderSize:], ef.Body)
		clear(dst[bodyEnd : offset+flen]) // zero padding (reused-buffer safety)
		offset += flen
	}
	return offset
}

// MarshalExtensionFieldsTo encodes efs into dst without allocating, returning
// bytes written. Like MarshalExtensionFields it is strict (rejects reserved
// types) and zeroes padding, so a caller may reuse one buffer across packets.
// dst must be at least the sum of each field's EncodedSize.
func MarshalExtensionFieldsTo(efs []ExtensionField, dst []byte) (int, error) {
	total := 0
	for _, ef := range efs {
		if err := ef.validate(); err != nil {
			return 0, err
		}
		total += ef.EncodedSize()
	}
	if len(dst) < total {
		return 0, fmt.Errorf("%w: need %d bytes, have %d", ErrExtensionTruncated, total, len(dst))
	}
	return writeExtensionFields(dst, efs), nil
}

// encodeExtensionFields marshals faithfully: it keeps the length-overflow guard
// but does NOT reject reserved types, so a parsed packet re-serializes
// byte-for-byte — required because RFC 8915 §5.2 covers unrecognized EFs in the
// authenticated portion, even reserved-range ones we would never originate.
func encodeExtensionFields(efs []ExtensionField) ([]byte, error) {
	totalSize := 0
	for _, ef := range efs {
		if err := ef.checkBodySize(); err != nil {
			return nil, err
		}
		totalSize += ef.EncodedSize()
	}
	out := make([]byte, totalSize)
	writeExtensionFields(out, efs)
	return out, nil
}

// ParseExtensionFields parses a buffer of zero or more concatenated extension
// fields per. The buffer must contain only extension fields;
// any legacy NTPv3/v4 MAC bytes must be stripped before calling.
func ParseExtensionFields(b []byte) ([]ExtensionField, error) {
	efs, _, err := parseExtensionFields(b)
	if err != nil {
		return nil, err
	}
	return efs, nil
}

// parseExtensionFields parses concatenated extension fields from the front of b,
// also returning how many octets they occupy. On failure it still returns both,
// so a caller whose buffer may end in an unframed legacy MAC can see where the
// framing stopped.
func parseExtensionFields(b []byte) ([]ExtensionField, int, error) {
	var efs []ExtensionField
	total := len(b)
	for len(b) > 0 {
		if len(b) < ExtensionHeaderSize {
			return efs, total - len(b), fmt.Errorf("%w: need %d header bytes, have %d",
				ErrExtensionTruncated, ExtensionHeaderSize, len(b))
		}
		ftype := ExtensionFieldType(binary.BigEndian.Uint16(b[0:2]))
		flen := int(binary.BigEndian.Uint16(b[2:4]))
		if flen < ExtensionMinSize || flen%ExtensionAlignment != 0 {
			return efs, total - len(b), fmt.Errorf("%w: type=%#x length=%d", ErrExtensionLengthInvalid, ftype, flen)
		}
		if flen > len(b) {
			return efs, total - len(b), fmt.Errorf("%w: type=%#x length=%d remaining=%d",
				ErrExtensionTruncated, ftype, flen, len(b))
		}
		// Copy so the body outlives any reuse of b.
		body := make([]byte, flen-ExtensionHeaderSize)
		copy(body, b[ExtensionHeaderSize:flen])
		efs = append(efs, ExtensionField{Type: ftype, Body: body})
		b = b[flen:]
	}
	return efs, total, nil
}

// namesNTSFieldType reports whether b opens with an extension-field header naming
// an RFC 8915 field type. It is read where parseExtensionFields stopped, so those
// four octets are the header that failed: a broken NTS field is never mistaken
// for an unframed legacy MAC.
func namesNTSFieldType(b []byte) bool {
	if len(b) < ExtensionHeaderSize {
		return false
	}
	return ExtensionFieldType(binary.BigEndian.Uint16(b[0:2])).isNTS()
}
