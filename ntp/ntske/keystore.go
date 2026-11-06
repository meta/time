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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/facebook/time/ntp/protocol"
	"github.com/facebook/time/ntp/protocol/nts"
	"github.com/facebook/time/ntp/protocol/nts/aessiv"
)

/*
Cookie keystore (RFC 8915 section 6).
An NTS cookie is server-opaque state: the client stores it verbatim and echoes
it back, and only the server can open it. It carries the two session keys
(C2S and S2C) negotiated during the NTS-KE handshake so the server does not
have to keep per-client state.
Wire format (chrony-compatible; see chrony's nts_ke_server.c):
	 0                   1                   2                   3
	 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
	+---------------------------------------------------------------+
	|                       Key ID (4 octets)                       |
	+---------------------------------------------------------------+
	|                                                               |
	~                        Nonce (16 octets)                      ~
	|                                                               |
	+---------------------------------------------------------------+
	|                                                               |
	~          AES-SIV ciphertext  =  tag(16) || (C2S || S2C)       ~
	|                                                               |
	+---------------------------------------------------------------+
- Key ID: big-endian identifier of the master key that sealed this cookie.
  It is carried in the clear so the server can pick the right key on open; it
  is not authenticated as associated data, but tampering with it selects a
  different (or unknown) master key and thus fails the AES-SIV tag check.
- Nonce: 16 random octets. The cookie is always sealed with AES-SIV-CMAC-512
  (RFC 5297), a deterministic AEAD. To keep cookies unlinkable across issuance
  we fold this random nonce into the SIV as associated data, so identical key
  material produces distinct cookies. This matches chrony, which passes the
  nonce as the SIV nonce component.
- Ciphertext: the AES-SIV output - the 16-octet synthetic IV (tag) followed
  by the encrypted C2S || S2C key material.
The *session* AEAD algorithm (the one the client will use for NTP, negotiated
via NTS-KE) is NOT stored in the cookie. It is inferred from the total cookie
length, because the C2S/S2C key length is fixed per algorithm:
	 68 octets  ->  AEAD_AES_128_GCM_SIV  (id 30, 16-octet session keys)
	100 octets  ->  AEAD_AES_SIV_CMAC_256 (id 15, 32-octet session keys)
	164 octets  ->  AEAD_AES_SIV_CMAC_512 (id 17, 64-octet session keys)
Note the distinction between the two AES-SIV-CMAC-512 uses here: the *keystore*
always seals cookies with it (a fixed 64-octet master key), independent of
whichever AEAD the *session* negotiated.
*/

// SharedTestMasterKey is a fixed 64-octet cookie master key used ONLY to bridge
// the gap until a Keychain-backed keystore lets every responder in the fleet
// share sealing keys. Seeding both the NTP responder and the standalone NTS-KE
// server with it (via InMemoryKeystoreOptions.InitialKey) lets cookies sealed
// by one open on the other.
//
// NOT for production: anyone holding these bytes can forge cookies. Delete this
// once the fleet-wide (Keychain) keystore lands.
var SharedTestMasterKey = []byte{
	0xad, 0x17, 0x97, 0x8b, 0x12, 0x2b, 0x0c, 0xc1, 0x66, 0x81, 0x04, 0x55,
	0xb4, 0xbb, 0x9a, 0xca, 0x0e, 0x85, 0x9b, 0xf0, 0x2d, 0x19, 0xe1, 0xdd,
	0xb8, 0x1a, 0x85, 0xdb, 0x41, 0xd7, 0x48, 0x5f, 0xe4, 0x4a, 0x27, 0x06,
	0xaa, 0x2d, 0x1b, 0x01, 0x7f, 0xab, 0x86, 0xaa, 0xf9, 0xe9, 0x01, 0xee,
	0x8b, 0x3c, 0x2d, 0x58, 0x90, 0xdd, 0xa4, 0xc3, 0x46, 0x63, 0xb8, 0xa4,
	0x2b, 0x55, 0xe5, 0x0c,
}

