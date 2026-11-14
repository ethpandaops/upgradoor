// Package identity implements the upgradoor host identity scheme: key
// derivation from a per-devnet seed, the deterministic host certificate and
// the per-request proof of possession.
//
// The same derivation is implemented in Python by the Ansible filter plugin
// (ansible-collection-general, plugins/filter/upgradoor.py). Both sides must
// produce byte-identical output; testdata/vectors.json is the shared truth.
package identity

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	// HKDFSalt is the fixed HKDF salt of every upgradoor derivation.
	HKDFSalt = "upgradoor-v1"

	// MinSeedLength is the minimum accepted seed length in bytes.
	MinSeedLength = 32

	infoSignerPrefix = "upgradoor/signer/"
	infoHostPrefix   = "upgradoor/host/"
)

var (
	// ErrSeedTooShort is returned when the seed is shorter than MinSeedLength.
	ErrSeedTooShort = errors.New("identity: seed shorter than 32 bytes")
	// ErrInvalidName is returned for network, host or role names outside the
	// allowed character set.
	ErrInvalidName = errors.New("identity: invalid name")
	// ErrInvalidKey is returned for malformed encoded keys.
	ErrInvalidKey = errors.New("identity: invalid key encoding")
)

// nameRe is the character set allowed for network names, hostnames and roles.
// It excludes '/' (the HKDF info separator) and everything that would need
// JSON escaping, which keeps the certificate encoding trivially canonical.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// ValidateName reports whether s is usable as a network, host or role name.
func ValidateName(s string) error {
	if !nameRe.MatchString(s) {
		return fmt.Errorf("%w: %q", ErrInvalidName, s)
	}

	return nil
}

// DeriveSignerKey derives the per-network certificate signer key.
//
// The seed is used as HKDF input keying material exactly as given: the raw
// bytes of the sops value, no decoding and no trimming.
func DeriveSignerKey(seed []byte, network string) (ed25519.PrivateKey, error) {
	if err := ValidateName(network); err != nil {
		return nil, err
	}

	return derive(seed, infoSignerPrefix+network)
}

// DeriveHostKey derives the key of one host. generation rotates a single
// leaked host key without touching the seed.
func DeriveHostKey(
	seed []byte, network, hostname string, generation uint64,
) (ed25519.PrivateKey, error) {
	if err := ValidateName(network); err != nil {
		return nil, err
	}

	if err := ValidateName(hostname); err != nil {
		return nil, err
	}

	info := infoHostPrefix + network + "/" + hostname + "/" +
		strconv.FormatUint(generation, 10)

	return derive(seed, info)
}

func derive(seed []byte, info string) (ed25519.PrivateKey, error) {
	if len(seed) < MinSeedLength {
		return nil, ErrSeedTooShort
	}

	okm, err := hkdf.Key(sha256.New, seed, []byte(HKDFSalt), info, ed25519.SeedSize)
	if err != nil {
		return nil, fmt.Errorf("identity: hkdf: %w", err)
	}

	return ed25519.NewKeyFromSeed(okm), nil
}

// EncodePrivateKey encodes the 32-byte Ed25519 seed of key as unpadded
// base64url. This is the content of /etc/upgradoor/host.key.
func EncodePrivateKey(key ed25519.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key.Seed())
}

// DecodePrivateKey parses the output of EncodePrivateKey. Surrounding
// whitespace is ignored.
func DecodePrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, ErrInvalidKey
	}

	return ed25519.NewKeyFromSeed(raw), nil
}

// EncodePublicKey encodes a public key as unpadded base64url, the same form
// used for the JWK "x" member and the controller's signerPublicKey setting.
func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key)
}

// DecodePublicKey parses the output of EncodePublicKey. Surrounding
// whitespace is ignored.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, ErrInvalidKey
	}

	return ed25519.PublicKey(raw), nil
}

// PublicKey returns the public half of key.
func PublicKey(key ed25519.PrivateKey) ed25519.PublicKey {
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		// ed25519.PrivateKey.Public always returns ed25519.PublicKey.
		panic("identity: unexpected public key type")
	}

	return pub
}
