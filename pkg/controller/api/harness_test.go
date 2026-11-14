package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	testNetwork = "testnet-devnet-0"
	testHost    = "upgradoor.testnet.example"
	testBaseURL = "https://" + testHost
	testSeed    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var testEpoch = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu          sync.Mutex
	hosts       map[string]*fleet.Host
	inventories map[string]*protocol.Inventory
	liveness    []fleet.Liveness
	rollouts    []*fleet.Rollout // newest first
	leases      []*fleet.Lease   // newest first
	audit       []fleet.AuditEntry
	packages    []store.PackageMatch

	// err, when set, fails every call.
	err error
	// auditErr fails AppendAudit only.
	auditErr error
	// lastSearch records the last SearchPackages arguments.
	lastSearchQuery string
	lastSearchLimit int
}

var _ Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{
		hosts:       make(map[string]*fleet.Host, 4),
		inventories: make(map[string]*protocol.Inventory, 4),
	}
}

func (f *fakeStore) RecordSync(_ context.Context, sync *store.HostSync) (*store.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	name := sync.Cert.Subject
	res := &store.SyncResult{}

	host, ok := f.hosts[name]
	if !ok {
		host = &fleet.Host{Name: name, FirstSeen: sync.At}
		f.hosts[name] = host
		res.Created = true

		// Like the real store: a new host that reports a controller hold
		// keeps it.
		if sync.Request != nil && sync.Request.Hold.Controller {
			res.HoldAdopted = true
			host.HoldWanted = true
			host.HoldReason = store.HoldReasonAdopted
		}
	} else if host.CertHash != sync.Cert.Hash {
		res.PreviousCertHash = host.CertHash
	}

	host.CertHash = sync.Cert.Hash
	host.Generation = max(host.Generation, sync.Cert.Generation)
	host.Validators = sync.Cert.Validators
	host.Roles = sync.Cert.Roles
	host.LastSeen = sync.At
	host.RemoteAddr = sync.RemoteAddr
	host.Status = sync.Request

	cp := *host
	res.Host = &cp

	return res, nil
}

func (f *fakeStore) GetHost(_ context.Context, name string) (*fleet.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	host, ok := f.hosts[name]
	if !ok {
		return nil, store.ErrNotFound
	}

	cp := *host

	return &cp, nil
}

func (f *fakeStore) ListHosts(_ context.Context) ([]*fleet.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	out := make([]*fleet.Host, 0, len(f.hosts))

	for _, host := range f.hosts {
		cp := *host
		out = append(out, &cp)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

func (f *fakeStore) SaveInventory(_ context.Context, host string, inv *protocol.Inventory) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.err
	}

	f.inventories[host] = inv

	if h, ok := f.hosts[host]; ok {
		h.InventoryHash = inv.Hash
	}

	return nil
}

func (f *fakeStore) GetInventory(_ context.Context, host string) (*protocol.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	inv, ok := f.inventories[host]
	if !ok {
		return nil, store.ErrNotFound
	}

	return inv, nil
}

func (f *fakeStore) SearchPackages(_ context.Context, query string, limit int) (
	[]store.PackageMatch, error,
) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	f.lastSearchQuery, f.lastSearchLimit = query, limit

	out := make([]store.PackageMatch, 0, len(f.packages))

	for _, m := range f.packages {
		if strings.Contains(strings.ToLower(m.Name), strings.ToLower(query)) && len(out) < limit {
			out = append(out, m)
		}
	}

	return out, nil
}

func (f *fakeStore) ListLiveness(_ context.Context, _ int) ([]fleet.Liveness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	return slices.Clone(f.liveness), nil
}

func (f *fakeStore) ListRollouts(_ context.Context, filter store.RolloutFilter) (
	[]*fleet.Rollout, error,
) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	out := make([]*fleet.Rollout, 0, len(f.rollouts))

	for _, r := range f.rollouts {
		if filter.OpenOnly && r.State.Terminal() {
			continue
		}

		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}

		out = append(out, r)
	}

	return out, nil
}

func (f *fakeStore) ListLeases(_ context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	out := make([]*fleet.Lease, 0, len(f.leases))

	for _, l := range f.leases {
		if (filter.OpenOnly && l.State.Terminal()) || (filter.Host != "" && l.Host != filter.Host) {
			continue
		}

		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}

		out = append(out, l)
	}

	return out, nil
}

func (f *fakeStore) AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Like a database driver: a cancelled context fails the write.
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, ok := ctx.Deadline(); !ok {
		return errors.New("audit write without deadline")
	}

	if f.err != nil {
		return f.err
	}

	if f.auditErr != nil {
		return f.auditErr
	}

	for _, e := range entries {
		e.ID = int64(len(f.audit) + 1)
		f.audit = append(f.audit, e)
	}

	return nil
}

