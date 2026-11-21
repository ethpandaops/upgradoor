// Package agenttest runs whole agents for tests: on a fake host, against a
// scripted fake controller that authenticates every request like the real
// one and signs its answers with a controller key.
package agenttest

import (
	"bufio"
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

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/fakehost"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	Seed = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// ControllerSeed is the controller seed: another secret than the
	// devnet seed above.
	ControllerSeed = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	Network        = "testnet-0"
	HostName       = "lighthouse-geth-1"

	GroupExecution = "execution"
	GroupConsensus = "consensus"

	GethImage = "ethpandaops/geth:master"
	TekuImage = "consensys/teku:develop"
	VCImage   = "sigp/lighthouse:stable"

	GethV1 = "sha256:geth-1"
	GethV2 = "sha256:geth-2"
	GethV3 = "sha256:geth-3"
	TekuV1 = "sha256:teku-1"
	TekuV2 = "sha256:teku-2"
	VCV1   = "sha256:vc-1"
	VCV2   = "sha256:vc-2"

	AwaitTimeout = 10 * time.Second
)

// The architecture and the blocked packages of the fake host.
const (
	arch       = "amd64"
	docker     = "docker-ce"
	containerd = "containerd.io"
	dockerV1   = "28.0"
)

// BaseScenario is a host with an EL, a CL, a VC and one container the agent
// does not manage, all up to date.
func BaseScenario() *fakehost.Scenario {
	return &fakehost.Scenario{
		BootID:        "boot",
		OS:            fakehost.OS{ID: "debian", VersionID: "13", PrettyName: "Debian 13", Arch: arch},
		RunningKernel: "6.12.41-amd64",
		NewestKernel:  "6.12.41-amd64",
		Containers: []fakehost.Container{
			{Name: "validator", Image: VCImage, Digest: VCV1, Group: GroupConsensus, Kind: "vc"},
			{Name: "geth", Image: GethImage, Digest: GethV1, Group: GroupExecution, Kind: "el"},
			{Name: "teku", Image: TekuImage, Digest: TekuV1, Group: GroupConsensus, Kind: "cl"},
			{Name: "dora", Image: "ethpandaops/dora:latest", Digest: "sha256:dora-1", Group: protocol.GroupNone},
		},
		Packages: fakehost.Packages{
			Blocked: []string{docker, containerd},
			Installed: []protocol.Package{
				{Name: docker, Version: dockerV1, Arch: arch},
				{Name: containerd, Version: "1.7", Arch: arch},
				{Name: "openssl", Version: "3.5.1", Arch: arch},
			},
		},
	}
}

// Env is one agent under test: a fake host, a fake controller and the
// agent's configuration and state directory.
type Env struct {
	t     *testing.T
	Host  *fakehost.Host
	Ctrl  *Controller
	Cfg   *config.Config
	Creds *identity.Credentials
	Log   *logrus.Logger
	// clockOffset shifts the agent's (and the controller's) clock.
	ClockOffset atomic.Int64
}

func NewEnv(t *testing.T, sc *fakehost.Scenario) *Env {
	t.Helper()

	host, err := fakehost.New(sc)
	require.NoError(t, err)

	e := &Env{t: t, Host: host, Log: logrus.New()}
	e.Log.SetOutput(io.Discard)

	key, err := identity.DeriveHostKey([]byte(Seed), Network, HostName, 0)
	require.NoError(t, err)

	cert, err := identity.IssueCertificate([]byte(Seed), identity.CertificateSpec{
		Network: Network, Hostname: HostName,
	})
	require.NoError(t, err)

	e.Creds, err = identity.NewCredentials(key, cert)
	require.NoError(t, err)

	e.Ctrl = NewController(t, e.Now)

	dir := t.TempDir()

	e.Cfg = &config.Config{
		Controller: config.Controller{
			URL:               e.Ctrl.srv.URL,
			SyncInterval:      15 * time.Millisecond,
			LeaseSyncInterval: 5 * time.Millisecond,
			MinSyncInterval:   5 * time.Millisecond,
			MaxSyncInterval:   15 * time.Millisecond,
			AllowInsecure:     true,
			PublicKeys:        []string{ControllerPublicKey(t, 0)},
		},
		StateDir:      filepath.Join(dir, "state"),
		OverridesFile: filepath.Join(dir, "home", "overrides.yaml"),
		Socket:        config.Socket{Path: SocketPath(t)},
		Verbs:         []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
		Groups: map[string]config.Group{
			GroupExecution: {Overrides: []string{protocol.OverrideImage, protocol.OverrideFlags}},
			GroupConsensus: {Overrides: []string{protocol.OverrideImage, protocol.OverrideFlags}},
		},
		Registry: config.Registry{PollInterval: 20 * time.Millisecond},
		Packages: config.Packages{Blocked: []string{docker, containerd}},
		Verify:   config.Verify{Window: 30 * time.Millisecond, Timeout: 3 * time.Second},
	}
	e.Cfg.ApplyDefaults()

	require.NoError(t, os.MkdirAll(filepath.Dir(e.Cfg.OverridesFile), 0o755))

	return e
}

