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

package nts

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/facebook/time/ntp/protocol"
)

/*
NTS Authenticator and Encrypted Extension Fields (RFC 8915 §5.6).

	0                   1                   2                   3
	0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
  +-------------------------------+-------------------------------+
  |    Field Type = 0x0404        |          Length               |
  +-------------------------------+-------------------------------+
  |          Nonce Length         |       Ciphertext Length       |
  +-------------------------------+-------------------------------+
  |                                                               |
  ~                            Nonce                              ~
  |                                                               |
  +---------------------------------------------------------------+
  |                                                               |
  ~                         Ciphertext                            ~
  |                                                               |
  +---------------------------------------------------------------+
  |                  Additional Padding (if any)                  |
  +---------------------------------------------------------------+

The associated data is the entire NTP packet from the start of the header
through the end of the extension field immediately preceding this one
(RFC 8915 §5.6.1). The plaintext, when present, is itself a sequence of
NTP extension fields ("encrypted EFs") which the receiver appends to its
parsed extension-field list.

RFC 8915 §5.6 specifies that the Nonce field is "as required by the negotiated
AEAD algorithm". For AES-SIV-CMAC (AEAD IDs 15 and 17) a 16-octet nonce is sent
and authenticated as an S2V component after the packet associated data.
*/

// AuthenticatorBody is the parsed body of an NTS Authenticator extension field.
type AuthenticatorBody struct {
	Nonce      []byte
	Ciphertext []byte
}

const authenticatorHeaderLen = 4

// Sentinel errors.
var (
	ErrAuthenticatorTruncated = errors.New("nts authenticator truncated")
	ErrAuthenticatorMalformed = errors.New("nts authenticator malformed")
	// ErrAuthenticatorVerify is returned when AEAD verification fails on
	// OpenAuthenticator. It is deliberately the only thing callers can
	// errors.Is on for verification failures — the underlying tink error
	// is included as text for debug visibility but not as part of the
	// error chain, to insulate callers from tink internals (defense in
	// depth in case a future tink revision adds sensitive detail to its
	// error messages, and to follow the crypto/cipher.GCM stdlib pattern
	// of "cipher: message authentication failed").
	ErrAuthenticatorVerify = errors.New("nts authenticator verification failed")
)

// padTo4 returns the smallest multiple of 4 >= n.
func padTo4(n int) int {
	return (n + 3) &^ 3
}

// MarshalAuthenticatorBody serializes nonce and ciphertext into the
// authenticator EF body wire format defined by RFC 8915 §5.6. Nonce and
// ciphertext are each padded with zeros to a 4-octet boundary.
func MarshalAuthenticatorBody(nonce, ciphertext []byte) ([]byte, error) {
	if len(nonce) > 0xFFFF {
		return nil, fmt.Errorf("nts: nonce length %d exceeds uint16", len(nonce))
	}
	if len(ciphertext) > 0xFFFF {
		return nil, fmt.Errorf("nts: ciphertext length %d exceeds uint16", len(ciphertext))
	}
	paddedNonceLen := padTo4(len(nonce))
	paddedCipherLen := padTo4(len(ciphertext))
	// RFC 8915 §5.6 requires the padding octets after nonce and ciphertext
	// to be zero. We rely on Go's make() zero-initialisation to satisfy this
	// implicitly — any future refactor that reuses a buffer (e.g. via
	// sync.Pool) MUST explicitly zero the padding region before writing the
	// nonce and ciphertext, or invalid frames will go on the wire.
	out := make([]byte, authenticatorHeaderLen+paddedNonceLen+paddedCipherLen)
	binary.BigEndian.PutUint16(out[0:2], uint16(len(nonce)))      // #nosec G115 -- bounds checked above
	binary.BigEndian.PutUint16(out[2:4], uint16(len(ciphertext))) // #nosec G115 -- bounds checked above
	copy(out[authenticatorHeaderLen:], nonce)
	copy(out[authenticatorHeaderLen+paddedNonceLen:], ciphertext)
	return out, nil
}

