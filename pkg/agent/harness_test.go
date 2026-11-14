package agent_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/fakehost"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	testSeed    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testNetwork = "testnet-0"
	testHost    = "lighthouse-geth-1"

	gethImage = "ethpandaops/geth:master"
	tekuImage = "consensys/teku:develop"
	vcImage   = "sigp/lighthouse:stable"

	gethV1 = "sha256:geth-1"
	gethV2 = "sha256:geth-2"
	gethV3 = "sha256:geth-3"
	tekuV1 = "sha256:teku-1"
	tekuV2 = "sha256:teku-2"
	vcV1   = "sha256:vc-1"
	vcV2   = "sha256:vc-2"

	awaitTimeout = 10 * time.Second
)

// baseScenario is a host with an EL, a CL, a VC and one container the agent
// does not manage, all up to date.
func baseScenario() *fakehost.Scenario {
	return &fakehost.Scenario{
		BootID:        "boot",
		OS:            fakehost.OS{ID: "debian", VersionID: "13", PrettyName: "Debian 13", Arch: "amd64"},
		RunningKernel: "6.12.41-amd64",
		NewestKernel:  "6.12.41-amd64",
		Containers: []fakehost.Container{
			{Name: "validator", Image: vcImage, Digest: vcV1},
			{Name: "geth", Image: gethImage, Digest: gethV1},
			{Name: "teku", Image: tekuImage, Digest: tekuV1},
			{Name: "dora", Image: "ethpandaops/dora:latest", Digest: "sha256:dora-1"},
		},
		Packages: fakehost.Packages{
			Blocked: []string{"docker-ce", "containerd.io"},
			Installed: []protocol.Package{
				{Name: "docker-ce", Version: "28.0", Arch: "amd64"},
				{Name: "containerd.io", Version: "1.7", Arch: "amd64"},
				{Name: "openssl", Version: "3.5.1", Arch: "amd64"},
			},
		},
	}
}

// env is one agent under test: a fake host, a fake controller and the
// agent's configuration and state directory.
type env struct {
	t     *testing.T
	host  *fakehost.Host
	ctrl  *controller
	cfg   *agent.Config
	creds *identity.Credentials
	log   *logrus.Logger
	// clockOffset shifts the agent's (and the controller's) clock.
	clockOffset atomic.Int64
}

func newEnv(t *testing.T, sc *fakehost.Scenario) *env {
	t.Helper()

	host, err := fakehost.New(sc)
	require.NoError(t, err)

	e := &env{t: t, host: host, log: logrus.New()}
	e.log.SetOutput(io.Discard)

	key, err := identity.DeriveHostKey([]byte(testSeed), testNetwork, testHost, 0)
	require.NoError(t, err)

	cert, err := identity.IssueCertificate([]byte(testSeed), identity.CertificateSpec{
		Network: testNetwork, Hostname: testHost,
		Validators: &identity.ValidatorRange{Start: 0, End: 100},
	})
	require.NoError(t, err)

	e.creds, err = identity.NewCredentials(key, cert)
	require.NoError(t, err)

	e.ctrl = newController(t, e.now)

	dir := t.TempDir()

	e.cfg = &agent.Config{
		Controller: agent.ControllerConfig{
			URL:               e.ctrl.srv.URL,
			SyncInterval:      15 * time.Millisecond,
			LeaseSyncInterval: 5 * time.Millisecond,
			AllowInsecure:     true,
		},
		HoldFile: filepath.Join(dir, "hold"),
		StateDir: filepath.Join(dir, "state"),
		Verbs:    []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
		Containers: []agent.ManagedContainer{
			// Spelled differently from the container's reference on purpose.
			{Name: "geth", Kind: protocol.KindEL, Image: "docker.io/" + gethImage},
			{Name: "teku", Kind: protocol.KindCL, Image: tekuImage},
			{Name: "validator", Kind: protocol.KindVC, Image: vcImage},
		},
		Registry: agent.RegistryConfig{PollInterval: 20 * time.Millisecond},
		Packages: agent.PackagesConfig{Blocked: []string{"docker-ce", "containerd.io"}},
		Verify:   agent.VerifyConfig{Window: 30 * time.Millisecond, Timeout: 3 * time.Second},
	}
	e.cfg.ApplyDefaults()

	return e
}

func (e *env) now() time.Time {
	return time.Now().Add(time.Duration(e.clockOffset.Load()))
}

// newAgent builds an agent on the env's host and state directory.
func (e *env) newAgent() agent.Service {
	e.t.Helper()

	svc, err := agent.New(e.log, e.cfg, e.creds, agent.Deps{
		Runtime:  e.host,
		Registry: e.host,
		System:   e.host,
		Now:      e.now,
		Version:  "test",
	})
	require.NoError(e.t, err)

	return svc
}

