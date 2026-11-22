package agenttest

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// post sends body to the controller as an agent does (gzip, certificate,
// proof) and returns the status and the answer as verified with the
// controller key (nil unless 200).
func post(t *testing.T, c *Controller, path string, body any) (int, []byte) {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	var wire bytes.Buffer

	zw := gzip.NewWriter(&wire)
	_, err = zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	creds := Credentials(t)
	url := c.URL() + path

	proof, id, err := creds.Proof(http.MethodPost, url, wire.Bytes(), time.Now())
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, &wire)
	require.NoError(t, err)
	req.Header.Set(identity.HeaderCert, creds.Certificate())
	req.Header.Set(identity.HeaderProof, proof)
	req.Header.Set("Content-Encoding", "gzip")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}

	key, err := identity.DecodePublicKey(ControllerPublicKey(t))
	require.NoError(t, err)

	verifier, err := identity.NewAnswerVerifier(key, Network, HostName)
	require.NoError(t, err)

	answer, err = verifier.Verify(id, answer)
	require.NoError(t, err)

	return resp.StatusCode, answer
}

func syncOnce(t *testing.T, c *Controller, req *protocol.SyncRequest) *protocol.SyncResponse {
	t.Helper()

	status, raw := post(t, c, protocol.PathSync, req)
	require.Equal(t, http.StatusOK, status)

	var resp protocol.SyncResponse
	require.NoError(t, json.Unmarshal(raw, &resp))

	return &resp
}

func TestController(t *testing.T) {
	c := NewController(t, time.Now)
	req := &protocol.SyncRequest{ProtocolVersion: protocol.Version}

	// Direct replaces the directives, and without any clears them.
	c.Direct(protocol.Directive{ID: "d-1", Kind: protocol.DirectiveHold, Holder: protocol.HolderStaff})
	c.Direct(protocol.Directive{ID: "d-2", Kind: protocol.DirectiveHold, Holder: protocol.HolderStaff})
	assert.Equal(t, "d-2", syncOnce(t, c, req).Directives[0].ID)
	assert.Len(t, syncOnce(t, c, req).Directives, 1, "restated, acknowledged or not")

	c.Direct()
	assert.Empty(t, syncOnce(t, c, req).Directives)

	// AutoRelease drops the lease once its operation is reported terminal.
	c.Grant(Lease(time.Now(), "l-1"))
	assert.Equal(t, "l-1", syncOnce(t, c, req).Lease.ID)

	done := &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		Operation:       &protocol.Operation{LeaseID: "l-1", Phase: protocol.PhaseDone},
	}
	resp := syncOnce(t, c, done)
	assert.Nil(t, resp.Lease)
	assert.Equal(t, &protocol.EndedLease{ID: "l-1", State: "released"}, resp.Ended)
	assert.Equal(t, 5, c.SyncCount())

	// What the real controller refuses, the fake refuses.
	other := &protocol.SyncRequest{ProtocolVersion: protocol.Version + 1}
	status, _ := post(t, c, protocol.PathSync, other)
	assert.Equal(t, http.StatusBadRequest, status)

	status, _ = post(t, c, protocol.PathInventory, &protocol.Inventory{Hash: "not-the-hash"})
	assert.Equal(t, http.StatusBadRequest, status)

	// The two rejections would fail the test at cleanup: they were meant.
	c.mu.Lock()
	assert.Len(t, c.rejected, 2)
	c.rejected = nil
	c.mu.Unlock()
}