// SocketPath is a path for a Unix socket that is short enough for one: the
// test's own temporary directory can be longer than a socket address.
func SocketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "ug")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "agent.sock")
}

func (e *Env) Now() time.Time {
	return time.Now().Add(time.Duration(e.ClockOffset.Load()))
}

// NewAgent builds an agent on the env's host and state directory.
func (e *Env) NewAgent() agent.Service {
	e.t.Helper()

	svc, err := agent.New(e.Log, e.Cfg, e.Creds, agent.Deps{
		Runtime:  e.Host,
		Registry: e.Host,
		System:   e.Host,
		Now:      e.Now,
		Version:  "test",
	})
	require.NoError(e.t, err)

	return svc
}

// Start runs an agent until the test ends (or the returned stop is called).
func (e *Env) Start() (stop func()) {
	e.t.Helper()

	svc := e.NewAgent()
	require.NoError(e.t, svc.Start(e.t.Context()))

	var once sync.Once

	stop = func() { once.Do(func() { require.NoError(e.t, svc.Stop()) }) }
	e.t.Cleanup(stop)

	return stop
}

// Deploy writes the deployed block of the overrides file as Ansible does:
// what the containers are deployed as.
func (e *Env) Deploy(containers ...overrides.GeneratedContainer) {
	e.t.Helper()

	require.NoError(e.t, overrides.NewFile(e.Cfg.OverridesFile).Change(func(t *overrides.Text) error {
		t.SetGeneratedBlock(overrides.GeneratedBlock(containers))

		return nil
	}))
}

func (e *Env) StatePath() string {
	return filepath.Join(e.Cfg.StateDir, journal.FileName)
}

// StateOnDisk reads state.json as generic JSON.
func (e *Env) StateOnDisk() map[string]any {
	raw, err := os.ReadFile(e.StatePath())
	if err != nil {
		return nil
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}

	return out
}

// Lease builds a lease that is valid for a minute.
func (e *Env) Lease(id string, needs ...protocol.Need) *protocol.Lease {
	now := e.Now()

	return &protocol.Lease{
		ID: id, GrantedAt: now, ExpiresAt: now.Add(time.Minute), TTLSeconds: 60, Needs: needs,
	}
}

func ImageNeedFor(container, image, from, to string) protocol.Need {
	return protocol.Need{
		ID:   protocol.ImageNeedID(container, to),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: container, Image: image, FromDigest: from, ToDigest: to,
		},
	}
}

func RebootNeed() protocol.Need {
	return protocol.Need{
		ID: protocol.RebootNeedID, Kind: protocol.NeedReboot,
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonFlag}},
	}
}

func DockerUpgrade() []protocol.Upgradable {
	return []protocol.Upgradable{
		{Name: docker, Arch: arch, Current: dockerV1, Candidate: "28.1"},
		{Name: "openssl", Arch: arch, Current: "3.5.1", Candidate: "3.5.2", Security: true},
	}
}

func PackagesNeed() protocol.Need {
	pkgs := []protocol.PackageChange{{Name: docker, From: dockerV1, To: "28.1"}}

	return protocol.Need{
		ID: protocol.PackagesNeedID(pkgs), Kind: protocol.NeedPackages,
		Packages: &protocol.PackagesNeed{Packages: pkgs},
	}
}

// ControllerPublicKey is the public key of the test network's controller key
// of a generation, as it stands in controller.publicKeys.
func ControllerPublicKey(t *testing.T, generation uint64) string {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(ControllerSeed), Network, generation)
	require.NoError(t, err)

	return identity.EncodePublicKey(identity.PublicKey(key))
}

// AnswerSigner signs like the test network's controller of a generation.
func AnswerSigner(t *testing.T, generation uint64) *identity.AnswerSigner {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(ControllerSeed), Network, generation)
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, Network)
	require.NoError(t, err)

	return signer
}