// start runs an agent until the test ends (or the returned stop is called).
func (e *env) start() (stop func()) {
	e.t.Helper()

	svc := e.newAgent()
	require.NoError(e.t, svc.Start(e.t.Context()))

	var once sync.Once

	stop = func() { once.Do(func() { require.NoError(e.t, svc.Stop()) }) }
	e.t.Cleanup(stop)

	return stop
}

func (e *env) statePath() string {
	return filepath.Join(e.cfg.StateDir, agent.StateFileName)
}

// stateOnDisk reads state.json as generic JSON.
func (e *env) stateOnDisk() map[string]any {
	raw, err := os.ReadFile(e.statePath())
	if err != nil {
		return nil
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}

	return out
}

// lease builds a lease that is valid for a minute.
func (e *env) lease(id string, needs ...protocol.Need) *protocol.Lease {
	now := e.now()

	return &protocol.Lease{
		ID: id, GrantedAt: now, ExpiresAt: now.Add(time.Minute), TTLSeconds: 60, Needs: needs,
	}
}

func imageNeedFor(container, image, from, to string) protocol.Need {
	return protocol.Need{
		ID:   protocol.ImageNeedID(container, to),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: container, Image: image, FromDigest: from, ToDigest: to,
		},
	}
}

func rebootNeed() protocol.Need {
	return protocol.Need{
		ID: protocol.RebootNeedID, Kind: protocol.NeedReboot,
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonFlag}},
	}
}

func dockerUpgrade() []protocol.Upgradable {
	return []protocol.Upgradable{
		{Name: "docker-ce", Arch: "amd64", Current: "28.0", Candidate: "28.1"},
		{Name: "openssl", Arch: "amd64", Current: "3.5.1", Candidate: "3.5.2", Security: true},
	}
}

func packagesNeed() protocol.Need {
	pkgs := []protocol.PackageChange{{Name: "docker-ce", From: "28.0", To: "28.1"}}

	return protocol.Need{
		ID: protocol.PackagesNeedID(pkgs), Kind: protocol.NeedPackages,
		Packages: &protocol.PackagesNeed{Packages: pkgs},
	}
}

// controller is a scripted fake controller. It authenticates every request
// the way the real one does (certificate, proof over method, URL and the
// body bytes on the wire) and answers from its settable state.
type controller struct {
	t        *testing.T
	srv      *httptest.Server
	verifier *identity.Verifier
	now      func() time.Time

	mu          sync.Mutex
	syncs       []protocol.SyncRequest
	inventories []protocol.Inventory
	rejected    []string

	lease         *protocol.Lease
	directives    []protocol.Directive
	queue         *protocol.Queue
	wantInventory bool
	// autoRelease stops restating the lease once its operation is reported
	// terminal; the terminal operations are kept in finished.
	autoRelease bool
	finished    []protocol.Operation
	// down answers 503 to everything.
	down bool
	// gate, if set, is called with every sync before it is answered.
	gate func(req *protocol.SyncRequest)
}

func newController(t *testing.T, now func() time.Time) *controller {
	t.Helper()

	signer, err := identity.DeriveSignerKey([]byte(testSeed), testNetwork)
	require.NoError(t, err)

	c := &controller{
		t:           t,
		now:         now,
		verifier:    identity.NewVerifier(identity.PublicKey(signer), testNetwork, identity.WithClock(now)),
		autoRelease: true,
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

func (c *controller) fail(w http.ResponseWriter, status int, msg string) {
	if status != http.StatusServiceUnavailable {
		c.rejected = append(c.rejected, msg)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{Error: msg, ServerTime: c.now()})
}

func (c *controller) serve(w http.ResponseWriter, r *http.Request) {
	wire, err := io.ReadAll(r.Body)

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil {
		c.fail(w, http.StatusBadRequest, "read body: "+err.Error())

		return
	}

	if c.down {
		c.fail(w, http.StatusServiceUnavailable, "controller is down")

		return
	}

	_, err = c.verifier.Verify(
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
		if err := dec.Decode(&req); err != nil {
			c.fail(w, http.StatusBadRequest, "decode sync: "+err.Error())

			return
		}

		// The gate runs without the lock: it may wait for the agent.
		if gate := c.gate; gate != nil {
			c.mu.Unlock()
			gate(&req)
			c.mu.Lock()
		}

		answer = c.answerSync(&req)
	case protocol.PathInventory:
		var inv protocol.Inventory
		if err := dec.Decode(&inv); err != nil {
			c.fail(w, http.StatusBadRequest, "decode inventory: "+err.Error())

			return
		}

		c.inventories = append(c.inventories, inv)
		answer = protocol.InventoryResponse{ServerTime: c.now(), Hash: inv.Hash}
	default:
		c.fail(w, http.StatusNotFound, "unknown path "+r.URL.Path)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// answerSync records a sync and builds the response; c.mu is held.
func (c *controller) answerSync(req *protocol.SyncRequest) *protocol.SyncResponse {
	c.syncs = append(c.syncs, *req)

	if op := req.Operation; c.autoRelease && c.lease != nil && op != nil &&
		op.LeaseID == c.lease.ID && op.Phase.Terminal() {
		c.finished = append(c.finished, *op)
		c.lease = nil
	}

	resp := &protocol.SyncResponse{
		ServerTime:    c.now(),
		Directives:    slices.Clone(c.directives),
		WantInventory: c.wantInventory,
	}

	if c.lease != nil {
		l := *c.lease
		resp.Lease = &l
	}

	if c.queue != nil {
		q := *c.queue
		resp.Queue = &q
	}

	return resp
}

func (c *controller) set(fn func(c *controller)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fn(c)
}

func (c *controller) grant(l *protocol.Lease) {
	c.set(func(c *controller) { c.lease = l })
}

func (c *controller) direct(d ...protocol.Directive) {
	c.set(func(c *controller) { c.directives = d })
}

func (c *controller) syncCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.syncs)
}

// allSyncs returns every sync received so far.
func (c *controller) allSyncs() []protocol.SyncRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.syncs)
}

