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

package aessiv

import (
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// newRNG returns a deterministic RNG. Tests want reproducible failures rather
// than unpredictability, so a fixed seed is the point.
func newRNG(seed1, seed2 uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed1, seed2)) //nolint:gosec // G404: test data, not key material
}

// randBytes returns n pseudo-random octets from a fixed seed, so that a failing
// case is reproducible.
func randBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	var word [8]byte
	for i := 0; i < n; i += len(word) {
		binary.LittleEndian.PutUint64(word[:], rng.Uint64())
		copy(b[i:], word[:])
	}
	return b
}

// mustSeal seals and fails the test on error, for the many call sites where
// Seal cannot fail and the error check would only add noise.
func mustSeal(t *testing.T, a *AESSIV, dst, plaintext []byte, components ...[]byte) []byte {
	t.Helper()
	out, err := a.Seal(dst, plaintext, components...)
	require.NoError(t, err)
	return out
}

// newTestAESSIV returns an instance keyed with deterministic pseudo-random bytes.
func newTestAESSIV(t *testing.T, rng *rand.Rand) *AESSIV {
	t.Helper()
	a, err := NewAESSIV(randBytes(rng, KeySize))
	require.NoError(t, err)
	return a
}

// TestOverhead pins the ciphertext expansion: Seal prepends one synthetic IV
// and nothing else.
func TestOverhead(t *testing.T) {
	rng := newRNG(1, 1)
	a := newTestAESSIV(t, rng)
	for _, ptLen := range []int{0, 1, BlockSize, 100} {
		sealed := mustSeal(t, a, nil, randBytes(rng, ptLen), nil)
		require.Len(t, sealed, ptLen+BlockSize)
	}
}

// TestRFC5297A1 pins the deterministic example from RFC 5297 Appendix A.1,
// which uses a 32-octet key and a plaintext shorter than a block, so it covers
// the short S2V branch end to end.
func TestRFC5297A1(t *testing.T) {
	key := mustHex(t, "fffefdfcfbfaf9f8f7f6f5f4f3f2f1f0"+
		"f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff")
	ad := mustHex(t, "101112131415161718191a1b1c1d1e1f2021222324252627")
	plaintext := mustHex(t, "112233445566778899aabbccddee")
	want := mustHex(t, "85632d07c6e8f37f950acd320a2ecc93"+
		"40c02b9690c4dc04daef7f6afe5c")

	a, err := NewAESSIV(key)
	require.NoError(t, err)
	require.Equal(t, want, mustSeal(t, a, nil, plaintext, ad))

	opened, err := a.Open(nil, want, ad)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
}

// TestRFC5297A2 pins the nonce-based example from RFC 5297 Appendix A.2: three
// ordered components and a 47-octet plaintext, so it drives the xorend path
// with a partial final block.
func TestRFC5297A2(t *testing.T) {
	key := mustHex(t, "7f7e7d7c7b7a797877767574737271704041424344454647"+
		"48494a4b4c4d4e4f")
	ad1 := mustHex(t, "00112233445566778899aabbccddeeff"+
		"deaddadadeaddadaffeeddccbbaa9988"+
		"7766554433221100")
	ad2 := mustHex(t, "102030405060708090a0")
	nonce := mustHex(t, "09f911029d74e35bd84156c5635688c0")
	plaintext := mustHex(t, "7468697320697320736f6d6520706c61"+
		"696e7465787420746f20656e63727970"+
		"74207573696e67205349562d414553")
	want := mustHex(t, "7bdb6e3b432667eb06f4d14bff2fbd0f"+
		"cb900f2fddbe404326601965c889bf17"+
		"dba77ceb094fa663b7a3f748ba8af829"+
		"ea64ad544a272e9c485b62a3fd5c0d")

	a, err := NewAESSIV(key)
	require.NoError(t, err)
	// RFC 5297 places the nonce last, immediately before the message.
	got := mustSeal(t, a, nil, plaintext, ad1, ad2, nonce)
	require.Equal(t, want, got)

	opened, err := a.Open(nil, want, ad1, ad2, nonce)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
}

