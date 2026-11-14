package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

const vectorsPath = "testdata/vectors.json"

const testSeed = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestVectors(t *testing.T) {
	computed := VectorInputs()
	for i := range computed {
		require.NoError(t, computed[i].Compute())

		if computed[i].Roles == nil {
			computed[i].Roles = []string{}
		}
	}

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(computed))

	if *update {
		require.NoError(t, os.WriteFile(vectorsPath, buf.Bytes(), 0o600))
	}

	stored, err := os.ReadFile(vectorsPath)
	require.NoError(t, err)
	require.Equal(t, string(stored), buf.String(),
		"vectors changed: the identity encoding is a wire contract shared with the "+
			"Ansible filter plugin; run with -update only for a deliberate change")

	var vectors []Vector
	require.NoError(t, json.Unmarshal(stored, &vectors))
	require.NotEmpty(t, vectors)

	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			signer, err := DecodePublicKey(v.SignerPublicKey)
			require.NoError(t, err)

			cert, err := VerifyCertificate(signer, v.Network, v.Certificate)
			require.NoError(t, err)
			assert.Equal(t, v.Hostname, cert.Subject)
			assert.Equal(t, v.Generation, cert.Generation)
			assert.Equal(t, v.Validators, cert.Validators)
			assert.Equal(t, v.HostPublicKey, cert.Cnf.JWK.X)
			assert.Equal(t, v.CertificateHash, cert.Hash)

			key, err := DecodePrivateKey(v.HostKey)
			require.NoError(t, err)
			assert.Equal(t, v.HostPublicKey, EncodePublicKey(PublicKey(key)))

			verifier := NewVerifier(signer, v.Network, WithClock(func() time.Time {
				return time.Unix(v.ProofClaims.IssuedAt+5, 0)
			}))
			_, err = verifier.Verify(v.Certificate, v.Proof, Request{
				Method: v.ProofClaims.Method,
				URLs:   []string{v.ProofClaims.URL},
				Body:   []byte(v.ProofBody),
			})
			require.NoError(t, err)
		})
	}
}

func TestCertificateEncoding(t *testing.T) {
	cert, err := IssueCertificate([]byte(testSeed), CertificateSpec{
		Network: "net", Hostname: "host-1",
		Validators: &ValidatorRange{Start: 0, End: 100},
		Roles:      []string{"b", "a", "b"},
	})
	require.NoError(t, err)

	parts := strings.Split(cert, ".")
	require.Len(t, parts, 3)

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	assert.Equal(t, `{"alg":"EdDSA","typ":"JWT"}`, string(header))

	host, err := DeriveHostKey([]byte(testSeed), "net", "host-1", 0)
	require.NoError(t, err)

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	assert.Equal(t, `{"iss":"upgradoor-signer","net":"net","sub":"host-1",`+
		`"cnf":{"jwk":{"kty":"OKP","crv":"Ed25519","x":"`+EncodePublicKey(PublicKey(host))+`"}},`+
		`"gen":0,"val":{"start":0,"end":100},"roles":["a","b"]}`, string(payload))

	noVal, err := IssueCertificate([]byte(testSeed), CertificateSpec{
		Network: "net", Hostname: "host-1",
	})
	require.NoError(t, err)

	payload, err = base64.RawURLEncoding.DecodeString(strings.Split(noVal, ".")[1])
	require.NoError(t, err)
	assert.NotContains(t, string(payload), `"val"`)
	assert.True(t, strings.HasSuffix(string(payload), `"gen":0,"roles":[]}`))
}

func TestDerivationIsolation(t *testing.T) {
	seed := []byte(testSeed)

	a, err := DeriveHostKey(seed, "net", "a", 0)
	require.NoError(t, err)
	b, err := DeriveHostKey(seed, "net", "b", 0)
	require.NoError(t, err)
	a1, err := DeriveHostKey(seed, "net", "a", 1)
	require.NoError(t, err)
	other, err := DeriveHostKey(seed, "net2", "a", 0)
	require.NoError(t, err)
	signer, err := DeriveSignerKey(seed, "net")
	require.NoError(t, err)

	keys := []ed25519.PrivateKey{a, b, a1, other, signer}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			assert.False(t, keys[i].Equal(keys[j]))
		}
	}

	_, err = DeriveSignerKey([]byte("short"), "net")
	require.ErrorIs(t, err, ErrSeedTooShort)

	for _, bad := range []string{"", "a/b", "-a", "a b", `a"b`, "ü", strings.Repeat("a", 254)} {
		_, err = DeriveHostKey(seed, "net", bad, 0)
		require.ErrorIs(t, err, ErrInvalidName, bad)
		_, err = DeriveSignerKey(seed, bad)
		require.ErrorIs(t, err, ErrInvalidName, bad)
	}
}