// await waits for a sync, received after the call, that satisfies pred.
func (c *controller) await(what string, pred func(req *protocol.SyncRequest) bool) protocol.SyncRequest {
	c.t.Helper()

	next := c.syncCount()
	deadline := time.Now().Add(awaitTimeout)

	for {
		// pred runs without the lock: it may fail the test.
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

			if all := c.allSyncs(); len(all) > 0 {
				raw, _ := json.MarshalIndent(all[len(all)-1], "", " ")
				last = string(raw)
			}

			require.FailNow(c.t, "no sync with: "+what, "last sync: %s", last)
		}

		time.Sleep(2 * time.Millisecond)
	}
}

// awaitSyncs waits for n more syncs and returns the last one.
func (c *controller) awaitSyncs(n int) protocol.SyncRequest {
	c.t.Helper()

	seen := 0

	return c.await("any", func(*protocol.SyncRequest) bool {
		seen++

		return seen >= n
	})
}

// awaitFinished waits until the operation of a lease was reported terminal
// (and with autoRelease, the lease released) and returns it.
func (c *controller) awaitFinished(leaseID string) protocol.Operation {
	c.t.Helper()

	req := c.await("terminal operation for lease "+leaseID, func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.LeaseID == leaseID &&
			req.Operation.Phase.Terminal()
	})

	return *req.Operation
}

func needIDs(req *protocol.SyncRequest) []string {
	ids := make([]string, 0, len(req.Needs))
	for _, n := range req.Needs {
		ids = append(ids, n.ID)
	}

	return ids
}

func hasNeed(req *protocol.SyncRequest, id string) bool {
	return slices.Contains(needIDs(req), id)
}

func findNeed(t *testing.T, req *protocol.SyncRequest, id string) protocol.Need {
	t.Helper()

	for _, n := range req.Needs {
		if n.ID == id {
			return n
		}
	}

	require.FailNow(t, "need not reported", "%s not in %v", id, needIDs(req))

	return protocol.Need{}
}

func containerOf(t *testing.T, req *protocol.SyncRequest, name string) protocol.Container {
	t.Helper()

	for _, c := range req.Containers {
		if c.Name == name {
			return c
		}
	}

	require.FailNow(t, "container not reported", name)

	return protocol.Container{}
}

func stepOf(t *testing.T, op protocol.Operation, needID string) protocol.Step {
	t.Helper()

	for _, s := range op.Steps {
		if s.NeedID == needID {
			return s
		}
	}

	require.FailNow(t, "step not reported", needID)

	return protocol.Step{}
}

func ackOf(req *protocol.SyncRequest, id string) *protocol.DirectiveAck {
	for i := range req.DirectiveAcks {
		if req.DirectiveAcks[i].ID == id {
			return &req.DirectiveAcks[i]
		}
	}

	return nil
}

// disruptive filters the host's calls down to those that change containers
// or the system, leaving out image pulls.
func disruptive(calls []string) []string {
	out := make([]string, 0, len(calls))

	for _, c := range calls {
		if !strings.HasPrefix(c, "pull ") {
			out = append(out, c)
		}
	}

	return out
}
