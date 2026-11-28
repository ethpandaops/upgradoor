package identity

import (
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

const (
	// HeaderCert is the request header carrying the host certificate.
	HeaderCert = "X-Upgradoor-Cert"
	// HeaderProof is the request header carrying the request proof.
	HeaderProof = "X-Upgradoor-Proof"

	// ProofLifetime is the validity window of a request proof.
	ProofLifetime = 60 * time.Second
	// ClockSkew is the tolerated clock difference between an agent and the
	// controller.
	ClockSkew = 30 * time.Second
)

var (
	// ErrBadProof is returned when a proof is malformed or not signed by the
	// certificate's host key.
	ErrBadProof = errors.New("identity: bad proof")
	// ErrProofExpired is returned when a proof is outside its time window.
	ErrProofExpired = errors.New("identity: proof outside its time window")
	// ErrProofReplayed is returned when a proof's jti was already seen.
	ErrProofReplayed = errors.New("identity: proof replayed")
	// ErrProofMismatch is returned when a proof does not match the request's
	// method, URL or body.
	ErrProofMismatch = errors.New("identity: proof does not match request")
)

// ProofClaims are the claims of a request proof. The field order is the
// canonical member order of the encoded payload.
type ProofClaims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	ID        string `json:"jti"`
	Method    string `json:"htm"`
	URL       string `json:"htu"`
	BodyHash  string `json:"bh"`
}

// BodyHash returns the unpadded base64url SHA-256 of a request body exactly
// as it is sent on the wire (after any content encoding).
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// SignProof signs claims with the host key. It is deterministic; use
// Credentials.Proof for real requests.
func SignProof(key *mldsa.PrivateKey, claims *ProofClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("identity: encode proof: %w", err)
	}

	return signCompact(key, proofHeader, payload)
}

// Credentials are an agent's key and certificate.
type Credentials struct {
	key  *mldsa.PrivateKey
	cert string
}

// NewCredentials pairs a host key with its certificate. It checks that the
// certificate is bound to the key; it cannot check the signer signature.
func NewCredentials(key *mldsa.PrivateKey, cert string) (*Credentials, error) {
	claims, err := UnverifiedClaims(cert)
	if err != nil {
		return nil, err
	}

	pub, err := claims.HostKey()
	if err != nil {
		return nil, err
	}

	if !pub.Equal(PublicKey(key)) {
		return nil, fmt.Errorf("%w: certificate is bound to another key", ErrBadCertificate)
	}

	return &Credentials{key: key, cert: cert}, nil
}

// Certificate returns the compact host certificate.
func (c *Credentials) Certificate() string { return c.cert }

// Proof returns a fresh proof for one request and the proof's jti, the id of
// the request. body is the body exactly as sent on the wire. A signed answer
// names the id, which binds the answer to this request.
func (c *Credentials) Proof(
	method, url string, body []byte, now time.Time,
) (proof, id string, err error) {
	if id, err = NewRequestID(); err != nil {
		return "", "", err
	}

	if proof, err = c.ProofFor(id, method, url, body, now); err != nil {
		return "", "", err
	}

	return proof, id, nil
}

// NewRequestID returns a fresh random request id, the jti of a proof.
func NewRequestID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("identity: jti: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// ProofFor returns the proof for one request whose id, from NewRequestID,
// is known before its body: a sealed body is bound to the id.
func (c *Credentials) ProofFor(
	id, method, url string, body []byte, now time.Time,
) (string, error) {
	return SignProof(c.key, &ProofClaims{
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(ProofLifetime).Unix(),
		ID:        id,
		Method:    method,
		URL:       url,
		BodyHash:  BodyHash(body),
	})
}

// UnverifiedClaims decodes a certificate's claims without checking its
// signature. Agents use it to learn their own name; controllers must use
// VerifyCertificate.
func UnverifiedClaims(cert string) (*Claims, error) {
	parts, err := splitCompact(cert)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCertificate, err)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: malformed payload", ErrBadCertificate)
	}

	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("%w: claims: %w", ErrBadCertificate, err)
	}

	return &claims, nil
}

// Request describes the HTTP request a proof must match.
type Request struct {
	Method string
	// URLs are the absolute URLs under which the controller accepts this
	// request (it may be reachable under more than one, e.g. behind an
	// ingress and directly). The proof's htu must equal one of them.
	URLs []string
	// Body is the request body as received on the wire.
	Body []byte
}

