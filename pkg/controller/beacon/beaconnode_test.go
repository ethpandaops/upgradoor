package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// Test chain: 12 s slots, 8 slots per epoch, so an epoch lasts 96 s.
const (
	testSlot          = 12 * time.Second
	testSlotsPerEpoch = 8
	testEpoch         = testSlot * testSlotsPerEpoch

	livenessPrefix = "/eth/v1/validator/liveness/"
)

var testGenesis = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// at returns the wall-clock time offset into epoch of the test chain.
func at(epoch uint64, offset time.Duration) time.Time {
	return testGenesis.Add(time.Duration(epoch)*testEpoch + offset)
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// livenessCall is one liveness request a fake node received.
type livenessCall struct {
	epoch   uint64
	indices []uint64
}

// fakeNode is a beacon node on httptest with controllable answers.
type fakeNode struct {
	srv *httptest.Server

	mu        sync.Mutex
	genesis   time.Time
	spec      map[string]any
	headSlot  uint64
	finalized uint64
	// down answers 503 to everything.
	down bool
	// failPath answers 500 for one path.
	failPath string
	// block, when set, holds every request until it is closed or the client
	// gives up.
	block chan struct{}
	// livenessStatus, when non-zero, is the status of every liveness answer.
	livenessStatus  int
	livenessMessage string
	// minEpoch is the oldest epoch liveness is served for (400 below it).
	minEpoch uint64
	// oldEpochStatus is the status for epochs below minEpoch (400 when zero).
	oldEpochStatus int
	// isLive decides per epoch and validator index.
	isLive func(epoch, index uint64) bool

	requests map[string]int
	total    int
	liveness []livenessCall
}

func newFakeNode(t *testing.T) *fakeNode {
	t.Helper()

	node := &fakeNode{
		genesis: testGenesis,
		spec: map[string]any{
			"SECONDS_PER_SLOT": "12",
			"SLOTS_PER_EPOCH":  strconv.Itoa(testSlotsPerEpoch),
			// Real specs carry values that are not integers.
			"DEPOSIT_CONTRACT_ADDRESS": "0x00000000219ab540356cbb839cbe05303d7705fa",
			"BLOB_SCHEDULE":            []any{map[string]string{"EPOCH": "1"}},
		},
		isLive:   func(uint64, uint64) bool { return true },
		requests: make(map[string]int, 8),
	}

	node.srv = httptest.NewServer(http.HandlerFunc(node.handle))
	t.Cleanup(node.srv.Close)

	return node
}

// set changes the node's behaviour under its lock.
func (n *fakeNode) set(change func(n *fakeNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()

	change(n)
}

// count returns how many requests a path received.
func (n *fakeNode) count(path string) int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.requests[path]
}

// totalRequests returns how many requests the node received.
func (n *fakeNode) totalRequests() int {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.total
}

// livenessCalls returns a copy of the liveness requests received.
func (n *fakeNode) livenessCalls() []livenessCall {
	n.mu.Lock()
	defer n.mu.Unlock()

	return append([]livenessCall(nil), n.liveness...)
}

// livenessEpochs returns the epochs of the liveness requests, in order.
func (n *fakeNode) livenessEpochs() []uint64 {
	calls := n.livenessCalls()
	epochs := make([]uint64, 0, len(calls))

	for _, call := range calls {
		epochs = append(epochs, call.epoch)
	}

	return epochs
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"code": status, "message": message})
}

