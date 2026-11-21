package identity

import (
	"crypto/mldsa"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	signer   *mldsa.PublicKey
	creds    *Credentials
	verifier *Verifier
	now      time.Time
}

const fixtureURL = "https://upgradoor.net.example/api/agent/v1/sync"

func newFixture(t *testing.T) *fixture {
	t.Helper()

	seed := []byte(testSeed)
	f := &fixture{now: time.Unix(1800000000, 0)}

	signer, err := DeriveSignerKey(seed, "net")
	require.NoError(t, err)

	f.signer = PublicKey(signer)

	key, err := DeriveHostKey(seed, "net", "host-1")
	require.NoError(t, err)
	cert, err := IssueCertificate(seed, CertificateSpec{Network: "net", Hostname: "host-1"})
	require.NoError(t, err)

	f.creds, err = NewCredentials(key, cert)
	require.NoError(t, err)

	f.verifier = NewVerifier(f.signer, "net", WithClock(func() time.Time { return f.now }))

	return f
}

func (f *fixture) request(body string) Request {
	return Request{Method: "POST", URLs: []string{fixtureURL}, Body: []byte(body)}
}

func TestVerifyRequest(t *testing.T) {
	f := newFixture(t)

	proof, _, err := f.creds.Proof("POST", fixtureURL, []byte("body"), f.now)
	require.NoError(t, err)

	cert, _, err := f.verifier.Verify(f.creds.Certificate(), proof, f.request("body"))
	require.NoError(t, err)
	assert.Equal(t, "host-1", cert.Subject)
	assert.Empty(t, cert.Roles)
	assert.NotNil(t, cert.Roles)

	t.Run("replay", func(t *testing.T) {
		_, _, err := f.verifier.Verify(f.creds.Certificate(), proof, f.request("body"))
		require.ErrorIs(t, err, ErrProofReplayed)
	})

	fresh := func(t *testing.T, method, url, body string, at time.Time) string {
		t.Helper()

		p, _, err := f.creds.Proof(method, url, []byte(body), at)
		require.NoError(t, err)

		return p
	}

	tests := []struct {
		name  string
		proof func(t *testing.T) string
		req   Request
		want  error
	}{
		{"body changed", func(t *testing.T) string {
			t.Helper()

			return fresh(t, "POST", fixtureURL, "body", f.now)
		}, f.request("other"), ErrProofMismatch},
		{"method changed", func(t *testing.T) string {
			t.Helper()

			return fresh(t, "GET", fixtureURL, "body", f.now)
		}, f.request("body"), ErrProofMismatch},
		{"url changed", func(t *testing.T) string {
			t.Helper()

			return fresh(t, "POST", fixtureURL+"x", "body", f.now)
		}, f.request("body"), ErrProofMismatch},
		{"expired", func(t *testing.T) string {
			t.Helper()

			return fresh(t, "POST", fixtureURL, "body", f.now.Add(-2*time.Minute))
		}, f.request("body"), ErrProofExpired},
		{"from the future", func(t *testing.T) string {
			t.Helper()

			return fresh(t, "POST", fixtureURL, "body", f.now.Add(2*time.Minute))
		}, f.request("body"), ErrProofExpired},
		{"within skew", func(t *testing.T) string {
			t.Helper()

			return fresh(t, "POST", fixtureURL, "body", f.now.Add(20*time.Second))
		}, f.request("body"), nil},
		{"garbage", func(*testing.T) string { return "a.b.c" }, f.request("body"), ErrBadProof},
		{"certificate as proof", func(*testing.T) string {
			return f.creds.Certificate()
		}, f.request("body"), ErrBadProof},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := f.verifier.Verify(f.creds.Certificate(), tt.proof(t), tt.req)
			if tt.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tt.want)
		})
	}
}