type fixture struct {
	signer   ed25519.PublicKey
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

	key, err := DeriveHostKey(seed, "net", "host-1", 0)
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

	proof, err := f.creds.Proof("POST", fixtureURL, []byte("body"), f.now)
	require.NoError(t, err)

	cert, err := f.verifier.Verify(f.creds.Certificate(), proof, f.request("body"))
	require.NoError(t, err)
	assert.Equal(t, "host-1", cert.Subject)
	assert.Empty(t, cert.Roles)
	assert.NotNil(t, cert.Roles)

	t.Run("replay", func(t *testing.T) {
		_, err := f.verifier.Verify(f.creds.Certificate(), proof, f.request("body"))
		require.ErrorIs(t, err, ErrProofReplayed)
	})

	fresh := func(t *testing.T, method, url, body string, at time.Time) string {
		t.Helper()

		p, err := f.creds.Proof(method, url, []byte(body), at)
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
			_, err := f.verifier.Verify(f.creds.Certificate(), tt.proof(t), tt.req)
			if tt.want == nil {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestVerifyRejectsForeignIdentities(t *testing.T) {
	f := newFixture(t)
	seed := []byte(testSeed)

	t.Run("other network", func(t *testing.T) {
		key, err := DeriveHostKey(seed, "net2", "host-1", 0)
		require.NoError(t, err)
		cert, err := IssueCertificate(seed, CertificateSpec{Network: "net2", Hostname: "host-1"})
		require.NoError(t, err)
		creds, err := NewCredentials(key, cert)
		require.NoError(t, err)
		proof, err := creds.Proof("POST", fixtureURL, nil, f.now)
		require.NoError(t, err)

		// Signed by net2's signer: fails the signature check under net's key.
		_, err = f.verifier.Verify(cert, proof, f.request(""))
		require.ErrorIs(t, err, ErrBadCertificate)

		// Signed by net's signer but claiming net2.
		signer, err := DeriveSignerKey(seed, "net")
		require.NoError(t, err)
		cross, err := SignCertificate(signer, PublicKey(key),
			CertificateSpec{Network: "net2", Hostname: "host-1"})
		require.NoError(t, err)
		_, err = f.verifier.Verify(cross, proof, f.request(""))
		require.ErrorIs(t, err, ErrWrongNetwork)
	})

	t.Run("self-signed certificate", func(t *testing.T) {
		key, err := DeriveHostKey(seed, "net", "evil", 0)
		require.NoError(t, err)
		cert, err := SignCertificate(key, PublicKey(key),
			CertificateSpec{Network: "net", Hostname: "host-2"})
		require.NoError(t, err)
		creds, err := NewCredentials(key, cert)
		require.NoError(t, err)
		proof, err := creds.Proof("POST", fixtureURL, nil, f.now)
		require.NoError(t, err)
		_, err = f.verifier.Verify(cert, proof, f.request(""))
		require.ErrorIs(t, err, ErrBadCertificate)
	})

	t.Run("stolen certificate, other key", func(t *testing.T) {
		thief, err := DeriveHostKey(seed, "net", "host-2", 0)
		require.NoError(t, err)

		_, err = NewCredentials(thief, f.creds.Certificate())
		require.ErrorIs(t, err, ErrBadCertificate)

		proof, err := SignProof(thief, &ProofClaims{
			IssuedAt: f.now.Unix(), ExpiresAt: f.now.Unix() + 60, ID: "x",
			Method: "POST", URL: fixtureURL, BodyHash: BodyHash(nil),
		})
		require.NoError(t, err)
		_, err = f.verifier.Verify(f.creds.Certificate(), proof, f.request(""))
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
		key, err := DeriveHostKey(seed, "net", "host-1", 0)
		require.NoError(t, err)
		proof, err := SignProof(key, &ProofClaims{
			IssuedAt: f.now.Unix(), ExpiresAt: f.now.Unix() + 3600, ID: "y",
			Method: "POST", URL: fixtureURL, BodyHash: BodyHash(nil),
		})
		require.NoError(t, err)
		_, err = f.verifier.Verify(f.creds.Certificate(), proof, f.request(""))
		require.ErrorIs(t, err, ErrBadProof)
	})
}

func TestKeyEncoding(t *testing.T) {
	key, err := DeriveHostKey([]byte(testSeed), "net", "host-1", 0)
	require.NoError(t, err)

	decoded, err := DecodePrivateKey(EncodePrivateKey(key) + "\n")
	require.NoError(t, err)
	assert.True(t, key.Equal(decoded))

	pub, err := DecodePublicKey(" " + EncodePublicKey(PublicKey(key)) + "\n")
	require.NoError(t, err)
	assert.True(t, pub.Equal(PublicKey(key)))

	_, err = DecodePrivateKey("nope")
	require.ErrorIs(t, err, ErrInvalidKey)
	_, err = DecodePublicKey(EncodePrivateKey(key) + "AA")
	require.ErrorIs(t, err, ErrInvalidKey)
}
