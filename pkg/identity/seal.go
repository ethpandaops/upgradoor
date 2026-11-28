package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Sealed bodies are single-shot HPKE (RFC 9180, base mode) with the
// MLKEM768-X25519 KEM of draft-ietf-hpke-pq, a.k.a. X-Wing
// (draft-connolly-cfrg-xwing-kem): its combiner is
// SHA3-256(ss_MLKEM || ss_X25519 || ct_X25519 || pk_X25519 || label).
// KDF HKDF-SHA256, AEAD AES-256-GCM. The request is the HPKE context's one
// Seal; the answer key and nonce are the context's export, so both
// directions are bound to the per-request encapsulation.
//
// A sealed request body is
//
//	version (1) | key id (8) | known key id (8) | enc (1120) | ciphertext
//
// and a sealed answer is
//
//	version (1) | ciphertext
//
// The first 17 bytes of the request are its header; it is part of the
// additional data of both directions.
const (
	sealVersion = 1

	// KeyIDSize is the length of a KeyID.
	KeyIDSize = 8

	sealHeaderSize = 1 + 2*KeyIDSize
	// sealEncSize is the size of an MLKEM768-X25519 encapsulation.
	sealEncSize = 1088 + 32

	sealInfo     = "upgradoor/seal/v1"
	exportAnswer = "upgradoor/seal/answer"

	labelRequest = "request"
	labelAnswer  = "answer"
)

var (
	// ErrSealed is returned for a sealed body that is malformed or does not
	// decrypt under its key and binding.
	ErrSealed = errors.New("identity: sealed body refused")
	// ErrUnknownSealKey is returned for a sealed request to a key the
	// controller does not hold (any more).
	ErrUnknownSealKey = errors.New("identity: sealed to an unknown key")
)

func sealSuite() (hpke.KEM, hpke.KDF, hpke.AEAD) {
	return hpke.MLKEM768X25519(), hpke.HKDFSHA256(), hpke.AES256GCM()
}

// KeyID names an encryption key: the first bytes of the SHA-256 of its
// public key.
type KeyID [KeyIDSize]byte

// String is the lower-case hex form of id.
func (id KeyID) String() string { return hex.EncodeToString(id[:]) }

// IsZero reports whether id names no key.
func (id KeyID) IsZero() bool { return id == KeyID{} }

func keyIDOf(pub []byte) KeyID {
	sum := sha256.Sum256(pub)

	var id KeyID

	copy(id[:], sum[:])

	return id
}

// SealKey is a public encryption key of the controller.
type SealKey struct {
	// ID names the key.
	ID KeyID
	// ActiveFrom is when the controller makes the key its current one.
	ActiveFrom time.Time

	key hpke.PublicKey
}

// NewSealKey parses the bytes of an encryption public key.
func NewSealKey(raw []byte, activeFrom time.Time) (*SealKey, error) {
	kem, _, _ := sealSuite()

	key, err := kem.NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: encryption key: %w", ErrInvalidKey, err)
	}

	return &SealKey{ID: keyIDOf(raw), ActiveFrom: activeFrom, key: key}, nil
}

// Bytes returns the encoded public key.
func (k *SealKey) Bytes() []byte { return k.key.Bytes() }

// SealBinding is what a sealed body is bound to besides its key: a body
// sealed for one network, host, request id or path opens for no other.
type SealBinding struct {
	Network string
	// Subject is the host, the subject of its certificate.
	Subject string
	// RequestID is the jti of the request's proof.
	RequestID string
	// Path is the endpoint path, without the controller's base URL.
	Path string
}

// aad is the additional data of one direction.
func (b *SealBinding) aad(label string, header []byte) []byte {
	fields := [][]byte{
		[]byte(label), []byte(b.Network), []byte(b.Subject), []byte(b.RequestID),
		[]byte(b.Path), header,
	}

	size := 0
	for _, f := range fields {
		size += 4 + len(f)
	}

	out := make([]byte, 0, size)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}

	return out
}

