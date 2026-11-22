package agenttest

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Identities of the test network.
const (
	Seed = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// ControllerSeed is the controller's seed, distinct from Seed.
	ControllerSeed = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	Network        = "testnet-0"
	HostName       = "lighthouse-geth-1"
)

// AwaitTimeout bounds every Await.
const AwaitTimeout = 10 * time.Second

// awaitPoll is how often Await looks at the syncs that arrived.
const awaitPoll = 2 * time.Millisecond

// Controller is a scripted controller on an httptest server. It
// authenticates requests like the real one and answers from its fields,
// which are read under its lock: set them with Set, Grant and Direct while
// an agent runs. A rejected request fails the test at cleanup.
type Controller struct {
	t        *testing.T
	srv      *httptest.Server
	verifier *identity.Verifier
	now      func() time.Time

	mu          sync.Mutex
	syncs       []protocol.SyncRequest
	inventories []protocol.Inventory
	rejected    []string

	// Lease is restated in every answer; with AutoRelease it is dropped once
	// its operation is reported terminal.
	Lease       *protocol.Lease
	AutoRelease bool
	// Directives are restated in every answer, acknowledged or not.
	Directives    []protocol.Directive
	WantInventory bool
	Ended         *protocol.EndedLease
	Queue         *protocol.Queue
	// Down answers 503 to everything.
	Down bool
	// Gate is called with every sync before it is answered.
	Gate func(req *protocol.SyncRequest)
	// Lookup looks every tag of a sync up for its Digests, as the
	// controller's registry lookup (Host.Resolve of the agent's host); nil
	// looks nothing up, and a tag it answers neither digest nor error for is
	// not looked up yet. Relay is answered as Digests before those.
	Lookup func(ctx context.Context, ref string) (string, error)
	Relay  []protocol.TagDigest
	// Signer signs the answers; nil answers unsigned.
	Signer *identity.AnswerSigner
	// Forge rewrites an answer's wire body, to play a party that is not the controller.
	Forge func(answer []byte, host, proofID string) []byte
}

// NewController starts the controller for the test network with
// AutoRelease and the Signer of the controller key; now is its clock.
func NewController(t *testing.T, now func() time.Time) *Controller {
	t.Helper()

	signer, err := identity.DeriveSignerKey([]byte(Seed), Network)
	require.NoError(t, err)

	c := &Controller{
		t:           t,
		now:         now,
		verifier:    identity.NewVerifier(identity.PublicKey(signer), Network, identity.WithClock(now)),
		AutoRelease: true,
		Signer:      AnswerSigner(t, ControllerSeed),
	}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))

	t.Cleanup(func() {
		c.srv.Close()

		c.mu.Lock()
		defer c.mu.Unlock()

		assert.Empty(t, c.rejected, "the controller rejected requests of the agent")
	})

	return c
}

// URL is the controller's base URL (http; the agent needs allowInsecure).
func (c *Controller) URL() string {
	return c.srv.URL
}

// fail answers an error; anything but 503 fails the test at cleanup. c.mu is held.
func (c *Controller) fail(w http.ResponseWriter, status int, msg string) {
	if status != http.StatusServiceUnavailable {
		c.rejected = append(c.rejected, msg)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{Error: msg, ServerTime: c.now()})
}

func (c *Controller) serve(w http.ResponseWriter, r *http.Request) {
	wire, err := io.ReadAll(r.Body)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.fail(w, http.StatusBadRequest, "read body: "+err.Error())

		return
	}

	if c.Down {
		c.fail(w, http.StatusServiceUnavailable, "controller is down")

		return
	}

	cert, proofID, err := c.verifier.Verify(
		r.Header.Get(identity.HeaderCert), r.Header.Get(identity.HeaderProof),
		identity.Request{Method: r.Method, URLs: []string{c.srv.URL + r.URL.Path}, Body: wire},
	)
	if err != nil {
		c.fail(w, http.StatusUnauthorized, "authentication: "+err.Error())

		return
	}

	if r.Header.Get("Content-Encoding") != "gzip" {
		c.fail(w, http.StatusBadRequest, "body is not gzip-encoded")

		return
	}

	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		c.fail(w, http.StatusBadRequest, "gunzip: "+err.Error())

		return
	}

	dec := json.NewDecoder(zr)
	dec.DisallowUnknownFields()

	var answer any

	switch r.URL.Path {
	case protocol.PathSync:
		var req protocol.SyncRequest
		if err = dec.Decode(&req); err != nil {
			c.fail(w, http.StatusBadRequest, "decode sync: "+err.Error())

			return
		}

		// As the real controller: another version is refused.
		if req.ProtocolVersion != protocol.Version {
			c.fail(w, http.StatusBadRequest, fmt.Sprintf(
				"protocol version %d is not supported, the controller speaks %d",
				req.ProtocolVersion, protocol.Version))

			return
		}

		// The gate runs without the lock: it may wait for the agent.
		if gate := c.Gate; gate != nil {
			c.mu.Unlock()
			gate(&req)
			c.mu.Lock()
		}

		answer = c.answerSync(&req)
	case protocol.PathInventory:
		var inv protocol.Inventory
		if err = dec.Decode(&inv); err != nil {
			c.fail(w, http.StatusBadRequest, "decode inventory: "+err.Error())

			return
		}

		// As the real controller: the hash must be the content's.
		hash, hashErr := inv.ComputeHash()
		if hashErr != nil {
			c.fail(w, http.StatusInternalServerError, "hash inventory: "+hashErr.Error())

			return
		}

		if inv.Hash != hash {
			c.fail(w, http.StatusBadRequest, "inventory hash mismatch: the body hashes to "+hash)

			return
		}

		c.inventories = append(c.inventories, inv)
		answer = protocol.InventoryResponse{ServerTime: c.now(), Hash: inv.Hash}
	default:
		c.fail(w, http.StatusNotFound, "unknown path "+r.URL.Path)

		return
	}

	body, err := json.Marshal(answer)
	if err != nil {
		c.fail(w, http.StatusInternalServerError, "encode answer: "+err.Error())

		return
	}

	switch {
	case c.Forge != nil && r.URL.Path == protocol.PathSync:
		body = c.Forge(body, cert.Subject, proofID)
	case c.Signer != nil:
		if body, err = c.Signer.Sign(cert.Subject, proofID, body); err != nil {
			c.fail(w, http.StatusInternalServerError, "sign answer: "+err.Error())

			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) //nolint:gosec // G705: JSON for the agent under test, not a page
}

