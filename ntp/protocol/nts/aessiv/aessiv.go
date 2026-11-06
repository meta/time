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

// Package aessiv implements AES-SIV-CMAC (RFC 5297) for NTS AEAD ids 15 and 17.
package aessiv

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
)

const (
	// BlockSize is the block size of AES, and so the length of the synthetic
	// IV that AES-SIV prepends to every ciphertext.
	BlockSize = aes.BlockSize

	// KeySize is the AES-SIV-CMAC-256 key length in octets.
	KeySize = 32

	// KeySize512 is the AES-SIV-CMAC-512 key length in octets.
	KeySize512 = 64

	// ctrBatchThreshold is where crypto/cipher's CTR overtakes a per-block
	// loop: below it, its 512-octet stream buffer costs more than its 8-block
	// AES-NI batching saves. Measured crossover on the package benchmarks.
	ctrBatchThreshold = 256
)

// Sentinel errors. Compare with errors.Is.
var (
	// ErrKeySize is returned by NewAESSIV for a key of any other length.
	ErrKeySize = errors.New("aessiv: invalid key size")
	// ErrPlaintextTooLong is returned by Seal when the ciphertext would not
	// fit in an int.
	ErrPlaintextTooLong = errors.New("aessiv: plaintext too long")
	// ErrCiphertextTooShort is returned by Open for input too short to hold a
	// synthetic IV.
	ErrCiphertextTooShort = errors.New("aessiv: ciphertext shorter than synthetic IV")
	// ErrVerify is returned by Open when the synthetic IV recomputed from the
	// recovered plaintext does not match the one on the wire.
	ErrVerify = errors.New("aessiv: invalid ciphertext")
)

var zeroBlock [BlockSize]byte

// AESSIV implements AES-SIV-CMAC and is safe for concurrent use.
// It deliberately does not implement crypto/cipher.AEAD: that contract is
// nonce-based while SIV is deterministic, as tink's DeterministicAEAD reflects.
type AESSIV struct {
	cmac *cmac
	ctr  cipher.Block
	// d0 is CMAC(zero block), the S2V starting value. It depends only on the
	// key, so it is derived once here instead of on every Seal and Open.
	d0 [BlockSize]byte
}

// scratch holds one call's working buffers. Each is handed to Encrypt, whose
// interface call defeats escape analysis, so grouping them costs a single
// allocation instead of one per buffer.
type scratch struct {
	siv [BlockSize]byte     // synthetic IV, and the CTR counter source
	tag [BlockSize]byte     // S2V recomputed by Open, compared against siv
	mac [BlockSize]byte     // per-component CMAC output
	ctr [2 * BlockSize]byte // CTR counter followed by its keystream block
}

// NewAESSIV returns an AESSIV for a KeySize or KeySize512 key. RFC 5297
// section 2.2 splits it in half: the left half keys S2V, the right half CTR.
func NewAESSIV(key []byte) (*AESSIV, error) {
	if len(key) != KeySize && len(key) != KeySize512 {
		return nil, fmt.Errorf("%w: got %d, want %d or %d", ErrKeySize, len(key), KeySize, KeySize512)
	}
	half := len(key) / 2
	c, err := newCMAC(key[:half])
	if err != nil {
		return nil, err
	}
	ctr, err := aes.NewCipher(key[half:])
	if err != nil {
		return nil, fmt.Errorf("aessiv: could not obtain cipher: %w", err)
	}
	a := &AESSIV{cmac: c, ctr: ctr}
	c.compute(&a.d0, zeroBlock[:])
	return a, nil
}

// Seal deterministically encrypts plaintext, binding it to the ordered
// associated-data components, and appends synthetic IV || ciphertext to dst.
// Reordering or resplitting components changes the synthetic IV.
func (a *AESSIV) Seal(dst, plaintext []byte, components ...[]byte) ([]byte, error) {
	if len(plaintext) > math.MaxInt-BlockSize {
		return nil, ErrPlaintextTooLong
	}
	// S2V reads plaintext before anything is written, so an aliasing dst is safe.
	s := new(scratch)
	if err := a.s2v(&s.siv, s, plaintext, components); err != nil {
		return nil, err
	}
	ret, out := sliceForAppend(dst, BlockSize+len(plaintext))
	// Move the plaintext up before stamping the IV over its first octets; copy
	// is memmove, so this is correct even when dst aliases plaintext.
	copy(out[BlockSize:], plaintext)
	copy(out[:BlockSize], s.siv[:])
	a.ctrCrypt(s, out[BlockSize:])
	return ret, nil
}