// Controller is a scripted fake controller. It authenticates every request
// the way the real one does (certificate, proof over method, URL and the
// body bytes on the wire), answers from its settable state and signs the
// answer with the controller key when the agent asks for that.
type Controller struct {
	t        *testing.T
	srv      *httptest.Server
	verifier *identity.Verifier
	now      func() time.Time

	mu          sync.Mutex
	syncs       []protocol.SyncRequest
	inventories []protocol.Inventory
	rejected    []string

	Lease         *protocol.Lease
	Directives    []protocol.Directive
	WantInventory bool
	// autoRelease stops restating the lease once its operation is reported
	// terminal.
	AutoRelease bool
	// ended and queue are what an answer without a lease says about the
	// host's last lease and its place in the queue.
	Ended *protocol.EndedLease
	Queue *protocol.Queue
	// down answers 503 to everything.
	Down bool
	// gate, if set, is called with every sync before it is answered.
	Gate func(req *protocol.SyncRequest)
	// noLookups hands the host no lookup duty; noPulls grants it no pull
	// window; relay is what the answers relay of other hosts' lookups.
	NoLookups bool
	NoPulls   bool
	Relay     []protocol.TagDigest

	// signer signs the answers an agent asks to be signed; nil answers
	// unsigned whatever the agent asks, like a controller without a key.
	Signer *identity.AnswerSigner
	// forge, if set, replaces how a sync answer is put on the wire: it gets
	// the answer's JSON, the host and the id of the request's proof and
	// returns the body to send. It is how the tests play a party that is not
	// the controller.
	Forge func(answer []byte, host, proofID string) []byte
}

func NewController(t *testing.T, now func() time.Time) *Controller {
	t.Helper()

	signer, err := identity.DeriveSignerKey([]byte(Seed), Network)
	require.NoError(t, err)

	c := &Controller{
		t:           t,
		now:         now,
		verifier:    identity.NewVerifier(identity.PublicKey(signer), Network, identity.WithClock(now)),
		AutoRelease: true,
		Signer:      AnswerSigner(t, 0),
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

// answerSync records a sync and builds the response; c.mu is held.
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
	}

	if c.Lease != nil {
		l := *c.Lease
		resp.Lease = &l
	} else {
		resp.Ended = c.Ended
		resp.Queue = c.Queue
	}

	// Unless a test says otherwise, the host looks up every tag it runs on
	// every sync and may pull whatever its needs wait for.
	if !c.NoLookups {
		for i := range req.Containers {
			if ctr := &req.Containers[i]; ctr.Managed {
				resp.Lookups = append(resp.Lookups, ctr.Image)
			}
		}
	}

	if !c.NoPulls {
		window := &protocol.PullWindow{}

		for _, n := range req.Needs {
			window.Images = append(window.Images, n.Unpulled...)
		}

		if len(window.Images) > 0 {
			resp.Pull = window
		}
	}

	resp.Digests = c.Relay

	return resp
}

func (c *Controller) Set(fn func(c *Controller)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	fn(c)
}

func (c *Controller) Grant(l *protocol.Lease) {
	c.Set(func(c *Controller) { c.Lease = l })
}

func (c *Controller) Direct(d ...protocol.Directive) {
	c.Set(func(c *Controller) { c.Directives = d })
}

// Inventories returns every inventory uploaded so far.
func (c *Controller) Inventories() []protocol.Inventory {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.inventories)
}

// SyncCount is the number of syncs received so far.
func (c *Controller) SyncCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.syncs)
}

// AllSyncs returns every sync received so far.
func (c *Controller) AllSyncs() []protocol.SyncRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.syncs)
}

// Await waits for a sync, received after the call, that satisfies pred.
func (c *Controller) Await(what string, pred func(req *protocol.SyncRequest) bool) protocol.SyncRequest {
	c.t.Helper()

	next := c.SyncCount()
	deadline := time.Now().Add(AwaitTimeout)

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

			if all := c.AllSyncs(); len(all) > 0 {
				raw, _ := json.MarshalIndent(all[len(all)-1], "", " ")
				last = string(raw)
			}

			require.FailNow(c.t, "no sync with: "+what, "last sync: %s", last)
		}

		time.Sleep(2 * time.Millisecond)
	}
}