// answerSync records a sync and builds the answer; c.mu is held.
func (c *Controller) answerSync(req *protocol.SyncRequest) *protocol.SyncResponse {
	c.syncs = append(c.syncs, *req)

	if op := req.Operation; c.AutoRelease && c.Lease != nil && op != nil &&
		op.LeaseID == c.Lease.ID && op.Phase.Terminal() {
		c.Ended = &protocol.EndedLease{ID: c.Lease.ID, State: "released"}
		c.Lease = nil
	}

	resp := &protocol.SyncResponse{
		ServerTime:    c.now(),
		Directives:    slices.Clone(c.Directives),
		WantInventory: c.WantInventory,
		Digests:       slices.Clone(c.Relay),
	}

	if c.Lookup != nil {
		for _, ref := range req.Tags {
			d := protocol.TagDigest{Ref: ref, CheckedAt: c.now()}

			digest, err := c.Lookup(context.Background(), ref)
			if err != nil {
				d.Error = err.Error()
			} else if digest == "" {
				continue
			}

			d.Digest = digest
			resp.Digests = append(resp.Digests, d)
		}
	}

	if c.Lease != nil {
		l := *c.Lease
		resp.Lease = &l
	} else {
		resp.Ended = c.Ended
		resp.Queue = c.Queue
	}

	return resp
}

// Set changes the scripted fields under the controller's lock.
func (c *Controller) Set(fn func(c *Controller)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fn(c)
}

// Grant restates l from the next answer on.
func (c *Controller) Grant(l *protocol.Lease) {
	c.Set(func(c *Controller) { c.Lease = l })
}

// Direct replaces the directives; without any it clears them.
func (c *Controller) Direct(d ...protocol.Directive) {
	c.Set(func(c *Controller) { c.Directives = d })
}

// Await waits for a sync arriving after the call that pred accepts; pred
// runs without the lock. On a timeout the test fails with the last sync.
func (c *Controller) Await(
	what string, pred func(req *protocol.SyncRequest) bool,
) protocol.SyncRequest {
	c.t.Helper()

	next := c.SyncCount()
	deadline := time.Now().Add(AwaitTimeout)

	for {
		c.mu.Lock()
		fresh := slices.Clone(c.syncs[next:])
		c.mu.Unlock()

		for i := range fresh {
			if pred(&fresh[i]) {
				return fresh[i]
			}
		}

		next += len(fresh)

		if time.Now().After(deadline) {
			last := "none"

			if all := c.Syncs(); len(all) > 0 {
				raw, _ := json.MarshalIndent(all[len(all)-1], "", " ")
				last = string(raw)
			}

			require.FailNow(c.t, "no sync with: "+what, "last sync: %s", last)
		}

		time.Sleep(awaitPoll)
	}
}

// AwaitSyncs waits until n more syncs have arrived and returns the last.
func (c *Controller) AwaitSyncs(n int) protocol.SyncRequest {
	c.t.Helper()

	seen := 0

	return c.Await("any", func(*protocol.SyncRequest) bool {
		seen++

		return seen >= n
	})
}

// SyncCount is the number of syncs so far.
func (c *Controller) SyncCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.syncs)
}

// AwaitFinished waits for the terminal operation of a lease.
func (c *Controller) AwaitFinished(leaseID string) protocol.Operation {
	c.t.Helper()

	req := c.Await("terminal operation for lease "+leaseID, func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.LeaseID == leaseID &&
			req.Operation.Phase.Terminal()
	})

	return *req.Operation
}

// Syncs returns the sync requests so far.
func (c *Controller) Syncs() []protocol.SyncRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.syncs)
}

// Inventories returns the inventory uploads so far.
func (c *Controller) Inventories() []protocol.Inventory {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.inventories)
}

// Credentials are the host key and certificate of HostName in Network.
func Credentials(t *testing.T) *identity.Credentials {
	t.Helper()

	key, err := identity.GenerateHostKey()
	require.NoError(t, err)

	cert, err := identity.IssueCertificate([]byte(Seed), identity.PublicKey(key), identity.CertificateSpec{
		Network: Network, Hostname: HostName,
	})
	require.NoError(t, err)

	creds, err := identity.NewCredentials(key, cert)
	require.NoError(t, err)

	return creds
}

// ControllerPublicKey is the controller public key, as in controller.publicKey.
func ControllerPublicKey(t *testing.T) string {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(ControllerSeed), Network)
	require.NoError(t, err)

	return identity.EncodePublicKey(identity.PublicKey(key))
}

// AnswerSigner signs like the controller key derived from controllerSeed.
func AnswerSigner(t *testing.T, controllerSeed string) *identity.AnswerSigner {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(controllerSeed), Network)
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, Network)
	require.NoError(t, err)

	return signer
}
