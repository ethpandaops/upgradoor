package agent

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
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

	"github.com/ethpandaops/upgradoor/pkg/agent/fakehost"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	testSeed = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// testControllerSeed is the controller seed: another secret than the
	// devnet seed above.
	testControllerSeed = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testNetwork        = "testnet-0"
	testHost           = "lighthouse-geth-1"

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
	cfg   *Config
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
	})
	require.NoError(t, err)

	e.creds, err = identity.NewCredentials(key, cert)
	require.NoError(t, err)

	e.ctrl = newController(t, e.now)

	dir := t.TempDir()

	e.cfg = &Config{
		Controller: ControllerConfig{
			URL:               e.ctrl.srv.URL,
			SyncInterval:      15 * time.Millisecond,
			LeaseSyncInterval: 5 * time.Millisecond,
			MinSyncInterval:   5 * time.Millisecond,
			MaxSyncInterval:   15 * time.Millisecond,
			AllowInsecure:     true,
			PublicKeys:        []string{controllerPublicKey(t, 0)},
		},
		HoldFile:      filepath.Join(dir, "hold"),
		StateDir:      filepath.Join(dir, "state"),
		OverridesFile: filepath.Join(dir, "home", "overrides.yaml"),
		Socket:        SocketConfig{Path: socketPath(t)},
		Verbs:         []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
		Containers: []ManagedContainer{
			// Spelled differently from the container's reference on purpose.
			{Name: "geth", Kind: protocol.KindEL, Image: "docker.io/" + gethImage},
			{Name: "teku", Kind: protocol.KindCL, Image: tekuImage},
			{Name: "validator", Kind: protocol.KindVC, Image: vcImage},
		},
		Registry: RegistryConfig{PollInterval: 20 * time.Millisecond},
		Packages: PackagesConfig{Blocked: []string{"docker-ce", "containerd.io"}},
		Verify:   VerifyConfig{Window: 30 * time.Millisecond, Timeout: 3 * time.Second},
	}
	e.cfg.ApplyDefaults()

	require.NoError(t, os.MkdirAll(filepath.Dir(e.cfg.OverridesFile), 0o755))

	return e
}

// socketPath is a path for a Unix socket that is short enough for one: the
// test's own temporary directory can be longer than a socket address.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "ug")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "agent.sock")
}

func (e *env) now() time.Time {
	return time.Now().Add(time.Duration(e.clockOffset.Load()))
}

// newAgent builds an agent on the env's host and state directory.
func (e *env) newAgent() Service {
	e.t.Helper()

	svc, err := New(e.log, e.cfg, e.creds, Deps{
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
	return filepath.Join(e.cfg.StateDir, StateFileName)
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

// controllerPublicKey is the public key of the test network's controller key
// of a generation, as it stands in controller.publicKeys.
func controllerPublicKey(t *testing.T, generation uint64) string {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(testControllerSeed), testNetwork, generation)
	require.NoError(t, err)

	return identity.EncodePublicKey(identity.PublicKey(key))
}

// answerSigner signs like the test network's controller of a generation.
func answerSigner(t *testing.T, generation uint64) *identity.AnswerSigner {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(testControllerSeed), testNetwork, generation)
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, testNetwork)
	require.NoError(t, err)

	return signer
}

