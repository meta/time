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

package ntske

import (
	"bytes"
	"crypto/rand"
	"errors"
	"sync"
	"testing"

	"github.com/facebook/time/ntp/protocol"
	"github.com/facebook/time/ntp/protocol/nts"
	"github.com/stretchr/testify/require"
)

// sessionKeys returns deterministic but distinct C2S and S2C key material of
// the given per-direction length, so round-trip tests can assert exact bytes.
func sessionKeys(keyLen int) (c2s, s2c []byte) {
	c2s = make([]byte, keyLen)
	s2c = make([]byte, keyLen)
	for i := range c2s {
		c2s[i] = byte(i)
		s2c[i] = byte(0xFF - i)
	}
	return c2s, s2c
}

// TestSealOpenRoundTrip verifies that a cookie sealed for each supported AEAD
// has the expected wire length, that ParseCookie recovers the algorithm from
// length alone, and that OpenCookie returns the exact C2S and S2C keys.
func TestSealOpenRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		aeadID  protocol.AEADAlgorithm
		keyLen  int
		wantLen int
	}{
		{"AES-128-GCM-SIV", protocol.AEADAES128GCMSIV, 16, 68},
		{"AES-SIV-CMAC-256", protocol.AEADAESSIVCMAC256, 32, 100},
		{"AES-SIV-CMAC-512", protocol.AEADAESSIVCMAC512, 64, 164},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{})
			require.NoError(t, err)

			c2s, s2c := sessionKeys(tc.keyLen)
			cookie, err := ks.SealCookie(tc.aeadID, c2s, s2c)
			require.NoError(t, err)
			require.Len(t, cookie, tc.wantLen, "cookie must be exactly %d octets", tc.wantLen)

			// Length alone must reveal the negotiated algorithm.
			parsed, err := ParseCookie(cookie)
			require.NoError(t, err)
			require.Equal(t, tc.aeadID, parsed.AEADID)

			openID, gotC2S, gotS2C, err := ks.OpenCookie(cookie)
			require.NoError(t, err)
			require.Equal(t, tc.aeadID, openID)
			require.Equal(t, c2s, gotC2S)
			require.Equal(t, s2c, gotS2C)
		})
	}
}

// TestSealCookieRejectsBadInput checks that SealCookie rejects an unknown AEAD
// ID, keys whose length does not match the algorithm, and C2S/S2C slices whose
// lengths differ from each other.
func TestSealCookieRejectsBadInput(t *testing.T) {
	ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{})
	require.NoError(t, err)

	t.Run("unsupported algorithm", func(t *testing.T) {
		_, err := ks.SealCookie(9999, make([]byte, 16), make([]byte, 16))
		require.ErrorIs(t, err, ErrUnsupportedAlgorithm)
	})

	t.Run("key length mismatch", func(t *testing.T) {
		// GCM-SIV wants 16-octet keys; hand it 64-octet ones.
		_, err := ks.SealCookie(protocol.AEADAES128GCMSIV, make([]byte, 64), make([]byte, 64))
		require.ErrorIs(t, err, ErrKeyLength)
	})

	t.Run("mismatched c2s/s2c lengths", func(t *testing.T) {
		_, err := ks.SealCookie(protocol.AEADAES128GCMSIV, make([]byte, 16), make([]byte, 15))
		require.ErrorIs(t, err, ErrKeyLength)
	})
}

// TestOpenCookieTamperDetection confirms that the AES-SIV tag catches tampering:
// flipping a bit in either the ciphertext or the nonce (bound in as associated
// data) makes OpenCookie fail with ErrCookieVerify.
func TestOpenCookieTamperDetection(t *testing.T) {
	ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{})
	require.NoError(t, err)
	c2s, s2c := sessionKeys(64)

	t.Run("flip ciphertext byte", func(t *testing.T) {
		cookie, err := ks.SealCookie(protocol.AEADAESSIVCMAC512, c2s, s2c)
		require.NoError(t, err)
		// Last byte is inside the ciphertext region.
		cookie[len(cookie)-1] ^= 0x01
		_, _, _, err = ks.OpenCookie(cookie)
		require.ErrorIs(t, err, ErrCookieVerify)
	})

	t.Run("flip nonce byte", func(t *testing.T) {
		cookie, err := ks.SealCookie(protocol.AEADAESSIVCMAC512, c2s, s2c)
		require.NoError(t, err)
		// Nonce sits at [KeyID:4 .. 4+16).
		cookie[cookieKeyIDLen] ^= 0x01
		_, _, _, err = ks.OpenCookie(cookie)
		require.ErrorIs(t, err, ErrCookieVerify)
	})
}

