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
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"

	"github.com/facebook/time/ntp/protocol"
	"github.com/facebook/time/ntp/protocol/nts/aessiv"
	aeadsubtle "github.com/tink-crypto/tink-go/v2/aead/subtle"
)

// AEAD is the authenticated-encryption interface used to protect the encrypted
// extension fields of an NTS-protected NTP packet (RFC 8915). Implementations
// wrap a specific negotiated IANA AEAD algorithm; construct one with NewAEAD.
type AEAD interface {
	// Seal encrypts and authenticates plaintext, binding it to ad. It returns
	// the algorithm-required wire nonce and ciphertext to place in the
	// Authenticator's corresponding fields.
	Seal(ad, plaintext []byte) (nonce, ciphertext []byte, err error)
	// Open verifies and decrypts ciphertext against ad and nonce. A nonce of the
	// wrong length returns ErrAEADNonceSize; authentication and decryption
	// failures are returned as the underlying error.
	Open(ad, nonce, ciphertext []byte) (plaintext []byte, err error)
}

var (
	// ErrUnsupportedAlgorithm is returned by NewAEAD when the IANA AEAD ID is
	// not one this package implements.
	ErrUnsupportedAlgorithm = errors.New("nts: unsupported AEAD algorithm")
	// ErrAEADKeySize is returned by NewAEAD when the key length does not match
	// the one required by the negotiated algorithm.
	ErrAEADKeySize = errors.New("nts: invalid AEAD key size")
	// ErrAEADNonceSize is returned by Open when the nonce has the wrong length
	// for the negotiated algorithm.
	ErrAEADNonceSize = errors.New("nts: invalid AEAD nonce size")
)

const (
	aesSIVCMACGeneratedNonceSize = 16

	// aes128GCMSIVKeySize is the AES-128-GCM-SIV key length in octets. tink's
	// NewAESGCMSIV also accepts 32-octet keys (AES-256), but IANA AEAD ID 30 is
	// specifically AES-128, so NewAEAD pins the key length.
	aes128GCMSIVKeySize = 16

	// aesGCMSIVNonceSize is fixed by RFC 8452.
	aesGCMSIVNonceSize = 12

	// aesGCMSIVTagSize is the AES-GCM-SIV authentication tag length in octets
	// (RFC 8452, same as AES block size). Used to validate tink output structure
	// nonce || ciphertext || tag.
	aesGCMSIVTagSize = 16
)

// NewAEAD builds an AEAD for the negotiated IANA AEAD algorithm ID. Supported
// IDs: AEADAESSIVCMAC256 (15), AEADAESSIVCMAC512 (17) and AEADAES128GCMSIV (30).
func NewAEAD(algorithm protocol.AEADAlgorithm, key []byte) (AEAD, error) {
	switch algorithm {
	case protocol.AEADAESSIVCMAC256:
		return newAESSIVCMAC("AES-SIV-CMAC-256", aessiv.KeySize, key)
	case protocol.AEADAESSIVCMAC512:
		return newAESSIVCMAC("AES-SIV-CMAC-512", aessiv.KeySize512, key)
	case protocol.AEADAES128GCMSIV:
		// key_len check
		if len(key) != aes128GCMSIVKeySize {
			return nil, fmt.Errorf("%w: AES-128-GCM-SIV requires %d bytes, got %d",
				ErrAEADKeySize, aes128GCMSIVKeySize, len(key))
		}
		gcm, err := aeadsubtle.NewAESGCMSIV(key)
		if err != nil {
			return nil, fmt.Errorf("nts: AES-128-GCM-SIV: %w", err)
		}
		return &aesGCMSIV{aead: gcm}, nil
	default:
		return nil, fmt.Errorf("%w: %#x", ErrUnsupportedAlgorithm, algorithm)
	}
}

func newAESSIVCMAC(name string, keySize int, key []byte) (AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("%w: %s requires %d bytes, got %d",
			ErrAEADKeySize, name, keySize, len(key))
	}
	siv, err := aessiv.NewAESSIV(key)
	if err != nil {
		return nil, fmt.Errorf("nts: %s: %w", name, err)
	}
	return &aesSIVCMAC{siv: siv}, nil
}

type aesSIVCMAC struct {
	siv *aessiv.AESSIV
}

// encrypt-and-authenticate
func (a *aesSIVCMAC) Seal(ad, plaintext []byte) ([]byte, []byte, error) {
	nonce := make([]byte, aesSIVCMACGeneratedNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("nts: AES-SIV-CMAC nonce: %w", err)
	}
	ct, err := a.siv.Seal(nil, plaintext, ad, nonce)
	if err != nil {
		return nil, nil, fmt.Errorf("nts: AES-SIV-CMAC encrypt: %w", err)
	}
	return nonce, ct, nil
}

// verify-and-decrypt
func (a *aesSIVCMAC) Open(ad, nonce, ciphertext []byte) ([]byte, error) {
	// RFC 5297 section 6: N_MIN = 1.
	if len(nonce) < 1 {
		return nil, fmt.Errorf("%w: AES-SIV-CMAC requires at least 1 byte", ErrAEADNonceSize)
	}
	pt, err := a.siv.Open(nil, ciphertext, ad, nonce)
	if err != nil {
		return nil, fmt.Errorf("nts: AES-SIV-CMAC decrypt: %w", err)
	}
	return pt, nil
}

// aesGCMSIV wraps tink's AES-GCM-SIV (RFC 8452, 16-octet key, 12-octet nonce).
type aesGCMSIV struct {
	aead *aeadsubtle.AESGCMSIV
}

func (a *aesGCMSIV) Seal(ad, plaintext []byte) ([]byte, []byte, error) {
	// tink returns (nonce || ciphertext || tag) as one blob; split the random
	// 12-octet nonce off the front so it lands in the Authenticator Nonce field.
	sealed, err := a.aead.Encrypt(plaintext, ad)
	if err != nil {
		return nil, nil, fmt.Errorf("nts: GCM-SIV encrypt: %w", err)
	}
	minSealedLen := aesGCMSIVNonceSize + aesGCMSIVTagSize
	if len(sealed) < minSealedLen {
		return nil, nil, fmt.Errorf("nts: GCM-SIV output %d bytes shorter than minimum %d (nonce+tag)", len(sealed), minSealedLen)
	}
	// Return independent copies to avoid slice aliasing hazard:
	// sealed[:n] has cap=len(sealed), so append to nonce would overwrite ciphertext.
	// bytes.Clone makes the copy intent explicit (Go 1.20+, Meta targets 1.26).
	nonce := bytes.Clone(sealed[:aesGCMSIVNonceSize])
	ciphertext := bytes.Clone(sealed[aesGCMSIVNonceSize:])
	return nonce, ciphertext, nil
}
func (a *aesGCMSIV) Open(ad, nonce, ciphertext []byte) ([]byte, error) {
	// validate nonce len
	if len(nonce) != aesGCMSIVNonceSize {
		return nil, fmt.Errorf("%w: GCM-SIV requires %d-byte nonce, got %d",
			ErrAEADNonceSize, aesGCMSIVNonceSize, len(nonce))
	}
	// Rejoin (nonce || ciphertext || tag) into the blob layout tink expects.
	// slices.Concat is Go 1.22+ modern idiom for concatenating slices.
	sealed := slices.Concat(nonce, ciphertext)
	pt, err := a.aead.Decrypt(sealed, ad)
	if err != nil {
		return nil, fmt.Errorf("nts: GCM-SIV decrypt: %w", err)
	}
	return pt, nil
}
