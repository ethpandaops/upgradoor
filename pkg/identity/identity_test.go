package identity

import (
	"bytes"
	"crypto/mldsa"
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
	computed := vectorInputs()
	for i := range computed {
		require.NoError(t, computed[i].compute())

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

	var vectors []vector
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
			assert.Equal(t, v.HostPublicKey, cert.Cnf.JWK.Pub)
			assert.Equal(t, v.CertificateHash, cert.Hash)

			key, err := DecodePrivateKey(v.HostKey)
			require.NoError(t, err)
			assert.Equal(t, v.HostPublicKey, EncodePublicKey(PublicKey(key)))

			verifier := NewVerifier(signer, v.Network, WithClock(func() time.Time {
				return time.Unix(v.ProofClaims.IssuedAt+5, 0)
			}))
			_, _, err = verifier.Verify(v.Certificate, v.Proof, Request{
				Method: v.ProofClaims.Method,
				URLs:   []string{v.ProofClaims.URL},
				Body:   []byte(v.ProofBody),
			})
			require.NoError(t, err)

			ctrl, err := DecodePrivateKey(v.ControllerKey)
			require.NoError(t, err)
			assert.Equal(t, v.ControllerPublicKey, EncodePublicKey(PublicKey(ctrl)))

			// The stored signature is the one of a signed answer: wrapped
			// with its body, the host's verifier takes it for that request.
			answers, err := NewAnswerVerifier(
				[]*mldsa.PublicKey{PublicKey(ctrl)}, v.Network, v.Hostname)
			require.NoError(t, err)

			envelope, err := json.Marshal(SignedAnswer{
				Payload:   base64.RawURLEncoding.EncodeToString([]byte(v.AnswerBody)),
				Signature: v.Answer,
			})
			require.NoError(t, err)

			body, err := answers.Verify(v.ProofClaims.ID, envelope)
			require.NoError(t, err)
			assert.Equal(t, v.AnswerBody, string(body))
		})
	}
}

// vector is one fixed identity test vector. The Go tests and the Ansible
// filter plugin tests both recompute every output from the inputs and compare
// byte for byte against testdata/vectors.json.
type vector struct {
	Name string `json:"name"`

	// Inputs.
	Seed       string   `json:"seed"`
	Network    string   `json:"network"`
	Hostname   string   `json:"hostname"`
	Generation uint64   `json:"generation"`
	Roles      []string `json:"roles"`
	// ControllerSeed is the controller seed, a secret of its own: the
	// controller outputs are derived from it and from nothing of Seed.
	ControllerSeed string `json:"controllerSeed"`
	// ControllerGeneration is the generation of the controller key.
	ControllerGeneration uint64 `json:"controllerGeneration"`

	// Outputs.
	SignerPublicKey string `json:"signerPublicKey"`
	HostKey         string `json:"hostKey"`
	HostPublicKey   string `json:"hostPublicKey"`
	Certificate     string `json:"certificate"`
	CertificateHash string `json:"certificateHash"`

	// Proof is a proof with fixed claims, signed by the host key.
	ProofClaims ProofClaims `json:"proofClaims"`
	ProofBody   string      `json:"proofBody"`
	Proof       string      `json:"proof"`

	// ControllerKey and ControllerPublicKey are the controller key pair of
	// Network at ControllerGeneration, derived from ControllerSeed.
	ControllerKey       string `json:"controllerKey"`
	ControllerPublicKey string `json:"controllerPublicKey"`

	// Answer is the controller key's signature for an answer with the body
	// AnswerBody (an input), given to Hostname for the request of the proof
	// above. AnswerClaims are its claims.
	AnswerBody   string       `json:"answerBody"`
	AnswerClaims AnswerClaims `json:"answerClaims"`
	Answer       string       `json:"answer"`
}

