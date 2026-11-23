package identity

import (
	"bytes"
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
		"vectors changed: the identity encoding is a wire contract; run with -update "+
			"only for a deliberate change")

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
			answers, err := NewAnswerVerifier(PublicKey(ctrl), v.Network, v.Hostname)
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

// vector is one fixed identity test vector. The test recomputes every output
// from the inputs and compares byte for byte against testdata/vectors.json.
type vector struct {
	Name string `json:"name"`

	// Inputs.
	Seed     string `json:"seed"`
	Network  string `json:"network"`
	Hostname string `json:"hostname"`
	// HostKey is the host's key, generated on the host.
	HostKey string `json:"hostKey"`
	// ControllerSeed is the controller seed, a secret of its own: the
	// controller outputs are derived from it and from nothing of Seed.
	ControllerSeed string `json:"controllerSeed"`

	// Outputs.
	SignerPublicKey string `json:"signerPublicKey"`
	HostPublicKey   string `json:"hostPublicKey"`
	Certificate     string `json:"certificate"`
	CertificateHash string `json:"certificateHash"`

	// Proof is a proof with fixed claims, signed by the host key.
	ProofClaims ProofClaims `json:"proofClaims"`
	ProofBody   string      `json:"proofBody"`
	Proof       string      `json:"proof"`

	// ControllerKey and ControllerPublicKey are the controller key pair,
	// derived from ControllerSeed.
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

	signer, err := DeriveSignerKey(seed)
	if err != nil {
		return err
	}

	host, err := DecodePrivateKey(v.HostKey)
	if err != nil {
		return err
	}

	cert, err := IssueCertificate(seed, PublicKey(host), CertificateSpec{
		Network:  v.Network,
		Hostname: v.Hostname,
	})
	if err != nil {
		return err
	}

	ctrl, err := DeriveControllerKey([]byte(v.ControllerSeed))
	if err != nil {
		return err
	}

	v.ProofClaims.BodyHash = BodyHash([]byte(v.ProofBody))

	proof, err := SignProof(host, &v.ProofClaims)
	if err != nil {
		return err
	}

	v.SignerPublicKey = EncodePublicKey(PublicKey(signer))
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
		seedA   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		netA    = "testnet-devnet-0"
		domainA = netA + ".ethpandaops.io"
		answerA = `{"serverTime":"2027-01-15T08:00:00Z","wantInventory":false}`
		// The controller seed is another secret than the devnet seed.
		ctrlSeedA = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
		ctrlSeedB = "a controller seed is used as raw UTF-8 bytes too ✓ ünïcödé"
	)

	return []vector{
		{
			Name: "validator-host", Seed: seedA, ControllerSeed: ctrlSeedA,
			Network: netA, Hostname: "lighthouse-geth-1",
			HostKey:     "JvfSWw6E6Y5GTrMknXzjsGp1AZ7zviOKOFKXLWhZ5Lc",
			ProofClaims: proof(domainA),
			ProofBody:   `{"protocolVersion":1}`,
			AnswerBody:  answerA,
		},
		{
			Name: "bootnode-no-validators", Seed: seedA, ControllerSeed: ctrlSeedA,
			Network: netA, Hostname: "bootnode-1",
			HostKey:     "pT38ueA7Qv0512P99l_dCYSH3Bs5vfY8xd-dSUQrkIc",
			ProofClaims: proof(domainA),
			ProofBody:   "",
			AnswerBody:  "",
		},
		{
			Name:           "other-network",
			Seed:           "correct horse battery staple, but at least 32 bytes long!",
			ControllerSeed: ctrlSeedA,
			Network:        "glamsterdam-devnet-8", Hostname: "prysm-besu-1",
			HostKey:     "gXglNnhGaKHc8KWeDEx_YBqGmeivXjhFEXKklV41EHc",
			ProofClaims: proof("glamsterdam-devnet-8.ethpandaops.io"),
			ProofBody:   "non-ASCII body ✓ with a newline\n",
			AnswerBody:  "non-ASCII answer ✓ with a newline\n",
		},
		{
			Name:           "mixed-case-names",
			Seed:           "ünïcödé seed bytes are used as raw UTF-8 input ✓",
			ControllerSeed: ctrlSeedB,
			Network:        "net.with.dots", Hostname: "Host_With-Mixed.Case01",
			HostKey:     "CU__vBsCiYmCDA4sKCsB_80P83ogGMX99efbWwzBeEM",
			ProofClaims: proof("net.with.dots.example"),
			ProofBody:   "x",
			AnswerBody:  "x",
		},
	}
}

func TestDerivationIsolation(t *testing.T) {
	seed := []byte(testSeed)

	signer, err := DeriveSignerKey(seed)
	require.NoError(t, err)

	ctrl, err := DeriveControllerKey(seed)
	require.NoError(t, err)
	assert.False(t, signer.Equal(ctrl))

	again, err := DeriveSignerKey(seed)
	require.NoError(t, err)
	assert.True(t, signer.Equal(again))

	_, err = DeriveSignerKey([]byte("short"))
	require.ErrorIs(t, err, ErrSeedTooShort)
	_, err = DeriveControllerKey([]byte("short"))
	require.ErrorIs(t, err, ErrSeedTooShort)
}

func TestValidateName(t *testing.T) {
	for _, bad := range []string{"", "a/b", "-a", "a b", `a"b`, "ü", strings.Repeat("a", 254)} {
		require.ErrorIs(t, ValidateName(bad), ErrInvalidName, bad)
	}

	require.NoError(t, ValidateName("Host_With-Mixed.Case01"))
}

func TestKeyEncoding(t *testing.T) {
	key, err := GenerateHostKey()
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