func TestNewAESSIVKeySizes(t *testing.T) {
	for _, n := range []int{KeySize, KeySize512} {
		_, err := NewAESSIV(make([]byte, n))
		require.NoError(t, err, "key size %d", n)
	}

	for _, n := range []int{0, 15, 16, 31, 33, 47, 48, 63, 65, 128} {
		_, err := NewAESSIV(make([]byte, n))
		require.ErrorIs(t, err, ErrKeySize, "key size %d", n)
	}
}

// TestTinkKnownAnswer512 checks vectors captured from tink's daead/subtle.AESSIV.
func TestTinkKnownAnswer512(t *testing.T) {
	a, err := NewAESSIV(make([]byte, KeySize512))
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		ad   []byte
		pt   []byte
		want string
	}{
		{"empty-ad-empty-pt", nil, nil, "7009249e60b613d224028adcc038d4ac"},
		{"ad-empty-pt", []byte("associated-data"), nil, "2d843364bf6958d9ea65ec6754f8481d"},
		{
			"ad-32byte-pt",
			[]byte("associated-data"),
			[]byte("plaintext-msg-32-bytes-long-xxx!"),
			"daec02b3076a9843466aff2cd869b39418baff9157c6a142023dafb90686902190fabb0c62052ff41fea3091a805660b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := mustHex(t, tc.want)
			require.Equal(t, want, mustSeal(t, a, nil, tc.pt, tc.ad))

			pt, err := a.Open(nil, want, tc.ad)
			require.NoError(t, err)
			require.True(t, bytes.Equal(tc.pt, pt))
		})
	}
}

// TestRoundTrip exercises plaintext lengths that straddle both s2v branches,
// against associated data and nonces of several lengths.
func TestRoundTrip(t *testing.T) {
	rng := newRNG(0x5297, 0x8915)
	a := newTestAESSIV(t, rng)
	for _, adLen := range []int{0, 1, BlockSize, 2*BlockSize + 7} {
		// A second component stands in for the RFC 5297 nonce slot.
		for _, extraLen := range []int{0, BlockSize} {
			ad, extra := randBytes(rng, adLen), randBytes(rng, extraLen)
			for ptLen := 0; ptLen <= 3*BlockSize+1; ptLen++ {
				plaintext := randBytes(rng, ptLen)
				sealed := mustSeal(t, a, nil, plaintext, ad, extra)
				require.Len(t, sealed, ptLen+BlockSize)

				opened, err := a.Open(nil, sealed, ad, extra)
				require.NoError(t, err, "adLen=%d extraLen=%d ptLen=%d", adLen, extraLen, ptLen)
				// bytes.Equal, not require.Equal: with a nil dst and an empty
				// plaintext the result is a nil slice, which is still correct.
				require.True(t, bytes.Equal(plaintext, opened),
					"adLen=%d extraLen=%d ptLen=%d", adLen, extraLen, ptLen)
			}
		}
	}
}

// ctrForkSizes straddle ctrBatchThreshold, where ctrCrypt switches from a
// per-block loop to crypto/cipher's batched CTR. Only one branch runs for any
// given size, so both sides must be covered explicitly.
var ctrForkSizes = []int{
	0, 1, BlockSize, ctrBatchThreshold - 1, ctrBatchThreshold,
	ctrBatchThreshold + 1, 2*ctrBatchThreshold + 7,
}

// TestCtrCryptMatchesStdlibCTR pins both ctrCrypt regimes against crypto/cipher
// at every size around the fork. The RFC 5297 vectors are 14 and 47 octets, so
// they only ever reach the per-block loop and cannot vouch for the other branch.
func TestCtrCryptMatchesStdlibCTR(t *testing.T) {
	rng := newRNG(131, 137)
	a := newTestAESSIV(t, rng)

	for _, size := range ctrForkSizes {
		data := randBytes(rng, size)

		var s scratch
		copy(s.siv[:], randBytes(rng, BlockSize))
		got := slices.Clone(data)
		a.ctrCrypt(&s, got)

		// Reference: crypto/cipher driven by the same masked counter block.
		iv := slices.Clone(s.siv[:])
		iv[8] &= 0x7f
		iv[12] &= 0x7f
		want := slices.Clone(data)
		cipher.NewCTR(a.ctr, iv).XORKeyStream(want, want)

		require.Equal(t, want, got, "size=%d", size)
	}
}