// TestOpenCookieUnknownKeyID checks that OpenCookie returns ErrUnknownKeyID when
// the cookie's key ID does not correspond to any master key in the ring.
func TestOpenCookieUnknownKeyID(t *testing.T) {
	ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{})
	require.NoError(t, err)
	c2s, s2c := sessionKeys(16)
	cookie, err := ks.SealCookie(protocol.AEADAES128GCMSIV, c2s, s2c)
	require.NoError(t, err)

	// Key IDs start at 1, so 0 is guaranteed absent from the ring.
	cookie[0], cookie[1], cookie[2], cookie[3] = 0, 0, 0, 0
	_, _, _, err = ks.OpenCookie(cookie)
	require.ErrorIs(t, err, ErrUnknownKeyID)
}

// TestOpenCookieRejectsMalformed checks that OpenCookie rejects cookies that are
// shorter than the fixed overhead, carry an unsupported key-material length, or
// carry an odd (thus unsplittable) key-material length.
func TestOpenCookieRejectsMalformed(t *testing.T) {
	ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{})
	require.NoError(t, err)

	t.Run("too short", func(t *testing.T) {
		_, _, _, err := ks.OpenCookie(make([]byte, cookieOverhead-1))
		require.ErrorIs(t, err, ErrCookieTooShort)
	})

	t.Run("unsupported key material length", func(t *testing.T) {
		// cookieOverhead + 2 => 1-octet per-direction key, which no algorithm uses.
		_, _, _, err := ks.OpenCookie(make([]byte, cookieOverhead+2))
		require.ErrorIs(t, err, ErrUnsupportedAlgorithm)
	})

	t.Run("odd key material length", func(t *testing.T) {
		_, _, _, err := ks.OpenCookie(make([]byte, cookieOverhead+1))
		require.ErrorIs(t, err, ErrCookieMalformed)
	})
}

// TestRotationKeepsOldCookiesThenAgesOut verifies the master-key ring lifecycle:
// a cookie remains openable while its sealing key is still retained, but becomes
// unopenable (ErrUnknownKeyID) once that key is evicted by later rotations,
// while freshly sealed cookies keep working.
func TestRotationKeepsOldCookiesThenAgesOut(t *testing.T) {
	// Ring holds the current key plus one previous key.
	ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{MaxKeys: 2})
	require.NoError(t, err)
	c2s, s2c := sessionKeys(16)

	// Sealed with the seed key (ID 1).
	cookie, err := ks.SealCookie(protocol.AEADAES128GCMSIV, c2s, s2c)
	require.NoError(t, err)

	// One rotation: seed key (ID 1) still in the ring alongside the new key.
	require.NoError(t, ks.Rotate())
	mustOpen(t, ks, cookie, c2s, s2c)

	// Second rotation: seed key is evicted, so the old cookie is unopenable.
	require.NoError(t, ks.Rotate())
	_, _, _, err = ks.OpenCookie(cookie)
	require.ErrorIs(t, err, ErrUnknownKeyID)

	// A cookie sealed after rotation still opens fine.
	fresh, err := ks.SealCookie(protocol.AEADAES128GCMSIV, c2s, s2c)
	require.NoError(t, err)
	_, _, _, err = ks.OpenCookie(fresh)
	require.NoError(t, err)
}

// mustOpen opens cookie, requires success, and asserts the recovered C2S and
// S2C keys match wantC2S and wantS2C.
func mustOpen(t *testing.T, ks *InMemoryKeystore, cookie, wantC2S, wantS2C []byte) {
	t.Helper()
	_, c2s, s2c, err := ks.OpenCookie(cookie)
	require.NoError(t, err)
	require.True(t, bytes.Equal(wantC2S, c2s))
	require.True(t, bytes.Equal(wantS2C, s2c))
}