// ParseAuthenticatorBody decodes the body of an NTS Authenticator EF.
//
// Trailing bytes past the declared nonce + ciphertext are silently accepted:
// RFC 8915 §5.6 explicitly allows "Additional Padding (if any)" after the
// ciphertext, and we have no way to distinguish legitimate padding from
// stray bytes without protocol-specific knowledge that doesn't belong at
// this layer.
func ParseAuthenticatorBody(body []byte) (AuthenticatorBody, error) {
	var ab AuthenticatorBody
	if len(body) < authenticatorHeaderLen {
		return ab, fmt.Errorf("%w: header needs %d bytes, have %d",
			ErrAuthenticatorTruncated, authenticatorHeaderLen, len(body))
	}
	nonceLen := int(binary.BigEndian.Uint16(body[0:2]))
	cipherLen := int(binary.BigEndian.Uint16(body[2:4]))
	paddedNonceLen := padTo4(nonceLen)
	paddedCipherLen := padTo4(cipherLen)
	required := authenticatorHeaderLen + paddedNonceLen + paddedCipherLen
	if required > len(body) {
		return ab, fmt.Errorf("%w: required=%d body_len=%d nonce_len=%d cipher_len=%d",
			ErrAuthenticatorTruncated, required, len(body), nonceLen, cipherLen)
	}
	ab.Nonce = make([]byte, nonceLen)
	copy(ab.Nonce, body[authenticatorHeaderLen:authenticatorHeaderLen+nonceLen])
	ab.Ciphertext = make([]byte, cipherLen)
	copy(ab.Ciphertext, body[authenticatorHeaderLen+paddedNonceLen:authenticatorHeaderLen+paddedNonceLen+cipherLen])
	return ab, nil
}

// SealAuthenticator encrypts plaintext under the given AEAD, authenticating ad
// as associated data, and returns the result as an NTS Authenticator extension
// field (RFC 8915 §5.6). The AEAD-produced nonce and ciphertext are packed into
// the EF body via MarshalAuthenticatorBody. A nil or empty plaintext yields an
// authenticator that protects ad only.
func SealAuthenticator(aead AEAD, ad, plaintext []byte) (protocol.ExtensionField, error) {
	nonce, ct, err := aead.Seal(ad, plaintext)
	if err != nil {
		return protocol.ExtensionField{}, err
	}
	body, err := MarshalAuthenticatorBody(nonce, ct)
	if err != nil {
		return protocol.ExtensionField{}, err
	}
	return protocol.ExtensionField{Type: protocol.NTSAuthenticator, Body: body}, nil
}

// OpenAuthenticator verifies and decrypts an NTS Authenticator extension field
// (RFC 8915 §5.6), authenticating ad as associated data, and returns the
// recovered plaintext. It returns ErrAuthenticatorMalformed if ef is not an
// authenticator EF, if its body is structurally invalid, or if the nonce length
// is rejected by the AEAD; it returns ErrAuthenticatorVerify if AEAD
// verification fails.
func OpenAuthenticator(aead AEAD, ad []byte, ef protocol.ExtensionField) ([]byte, error) {
	if ef.Type != protocol.NTSAuthenticator {
		return nil, fmt.Errorf("%w: expected type %#x got %#x",
			ErrAuthenticatorMalformed, protocol.NTSAuthenticator, ef.Type)
	}
	body, err := ParseAuthenticatorBody(ef.Body)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(ad, body.Nonce, body.Ciphertext)
	if err != nil {
		// A wrong-length nonce is a structural (malformed) fault, not an
		// authentication failure — surface it as ErrAuthenticatorMalformed.
		if errors.Is(err, ErrAEADNonceSize) {
			return nil, fmt.Errorf("%w: %s", ErrAuthenticatorMalformed, err.Error())
		}
		// Keep the AEAD err text but not in the errors.Is chain — see the
		// ErrAuthenticatorVerify godoc. %s (not %w) is deliberate.
		return nil, fmt.Errorf("%w: %s", ErrAuthenticatorVerify, err.Error())
	}
	return pt, nil
}