// TestRoundTripAcrossCTRThreshold seals and opens on both sides of the fork, so
// neither regime can regress unnoticed through the public API.
func TestRoundTripAcrossCTRThreshold(t *testing.T) {
	rng := newRNG(139, 149)
	a := newTestAESSIV(t, rng)
	ad := randBytes(rng, 24)

	for _, size := range ctrForkSizes {
		plaintext := randBytes(rng, size)
		sealed := mustSeal(t, a, nil, plaintext, ad)
		require.Len(t, sealed, size+BlockSize)

		opened, err := a.Open(nil, sealed, ad)
		require.NoError(t, err, "size=%d", size)
		require.True(t, bytes.Equal(plaintext, opened), "size=%d", size)
	}
}

// TestSealIsDeterministic confirms the defining property of the mode. NTS
// relies on it for cookies, whose unlinkability comes from a random associated
// data component rather than from the mode itself.
func TestSealIsDeterministic(t *testing.T) {
	rng := newRNG(1, 2)
	a := newTestAESSIV(t, rng)
	plaintext, ad := randBytes(rng, 64), randBytes(rng, 20)
	require.Equal(t, mustSeal(t, a, nil, plaintext, ad), mustSeal(t, a, nil, plaintext, ad))
}

// TestSealAppendsToDst checks the append contract: existing dst content is
// preserved and the sealed blob follows it.
func TestSealAppendsToDst(t *testing.T) {
	rng := newRNG(83, 89)
	a := newTestAESSIV(t, rng)
	plaintext, ad := randBytes(rng, 40), randBytes(rng, 8)

	prefix := []byte("keep-me")
	got := mustSeal(t, a, slices.Clone(prefix), plaintext, ad)
	require.Equal(t, prefix, got[:len(prefix)])
	require.Len(t, got, len(prefix)+len(plaintext)+BlockSize)

	opened, err := a.Open(nil, got[len(prefix):], ad)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
}

// TestSealInPlace covers the documented plaintext[:0] aliasing case, where the
// output overlaps the input shifted by the synthetic IV. Getting the copy order
// wrong here corrupts the plaintext before it is encrypted.
func TestSealInPlace(t *testing.T) {
	rng := newRNG(97, 101)
	a := newTestAESSIV(t, rng)
	ad := randBytes(rng, 12)

	for _, ptLen := range []int{0, 1, BlockSize, 2*BlockSize + 5, 100, ctrBatchThreshold, ctrBatchThreshold + 7} {
		plaintext := randBytes(rng, ptLen)
		want := mustSeal(t, a, nil, plaintext, ad)

		buf := make([]byte, ptLen, ptLen+BlockSize)
		copy(buf, plaintext)
		require.Equal(t, want, mustSeal(t, a, buf[:0], buf, ad), "ptLen=%d", ptLen)
	}
}

// TestOpenInPlace covers the matching ciphertext[:0] aliasing case, where the
// recovered plaintext is written over the synthetic IV it was verified against.
func TestOpenInPlace(t *testing.T) {
	rng := newRNG(103, 107)
	a := newTestAESSIV(t, rng)
	ad := randBytes(rng, 12)

	for _, ptLen := range []int{0, 1, BlockSize, 2*BlockSize + 5, 100, ctrBatchThreshold, ctrBatchThreshold + 7} {
		plaintext := randBytes(rng, ptLen)
		sealed := mustSeal(t, a, nil, plaintext, ad)

		buf := slices.Clone(sealed)
		opened, err := a.Open(buf[:0], buf, ad)
		require.NoError(t, err, "ptLen=%d", ptLen)
		require.Equal(t, plaintext, opened, "ptLen=%d", ptLen)
	}
}

// TestSealDoesNotMutateInput guards the caller's buffers when dst does not
// alias them.
func TestSealDoesNotMutateInput(t *testing.T) {
	rng := newRNG(23, 29)
	a := newTestAESSIV(t, rng)

	for _, ptLen := range []int{0, BlockSize - 1, BlockSize, 2*BlockSize + 3} {
		plaintext, ad, extra := randBytes(rng, ptLen), randBytes(rng, 24), randBytes(rng, BlockSize)
		ptCopy, adCopy, extraCopy := slices.Clone(plaintext), slices.Clone(ad), slices.Clone(extra)
		mustSeal(t, a, nil, plaintext, ad, extra)
		require.Equal(t, ptCopy, plaintext, "ptLen=%d", ptLen)
		require.Equal(t, adCopy, ad)
		require.Equal(t, extraCopy, extra)
	}
}

