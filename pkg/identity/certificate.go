package identity

import (
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	// Issuer is the fixed "iss" claim of every host certificate.
	Issuer = "upgradoor-signer"

	// certHeader is the fixed JOSE header of a host certificate.
	certHeader = `{"alg":"` + Algorithm + `","typ":"JWT"}`
	// proofHeader is the fixed JOSE header of a request proof.
	proofHeader = `{"alg":"` + Algorithm + `","typ":"upgradoor-proof+jwt"}`

	// jwkKeyType is the JWK key type of ML-DSA keys ("algorithm key pair").
	jwkKeyType = "AKP"
)

var (
	// ErrBadCertificate is returned when a certificate is malformed, carries
	// a bad signature or invalid claims.
	ErrBadCertificate = errors.New("identity: bad certificate")
	// ErrWrongNetwork is returned when a certificate is valid but issued for
	// another network.
	ErrWrongNetwork = errors.New("identity: certificate is for another network")
)

// ValidatorRange is the half-open range [Start, End) of beacon validator
// indices operated by a host. It is not part of the certificate: a host
// reports its range itself (protocol.Validators), and the controller keeps
// the range it counts for the host in this form.
type ValidatorRange struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

// Count returns the number of validators in the range.
func (r *ValidatorRange) Count() uint64 {
	if r == nil || r.End <= r.Start {
		return 0
	}

	return r.End - r.Start
}

// JWK is the ML-DSA public key in JWK form.
type JWK struct {
	Kty string `json:"kty"`
	Alg string `json:"alg"`
	Pub string `json:"pub"`
}

// Confirmation is the "cnf" claim binding the certificate to the host key.
type Confirmation struct {
	JWK JWK `json:"jwk"`
}

// Claims are the claims of a host certificate. The field order is the
// canonical member order of the encoded payload and must not change.
type Claims struct {
	Issuer     string       `json:"iss"`
	Network    string       `json:"net"`
	Subject    string       `json:"sub"`
	Cnf        Confirmation `json:"cnf"`
	Generation uint64       `json:"gen"`
	Roles      []string     `json:"roles"`
}

// HostKey returns the host public key the certificate is bound to.
func (c *Claims) HostKey() (*mldsa.PublicKey, error) {
	if c.Cnf.JWK.Kty != jwkKeyType || c.Cnf.JWK.Alg != Algorithm {
		return nil, fmt.Errorf("%w: unsupported cnf key type", ErrBadCertificate)
	}

	pub, err := DecodePublicKey(c.Cnf.JWK.Pub)
	if err != nil {
		return nil, fmt.Errorf("%w: cnf key: %w", ErrBadCertificate, err)
	}

	return pub, nil
}

// CertificateSpec is the operator-side input of a host certificate.
type CertificateSpec struct {
	Network    string
	Hostname   string
	Generation uint64
	// Roles are sorted and de-duplicated before encoding.
	Roles []string
}

// IssueCertificate derives the signer and host keys from seed and returns the
// host certificate as a compact JWT. The output is deterministic: the same
// inputs always yield the same bytes.
func IssueCertificate(seed []byte, spec CertificateSpec) (string, error) {
	signer, err := DeriveSignerKey(seed, spec.Network)
	if err != nil {
		return "", err
	}

	host, err := DeriveHostKey(seed, spec.Network, spec.Hostname, spec.Generation)
	if err != nil {
		return "", err
	}

	return SignCertificate(signer, PublicKey(host), spec)
}

// SignCertificate signs a certificate for hostKey with the signer key.
func SignCertificate(
	signer *mldsa.PrivateKey, hostKey *mldsa.PublicKey, spec CertificateSpec,
) (string, error) {
	if err := ValidateName(spec.Network); err != nil {
		return "", err
	}

	if err := ValidateName(spec.Hostname); err != nil {
		return "", err
	}

	roles := make([]string, 0, len(spec.Roles))

	for _, role := range spec.Roles {
		if err := ValidateName(role); err != nil {
			return "", err
		}

		roles = append(roles, role)
	}

	slices.Sort(roles)
	roles = slices.Compact(roles)

	claims := Claims{
		Issuer:  Issuer,
		Network: spec.Network,
		Subject: spec.Hostname,
		Cnf: Confirmation{JWK: JWK{
			Kty: jwkKeyType,
			Alg: Algorithm,
			Pub: EncodePublicKey(hostKey),
		}},
		Generation: spec.Generation,
		Roles:      roles,
	}

	// All string members are restricted to nameRe or base64url, so
	// encoding/json emits them verbatim and the struct order is the
	// canonical order.
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("identity: encode claims: %w", err)
	}

	return signCompact(signer, certHeader, payload)
}

// Certificate is a verified host certificate.
type Certificate struct {
	Claims
	// Raw is the compact JWT as presented.
	Raw string
	// Hash is the lower-case hex SHA-256 of Raw.
	Hash string
}

// VerifyCertificate checks the signer signature and the claims of raw and
// that it was issued for network.
func VerifyCertificate(
	signer *mldsa.PublicKey, network, raw string,
) (*Certificate, error) {
	payload, err := verifyCompact(signer, certHeader, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCertificate, err)
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %w", ErrBadCertificate, err)
	}

	if claims.Issuer != Issuer {
		return nil, fmt.Errorf("%w: unexpected issuer", ErrBadCertificate)
	}

	if err := ValidateName(claims.Subject); err != nil {
		return nil, fmt.Errorf("%w: subject: %w", ErrBadCertificate, err)
	}

	if _, err := claims.HostKey(); err != nil {
		return nil, err
	}

	if claims.Network != network {
		return nil, ErrWrongNetwork
	}

	if claims.Roles == nil {
		claims.Roles = []string{}
	}

	return &Certificate{Claims: claims, Raw: raw, Hash: CertificateHash(raw)}, nil
}

// CertificateHash returns the lower-case hex SHA-256 of a compact certificate.
func CertificateHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))

	return hex.EncodeToString(sum[:])
}

// signCompact returns header.payload.signature in JWS compact form. The
// signature is the deterministic ML-DSA variant, so the same input always
// yields the same token.
func signCompact(key *mldsa.PrivateKey, header string, payload []byte) (string, error) {
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString([]byte(header)) + "." + enc.EncodeToString(payload)

	sig, err := key.SignDeterministic([]byte(signingInput), nil)
	if err != nil {
		return "", fmt.Errorf("identity: sign: %w", err)
	}

	return signingInput + "." + enc.EncodeToString(sig), nil
}

// verifyCompact verifies a JWS in compact form against key and returns the
// decoded payload. The header must equal wantHeader byte for byte: there is
// exactly one algorithm and one type per token kind, so nothing is negotiated.
func verifyCompact(key *mldsa.PublicKey, wantHeader, raw string) ([]byte, error) {
	if key == nil {
		return nil, errors.New("invalid verification key")
	}

	parts, err := splitCompact(raw)
	if err != nil {
		return nil, err
	}

	enc := base64.RawURLEncoding

	header, err := enc.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed header")
	}

	if string(header) != wantHeader {
		return nil, errors.New("unexpected header")
	}

	payload, err := enc.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed payload")
	}

	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("malformed signature")
	}

	if mldsa.Verify(key, []byte(parts[0]+"."+parts[1]), sig, nil) != nil {
		return nil, errors.New("signature mismatch")
	}

	return payload, nil
}

func splitCompact(raw string) ([]string, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a compact JWS")
	}

	return parts, nil
}