// TestConcurrentSealOpen exercises the keystore under contention. Run with -race.
func TestConcurrentSealOpen(t *testing.T) {
	ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{})
	require.NoError(t, err)

	const goroutines = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func(seed byte) {
			defer wg.Done()
			c2s := bytes.Repeat([]byte{seed}, 16)
			s2c := bytes.Repeat([]byte{seed ^ 0xFF}, 16)
			for range 100 {
				cookie, err := ks.SealCookie(protocol.AEADAES128GCMSIV, c2s, s2c)
				if err != nil {
					errs <- err
					return
				}
				_, gotC2S, gotS2C, err := ks.OpenCookie(cookie)
				if err != nil {
					errs <- err
					return
				}
				if !bytes.Equal(c2s, gotC2S) || !bytes.Equal(s2c, gotS2C) {
					errs <- errors.New("recovered keys do not match sealed keys")
					return
				}
			}
		}(byte(g))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// TestInitialKeyEnablesCrossKeystoreOpen verifies the shared-key bridge: two
// independent keystores seeded with the same InitialKey can open each other's
// cookies, which is what lets the standalone NTS-KE server and the NTP
// responder interoperate.
func TestInitialKeyEnablesCrossKeystoreOpen(t *testing.T) {
	sealer, err := NewInMemoryKeystore(InMemoryKeystoreOptions{InitialKey: SharedTestMasterKey})
	require.NoError(t, err)
	opener, err := NewInMemoryKeystore(InMemoryKeystoreOptions{InitialKey: SharedTestMasterKey})
	require.NoError(t, err)

	c2s, s2c := sessionKeys(64)
	cookie, err := sealer.SealCookie(protocol.AEADAESSIVCMAC512, c2s, s2c)
	require.NoError(t, err)

	_, gotC2S, gotS2C, err := opener.OpenCookie(cookie)
	require.NoError(t, err)
	require.Equal(t, c2s, gotC2S)
	require.Equal(t, s2c, gotS2C)
}

// TestSealingKeyWrongLength checks that sealing and opening with a master key
// of the wrong size fail with nts.ErrAEADKeySize.
func TestSealingKeyWrongLength(t *testing.T) {
	c2s, s2c := make([]byte, 64), make([]byte, 64)
	cookie, err := SealEnvelope(rand.Reader, 1, make([]byte, MasterKeyLen()), protocol.AEADAESSIVCMAC512, c2s, s2c)
	require.NoError(t, err)
	c, err := ParseCookie(cookie)
	require.NoError(t, err)
	for _, n := range []int{0, 32, MasterKeyLen() - 1, MasterKeyLen() + 1} {
		_, err := SealEnvelope(rand.Reader, 1, make([]byte, n), protocol.AEADAESSIVCMAC512, c2s, s2c)
		require.ErrorIs(t, err, nts.ErrAEADKeySize, "seal key %d", n)
		_, _, _, err = c.Open(make([]byte, n))
		require.ErrorIs(t, err, nts.ErrAEADKeySize, "open key %d", n)
	}
}

// TestInitialKeyWrongLength checks that a master key of the wrong size is
// rejected rather than silently producing a broken keystore.
func TestInitialKeyWrongLength(t *testing.T) {
	_, err := NewInMemoryKeystore(InMemoryKeystoreOptions{InitialKey: make([]byte, MasterKeyLen()-1)})
	require.Error(t, err)
}

// MasterKeyLen must equal the AES-SIV-CMAC-512 key length.
func TestMasterKeyLen(t *testing.T) {
	require.Equal(t, 64, MasterKeyLen())
}

