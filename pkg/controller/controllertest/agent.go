package controllertest

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Agent is a host identity.
type Agent struct {
	Name  string
	Creds *identity.Credentials
}

// NewAgent issues a certificate for a new key of a host of the test network.
func NewAgent(t *testing.T, spec identity.CertificateSpec) *Agent {
	t.Helper()

	if spec.Network == "" {
		spec.Network = Network
	}

	key, err := identity.GenerateHostKey()
	require.NoError(t, err)

	cert, err := identity.IssueCertificate([]byte(Seed), identity.PublicKey(key), spec)
	require.NoError(t, err)

	creds, err := identity.NewCredentials(key, cert)
	require.NoError(t, err)

	return &Agent{Name: spec.Hostname, Creds: creds}
}

// CallOpts shape one agent request.
type CallOpts struct {
	// Gzip compresses the body before signing.
	Gzip bool
	// ProofURL overrides the URL the proof names.
	ProofURL string
	// ProofTime overrides the proof's issue time.
	ProofTime time.Time
	// Mutate changes the request after it was signed.
	Mutate func(*http.Request)
}

// Gzip compresses raw.
func Gzip(t *testing.T, raw []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

// Request builds a signed agent request; now is the proof's issue time
// unless o names another.
func (a *Agent) Request(t *testing.T, now time.Time, path string, body []byte, o CallOpts) *http.Request {
	t.Helper()

	wire := body
	if o.Gzip {
		wire = Gzip(t, body)
	}

	proofURL := o.ProofURL
	if proofURL == "" {
		proofURL = BaseURL + path
	}

	proofTime := o.ProofTime
	if proofTime.IsZero() {
		proofTime = now
	}

	proof, _, err := a.Creds.Proof(http.MethodPost, proofURL, wire, proofTime)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, BaseURL+path, bytes.NewReader(wire))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderCert, a.Creds.Certificate())
	req.Header.Set(identity.HeaderProof, proof)

	if o.Gzip {
		req.Header.Set("Content-Encoding", "gzip")
	}

	if o.Mutate != nil {
		o.Mutate(req)
	}

	return req
}

// SyncRequest is a small valid sync request.
func SyncRequest() *protocol.SyncRequest {
	return &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    "agent-1.0.0",
		SentAt:          Epoch,
		BootID:          "boot-1",
		UptimeSeconds:   3600,
		Verbs:           []protocol.Verb{protocol.VerbRoll, protocol.VerbReboot},
	}
}

// InventoryFor returns an inventory with a correct hash.
func InventoryFor(t *testing.T, pkgs ...protocol.Package) *protocol.Inventory {
	t.Helper()

	inv := &protocol.Inventory{
		CollectedAt: Epoch,
		OS: protocol.OSInfo{
			ID: "debian", VersionID: "13", Codename: "trixie",
			PrettyName: "Debian GNU/Linux 13 (trixie)", Arch: "amd64",
		},
		Kernels: protocol.KernelInfo{
			Running: "6.12.41+deb13-amd64", Newest: "6.12.48+deb13-amd64",
			Installed: []string{"6.12.41+deb13-amd64", "6.12.48+deb13-amd64"},
		},
		Packages:   pkgs,
		Upgradable: []protocol.Upgradable{},
	}

	hash, err := inv.ComputeHash()
	require.NoError(t, err)

	inv.Hash = hash

	return inv
}

// ValidatorAgent is the agent of lighthouse-geth-1, a validator host.
func ValidatorAgent(t *testing.T) *Agent {
	t.Helper()

	return NewAgent(t, identity.CertificateSpec{
		Hostname: "lighthouse-geth-1",
	})
}