// AwaitSyncs waits for n more syncs and returns the last one.
func (c *Controller) AwaitSyncs(n int) protocol.SyncRequest {
	c.t.Helper()

	seen := 0

	return c.Await("any", func(*protocol.SyncRequest) bool {
		seen++

		return seen >= n
	})
}

// AwaitFinished waits until the operation of a lease was reported terminal
// (and with autoRelease, the lease released) and returns it.
func (c *Controller) AwaitFinished(leaseID string) protocol.Operation {
	c.t.Helper()

	req := c.Await("terminal operation for lease "+leaseID, func(req *protocol.SyncRequest) bool {
		return req.Operation != nil && req.Operation.LeaseID == leaseID &&
			req.Operation.Phase.Terminal()
	})

	return *req.Operation
}

func NeedIDs(req *protocol.SyncRequest) []string {
	ids := make([]string, 0, len(req.Needs))
	for _, n := range req.Needs {
		ids = append(ids, n.ID)
	}

	return ids
}

// HasPulledNeed reports whether the need is reported and its image is local.
func HasPulledNeed(req *protocol.SyncRequest, id string) bool {
	return slices.ContainsFunc(req.Needs, func(n protocol.Need) bool { return n.ID == id && len(n.Unpulled) == 0 })
}

func HasNeed(req *protocol.SyncRequest, id string) bool {
	return slices.Contains(NeedIDs(req), id)
}

func FindNeed(t *testing.T, req *protocol.SyncRequest, id string) protocol.Need {
	t.Helper()

	for _, n := range req.Needs {
		if n.ID == id {
			return n
		}
	}

	require.FailNow(t, "need not reported", "%s not in %v", id, NeedIDs(req))

	return protocol.Need{}
}

// ContainerOf returns a container of a sync.
func ContainerOf(t *testing.T, req *protocol.SyncRequest, name string) protocol.Container {
	t.Helper()

	for i := range req.Containers {
		if req.Containers[i].Name == name {
			return req.Containers[i]
		}
	}

	require.FailNow(t, "container not reported", name)

	return protocol.Container{}
}

// StepOf returns the step of a need in a reported operation.
//
//nolint:gocritic // hugeParam: the tests hand in the operation as Await returned it
func StepOf(t *testing.T, op protocol.Operation, needID string) protocol.Step {
	t.Helper()

	for _, s := range op.Steps {
		if s.NeedID == needID {
			return s
		}
	}

	require.FailNow(t, "step not reported", needID)

	return protocol.Step{}
}

func AckOf(req *protocol.SyncRequest, id string) *protocol.DirectiveAck {
	for i := range req.DirectiveAcks {
		if req.DirectiveAcks[i].ID == id {
			return &req.DirectiveAcks[i]
		}
	}

	return nil
}