// compute fills the outputs of v from its inputs.
func (v *vector) compute() error {
	seed := []byte(v.Seed)

	signer, err := DeriveSignerKey(seed, v.Network)
	if err != nil {
		return err
	}

	host, err := DeriveHostKey(seed, v.Network, v.Hostname, v.Generation)
	if err != nil {
		return err
	}

	cert, err := IssueCertificate(seed, CertificateSpec{
		Network:    v.Network,
		Hostname:   v.Hostname,
		Generation: v.Generation,
		Roles:      v.Roles,
	})
	if err != nil {
		return err
	}

	ctrl, err := DeriveControllerKey(
		[]byte(v.ControllerSeed), v.Network, v.ControllerGeneration)
	if err != nil {
		return err
	}

	v.ProofClaims.BodyHash = BodyHash([]byte(v.ProofBody))

	proof, err := SignProof(host, &v.ProofClaims)
	if err != nil {
		return err
	}

	v.SignerPublicKey = EncodePublicKey(PublicKey(signer))
	v.HostKey = EncodePrivateKey(host)
	v.HostPublicKey = EncodePublicKey(PublicKey(host))
	v.Certificate = cert
	v.CertificateHash = CertificateHash(cert)
	v.Proof = proof

	v.AnswerClaims = AnswerClaims{
		Network:   v.Network,
		Subject:   v.Hostname,
		RequestID: v.ProofClaims.ID,
		BodyHash:  BodyHash([]byte(v.AnswerBody)),
	}

	answer, err := SignAnswer(ctrl, &v.AnswerClaims)
	if err != nil {
		return err
	}

	v.ControllerKey = EncodePrivateKey(ctrl)
	v.ControllerPublicKey = EncodePublicKey(PublicKey(ctrl))
	v.Answer = answer

	return nil
}

// vectorInputs returns the inputs of the fixed test vectors, without outputs.
func vectorInputs() []vector {
	proof := func(host string) ProofClaims {
		return ProofClaims{
			IssuedAt:  1800000000,
			ExpiresAt: 1800000060,
			ID:        "AAECAwQFBgcICQoLDA0ODw",
			Method:    "POST",
			URL:       "https://upgradoor." + host + "/api/agent/v1/sync",
		}
	}

	const (
		seedA    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		netA     = "testnet-devnet-0"
		domainA  = netA + ".ethpandaops.io"
		roleNode = "ethereum_node"
		answerA  = `{"serverTime":"2027-01-15T08:00:00Z","wantInventory":false}`
		// The controller seed is another secret than the devnet seed.
		ctrlSeedA = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
		ctrlSeedB = "a controller seed is used as raw UTF-8 bytes too ✓ ünïcödé"
	)

	return []vector{
		{
			Name: "validator-host", Seed: seedA, ControllerSeed: ctrlSeedA,
			Network: netA, Hostname: "lighthouse-geth-1",
			Roles:       []string{roleNode},
			ProofClaims: proof(domainA),
			ProofBody:   `{"protocolVersion":1}`,
			AnswerBody:  answerA,
		},
		{
			Name: "bootnode-no-validators", Seed: seedA, ControllerSeed: ctrlSeedA,
			Network: netA, Hostname: "bootnode-1",
			Roles:       []string{"bootnode"},
			ProofClaims: proof(domainA),
			ProofBody:   "",
			AnswerBody:  "",
		},
		{
			// The controller generation is rotated as well.
			Name: "rotated-generation", Seed: seedA, ControllerSeed: ctrlSeedA,
			Network: netA, Hostname: "lighthouse-geth-1", Generation: 3,
			Roles:                []string{roleNode},
			ControllerGeneration: 1,
			ProofClaims:          proof(domainA),
			ProofBody:            `{"protocolVersion":1}`,
			AnswerBody:           answerA,
		},
		{
			Name:           "roles-sorted-and-deduplicated",
			Seed:           "correct horse battery staple, but at least 32 bytes long!",
			ControllerSeed: ctrlSeedA,
			Network:        "glamsterdam-devnet-8", Hostname: "prysm-besu-1",
			Roles:       []string{roleNode, "bootnode", roleNode, "Mev.relay_1"},
			ProofClaims: proof("glamsterdam-devnet-8.ethpandaops.io"),
			ProofBody:   "non-ASCII body ✓ with a newline\n",
			AnswerBody:  "non-ASCII answer ✓ with a newline\n",
		},
		{
			Name:           "no-roles",
			Seed:           "ünïcödé seed bytes are used as raw UTF-8 input ✓",
			ControllerSeed: ctrlSeedB,
			Network:        "net.with.dots", Hostname: "Host_With-Mixed.Case01",
			Generation:           18446744073709551615,
			Roles:                nil,
			ControllerGeneration: 18446744073709551615,
			ProofClaims:          proof("net.with.dots.example"),
			ProofBody:            "x",
			AnswerBody:           "x",
		},
	}
}