// TestParseCookie verifies ParseCookie reads the Key ID and infers the session AEAD
// from length without decrypting, and rejects malformed framing before any sealing
// key is resolved (a rejected cookie yields no *Cookie, so Open can never run on it).
func TestParseCookie(t *testing.T) {
	t.Run("valid key id and aead", func(t *testing.T) {
		sealingKey := bytes.Repeat([]byte{0x5a}, MasterKeyLen())
		c2s, s2c := sessionKeys(16)
		cookie, err := SealEnvelope(rand.Reader, 7, sealingKey, protocol.AEADAES128GCMSIV, c2s, s2c)
		require.NoError(t, err)

		c, err := ParseCookie(cookie)
		require.NoError(t, err)
		require.Equal(t, uint32(7), c.KeyID)
		require.Equal(t, protocol.AEADAES128GCMSIV, c.AEADID)
	})

	t.Run("short cookie", func(t *testing.T) {
		c, err := ParseCookie(make([]byte, cookieOverhead-1))
		require.ErrorIs(t, err, ErrCookieTooShort)
		require.Nil(t, c)
	})

	t.Run("malformed rejected before open", func(t *testing.T) {
		// Odd key-material length fails framing, so no *Cookie is returned and
		// there is nothing to resolve a key for or Open.
		c, err := ParseCookie(make([]byte, cookieOverhead+1))
		require.ErrorIs(t, err, ErrCookieMalformed)
		require.Nil(t, c)
	})

	t.Run("exact overhead has no key material", func(t *testing.T) {
		// A cookie of exactly the overhead length carries zero key material.
		c, err := ParseCookie(make([]byte, cookieOverhead))
		require.ErrorIs(t, err, ErrCookieMalformed)
		require.Nil(t, c)
	})

	t.Run("unsupported key length", func(t *testing.T) {
		// cookieOverhead+2 => 1-octet per-direction key, which no AEAD uses.
		c, err := ParseCookie(make([]byte, cookieOverhead+2))
		require.ErrorIs(t, err, ErrUnsupportedAlgorithm)
		require.Nil(t, c)
	})
}

// TestCookieOpen verifies decrypting a parsed cookie with an already-resolved key
// returns the exact session keys and fails closed under a wrong key. Framing is
// validated at parse time, so Open never sees a malformed cookie.
func TestCookieOpen(t *testing.T) {
	sealingKey := bytes.Repeat([]byte{0x5a}, MasterKeyLen())
	c2s, s2c := sessionKeys(64)
	cookie, err := SealEnvelope(rand.Reader, 3, sealingKey, protocol.AEADAESSIVCMAC512, c2s, s2c)
	require.NoError(t, err)

	t.Run("round trip", func(t *testing.T) {
		c, err := ParseCookie(cookie)
		require.NoError(t, err)

		aeadID, gotC2S, gotS2C, err := c.Open(sealingKey)
		require.NoError(t, err)
		require.Equal(t, protocol.AEADAESSIVCMAC512, aeadID)
		require.Equal(t, c2s, gotC2S)
		require.Equal(t, s2c, gotS2C)
	})

	t.Run("wrong key", func(t *testing.T) {
		c, err := ParseCookie(cookie)
		require.NoError(t, err)

		wrongKey := bytes.Repeat([]byte{0x99}, MasterKeyLen())
		_, _, _, err = c.Open(wrongKey)
		require.ErrorIs(t, err, ErrCookieVerify)
	})
}

// TestOpenCookieValidatesFramingBeforeKeyLookup proves framing is checked before the
// key lookup: a malformed cookie fails with the framing sentinel, not ErrUnknownKeyID.
func TestOpenCookieValidatesFramingBeforeKeyLookup(t *testing.T) {
	ks, err := NewInMemoryKeystore(InMemoryKeystoreOptions{})
	require.NoError(t, err)

	t.Run("too short", func(t *testing.T) {
		_, _, _, err := ks.OpenCookie(make([]byte, cookieOverhead-1))
		require.ErrorIs(t, err, ErrCookieTooShort)
		require.NotErrorIs(t, err, ErrUnknownKeyID)
	})

	t.Run("odd key material", func(t *testing.T) {
		_, _, _, err := ks.OpenCookie(make([]byte, cookieOverhead+1))
		require.ErrorIs(t, err, ErrCookieMalformed)
		require.NotErrorIs(t, err, ErrUnknownKeyID)
	})
}
