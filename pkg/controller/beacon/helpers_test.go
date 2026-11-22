package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

	blockPrefix = "/eth/v2/beacon/blocks/"
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

	// headRoot is the root of the head ("0x01" when empty). blocks are the
	// blocks served by root; the head is served without one, as a block of
	// headSlot that carries nothing and has no parent. committees are the
	// attestation committees per slot, by committee index. syncing and
	// elOffline are what the node says about itself.
	headRoot   string
	blocks     map[string]fakeBlock
	committees map[uint64][][]uint64
	syncing    bool
	elOffline  bool
	// committeesStatus, when set, answers a committees request of an epoch
	// with that status.
	committeesStatus map[uint64]int

	requests map[string]int
	total    int
}

// fakeBlock is a block a fake node serves by root.
type fakeBlock struct {
	slot         uint64
	parent       string
	attestations []map[string]any
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

// sentCounter is a transport that counts the requests the service sends.
// The count moves in the poll goroutine, so it is settled once Stop returned;
// the node's count is not, a request sent before the cancel may still reach
// its handler after that.
type sentCounter struct {
	mu   sync.Mutex
	sent int
}

func (c *sentCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.sent++
	c.mu.Unlock()

	return http.DefaultTransport.RoundTrip(req)
}

func (c *sentCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.sent
}

// countSent makes the harness's service send through a sentCounter.
func (h *harness) countSent() *sentCounter {
	counter := &sentCounter{}
	h.opts.HTTPClient = &http.Client{Transport: counter, Timeout: 2 * requestTimeout}

	return counter
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
				"root":      n.head(),
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
	case "/eth/v1/node/syncing":
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"is_syncing": n.syncing, "el_offline": n.elOffline, "head_slot": strconv.FormatUint(n.headSlot, 10),
		}})
	case "/eth/v1/beacon/states/head/committees":
		n.handleCommittees(w, r)
	default:
		if root, ok := strings.CutPrefix(r.URL.Path, blockPrefix); ok {
			n.handleBlock(w, root)

			return
		}

		writeError(w, http.StatusNotFound, "not found")
	}
}

// head returns the root of the head. The caller holds the lock.
func (n *fakeNode) head() string {
	if n.headRoot == "" {
		return "0x01"
	}

	return n.headRoot
}

// handleBlock serves one block by root. The caller holds the lock.
func (n *fakeNode) handleBlock(w http.ResponseWriter, root string) {
	block, ok := n.blocks[root]
	if !ok {
		if root != n.head() {
			writeError(w, http.StatusNotFound, "block not found")

			return
		}

		block = fakeBlock{slot: n.headSlot, parent: "0x00"}
	}

	attestations := block.attestations
	if attestations == nil {
		attestations = []map[string]any{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version": "fulu",
		"data": map[string]any{
			"message": map[string]any{
				"slot":        strconv.FormatUint(block.slot, 10),
				"parent_root": block.parent,
				"body":        map[string]any{"attestations": attestations, "graffiti": "0x00"},
			},
			"signature": "0x02",
		},
	})
}

// handleCommittees serves the committees of an epoch. The caller holds the
// lock.
func (n *fakeNode) handleCommittees(w http.ResponseWriter, r *http.Request) {
	epoch, err := strconv.ParseUint(r.URL.Query().Get("epoch"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad epoch")

		return
	}

	n.requests[r.URL.Path+"?epoch="+strconv.FormatUint(epoch, 10)]++

	if status := n.committeesStatus[epoch]; status != 0 {
		writeError(w, status, "epoch out of bounds")

		return
	}

	data := make([]map[string]any, 0, testSlotsPerEpoch)

	for slot := epoch * testSlotsPerEpoch; slot < (epoch+1)*testSlotsPerEpoch; slot++ {
		for index, members := range n.committees[slot] {
			validators := make([]string, 0, len(members))
			for _, validator := range members {
				validators = append(validators, strconv.FormatUint(validator, 10))
			}

			data = append(data, map[string]any{
				"index":      strconv.Itoa(index),
				"slot":       strconv.FormatUint(slot, 10),
				"validators": validators,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu    sync.Mutex
	hosts []*fleet.Host

	listHostsErr   error
	listHostsCalls int

	// slots are the stored slot rows; slotSaves counts how often each slot
	// was written, slotPrunes the prune boundaries applied.
	slots       map[uint64]fleet.SlotParticipation
	slotSaves   map[uint64]int
	slotSaveErr error
	slotPrunes  []uint64
}

var _ Store = (*fakeStore)(nil)

func newFakeStore(hosts ...*fleet.Host) *fakeStore {
	return &fakeStore{
		hosts:     hosts,
		slots:     make(map[uint64]fleet.SlotParticipation, 16),
		slotSaves: make(map[uint64]int, 16),
	}
}

// host builds a host with the validator range [start, end); start == end
// means no validators.
func host(name string, start, end uint64) *fleet.Host {
	h := &fleet.Host{Name: name}
	if end > start {
		h.Validators = []identity.ValidatorRange{{Start: start, End: end}}
	}

	return h
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

func (s *fakeStore) SaveSlotParticipation(_ context.Context, rows []fleet.SlotParticipation) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.slotSaveErr != nil {
		return s.slotSaveErr
	}

	for i := range rows {
		s.slots[rows[i].Slot] = rows[i]
		s.slotSaves[rows[i].Slot]++
	}

	return nil
}

func (s *fakeStore) PruneSlotParticipation(_ context.Context, beforeSlot uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.slotPrunes = append(s.slotPrunes, beforeSlot)

	for slot := range s.slots {
		if slot < beforeSlot {
			delete(s.slots, slot)
		}
	}

	return nil
}

// slot returns the stored row of a slot.
func (s *fakeStore) slot(slot uint64) (fleet.SlotParticipation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row, ok := s.slots[slot]

	return row, ok
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