const (
	cookieKeyIDLen = 4  // big-endian master-key identifier
	cookieNonceLen = 16 // random per-cookie nonce, mixed into the SIV as AD
	sivTagLen      = 16 // AES-SIV synthetic IV prepended to the ciphertext
	// cookieOverhead is everything in a cookie that is not session key material:
	// Key ID + Nonce + SIV tag.
	cookieOverhead = cookieKeyIDLen + cookieNonceLen + sivTagLen
	// defaultMaxKeys is the master-key ring capacity: the current key plus this
	// many previous keys are retained so cookies issued just before a rotation
	// remain openable until they age out.
	defaultMaxKeys uint32 = 3
)

// MasterKeyLen returns the AES-SIV-CMAC-512 cookie master-key length.
func MasterKeyLen() int {
	return aessiv.KeySize512
}

// Sentinel errors. Compare with errors.Is. As in the nts package, the
// underlying AES-SIV error is surfaced as text on ErrCookieVerify but kept out
// of the errors.Is chain.
var (
	ErrCookieTooShort       = errors.New("ntske: cookie too short")
	ErrCookieMalformed      = errors.New("ntske: cookie malformed")
	ErrUnknownKeyID         = errors.New("ntske: unknown cookie key id")
	ErrUnsupportedAlgorithm = errors.New("ntske: unsupported aead algorithm")
	ErrKeyLength            = errors.New("ntske: session key length mismatch")
	ErrCookieVerify         = errors.New("ntske: cookie verification failed")
	ErrCookieExpired        = errors.New("ntske: cookie key expired")
	ErrCookieFuture         = errors.New("ntske: cookie key in the future")
)

// Keystore seals session keys into opaque cookies and opens them again. A
// single Keystore is shared across all NTS-KE and NTP request handlers, so
// implementations must be safe for concurrent use.
type Keystore interface {
	// SealCookie encrypts the C2S and S2C session keys for the negotiated
	// aeadID into a fresh cookie.
	SealCookie(aeadID protocol.AEADAlgorithm, c2s, s2c []byte) ([]byte, error)
	// OpenCookie decrypts a cookie, returning the negotiated session aeadID and
	// the C2S and S2C keys. The returned key slices are freshly allocated.
	OpenCookie(cookie []byte) (aeadID protocol.AEADAlgorithm, c2s, s2c []byte, err error)
}

type InMemoryKeystore struct {
	mu      sync.RWMutex
	ring    map[uint32][]byte // Key ID -> 64-octet master key
	order   []uint32          // Key ID order, older first
	current uint32            // Key ID used for sealing
	nextID  uint32            // Next key ID to use
	maxKeys uint32            // ring capacity (current + maxKeys-1 previous keys)
	rand    io.Reader         // nonce and master-key source; crypto/rand by default
}

// check that InMemoryKeystore implements the Keystore interface
var _ Keystore = (*InMemoryKeystore)(nil)

type InMemoryKeystoreOptions struct {
	MaxKeys uint32
	Rand    io.Reader
	// InitialKey, if set, seeds the ring with this exact master key instead of
	// generating a random one, letting separate processes share a sealing key.
	// Must be masterKeyLen octets.
	InitialKey []byte
}

// NewInMemoryKeystore returns a keystore seeded with one freshly generated
// master key, ready to seal and open cookies.
func NewInMemoryKeystore(opts InMemoryKeystoreOptions) (*InMemoryKeystore, error) {
	ks := &InMemoryKeystore{
		ring:    make(map[uint32][]byte),
		maxKeys: defaultMaxKeys,
		rand:    rand.Reader,
	}

	if opts.MaxKeys != 0 {
		ks.maxKeys = opts.MaxKeys
	}
	if opts.Rand != nil {
		ks.rand = opts.Rand
	}
	if len(opts.InitialKey) > 0 {
		if err := ks.seedKey(opts.InitialKey); err != nil {
			return nil, err
		}
		return ks, nil
	}
	if err := ks.Rotate(); err != nil {
		return nil, err
	}
	return ks, nil
}