// TestOpenDoesNotMutateInput guards the same for the decrypt direction, where
// ctrCrypt masks two bits of a slice aliasing the ciphertext.
func TestOpenDoesNotMutateInput(t *testing.T) {
	rng := newRNG(41, 43)
	a := newTestAESSIV(t, rng)
	ad := randBytes(rng, 24)
	sealed := mustSeal(t, a, nil, randBytes(rng, 40), ad)

	sealedCopy, adCopy := slices.Clone(sealed), slices.Clone(ad)
	_, err := a.Open(nil, sealed, ad)
	require.NoError(t, err)
	require.Equal(t, sealedCopy, sealed)
	require.Equal(t, adCopy, ad)
}

// TestOpenRejectsAnyBitFlip flips every bit of every octet of a sealed blob and
// requires all of them to fail, covering the synthetic IV, the ciphertext, and
// the two IV bits ctrCrypt masks out.
func TestOpenRejectsAnyBitFlip(t *testing.T) {
	rng := newRNG(3, 5)
	a := newTestAESSIV(t, rng)
	ad := []byte("associated-data")
	sealed := mustSeal(t, a, nil, randBytes(rng, 48), ad)

	for i := range sealed {
		for bit := range 8 {
			bad := slices.Clone(sealed)
			bad[i] ^= 1 << bit
			_, err := a.Open(nil, bad, ad)
			require.ErrorIs(t, err, ErrVerify, "octet %d bit %d", i, bit)
		}
	}
}

// TestOpenRejectsWrongInputs checks that altered associated data, an altered
// component, a truncated blob or the wrong key all fail verification rather than
// returning garbage plaintext.
func TestOpenRejectsWrongInputs(t *testing.T) {
	rng := newRNG(7, 11)
	a := newTestAESSIV(t, rng)
	ad, extra := []byte("associated-data"), randBytes(rng, BlockSize)
	sealed := mustSeal(t, a, nil, randBytes(rng, 48), ad, extra)

	t.Run("associated data", func(t *testing.T) {
		_, err := a.Open(nil, sealed, []byte("associated-datb"), extra)
		require.ErrorIs(t, err, ErrVerify)
	})
	t.Run("altered trailing component", func(t *testing.T) {
		other := slices.Clone(extra)
		other[0] ^= 0x01
		_, err := a.Open(nil, sealed, ad, other)
		require.ErrorIs(t, err, ErrVerify)
	})
	t.Run("dropped trailing component", func(t *testing.T) {
		_, err := a.Open(nil, sealed, ad)
		require.ErrorIs(t, err, ErrVerify)
	})
	t.Run("truncated", func(t *testing.T) {
		_, err := a.Open(nil, sealed[:len(sealed)-1], ad, extra)
		require.ErrorIs(t, err, ErrVerify)
	})
	t.Run("wrong key", func(t *testing.T) {
		_, err := newTestAESSIV(t, rng).Open(nil, sealed, ad, extra)
		require.ErrorIs(t, err, ErrVerify)
	})
}

// TestOpenClearsDstOnFailure checks a failed Open leaves no plaintext behind.
// dst may alias the ciphertext, so returning early would hand the caller
// attacker-controlled data recovered from a forged blob.
func TestOpenClearsDstOnFailure(t *testing.T) {
	rng := newRNG(113, 127)
	a := newTestAESSIV(t, rng)
	ad := []byte("associated-data")
	plaintext := randBytes(rng, 64)
	sealed := mustSeal(t, a, nil, plaintext, ad)
	ptLen := len(sealed) - BlockSize

	t.Run("in place", func(t *testing.T) {
		// The ciphertext[:0] form, where dst aliases the input directly.
		bad := slices.Clone(sealed)
		bad[len(bad)-1] ^= 0x01
		_, err := a.Open(bad[:0], bad, ad)
		require.ErrorIs(t, err, ErrVerify)
		require.Equal(t, make([]byte, ptLen), bad[:ptLen],
			"forged blob must not leave plaintext in the caller's buffer")
	})

	t.Run("separate dst", func(t *testing.T) {
		bad := slices.Clone(sealed)
		bad[0] ^= 0x01
		dst := make([]byte, 0, ptLen)
		_, err := a.Open(dst, bad, ad)
		require.ErrorIs(t, err, ErrVerify)
		require.Equal(t, make([]byte, ptLen), dst[:ptLen])
	})

	t.Run("wrong associated data", func(t *testing.T) {
		buf := slices.Clone(sealed)
		_, err := a.Open(buf[:0], buf, []byte("associated-datb"))
		require.ErrorIs(t, err, ErrVerify)
		require.Equal(t, make([]byte, ptLen), buf[:ptLen])
	})
}

