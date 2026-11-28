package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"testing/cryptotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testBinding() *SealBinding {
	return &SealBinding{
		Network: "testnet", Subject: "lighthouse-geth-1", RequestID: "jti-1",
		Path: "/api/agent/v1/sync",
	}
}

// currentKey is the current key of keyring.
func currentKey(t *testing.T, keyring *Keyring) *SealKey {
	t.Helper()

	keys, err := keyring.Announced()
	require.NoError(t, err)

	return keys[0]
}

func TestSealRoundTrip(t *testing.T) {
	t.Parallel()

	keyring := NewKeyring()
	key := currentKey(t, keyring)
	known := KeyID{1, 2, 3, 4, 5, 6, 7, 8}
	body := []byte(`{"protocolVersion":1}`)

	wire, agentSeal, err := SealRequest(key, known, testBinding(), body)
	require.NoError(t, err)
	assert.Len(t, wire, sealHeaderSize+sealEncSize+len(body)+16)
	assert.NotContains(t, string(wire), "protocolVersion")

	req, err := ParseSealedRequest(wire)
	require.NoError(t, err)
	assert.Equal(t, key.ID, req.KeyID)
	assert.Equal(t, known, req.Known)

	opened, controllerSeal, err := keyring.Open(req, testBinding())
	require.NoError(t, err)
	assert.Equal(t, body, opened)

	answer := controllerSeal.Seal([]byte(`{"payload":"x"}`))
	assert.NotContains(t, string(answer), "payload")

	out, err := agentSeal.Open(answer)
	require.NoError(t, err)
	assert.JSONEq(t, `{"payload":"x"}`, string(out))
}

func TestSealedRequestRefused(t *testing.T) {
	t.Parallel()

	keyring := NewKeyring()
	key := currentKey(t, keyring)

	wire, _, err := SealRequest(key, KeyID{}, testBinding(), []byte("body"))
	require.NoError(t, err)

	flip := func(at int) []byte {
		out := append([]byte(nil), wire...)
		out[at] ^= 1

		return out
	}

	other := func(mod func(b *SealBinding)) *SealBinding {
		b := testBinding()
		mod(b)

		return b
	}

	tests := []struct {
		name    string
		wire    []byte
		binding *SealBinding
		want    error
	}{
		{name: "known key id altered", wire: flip(1 + KeyIDSize), want: ErrSealed},
		{name: "encapsulation altered", wire: flip(sealHeaderSize + 5), want: ErrSealed},
		{name: "X25519 share altered", wire: flip(sealHeaderSize + sealEncSize - 1), want: ErrSealed},
		{name: "ciphertext altered", wire: flip(len(wire) - 1), want: ErrSealed},
		{name: "truncated", wire: wire[:sealHeaderSize+sealEncSize-1], want: ErrSealed},
		{name: "unknown version", wire: append([]byte{2}, wire[1:]...), want: ErrSealed},
		{name: "key id altered", wire: flip(1), want: ErrUnknownSealKey},
		{
			name: "another host", wire: wire, want: ErrSealed,
			binding: other(func(b *SealBinding) { b.Subject = "prysm-besu-1" }),
		},
		{
			name: "another network", wire: wire, want: ErrSealed,
			binding: other(func(b *SealBinding) { b.Network = "other" }),
		},
		{
			name: "another request", wire: wire, want: ErrSealed,
			binding: other(func(b *SealBinding) { b.RequestID = "jti-2" }),
		},
		{
			name: "another path", wire: wire, want: ErrSealed,
			binding: other(func(b *SealBinding) { b.Path = "/api/agent/v1/inventory" }),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			binding := tt.binding
			if binding == nil {
				binding = testBinding()
			}

			req, err := ParseSealedRequest(tt.wire)
			if err == nil {
				_, _, err = keyring.Open(req, binding)
			}

			require.ErrorIs(t, err, tt.want)
		})
	}

	t.Run("a key of another keyring", func(t *testing.T) {
		t.Parallel()

		req, err := ParseSealedRequest(wire)
		require.NoError(t, err)

		_, _, err = NewKeyring().Open(req, testBinding())
		require.ErrorIs(t, err, ErrUnknownSealKey)
	})
}

func TestSealedAnswerRefused(t *testing.T) {
	t.Parallel()

	keyring := NewKeyring()
	key := currentKey(t, keyring)

	seal := func() (agent, controller *AnswerSeal) {
		wire, agentSeal, err := SealRequest(key, KeyID{}, testBinding(), []byte("body"))
		require.NoError(t, err)

		req, err := ParseSealedRequest(wire)
		require.NoError(t, err)

		_, controllerSeal, err := keyring.Open(req, testBinding())
		require.NoError(t, err)

		return agentSeal, controllerSeal
	}

	agentSeal, controllerSeal := seal()
	answer := controllerSeal.Seal([]byte("answer"))

	tampered := append([]byte(nil), answer...)
	tampered[len(tampered)-1] ^= 1

	_, err := agentSeal.Open(tampered)
	require.ErrorIs(t, err, ErrSealed)

	_, err = agentSeal.Open(answer[:5])
	require.ErrorIs(t, err, ErrSealed)

	otherAgent, _ := seal()

	_, err = otherAgent.Open(answer)
	require.ErrorIs(t, err, ErrSealed, "the answer to one request opens for no other")
}