// seedKey installs key as the initial sealing key (Key ID 1). Unlike Rotate it
// takes a caller-supplied key rather than generating a random one.
func (ks *InMemoryKeystore) seedKey(key []byte) error {
	if len(key) != MasterKeyLen() {
		return fmt.Errorf("ntske: initial key must be %d octets, got %d", MasterKeyLen(), len(key))
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.nextID++
	id := ks.nextID
	ks.ring[id] = bytes.Clone(key)
	ks.order = append(ks.order, id)
	ks.current = id
	return nil
}

// Rotate generates a new master key, makes it the sealing key, and ages out the
// oldest key if the ring is over capacity. Cookies sealed with a still-present
// key remain openable.
func (ks *InMemoryKeystore) Rotate() error {
	// generate 64 byte random master key
	key := make([]byte, MasterKeyLen())
	if _, err := io.ReadFull(ks.rand, key); err != nil {
		return fmt.Errorf("ntske: generate master key: %w", err)
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	// assign incremental ID
	ks.nextID++
	id := ks.nextID
	ks.ring[id] = key
	ks.order = append(ks.order, id)
	ks.current = id
	// check maxKey capacity and eventually delete older ones
	for len(ks.order) > int(ks.maxKeys) {
		oldest := ks.order[0]
		ks.order = ks.order[1:]
		delete(ks.ring, oldest)
	}
	return nil
}

// SealCookie encrypts c2s || s2c under the current master key via the shared
// envelope (see SealEnvelope). c2s and s2c must both have the key length required
// by aeadID.
func (ks *InMemoryKeystore) SealCookie(aeadID protocol.AEADAlgorithm, c2s, s2c []byte) ([]byte, error) {
	ks.mu.RLock()
	keyID := ks.current
	master := ks.ring[keyID]
	ks.mu.RUnlock()
	return SealEnvelope(ks.rand, keyID, master, aeadID, c2s, s2c)
}

// OpenCookie parses the cookie, resolves the master key by the cookie's Key ID
// from the ring, and decrypts it.
func (ks *InMemoryKeystore) OpenCookie(cookie []byte) (protocol.AEADAlgorithm, []byte, []byte, error) {
	c, err := ParseCookie(cookie)
	if err != nil {
		return 0, nil, nil, err
	}
	ks.mu.RLock()
	master, ok := ks.ring[c.KeyID]
	ks.mu.RUnlock()
	if !ok {
		return 0, nil, nil, fmt.Errorf("%w: keyID=%d", ErrUnknownKeyID, c.KeyID)
	}
	return c.Open(master)
}

// cookieSIV seals cookies deterministically, with the cookie nonce as the only
// S2V component; nts.NewAEAD would add a random second one.
func cookieSIV(sealingKey []byte) (*aessiv.AESSIV, error) {
	if len(sealingKey) != MasterKeyLen() {
		return nil, fmt.Errorf("ntske: master aead: %w: AES-SIV-CMAC-512 requires %d bytes, got %d",
			nts.ErrAEADKeySize, MasterKeyLen(), len(sealingKey))
	}
	return aessiv.NewAESSIV(sealingKey)
}

// SealEnvelope seals c2s || s2c under sealingKey and assembles the cookie
// [Key ID][Nonce][AES-SIV ciphertext]; nonce bytes are read from rnd.
func SealEnvelope(rnd io.Reader, keyID uint32, sealingKey []byte, aeadID protocol.AEADAlgorithm, c2s, s2c []byte) ([]byte, error) {
	keyLen, err := aeadIDToKeyLen(aeadID)
	if err != nil {
		return nil, err
	}
	if len(c2s) != keyLen || len(s2c) != keyLen {
		return nil, fmt.Errorf("%w: aead=%d wants %d-octet keys, got c2s=%d s2c=%d",
			ErrKeyLength, aeadID, keyLen, len(c2s), len(s2c))
	}
	nonce := make([]byte, cookieNonceLen)
	if _, err := io.ReadFull(rnd, nonce); err != nil {
		return nil, fmt.Errorf("ntske: read nonce: %w", err)
	}
	siv, err := cookieSIV(sealingKey)
	if err != nil {
		return nil, err
	}
	plaintext := make([]byte, 0, 2*keyLen)
	plaintext = append(plaintext, c2s...)
	plaintext = append(plaintext, s2c...)
	ct, err := siv.Seal(nil, plaintext, nonce)
	if err != nil {
		return nil, fmt.Errorf("ntske: seal cookie: %w", err)
	}
	out := make([]byte, cookieKeyIDLen+cookieNonceLen+len(ct))
	binary.BigEndian.PutUint32(out[0:cookieKeyIDLen], keyID)
	copy(out[cookieKeyIDLen:cookieKeyIDLen+cookieNonceLen], nonce)
	copy(out[cookieKeyIDLen+cookieNonceLen:], ct)
	return out, nil
}

// Cookie is a parsed cookie envelope: [Key ID][Nonce][AES-SIV ciphertext], its
// framing validated once at parse time. The nonce and ciphertext alias the input
// slice, so callers must not mutate the cookie while a Cookie references it.
type Cookie struct {
	KeyID  uint32                 // master-key identifier stamped in the clear
	AEADID protocol.AEADAlgorithm // session AEAD, inferred from the total length
	nonce  []byte
	ct     []byte
}

// ParseCookie validates a cookie's framing and splits it into its fields without
// decrypting it. The Key ID and the session AEAD (inferred from the total length)
// are read here, so both are available before the sealing key is resolved.
func ParseCookie(cookie []byte) (*Cookie, error) {
	if len(cookie) < cookieOverhead {
		return nil, fmt.Errorf("%w: len=%d, need at least %d", ErrCookieTooShort, len(cookie), cookieOverhead)
	}
	ptLen := len(cookie) - cookieOverhead
	if ptLen == 0 || ptLen%2 != 0 {
		return nil, fmt.Errorf("%w: key material length %d is not a positive even number",
			ErrCookieMalformed, ptLen)
	}
	aeadID, err := keyLenToAEADID(ptLen / 2)
	if err != nil {
		return nil, err
	}
	return &Cookie{
		KeyID:  binary.BigEndian.Uint32(cookie[:cookieKeyIDLen]),
		AEADID: aeadID,
		nonce:  cookie[cookieKeyIDLen : cookieKeyIDLen+cookieNonceLen],
		ct:     cookie[cookieKeyIDLen+cookieNonceLen:],
	}, nil
}

// Open decrypts the cookie with an already-resolved sealing key, returning the
// session AEAD and the C2S/S2C keys. The returned key slices are freshly allocated.
func (c *Cookie) Open(sealingKey []byte) (protocol.AEADAlgorithm, []byte, []byte, error) {
	keyLen, err := aeadIDToKeyLen(c.AEADID)
	if err != nil {
		return 0, nil, nil, err
	}
	siv, err := cookieSIV(sealingKey)
	if err != nil {
		return 0, nil, nil, err
	}
	pt, err := siv.Open(nil, c.ct, c.nonce)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w: %s", ErrCookieVerify, err.Error())
	}
	if len(pt) != 2*keyLen {
		return 0, nil, nil, fmt.Errorf("%w: decrypted %d octets, want %d",
			ErrCookieMalformed, len(pt), 2*keyLen)
	}
	return c.AEADID, bytes.Clone(pt[:keyLen]), bytes.Clone(pt[keyLen:]), nil
}

// keyLenToAEADID maps a per-direction session key length to its AEAD algorithm ID.
func keyLenToAEADID(keyLen int) (protocol.AEADAlgorithm, error) {
	switch keyLen {
	case 16:
		return protocol.AEADAES128GCMSIV, nil
	case 32:
		return protocol.AEADAESSIVCMAC256, nil
	case 64:
		return protocol.AEADAESSIVCMAC512, nil
	default:
		return 0, fmt.Errorf("%w: %d-octet session key", ErrUnsupportedAlgorithm, keyLen)
	}
}

// aeadIDToKeyLen maps an AEAD algorithm ID to its per-direction session key length.
func aeadIDToKeyLen(aeadID protocol.AEADAlgorithm) (int, error) {
	switch aeadID {
	case protocol.AEADAES128GCMSIV:
		return 16, nil
	case protocol.AEADAESSIVCMAC256:
		return 32, nil
	case protocol.AEADAESSIVCMAC512:
		return 64, nil
	default:
		return 0, fmt.Errorf("%w: aead id %d", ErrUnsupportedAlgorithm, aeadID)
	}
}