// TestOpenRejectsShortInput checks that input too small to hold a synthetic IV
// is rejected on framing grounds instead of indexing out of range.
func TestOpenRejectsShortInput(t *testing.T) {
	a := newTestAESSIV(t, newRNG(2, 3))
	for n := range BlockSize {
		_, err := a.Open(nil, make([]byte, n), nil)
		require.ErrorIs(t, err, ErrCiphertextTooShort, "len %d", n)
	}
	// One block is a valid sealing of the empty plaintext, so it must reach the
	// tag check rather than being rejected as too short.
	_, err := a.Open(nil, make([]byte, BlockSize), nil)
	require.ErrorIs(t, err, ErrVerify)
}

// TestComponentVectorIsOrdered checks the components form a vector rather than
// a concatenation, so a caller that flattens or reorders them cannot silently
// produce a blob that still verifies.
func TestComponentVectorIsOrdered(t *testing.T) {
	rng := newRNG(53, 59)
	a := newTestAESSIV(t, rng)
	plaintext := randBytes(rng, 40)
	alpha, beta := []byte("alpha"), []byte("beta")

	sealed := mustSeal(t, a, nil, plaintext, alpha, beta)

	_, err := a.Open(nil, sealed, beta, alpha)
	require.ErrorIs(t, err, ErrVerify, "component order must be significant")
	_, err = a.Open(nil, sealed, []byte("alphabeta"))
	require.ErrorIs(t, err, ErrVerify, "component split must be significant")
	_, err = a.Open(nil, sealed, alpha)
	require.ErrorIs(t, err, ErrVerify, "dropping a component must be significant")

	opened, err := a.Open(nil, sealed, alpha, beta)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
}

// TestEmptyComponentVector checks that no components at all round-trips, and is
// distinct from a single empty component: the first skips the S2V loop entirely
// while the second folds in CMAC of the empty string.
func TestEmptyComponentVector(t *testing.T) {
	rng := newRNG(61, 67)
	a := newTestAESSIV(t, rng)
	plaintext := randBytes(rng, 40)

	none := mustSeal(t, a, nil, plaintext)
	oneEmpty := mustSeal(t, a, nil, plaintext, []byte{})
	require.NotEqual(t, none, oneEmpty)

	// The exported API always contributes additionalData, so it matches the
	// single-empty-component form rather than the empty vector.
	require.Equal(t, oneEmpty, mustSeal(t, a, nil, plaintext, nil))

	opened, err := a.Open(nil, none)
	require.NoError(t, err)
	require.Equal(t, plaintext, opened)
}

// benchSizes span what NTS actually seals: a small authenticator body, a
// 64-octet cookie payload, and larger sizes that show where bulk throughput
// takes over from fixed per-call overhead.
var benchSizes = []int{16, 64, 100, 256, 1024}

func BenchmarkSeal(b *testing.B) {
	rng := newRNG(1, 1)
	a, err := NewAESSIV(randBytes(rng, KeySize))
	require.NoError(b, err)
	ad := randBytes(rng, 32)

	for _, size := range benchSizes {
		plaintext := randBytes(rng, size)
		dst := make([]byte, 0, size+BlockSize)
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				if _, err := a.Seal(dst[:0], plaintext, ad); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkOpen(b *testing.B) {
	rng := newRNG(1, 1)
	a, err := NewAESSIV(randBytes(rng, KeySize))
	require.NoError(b, err)
	ad := randBytes(rng, 32)

	for _, size := range benchSizes {
		sealed, err := a.Seal(nil, randBytes(rng, size), ad)
		require.NoError(b, err)
		dst := make([]byte, 0, size)
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for b.Loop() {
				if _, err := a.Open(dst[:0], sealed, ad); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkNewAESSIV(b *testing.B) {
	key := randBytes(newRNG(1, 1), KeySize)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewAESSIV(key); err != nil {
			b.Fatal(err)
		}
	}
}