// Verifier authenticates agent requests for one network.
type Verifier struct {
	signer  *mldsa.PublicKey
	network string
	now     func() time.Time

	mu   sync.Mutex
	seen map[string]int64 // host key + jti -> unix expiry

	// certs are the certificates verified so far, by their hash. A
	// certificate's validity depends on the signer and the network alone,
	// both fixed for a verifier, so one that verified once always does; the
	// proof is still verified on every request.
	certMu sync.Mutex
	certs  map[string]*verifiedCert
}

// verifiedCert is a certificate that passed VerifyCertificate and its host
// key.
type verifiedCert struct {
	cert    Certificate
	hostKey *mldsa.PublicKey
}

// maxVerifiedCerts bounds the certificate cache; it is emptied when full.
// A fleet presents one certificate per host.
const maxVerifiedCerts = 8192

// VerifierOption configures a Verifier.
type VerifierOption func(*Verifier)

// WithClock overrides the verifier's time source.
func WithClock(now func() time.Time) VerifierOption {
	return func(v *Verifier) { v.now = now }
}

// NewVerifier returns a verifier that accepts certificates signed by signer
// for network.
func NewVerifier(signer *mldsa.PublicKey, network string, opts ...VerifierOption) *Verifier {
	v := &Verifier{
		signer:  signer,
		network: network,
		now:     time.Now,
		seen:    make(map[string]int64, 64),
		certs:   make(map[string]*verifiedCert, 64),
	}

	for _, opt := range opts {
		opt(v)
	}

	return v
}

// Verify authenticates one request: the certificate against the signer and
// network, the proof against the certificate's host key, its time window, its
// binding to the request and its jti against replay. It returns the
// certificate and the proof's jti, the id of the request, which a signed
// answer to it names.
func (v *Verifier) Verify(cert, proof string, req Request) (*Certificate, string, error) {
	vc, err := v.certificate(cert)
	if err != nil {
		return nil, "", err
	}

	c := new(Certificate)
	*c = vc.cert

	payload, err := verifyCompact(vc.hostKey, proofHeader, proof)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrBadProof, err)
	}

	var claims ProofClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, "", fmt.Errorf("%w: claims: %w", ErrBadProof, err)
	}

	if err := v.checkProof(c, &claims, req); err != nil {
		return nil, "", err
	}

	return c, claims.ID, nil
}

// certificate verifies a certificate, or finds it verified before.
func (v *Verifier) certificate(raw string) (*verifiedCert, error) {
	hash := CertificateHash(raw)

	v.certMu.Lock()
	vc, ok := v.certs[hash]
	v.certMu.Unlock()

	if ok {
		return vc, nil
	}

	c, err := VerifyCertificate(v.signer, v.network, raw)
	if err != nil {
		return nil, err
	}

	hostKey, err := c.HostKey()
	if err != nil {
		return nil, err
	}

	vc = &verifiedCert{cert: *c, hostKey: hostKey}

	v.certMu.Lock()
	defer v.certMu.Unlock()

	if len(v.certs) >= maxVerifiedCerts {
		clear(v.certs)
	}

	v.certs[hash] = vc

	return vc, nil
}

// checkProof checks the claims of a proof whose signature is verified.
func (v *Verifier) checkProof(c *Certificate, claims *ProofClaims, req Request) error {
	if claims.ID == "" {
		return fmt.Errorf("%w: missing jti", ErrBadProof)
	}

	now := v.now()
	skew := int64(ClockSkew / time.Second)
	lifetime := int64(ProofLifetime / time.Second)

	if claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > lifetime {
		return fmt.Errorf("%w: invalid lifetime", ErrBadProof)
	}

	if now.Unix() < claims.IssuedAt-skew || now.Unix() > claims.ExpiresAt+skew {
		return fmt.Errorf("%w (controller time %s)", ErrProofExpired,
			now.UTC().Format(time.RFC3339))
	}

	if claims.Method != req.Method || !slices.Contains(req.URLs, claims.URL) {
		return fmt.Errorf("%w: method or URL", ErrProofMismatch)
	}

	if claims.BodyHash != BodyHash(req.Body) {
		return fmt.Errorf("%w: body hash", ErrProofMismatch)
	}

	if !v.remember(c.Hash+"/"+claims.ID, claims.ExpiresAt+skew, now.Unix()) {
		return ErrProofReplayed
	}

	return nil
}

// remember records key until expiry and reports whether it was new.
func (v *Verifier) remember(key string, expiry, now int64) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	if exp, ok := v.seen[key]; ok && exp >= now {
		return false
	}

	if len(v.seen) >= 4096 {
		for k, exp := range v.seen {
			if exp < now {
				delete(v.seen, k)
			}
		}
	}

	v.seen[key] = expiry

	return true
}