// Disruptive filters the host's calls down to those that change containers
// or the system, leaving out image pulls.
func Disruptive(calls []string) []string {
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
const parkLimit = 3 * AwaitTimeout

// BlockIn parks the first host call with the given prefix until the returned
// release is called; entered is closed once the call is parked. A test that
// has failed releases the call by itself, so that the agent can be stopped
// and the failure is the one that is reported.
func BlockIn(e *Env, prefix string) (entered <-chan struct{}, release func()) {
	var (
		parked atomic.Bool
		in     = make(chan struct{})
		out    = make(chan struct{})
		opened atomic.Bool
	)

	e.Host.OnCall(func(call string) {
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

// AwaitParked waits until the agent is inside the call that BlockIn parks,
// and fails by name when it never gets there.
func AwaitParked(t *testing.T, entered <-chan struct{}, call string) {
	t.Helper()

	select {
	case <-entered:
	case <-time.After(AwaitTimeout):
		require.FailNow(t, "the agent never began the call: "+call)
	}
}

// Cancel restates the controller's lease with the cancel mark and waits until
// the agent has taken it in.
func Cancel(t *testing.T, e *Env) {
	t.Helper()

	e.Ctrl.Set(func(c *Controller) {
		l := *c.Lease
		l.Cancel = true
		c.Lease = &l
	})

	require.Eventually(t, func() bool {
		cur, ok := e.StateOnDisk()["current"].(map[string]any)

		return ok && cur["cancelled"] == true
	}, AwaitTimeout, 2*e.Cfg.Controller.LeaseSyncInterval,
		"the agent did not take in that its lease was cancelled (service.handleLease, service.cancelLease)")
}

// CallsAfter returns the disruptive calls that follow the first one with the
// prefix.
func CallsAfter(calls []string, prefix string) []string {
	for i, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return calls[i+1:]
		}
	}

	return nil
}

func AssertAllRunning(t *testing.T, e *Env) {
	t.Helper()

	for _, name := range []string{"geth", "teku", "validator"} {
		assert.Equal(t, protocol.StateRunning, e.Host.Container(name).State, name)
	}
}

// Field walks nested JSON objects.
func Field(t *testing.T, doc map[string]any, path ...string) any {
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

// AwaitNeeds waits for a sync that reports all the given needs and returns
// them as the agent reported them, in the given order.
func AwaitNeeds(t *testing.T, e *Env, ids ...string) []protocol.Need {
	t.Helper()

	req := e.Ctrl.Await("needs "+strings.Join(ids, ", "), func(req *protocol.SyncRequest) bool {
		for _, id := range ids {
			if !HasPulledNeed(req, id) {
				return false
			}
		}

		return true
	})

	out := make([]protocol.Need, 0, len(ids))
	for _, id := range ids {
		out = append(out, FindNeed(t, &req, id))
	}

	return out
}

// AwaitLeaseOnDisk waits until the agent has persisted a lease.
func AwaitLeaseOnDisk(t *testing.T, e *Env, id string) {
	t.Helper()

	require.Eventually(t, func() bool {
		cur, ok := e.StateOnDisk()["current"].(map[string]any)
		if !ok {
			return false
		}

		lease, ok := cur["lease"].(map[string]any)

		return ok && lease["id"] == id
	}, AwaitTimeout, 2*time.Millisecond)
}

// GrantUnexamined grants a lease while the host cannot be examined (Docker
// answers with the given error) and returns once the agent holds the lease:
// persisted, and not started. The caller lets the host answer again with
// e.Host.Fail("observe", nil).
func GrantUnexamined(t *testing.T, e *Env, daemonError string, lease *protocol.Lease) {
	t.Helper()

	e.Host.Fail("observe", errors.New(daemonError))
	e.Ctrl.Grant(lease)
	AwaitLeaseOnDisk(t, e, lease.ID)
}

// The override image of NewOverrideEnv and its digests.
const (
	FixImage = "ethpandaops/geth:fix"
	FixV1    = "sha256:fix-1"
	FixV2    = "sha256:fix-2"
)

// NewOverrideEnv is an agent on a host whose geth has a command line, with the
// override image in the registry.
func NewOverrideEnv(t *testing.T) *Env {
	t.Helper()

	sc := BaseScenario()
	sc.Containers[1].Command = []string{"--http", "--maxpeers=50"}

	e := NewEnv(t, sc)
	e.Host.SetRegistry(FixImage, FixV1, "rev-fix")

	return e
}

// WriteOverrides replaces the overrides file.
func (e *Env) WriteOverrides(content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(e.Cfg.OverridesFile, []byte(content), 0o600))
}

// ReadOverrides reads the overrides file.
func (e *Env) ReadOverrides() string {
	e.t.Helper()

	raw, err := os.ReadFile(e.Cfg.OverridesFile)
	require.NoError(e.t, err)

	return string(raw)
}

// AuditRecords reads the host's audit log, oldest first.
func (e *Env) AuditRecords() []auditlog.Record {
	e.t.Helper()

	f, err := os.Open(e.Cfg.AuditLog.File)
	if err != nil {
		return nil
	}

	defer func() { _ = f.Close() }()

	var out []auditlog.Record

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec auditlog.Record
		require.NoError(e.t, json.Unmarshal(sc.Bytes(), &rec))
		out = append(out, rec)
	}

	return out
}

// AuditActions are the actions of audit records with their results.
func AuditActions(records []auditlog.Record) []string {
	out := make([]string, 0, len(records))
	for i := range records {
		out = append(out, records[i].Action+" "+records[i].Result)
	}

	return out
}

// AwaitOverrideNeed waits for the override need of a container.
func AwaitOverrideNeed(t *testing.T, e *Env, container string, restore bool) protocol.Need {
	t.Helper()

	var found protocol.Need

	e.Ctrl.Await("an override need for "+container, func(req *protocol.SyncRequest) bool {
		for _, n := range req.Needs {
			if n.Kind == protocol.NeedOverride && n.Override.Container == container && len(n.Unpulled) == 0 &&
				n.Override.Restore == restore {
				found = n

				return true
			}
		}

		return false
	})

	return found
}