func (f *fakeStore) ListAudit(_ context.Context, filter store.AuditFilter) (
	[]fleet.AuditEntry, error,
) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}

	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}

	out := make([]fleet.AuditEntry, 0, limit)

	for i := len(f.audit) - 1; i >= 0 && len(out) < limit; i-- {
		e := f.audit[i]

		if (filter.Target != "" && e.Target != filter.Target) ||
			!strings.HasPrefix(e.Action, filter.ActionPrefix) ||
			(filter.BeforeID > 0 && e.ID >= filter.BeforeID) {
			continue
		}

		out = append(out, e)
	}

	return out, nil
}

func (f *fakeStore) auditEntries() []fleet.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.audit)
}

func (f *fakeStore) host(name string) *fleet.Host {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.hosts[name]
}

func (f *fakeStore) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.err = err
}

// schedulerCall is one recorded scheduler action.
type schedulerCall struct {
	Name  string
	Actor string
	Req   any
}

// fakeScheduler is a recording scheduler.Service.
type fakeScheduler struct {
	mu          sync.Mutex
	plan        *scheduler.Plan
	settings    fleet.Settings
	assignments map[string]*scheduler.Assignment
	calls       []schedulerCall
	nudges      int

	// actionErr fails every action and UpdateSettings; assignmentErr fails
	// Assignment; settingsErr fails Settings.
	actionErr     error
	assignmentErr error
	settingsErr   error
	// result is returned by every action.
	result *scheduler.ActionResult
}

var _ scheduler.Service = (*fakeScheduler)(nil)

func newFakeScheduler() *fakeScheduler {
	return &fakeScheduler{
		settings:    fleet.DefaultSettings(),
		assignments: make(map[string]*scheduler.Assignment, 2),
		result:      &scheduler.ActionResult{Message: "done", Hosts: []string{"a"}},
	}
}

func (f *fakeScheduler) Start(context.Context) error { return nil }
func (f *fakeScheduler) Stop() error                 { return nil }

func (f *fakeScheduler) Nudge() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nudges++
}

func (f *fakeScheduler) Assignment(_ context.Context, host string) (*scheduler.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.assignmentErr != nil {
		return nil, f.assignmentErr
	}

	if a, ok := f.assignments[host]; ok {
		return a, nil
	}

	return &scheduler.Assignment{}, nil
}

func (f *fakeScheduler) Plan() *scheduler.Plan {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.plan
}

func (f *fakeScheduler) Settings(context.Context) (fleet.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.settings, f.settingsErr
}

func (f *fakeScheduler) UpdateSettings(_ context.Context, actor string, s fleet.Settings) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, schedulerCall{Name: "settings", Actor: actor, Req: s})

	if f.actionErr != nil {
		return f.actionErr
	}

	f.settings = s

	return nil
}

func (f *fakeScheduler) record(name, actor string, req any) (*scheduler.ActionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, schedulerCall{Name: name, Actor: actor, Req: req})

	if f.actionErr != nil {
		return nil, f.actionErr
	}

	return f.result, nil
}

func (f *fakeScheduler) Roll(_ context.Context, actor string, req scheduler.RollRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("roll", actor, req)
}

func (f *fakeScheduler) Reboot(_ context.Context, actor string, req scheduler.RebootRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("reboot", actor, req)
}

func (f *fakeScheduler) Hold(_ context.Context, actor string, req scheduler.HoldRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("hold", actor, req)
}

func (f *fakeScheduler) Unhold(_ context.Context, actor string, req scheduler.HoldRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("unhold", actor, req)
}

func (f *fakeScheduler) Pause(_ context.Context, actor string, req scheduler.ScopeRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("pause", actor, req)
}

func (f *fakeScheduler) Resume(_ context.Context, actor string, req scheduler.ScopeRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("resume", actor, req)
}

func (f *fakeScheduler) Skip(_ context.Context, actor string, req scheduler.RolloutHostsRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("skip", actor, req)
}

func (f *fakeScheduler) Retry(_ context.Context, actor string, req scheduler.RolloutHostsRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("retry", actor, req)
}

func (f *fakeScheduler) Abort(_ context.Context, actor string, req scheduler.ScopeRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("abort", actor, req)
}

func (f *fakeScheduler) Forget(_ context.Context, actor string, req scheduler.HoldRequest) (
	*scheduler.ActionResult, error,
) {
	return f.record("forget", actor, req)
}

func (f *fakeScheduler) recorded() []schedulerCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.calls)
}

func (f *fakeScheduler) nudgeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.nudges
}

func (f *fakeScheduler) set(mod func(*fakeScheduler)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	mod(f)
}

// fakeBeacon is a fixed Beacon.
type fakeBeacon struct {
	health fleet.NetworkHealth
}

func (f *fakeBeacon) Health() fleet.NetworkHealth { return f.health }

// harness is a Server over fakes.
type harness struct {
	t      *testing.T
	srv    *Server
	store  *fakeStore
	sched  *fakeScheduler
	beacon *fakeBeacon
	clock  *clock
	reg    *prometheus.Registry
}

