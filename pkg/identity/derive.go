// Package identity implements the upgradoor identity scheme: key derivation,
// the deterministic host certificate, the per-request proof of possession
// with which a host authenticates to the controller, and the answer
// signature with which the controller authenticates to the host.
//
// There are two seeds, and they are different secrets. The devnet seed, one
// per devnet, yields the signer key and the host keys of that devnet. The
// controller seed, one per group of devnets (one devnet repository), yields
// the controller key of each devnet of the group. A function takes the one
// or the other, never both.
//
// Keys and signatures are ML-DSA-44 (FIPS 204), a post-quantum scheme.
//
// The same derivation is implemented in Python by the Ansible filter plugin
// (ansible-collection-general, plugins/filter/upgradoor.py). Keys and the
// certificate's header and claims must be byte-identical on both sides;
// signatures are deterministic here and randomized there, so they are
// verified across implementations instead. testdata/vectors.json is the
// shared truth.
package identity

import (
	"crypto/hkdf"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
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
	// ErrNoSeed is returned when a seed file cannot be read.
	ErrNoSeed = errors.New("identity: cannot read seed file")
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

// ReadSeedFile reads a seed from a file: the devnet seed or the controller
// seed. The content is the seed as raw bytes, exactly as stored, without
// decoding; only one trailing newline (LF or CRLF), which editors and "echo"
// add, is dropped. The error never carries the content.
func ReadSeedFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoSeed, err)
	}

	seed := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(seed) < MinSeedLength {
		return nil, fmt.Errorf("%s: %w", path, ErrSeedTooShort)
	}

	return []byte(seed), nil
}

// DeriveSignerKey derives the per-network certificate signer key.
//
// The seed is used as HKDF input keying material exactly as given: the raw
// bytes of the sops value, no decoding and no trimming.
func DeriveSignerKey(seed []byte, network string) (*mldsa.PrivateKey, error) {
	if err := ValidateName(network); err != nil {
		return nil, err
	}

	return derive(seed, infoSignerPrefix+network)
}

// DeriveHostKey derives the key of one host. generation rotates a single
// leaked host key without touching the seed.
func DeriveHostKey(
	seed []byte, network, hostname string, generation uint64,
) (*mldsa.PrivateKey, error) {
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

func derive(seed []byte, info string) (*mldsa.PrivateKey, error) {
	if len(seed) < MinSeedLength {
		return nil, ErrSeedTooShort
	}

	okm, err := hkdf.Key(sha256.New, seed, []byte(HKDFSalt), info, mldsa.PrivateKeySize)
	if err != nil {
		return nil, fmt.Errorf("identity: hkdf: %w", err)
	}

	key, err := mldsa.NewPrivateKey(params(), okm)
	if err != nil {
		return nil, fmt.Errorf("identity: ml-dsa key: %w", err)
	}

	return key, nil
}

// EncodePrivateKey encodes the 32-byte ML-DSA seed of key as unpadded
// base64url. This is the content of /etc/upgradoor/host.key and of the
// controller's key file.
func EncodePrivateKey(key *mldsa.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key.Bytes())
}

// DecodePrivateKey parses the output of EncodePrivateKey. Surrounding
// whitespace is ignored.
func DecodePrivateKey(s string) (*mldsa.PrivateKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != mldsa.PrivateKeySize {
		return nil, ErrInvalidKey
	}

	key, err := mldsa.NewPrivateKey(params(), raw)
	if err != nil {
		return nil, ErrInvalidKey
	}

	return key, nil
}

// EncodePublicKey encodes a public key as unpadded base64url, the same form
// used for the JWK "pub" member and the controller's signerPublicKey setting.
func EncodePublicKey(key *mldsa.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(key.Bytes())
}

// DecodePublicKey parses the output of EncodePublicKey. Surrounding
// whitespace is ignored.
func DecodePublicKey(s string) (*mldsa.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != params().PublicKeySize() {
		return nil, ErrInvalidKey
	}

	key, err := mldsa.NewPublicKey(params(), raw)
	if err != nil {
		return nil, ErrInvalidKey
	}

	return key, nil
}

// PublicKey returns the public half of key.
func PublicKey(key *mldsa.PrivateKey) *mldsa.PublicKey {
	return key.PublicKey()
}

// Algorithm is the signature algorithm of every upgradoor key: ML-DSA-44
// (FIPS 204), a post-quantum scheme. It is also the JOSE "alg" value.
const Algorithm = "ML-DSA-44"

func params() mldsa.Parameters { return mldsa.MLDSA44() }