// A certificate verified once is not verified again, but every proof under
// it is: a proof signed by another key is refused although its certificate
// is known.
func TestVerifyKnownCertificateStillChecksProof(t *testing.T) {
	f := newFixture(t)

	proof, _, err := f.creds.Proof("POST", fixtureURL, []byte("body"), f.now)
	require.NoError(t, err)

	_, _, err = f.verifier.Verify(f.creds.Certificate(), proof, f.request("body"))
	require.NoError(t, err)
	require.Len(t, f.verifier.certs, 1)

	other, err := DeriveHostKey([]byte(testSeed), "net", "host-2")
	require.NoError(t, err)

	forged, err := SignProof(other, &ProofClaims{
		ID: "forged", Method: "POST", URL: fixtureURL, BodyHash: BodyHash([]byte("body")),
		IssuedAt: f.now.Unix(), ExpiresAt: f.now.Unix() + 30,
	})
	require.NoError(t, err)

	_, _, err = f.verifier.Verify(f.creds.Certificate(), forged, f.request("body"))
	require.ErrorIs(t, err, ErrBadProof)

	tampered := f.creds.Certificate() + "x"
	_, _, err = f.verifier.Verify(tampered, proof, f.request("body"))
	require.ErrorIs(t, err, ErrBadCertificate)
}

func TestVerifyRejectsForeignIdentities(t *testing.T) {
	f := newFixture(t)
	seed := []byte(testSeed)

	t.Run("other network", func(t *testing.T) {
		key, err := DeriveHostKey(seed, "net2", "host-1")
		require.NoError(t, err)
		cert, err := IssueCertificate(seed, CertificateSpec{Network: "net2", Hostname: "host-1"})
		require.NoError(t, err)
		creds, err := NewCredentials(key, cert)
		require.NoError(t, err)
		proof, _, err := creds.Proof("POST", fixtureURL, nil, f.now)
		require.NoError(t, err)

		// Signed by net2's signer: fails the signature check under net's key.
		_, _, err = f.verifier.Verify(cert, proof, f.request(""))
		require.ErrorIs(t, err, ErrBadCertificate)

		// Signed by net's signer but claiming net2.
		signer, err := DeriveSignerKey(seed, "net")
		require.NoError(t, err)
		cross, err := SignCertificate(signer, PublicKey(key),
			CertificateSpec{Network: "net2", Hostname: "host-1"})
		require.NoError(t, err)
		_, _, err = f.verifier.Verify(cross, proof, f.request(""))
		require.ErrorIs(t, err, ErrWrongNetwork)
	})

	t.Run("self-signed certificate", func(t *testing.T) {
		key, err := DeriveHostKey(seed, "net", "evil")
		require.NoError(t, err)
		cert, err := SignCertificate(key, PublicKey(key),
			CertificateSpec{Network: "net", Hostname: "host-2"})
		require.NoError(t, err)
		creds, err := NewCredentials(key, cert)
		require.NoError(t, err)
		proof, _, err := creds.Proof("POST", fixtureURL, nil, f.now)
		require.NoError(t, err)
		_, _, err = f.verifier.Verify(cert, proof, f.request(""))
		require.ErrorIs(t, err, ErrBadCertificate)
	})

	t.Run("stolen certificate, other key", func(t *testing.T) {
		thief, err := DeriveHostKey(seed, "net", "host-2")
		require.NoError(t, err)

		_, err = NewCredentials(thief, f.creds.Certificate())
		require.ErrorIs(t, err, ErrBadCertificate)

		proof, err := SignProof(thief, &ProofClaims{
			IssuedAt: f.now.Unix(), ExpiresAt: f.now.Unix() + 60, ID: "x",
			Method: "POST", URL: fixtureURL, BodyHash: BodyHash(nil),
		})
		require.NoError(t, err)
		_, _, err = f.verifier.Verify(f.creds.Certificate(), proof, f.request(""))
		require.ErrorIs(t, err, ErrBadProof)
	})

	t.Run("tampered claims", func(t *testing.T) {
		parts := strings.Split(f.creds.Certificate(), ".")
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		require.NoError(t, err)

		forged := strings.Replace(string(payload), "host-1", "host-9", 1)
		parts[1] = base64.RawURLEncoding.EncodeToString([]byte(forged))
		_, err = VerifyCertificate(f.signer, "net", strings.Join(parts, "."))
		require.ErrorIs(t, err, ErrBadCertificate)
	})

	t.Run("over-long proof lifetime", func(t *testing.T) {
		key, err := DeriveHostKey(seed, "net", "host-1")
		require.NoError(t, err)
		proof, err := SignProof(key, &ProofClaims{
			IssuedAt: f.now.Unix(), ExpiresAt: f.now.Unix() + 3600, ID: "y",
			Method: "POST", URL: fixtureURL, BodyHash: BodyHash(nil),
		})
		require.NoError(t, err)
		_, _, err = f.verifier.Verify(f.creds.Certificate(), proof, f.request(""))
		require.ErrorIs(t, err, ErrBadProof)
	})
}