// controller is a scripted fake controller. It authenticates every request
// the way the real one does (certificate, proof over method, URL and the
// body bytes on the wire), answers from its settable state and signs the
// answer with the controller key when the agent asks for that.
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
	wantInventory bool
	// autoRelease stops restating the lease once its operation is reported
	// terminal.
	autoRelease bool
	// ended and queue are what an answer without a lease says about the
	// host's last lease and its place in the queue.
	ended *protocol.EndedLease
	queue *protocol.Queue
	// down answers 503 to everything.
	down bool
	// gate, if set, is called with every sync before it is answered.
	gate func(req *protocol.SyncRequest)

	// signer signs the answers an agent asks to be signed; nil answers
	// unsigned whatever the agent asks, like a controller without a key.
	signer *identity.AnswerSigner
	// forge, if set, replaces how a sync answer is put on the wire: it gets
	// the answer's JSON, the host and the id of the request's proof and
	// returns the body to send. It is how the tests play a party that is not
	// the controller.
	forge func(answer []byte, host, proofID string) []byte
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
		signer:      answerSigner(t, 0),
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

		// The gate runs without the lock: it may wait for the agent.
		if gate := c.gate; gate != nil {
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
	case c.forge != nil && r.URL.Path == protocol.PathSync:
		body = c.forge(body, cert.Subject, proofID)
	case c.signer != nil:
		if body, err = c.signer.Sign(cert.Subject, proofID, body); err != nil {
			c.fail(w, http.StatusInternalServerError, "sign answer: "+err.Error())

			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

// answerSync records a sync and builds the response; c.mu is held.
func (c *controller) answerSync(req *protocol.SyncRequest) *protocol.SyncResponse {
	c.syncs = append(c.syncs, *req)

	if op := req.Operation; c.autoRelease && c.lease != nil && op != nil &&
		op.LeaseID == c.lease.ID && op.Phase.Terminal() {
		c.ended = &protocol.EndedLease{ID: c.lease.ID, State: "released"}
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
	} else {
		resp.Ended = c.ended
		resp.Queue = c.queue
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

// parkLimit bounds how long a parked host call waits for its release. A test
// that fails while a call is parked is over long before; the limit only keeps
// a forgotten release from holding the agent's Stop until the package's
// timeout.
const parkLimit = 3 * awaitTimeout

// blockIn parks the first host call with the given prefix until the returned
// release is called; entered is closed once the call is parked. A test that
// has failed releases the call by itself, so that the agent can be stopped
// and the failure is the one that is reported.
func blockIn(e *env, prefix string) (entered <-chan struct{}, release func()) {
	var (
		parked atomic.Bool
		in     = make(chan struct{})
		out    = make(chan struct{})
		opened atomic.Bool
	)

	e.host.OnCall(func(call string) {
		if !strings.HasPrefix(call, prefix) || !parked.CompareAndSwap(false, true) {
			return
		}

		close(in)

		limit := time.After(parkLimit)
		tick := time.NewTicker(20 * time.Millisecond)

		defer tick.Stop()

		for {
			select {
			case <-out:
				return
			case <-limit:
				return
			case <-tick.C:
				if e.t.Failed() {
					return
				}
			}
		}
	})

	release = func() {
		if opened.CompareAndSwap(false, true) {
			close(out)
		}
	}

	e.t.Cleanup(release)

	return in, release
}

// awaitParked waits until the agent is inside the call that blockIn parks,
// and fails by name when it never gets there.
func awaitParked(t *testing.T, entered <-chan struct{}, call string) {
	t.Helper()

	select {
	case <-entered:
	case <-time.After(awaitTimeout):
		require.FailNow(t, "the agent never began the call: "+call)
	}
}

// cancel restates the controller's lease with the cancel mark and waits until
// the agent has taken it in.
func cancel(t *testing.T, e *env) {
	t.Helper()

	e.ctrl.set(func(c *controller) {
		l := *c.lease
		l.Cancel = true
		c.lease = &l
	})

	require.Eventually(t, func() bool {
		cur, ok := e.stateOnDisk()["current"].(map[string]any)

		return ok && cur["cancelled"] == true
	}, awaitTimeout, 2*e.cfg.Controller.LeaseSyncInterval,
		"the agent did not take in that its lease was cancelled (service.handleLease, service.cancelLease)")
}

// callsAfter returns the disruptive calls that follow the first one with the
// prefix.
func callsAfter(calls []string, prefix string) []string {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return calls[i+1:]
		}
	}

	return nil
}

func assertAllRunning(t *testing.T, e *env) {
	t.Helper()

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(t, protocol.StateRunning, e.host.Container(name).State, name)
	}
}

// field walks nested JSON objects.
func field(t *testing.T, doc map[string]any, path ...string) any {
	t.Helper()

	var cur any = doc

	for _, key := range path {
		obj, ok := cur.(map[string]any)
		require.True(t, ok, "%v: %q is not in an object", path, key)

		cur, ok = obj[key]
		require.True(t, ok, "%v: no %q", path, key)
	}

	return cur
}

// awaitNeeds waits for a sync that reports all the given needs and returns
// them as the agent reported them, in the given order.
func awaitNeeds(t *testing.T, e *env, ids ...string) []protocol.Need {
	t.Helper()

	req := e.ctrl.await("needs "+strings.Join(ids, ", "), func(req *protocol.SyncRequest) bool {
		for _, id := range ids {
			if !hasNeed(req, id) {
				return false
			}
		}

		return true
	})

	out := make([]protocol.Need, 0, len(ids))
	for _, id := range ids {
		out = append(out, findNeed(t, &req, id))
	}

	return out
}

// awaitLeaseOnDisk waits until the agent has persisted a lease.
func awaitLeaseOnDisk(t *testing.T, e *env, id string) {
	t.Helper()

	require.Eventually(t, func() bool {
		cur, ok := e.stateOnDisk()["current"].(map[string]any)
		if !ok {
			return false
		}

		lease, ok := cur["lease"].(map[string]any)

		return ok && lease["id"] == id
	}, awaitTimeout, 2*time.Millisecond)
}

// grantUnexamined grants a lease while the host cannot be examined (Docker
// answers with the given error) and returns once the agent holds the lease:
// persisted, and not started. The caller lets the host answer again with
// e.host.Fail("observe", nil).
func grantUnexamined(t *testing.T, e *env, daemonError string, lease *protocol.Lease) {
	t.Helper()

	e.host.Fail("observe", errors.New(daemonError))
	e.ctrl.grant(lease)
	awaitLeaseOnDisk(t, e, lease.ID)
}

func quietLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(io.Discard)

	return l
}