func TestCertificateEncoding(t *testing.T) {
	cert, err := IssueCertificate([]byte(testSeed), CertificateSpec{
		Network: "net", Hostname: "host-1",
		Roles: []string{"b", "a", "b"},
	})
	require.NoError(t, err)

	parts := strings.Split(cert, ".")
	require.Len(t, parts, 3)

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	assert.Equal(t, `{"alg":"ML-DSA-44","typ":"JWT"}`, string(header))

	host, err := DeriveHostKey([]byte(testSeed), "net", "host-1", 0)
	require.NoError(t, err)

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	assert.Equal(t, `{"iss":"upgradoor-signer","net":"net","sub":"host-1",`+
		`"cnf":{"jwk":{"kty":"AKP","alg":"ML-DSA-44","pub":"`+EncodePublicKey(PublicKey(host))+`"}},`+
		`"gen":0,"roles":["a","b"]}`, string(payload))

	noRoles, err := IssueCertificate([]byte(testSeed), CertificateSpec{
		Network: "net", Hostname: "host-1",
	})
	require.NoError(t, err)

	payload, err = base64.RawURLEncoding.DecodeString(strings.Split(noRoles, ".")[1])
	require.NoError(t, err)
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

	ctrl, err := DeriveControllerKey(seed, "net", 0)
	require.NoError(t, err)
	ctrl1, err := DeriveControllerKey(seed, "net", 1)
	require.NoError(t, err)
	ctrlOther, err := DeriveControllerKey(seed, "net2", 0)
	require.NoError(t, err)
	// A host named like the controller's info string cannot collide with it.
	hostNamedController, err := DeriveHostKey(seed, "net", "controller", 0)
	require.NoError(t, err)

	keys := []*mldsa.PrivateKey{
		a, b, a1, other, signer, ctrl, ctrl1, ctrlOther, hostNamedController,
	}
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
		_, err = DeriveControllerKey(seed, bad, 0)
		require.ErrorIs(t, err, ErrInvalidName, bad)
	}
}

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

	other, err := DeriveHostKey([]byte(testSeed), "net", "host-2", 0)
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
		key, err := DeriveHostKey(seed, "net2", "host-1", 0)
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
		key, err := DeriveHostKey(seed, "net", "evil", 0)
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
		thief, err := DeriveHostKey(seed, "net", "host-2", 0)
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
		key, err := DeriveHostKey(seed, "net", "host-1", 0)
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

func TestReadSeedFile(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		path := dir + "/seed"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		return path
	}

	// Raw bytes, exactly as stored; one trailing newline is not part of it.
	for content, want := range map[string]string{
		testSeed:             testSeed,
		testSeed + "\n":      testSeed,
		testSeed + "\r\n":    testSeed,
		testSeed + "\n\n":    testSeed + "\n",
		" " + testSeed + " ": " " + testSeed + " ",
		"ünïcödé seed bytes are used as raw UTF-8 input ✓\n": "ünïcödé seed bytes are used as raw UTF-8 input ✓",
	} {
		got, err := ReadSeedFile(write(content))
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}

	const short = "0123456789abcdef0123456789abcde"

	_, err := ReadSeedFile(write(short + "\n"))
	require.ErrorIs(t, err, ErrSeedTooShort)
	assert.NotContains(t, err.Error(), short, "the error never carries the seed")

	_, err = ReadSeedFile(dir + "/missing")
	require.ErrorIs(t, err, ErrNoSeed)
}
