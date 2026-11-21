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