func (n *fakeNode) handle(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	n.requests[r.URL.Path]++
	n.total++
	block := n.block
	down := n.down
	failPath := n.failPath
	n.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-r.Context().Done():
			return
		}
	}

	if down {
		writeError(w, http.StatusServiceUnavailable, "beacon node is syncing")

		return
	}

	if failPath == r.URL.Path {
		writeError(w, http.StatusInternalServerError, "internal error")

		return
	}

	if strings.HasPrefix(r.URL.Path, livenessPrefix) {
		n.handleLiveness(w, r)

		return
	}

	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")

		return
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	switch r.URL.Path {
	case "/eth/v1/beacon/genesis":
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]string{
			"genesis_time":            strconv.FormatInt(n.genesis.Unix(), 10),
			"genesis_validators_root": "0x00",
		}})
	case "/eth/v1/config/spec":
		writeJSON(w, http.StatusOK, map[string]any{"data": n.spec})
	case "/eth/v1/beacon/headers/head":
		writeJSON(w, http.StatusOK, map[string]any{
			"execution_optimistic": false,
			"data": map[string]any{
				"root":      "0x01",
				"canonical": true,
				"header": map[string]any{
					"message":   map[string]string{"slot": strconv.FormatUint(n.headSlot, 10)},
					"signature": "0x02",
				},
			},
		})
	case "/eth/v1/beacon/states/head/finality_checkpoints":
		epoch := map[string]string{"epoch": strconv.FormatUint(n.finalized, 10), "root": "0x03"}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"previous_justified": epoch,
			"current_justified":  epoch,
			"finalized":          epoch,
		}})
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (n *fakeNode) handleLiveness(w http.ResponseWriter, r *http.Request) {
	epoch, err := strconv.ParseUint(strings.TrimPrefix(r.URL.Path, livenessPrefix), 10, 64)
	if err != nil || r.Method != http.MethodPost {
		writeError(w, http.StatusBadRequest, "bad liveness request")

		return
	}

	var raw []string
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "body is not a list of indices")

		return
	}

	indices := make([]uint64, 0, len(raw))

	for _, text := range raw {
		index, parseErr := strconv.ParseUint(text, 10, 64)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, "index is not a decimal string")

			return
		}

		indices = append(indices, index)
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	n.liveness = append(n.liveness, livenessCall{epoch: epoch, indices: indices})

	if n.livenessStatus != 0 {
		writeError(w, n.livenessStatus, n.livenessMessage)

		return
	}

	if epoch < n.minEpoch {
		status := http.StatusBadRequest
		if n.oldEpochStatus != 0 {
			status = n.oldEpochStatus
		}

		writeError(w, status, "epoch is too far in the past")

		return
	}

	data := make([]map[string]any, 0, len(indices))
	for _, index := range indices {
		data = append(data, map[string]any{
			"index":   strconv.FormatUint(index, 10),
			"is_live": n.isLive(epoch, index),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu    sync.Mutex
	hosts []*fleet.Host
	rows  map[string]fleet.Liveness

	listHostsErr error
	saveErr      error
	listErr      error

	listHostsCalls int
	saveCalls      int
	pruneCalls     []uint64
}

var _ Store = (*fakeStore)(nil)

func newFakeStore(hosts ...*fleet.Host) *fakeStore {
	return &fakeStore{hosts: hosts, rows: make(map[string]fleet.Liveness, 16)}
}

// host builds a host with the validator range [start, end); start == end
// means no validators.
func host(name string, start, end uint64) *fleet.Host {
	h := &fleet.Host{Name: name}
	if end > start {
		h.Validators = &identity.ValidatorRange{Start: start, End: end}
	}

	return h
}

func rowKey(name string, epoch uint64) string {
	return name + "@" + strconv.FormatUint(epoch, 10)
}

func (s *fakeStore) set(change func(s *fakeStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	change(s)
}

func (s *fakeStore) ListHosts(context.Context) ([]*fleet.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.listHostsCalls++

	if s.listHostsErr != nil {
		return nil, s.listHostsErr
	}

	return append([]*fleet.Host(nil), s.hosts...), nil
}

func (s *fakeStore) SaveLiveness(_ context.Context, rows []fleet.Liveness) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.saveCalls++

	if s.saveErr != nil {
		return s.saveErr
	}

	for _, row := range rows {
		s.rows[rowKey(row.Host, row.Epoch)] = row
	}

	return nil
}

func (s *fakeStore) ListLiveness(_ context.Context, epochs int) ([]fleet.Liveness, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listErr != nil {
		return nil, s.listErr
	}

	rows := make([]fleet.Liveness, 0, len(s.rows))
	for _, row := range s.rows {
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Epoch != rows[j].Epoch {
			return rows[i].Epoch > rows[j].Epoch
		}

		return rows[i].Host < rows[j].Host
	})

	distinct := 0

	for i, row := range rows {
		if i == 0 || row.Epoch != rows[i-1].Epoch {
			distinct++
		}

		if distinct > epochs {
			return rows[:i], nil
		}
	}

	return rows, nil
}

func (s *fakeStore) PruneLiveness(_ context.Context, beforeEpoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneCalls = append(s.pruneCalls, beforeEpoch)

	for key, row := range s.rows {
		if row.Epoch < beforeEpoch {
			delete(s.rows, key)
		}
	}

	return nil
}

// row returns the stored row of a host and epoch.
func (s *fakeStore) row(name string, epoch uint64) (fleet.Liveness, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.rows[rowKey(name, epoch)]

	return row, ok
}

// epochs returns the distinct stored epochs, ascending.
func (s *fakeStore) epochs() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[uint64]struct{}, len(s.rows))
	for _, row := range s.rows {
		seen[row.Epoch] = struct{}{}
	}

	epochs := make([]uint64, 0, len(seen))
	for epoch := range seen {
		epochs = append(epochs, epoch)
	}

	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })

	return epochs
}

// harness wires a service to fake nodes, a fake store and a fake clock.
type harness struct {
	nodes []*fakeNode
	node  *fakeNode
	store *fakeStore
	clock *fakeClock
	logs  *logtest.Hook
	log   *logrus.Logger
	opts  Options
	svc   *service
}

// newHarness builds an initialised but not started service against `nodes`
// fake nodes. The clock starts 30 s into epoch 10; tune changes the options
// before the service is built.
func newHarness(t *testing.T, nodes int, store *fakeStore, tune func(o *Options)) *harness {
	t.Helper()

	log, hook := logtest.NewNullLogger()
	log.SetLevel(logrus.DebugLevel)

	h := &harness{
		store: store,
		clock: &fakeClock{now: at(10, 30*time.Second)},
		logs:  hook,
		log:   log,
	}

	endpoints := make([]string, 0, nodes)

	for range nodes {
		node := newFakeNode(t)
		node.set(func(n *fakeNode) {
			n.headSlot = 82
			n.finalized = 8
		})

		h.nodes = append(h.nodes, node)
		endpoints = append(endpoints, node.srv.URL)
	}

	h.node = h.nodes[0]
	h.opts = Options{Endpoints: endpoints, Store: store, Now: h.clock.Now}

	if tune != nil {
		tune(&h.opts)
	}

	h.svc = h.build(t)

	return h
}

// build creates a fresh, initialised service on the harness (a restart).
func (h *harness) build(t *testing.T) *service {
	t.Helper()

	svc, ok := New(h.log, h.opts).(*service)
	require.True(t, ok)
	require.NoError(t, svc.init())

	return svc
}

// warnings returns the logged warnings that contain text.
func (h *harness) warnings(text string) int {
	count := 0

	for _, entry := range h.logs.AllEntries() {
		if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, text) {
			count++
		}
	}

	return count
}

var errStore = errors.New("database is locked")