// Open verifies and decrypts a blob produced by Seal under the same ordered
// component vector, appending the plaintext to dst. On failure it returns
// ErrVerify and zeroes whatever it wrote to dst.
func (a *AESSIV) Open(dst, ciphertext []byte, components ...[]byte) ([]byte, error) {
	if len(ciphertext) < BlockSize {
		return nil, fmt.Errorf("%w: got %d, want at least %d",
			ErrCiphertextTooShort, len(ciphertext), BlockSize)
	}
	// Take the IV before the output can overwrite it, since dst may alias.
	s := new(scratch)
	copy(s.siv[:], ciphertext[:BlockSize])

	ret, out := sliceForAppend(dst, len(ciphertext)-BlockSize)
	copy(out, ciphertext[BlockSize:])
	a.ctrCrypt(s, out)

	// RFC 5297 section 2.7: recompute S2V over the recovered plaintext and
	// return nothing unless it matches.
	if err := a.s2v(&s.tag, s, out, components); err != nil {
		clear(out)
		return nil, err
	}
	if subtle.ConstantTimeCompare(s.siv[:], s.tag[:]) != 1 {
		// Never hand back unauthenticated plaintext: dst may alias the
		// ciphertext, so a forged blob would otherwise leave attacker-controlled
		// data in the caller's buffer. crypto/cipher's GCM clears here too.
		clear(out)
		return nil, ErrVerify
	}
	return ret, nil
}

// sliceForAppend extends in by n octets, reusing its capacity where possible,
// and returns the whole slice plus the n-octet tail to write into.
func sliceForAppend(in []byte, n int) (head, tail []byte) {
	if total := len(in) + n; cap(in) >= total {
		head = in[:total]
	} else {
		head = make([]byte, total)
		copy(head, in)
	}
	return head, head[len(in):]
}

// ctrCrypt encrypts or decrypts buf in place, keyed by s.siv. RFC 5297 section
// 2.5 clears two bits of the synthetic IV first, so the 32-bit counter cannot
// carry into the rest of the block.
func (a *AESSIV) ctrCrypt(s *scratch, buf []byte) {
	counter, keystream := s.ctr[:BlockSize], s.ctr[BlockSize:]
	copy(counter, s.siv[:])
	counter[8] &= 0x7f
	counter[12] &= 0x7f

	// Large payloads go to crypto/cipher, which batches 8 blocks per AES-NI
	// call; that wins once there is enough data to amortise its buffer.
	if len(buf) >= ctrBatchThreshold {
		cipher.NewCTR(a.ctr, counter).XORKeyStream(buf, buf)
		return
	}
	for len(buf) > 0 {
		a.ctr.Encrypt(keystream, counter)
		n := subtle.XORBytes(buf, buf, keystream)
		buf = buf[n:]
		incrementCounter(counter)
	}
}

// incrementCounter adds one to the counter block as a big-endian integer,
// matching the counter progression of crypto/cipher's CTR mode.
func incrementCounter(counter []byte) {
	for i := len(counter) - 1; i >= 0; i-- {
		counter[i]++
		if counter[i] != 0 {
			return
		}
	}
}

// s2v writes the string-to-vector PRF of RFC 5297 section 2.4 into out,
// evaluated over the component vector followed by the message, which is always
// the last element. An empty vector is valid: the message is then the only one.
func (a *AESSIV) s2v(out *[BlockSize]byte, s *scratch, msg []byte, components [][]byte) error {
	block := a.d0 // CMAC(zero block), derived once per key

	// block = mulByX(block) XOR CMAC(component), once per component in order.
	for _, c := range components {
		mulByX(block[:])
		a.cmac.compute(&s.mac, c)
		subtle.XORBytes(block[:], block[:], s.mac[:])
	}
	if len(msg) >= BlockSize {
		// v = CMAC(msg xorend block). The size preconditions hold by
		// construction here, but surface a violation instead of panicking:
		// a reachable panic in a primitive is a denial-of-service vector.
		return a.cmac.xorEndAndCompute(out, msg, block[:])
	}
	// block = mulByX(block) XOR pad(msg)
	mulByX(block[:])
	subtle.XORBytes(block[:], block[:], msg)
	block[len(msg)] ^= pad
	a.cmac.compute(out, block[:])
	return nil
}