// newHarness builds a Server in open mode; mod can change the options.
func newHarness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()

	h := &harness{
		t:      t,
		store:  newFakeStore(),
		sched:  newFakeScheduler(),
		beacon: &fakeBeacon{},
		clock:  &clock{now: testEpoch},
		reg:    prometheus.NewRegistry(),
	}

	signer, err := identity.DeriveSignerKey([]byte(testSeed), testNetwork)
	require.NoError(t, err)

	opts := Options{
		Network: testNetwork,
		Version: "v1.2.3-test",
		Verifier: identity.NewVerifier(identity.PublicKey(signer), testNetwork,
			identity.WithClock(h.clock.Now)),
		Store:           h.store,
		Scheduler:       h.sched,
		Beacon:          h.beacon,
		DefaultSettings: fleet.DefaultSettings(),
		Now:             h.clock.Now,
		Registry:        h.reg,
	}

	if mod != nil {
		mod(&opts)
	}

	log := logrus.New()
	log.SetOutput(io.Discard)

	h.srv, err = New(log, opts)
	require.NoError(t, err)

	return h
}

// start starts the server and stops it at the end of the test.
func (h *harness) start() {
	h.t.Helper()

	require.NoError(h.t, h.srv.Start(context.Background()))
	h.t.Cleanup(func() { require.NoError(h.t, h.srv.Stop()) })
}

// serve runs one request through the handler.
func (h *harness) serve(req *http.Request) *httptest.ResponseRecorder {
	h.t.Helper()

	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)

	return rec
}

// get runs a GET without credentials.
func (h *harness) get(target string) *httptest.ResponseRecorder {
	h.t.Helper()

	return h.serve(httptest.NewRequest(http.MethodGet, testBaseURL+target, http.NoBody))
}

// sendJSON runs a request with a JSON body and no credentials.
func (h *harness) sendJSON(method, target, body string) *httptest.ResponseRecorder {
	h.t.Helper()

	req := httptest.NewRequest(method, testBaseURL+target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	return h.serve(req)
}

// decodeJSON decodes a recorded body strictly into T.
func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	require.Equal(t, contentTypeJSON, rec.Header().Get("Content-Type"), rec.Body.String())

	var v T

	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&v), rec.Body.String())

	return v
}

// testAgent is a host identity.
type testAgent struct {
	name  string
	creds *identity.Credentials
}

// newAgent issues a certificate for a host of the test network.
func newAgent(t *testing.T, spec identity.CertificateSpec) *testAgent {
	t.Helper()

	if spec.Network == "" {
		spec.Network = testNetwork
	}

	seed := []byte(testSeed)

	cert, err := identity.IssueCertificate(seed, spec)
	require.NoError(t, err)

	key, err := identity.DeriveHostKey(seed, spec.Network, spec.Hostname, spec.Generation)
	require.NoError(t, err)

	creds, err := identity.NewCredentials(key, cert)
	require.NoError(t, err)

	return &testAgent{name: spec.Hostname, creds: creds}
}

// agentCallOpts shape one agent request.
type agentCallOpts struct {
	// Gzip compresses the body before signing.
	Gzip bool
	// ProofURL overrides the URL the proof names.
	ProofURL string
	// ProofTime overrides the proof's issue time.
	ProofTime time.Time
	// Mutate changes the request after it was signed.
	Mutate func(*http.Request)
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(raw)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

// agentRequest builds a signed agent request.
func (h *harness) agentRequest(a *testAgent, path string, body []byte, o agentCallOpts) *http.Request {
	h.t.Helper()

	wire := body
	if o.Gzip {
		wire = gzipBytes(h.t, body)
	}

	proofURL := o.ProofURL
	if proofURL == "" {
		proofURL = testBaseURL + path
	}

	proofTime := o.ProofTime
	if proofTime.IsZero() {
		proofTime = h.clock.Now()
	}

	proof, err := a.creds.Proof(http.MethodPost, proofURL, wire, proofTime)
	require.NoError(h.t, err)

	req := httptest.NewRequest(http.MethodPost, testBaseURL+path, bytes.NewReader(wire))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(identity.HeaderCert, a.creds.Certificate())
	req.Header.Set(identity.HeaderProof, proof)

	if o.Gzip {
		req.Header.Set("Content-Encoding", "gzip")
	}

	if o.Mutate != nil {
		o.Mutate(req)
	}

	return req
}

// agentPost signs and serves an agent request with v as JSON body.
func (h *harness) agentPost(a *testAgent, path string, v any, o agentCallOpts) *httptest.ResponseRecorder {
	h.t.Helper()

	body, err := json.Marshal(v)
	require.NoError(h.t, err)

	return h.serve(h.agentRequest(a, path, body, o))
}

// syncRequest is a small valid sync request.
func syncRequest() *protocol.SyncRequest {
	return &protocol.SyncRequest{
		ProtocolVersion: protocol.Version,
		AgentVersion:    "agent-1.0.0",
		SentAt:          testEpoch,
		BootID:          "boot-1",
		UptimeSeconds:   3600,
		Verbs:           []protocol.Verb{protocol.VerbRoll, protocol.VerbReboot},
	}
}

// inventoryFor returns an inventory with a correct hash.
func inventoryFor(t *testing.T, pkgs ...protocol.Package) *protocol.Inventory {
	t.Helper()

	inv := &protocol.Inventory{
		CollectedAt: testEpoch,
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