// The suite is HPKE base mode with X-Wing, HKDF-SHA256 and AES-256-GCM: the
// answer key is recomputed here from the X-Wing combiner and the RFC 9180
// key schedule, from the parts of a hybrid key built by hand.
func TestSealIsXWingHPKE(t *testing.T) {
	t.Parallel()

	kem, kdf, aead := sealSuite()
	require.Equal(t, uint16(0x647a), kem.ID(), "MLKEM768-X25519")
	require.Equal(t, uint16(0x0001), kdf.ID(), "HKDF-SHA256")
	require.Equal(t, uint16(0x0002), aead.ID(), "AES-256-GCM")

	dk, err := mlkem.GenerateKey768()
	require.NoError(t, err)

	xk, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)

	priv, err := hpke.NewHybridPrivateKey(dk, xk)
	require.NoError(t, err)

	pubBytes := priv.PublicKey().Bytes()
	require.Equal(t, append(dk.EncapsulationKey().Bytes(), xk.PublicKey().Bytes()...), pubBytes)

	key, err := NewSealKey(pubBytes, time.Time{})
	require.NoError(t, err)

	b := testBinding()

	wire, agentSeal, err := SealRequest(key, KeyID{}, b, []byte("body"))
	require.NoError(t, err)

	req, err := ParseSealedRequest(wire)
	require.NoError(t, err)

	// X-Wing: SHA3-256(ss_M || ss_X || ct_X || pk_X || label).
	ctM, ctX := req.enc[:mlkem.CiphertextSize768], req.enc[mlkem.CiphertextSize768:]

	ssM, err := dk.Decapsulate(ctM)
	require.NoError(t, err)

	peer, err := ecdh.X25519().NewPublicKey(ctX)
	require.NoError(t, err)

	ssX, err := xk.ECDH(peer)
	require.NoError(t, err)

	combined := make([]byte, 0, 32*4+6)
	combined = append(combined, ssM...)
	combined = append(combined, ssX...)
	combined = append(combined, ctX...)
	combined = append(combined, xk.PublicKey().Bytes()...)
	combined = append(combined, `\.//^\`...)
	shared := sha3.Sum256(combined)

	// RFC 9180 key schedule, mode_base, then the exporter.
	suite := []byte("HPKE")
	suite = binary.BigEndian.AppendUint16(suite, kem.ID())
	suite = binary.BigEndian.AppendUint16(suite, kdf.ID())
	suite = binary.BigEndian.AppendUint16(suite, aead.ID())

	extract := func(salt []byte, label string, ikm []byte) []byte {
		in := append(append(append([]byte("HPKE-v1"), suite...), label...), ikm...)

		out, xerr := hkdf.Extract(sha256.New, in, salt)
		require.NoError(t, xerr)

		return out
	}
	expand := func(prk []byte, label string, info []byte, n int) []byte {
		in := binary.BigEndian.AppendUint16(nil, uint16(n))
		in = append(append(append(append(in, "HPKE-v1"...), suite...), label...), info...)

		out, xerr := hkdf.Expand(sha256.New, prk, string(in), n)
		require.NoError(t, xerr)

		return out
	}

	ksc := append([]byte{0}, extract(nil, "psk_id_hash", nil)...)
	ksc = append(ksc, extract(nil, "info_hash", []byte(sealInfo))...)
	secret := extract(shared[:], "secret", nil)
	exporter := expand(secret, "exp", ksc, 32)
	answerKey := expand(exporter, "sec", []byte(exportAnswer), 44)

	block, err := aes.NewCipher(answerKey[:32])
	require.NoError(t, err)

	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)

	answer := gcm.Seal([]byte{sealVersion}, answerKey[32:], []byte("answer"),
		b.aad(labelAnswer, req.header))

	out, err := agentSeal.Open(answer)
	require.NoError(t, err)
	assert.Equal(t, "answer", string(out))
}

// The encoding is pinned: a fixed key and fixed randomness give these bytes.
func TestSealKnownAnswer(t *testing.T) {
	cryptotest.SetGlobalRandom(t, 1)

	kem, _, _ := sealSuite()

	priv, err := kem.DeriveKeyPair([]byte("upgradoor seal known answer ikm, 32+ bytes"))
	require.NoError(t, err)

	key, err := NewSealKey(priv.PublicKey().Bytes(), time.Time{})
	require.NoError(t, err)

	wire, agentSeal, err := SealRequest(key, KeyID{9, 9, 9, 9, 9, 9, 9, 9}, testBinding(),
		[]byte(`{"protocolVersion":1}`))
	require.NoError(t, err)

	sum := sha256.Sum256(wire)
	answer := agentSeal.Seal([]byte(`{"payload":"x"}`))

	assert.Equal(t, "2a9c97fae38ae9ba", key.ID.String())
	assert.Equal(t, "594427ca7a1b39a3b6be0244cbe464b35e869ce2cbb52c868741c4becf50ef0d",
		hex.EncodeToString(sum[:]))
	assert.Equal(t, "01d68ffe790f3bd04bea011a1110a0868ee0d8613690da00e74cbcb3b19c6dad",
		hex.EncodeToString(answer))
}