// SealRequest encrypts body, the request b describes, to key. known names
// another key the sender holds (zero for none), which tells the controller
// whether to send its keys. It returns the wire body and the AnswerSeal
// that opens the answer.
func SealRequest(
	key *SealKey, known KeyID, b *SealBinding, body []byte,
) ([]byte, *AnswerSeal, error) {
	_, kdf, aead := sealSuite()

	enc, sender, err := hpke.NewSender(key.key, kdf, aead, []byte(sealInfo))
	if err != nil {
		return nil, nil, fmt.Errorf("identity: seal: %w", err)
	}

	if len(enc) != sealEncSize {
		return nil, nil, fmt.Errorf("identity: seal: encapsulation of %d bytes", len(enc))
	}

	header := make([]byte, 0, sealHeaderSize)
	header = append(header, sealVersion)
	header = append(header, key.ID[:]...)
	header = append(header, known[:]...)

	ct, err := sender.Seal(b.aad(labelRequest, header), body)
	if err != nil {
		return nil, nil, fmt.Errorf("identity: seal: %w", err)
	}

	answer, err := newAnswerSeal(sender.Export, b, header)
	if err != nil {
		return nil, nil, err
	}

	wire := make([]byte, 0, len(header)+len(enc)+len(ct))
	wire = append(wire, header...)
	wire = append(wire, enc...)
	wire = append(wire, ct...)

	return wire, answer, nil
}

// SealedRequest is a parsed sealed request body, not yet opened.
type SealedRequest struct {
	// KeyID is the key it is sealed to.
	KeyID KeyID
	// Known is another key the sender holds; zero for none.
	Known KeyID

	header []byte
	enc    []byte
	ct     []byte
}

// ParseSealedRequest splits a sealed request body. It decrypts nothing.
func ParseSealedRequest(wire []byte) (*SealedRequest, error) {
	if len(wire) < sealHeaderSize+sealEncSize || wire[0] != sealVersion {
		return nil, fmt.Errorf("%w: malformed", ErrSealed)
	}

	req := &SealedRequest{
		header: wire[:sealHeaderSize],
		enc:    wire[sealHeaderSize : sealHeaderSize+sealEncSize],
		ct:     wire[sealHeaderSize+sealEncSize:],
	}

	copy(req.KeyID[:], wire[1:1+KeyIDSize])
	copy(req.Known[:], wire[1+KeyIDSize:sealHeaderSize])

	return req, nil
}

// openSealed decrypts req with priv under b.
func openSealed(
	priv hpke.PrivateKey, req *SealedRequest, b *SealBinding,
) ([]byte, *AnswerSeal, error) {
	_, kdf, aead := sealSuite()

	recipient, err := hpke.NewRecipient(req.enc, priv, kdf, aead, []byte(sealInfo))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrSealed, err)
	}

	body, err := recipient.Open(b.aad(labelRequest, req.header), req.ct)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: it does not decrypt for this host, request and key", ErrSealed)
	}

	answer, err := newAnswerSeal(recipient.Export, b, req.header)
	if err != nil {
		return nil, nil, err
	}

	return body, answer, nil
}

// AnswerSeal encrypts and decrypts the one answer to a sealed request.
type AnswerSeal struct {
	aead  cipher.AEAD
	nonce []byte
	aad   []byte
}

// newAnswerSeal derives the answer key and nonce from the HPKE context.
// Each request has its own encapsulation, so a key seals exactly one
// answer and the fixed nonce is never reused.
func newAnswerSeal(
	export func(string, int) ([]byte, error), b *SealBinding, header []byte,
) (*AnswerSeal, error) {
	secret, err := export(exportAnswer, 32+12)
	if err != nil {
		return nil, fmt.Errorf("identity: answer key: %w", err)
	}

	block, err := aes.NewCipher(secret[:32])
	if err != nil {
		return nil, fmt.Errorf("identity: answer key: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("identity: answer key: %w", err)
	}

	return &AnswerSeal{aead: aead, nonce: secret[32:], aad: b.aad(labelAnswer, header)}, nil
}

// Seal encrypts answer.
func (a *AnswerSeal) Seal(answer []byte) []byte {
	out := make([]byte, 1, 1+len(answer)+a.aead.Overhead())
	out[0] = sealVersion

	return a.aead.Seal(out, a.nonce, answer, a.aad)
}

// Open decrypts a sealed answer.
func (a *AnswerSeal) Open(wire []byte) ([]byte, error) {
	if len(wire) < 1+a.aead.Overhead() || wire[0] != sealVersion {
		return nil, fmt.Errorf("%w: malformed answer", ErrSealed)
	}

	out, err := a.aead.Open(nil, a.nonce, wire[1:], a.aad)
	if err != nil {
		return nil, fmt.Errorf("%w: the answer does not decrypt with this request's key", ErrSealed)
	}

	return out, nil
}
