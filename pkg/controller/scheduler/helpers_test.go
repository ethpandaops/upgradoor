package scheduler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
	"github.com/stretchr/testify/require"
)

// errWrite is what the fake store returns for an injected write failure.
var errWrite = errors.New("fakestore: write failed")

// fakeStore is an in-memory Store. Reads return copies of everything the
// scheduler may change, like a database would; a host's Status and closed
// rollouts are shared and must be treated as immutable.
//
// Rollouts, leases and directives are kept the way the SQLite store gives
// them back (storedRollout, storedLease): through their JSON, with times in
// UTC at millisecond precision and a rollout's members from a table of their
// own. So what a scheduler reads after a restart is not the object the one
// before it wrote, and a decision that hangs on a difference between the two
// (an empty list that comes back as none) shows as a restart that decides
// differently.
type fakeStore struct {
	mu sync.Mutex

	hosts    map[string]*fleet.Host
	liveness map[uint64]map[string]fleet.Liveness
	// attested stands in for the beacon service's attested slots.
	attested   map[string][]uint64
	rollouts   map[string]*fleet.Rollout
	rolloutSeq []string
	leases     map[string]*fleet.Lease
	leaseSeq   []string
	directives map[string]*fleet.Directive
	dirSeq     []string
	settings   *fleet.Settings
	audit      []fleet.AuditEntry

	// failWrites makes every write fail; failNext fails that many writes,
	// after letting failSkip writes through.
	failWrites bool
	failNext   int
	failSkip   int

	// reads names the rollout and directive listings since it was last
	// cleared, e.g. "rollouts:open" or "directives:pending".
	reads []string
}

var _ Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{
		hosts:      map[string]*fleet.Host{},
		liveness:   map[uint64]map[string]fleet.Liveness{},
		attested:   map[string][]uint64{},
		rollouts:   map[string]*fleet.Rollout{},
		leases:     map[string]*fleet.Lease{},
		directives: map[string]*fleet.Directive{},
	}
}

// storedAt is a time as the store's time columns hold it.
func storedAt(t time.Time) time.Time { return time.UnixMilli(t.UnixMilli()).UTC() }

func storedAtPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}

	at := storedAt(*t)

	return &at
}

// viaJSON writes a row as the store's data column holds it and reads it back.
func viaJSON[T any](row *T) *T {
	raw, err := json.Marshal(row)
	if err != nil {
		panic(fmt.Sprintf("fakestore: encode: %v", err))
	}

	out := new(T)
	if err := json.Unmarshal(raw, out); err != nil {
		panic(fmt.Sprintf("fakestore: decode: %v", err))
	}

	return out
}

// storedRollout returns a rollout as store.Commit followed by a read gives it
// back: everything but the members through JSON, the members from their
// table, ordered by host and never nil.
func storedRollout(r *fleet.Rollout) *fleet.Rollout {
	bare := *r
	bare.Hosts = nil
	bare.CreatedAt = storedAt(bare.CreatedAt)
	bare.UpdatedAt = storedAt(bare.UpdatedAt)
	bare.EndedAt = storedAtPtr(bare.EndedAt)

	out := viaJSON(&bare)
	out.Hosts = make([]fleet.RolloutHost, 0, max(len(r.Hosts), 8))

	for i := range r.Hosts {
		m := r.Hosts[i]
		m.UpdatedAt = storedAt(m.UpdatedAt)
		out.Hosts = append(out.Hosts, m)
	}

	slices.SortStableFunc(out.Hosts, func(a, b fleet.RolloutHost) int { return cmp.Compare(a.Host, b.Host) })

	return out
}

// storedLease returns a lease as store.Commit followed by a read gives it
// back.
func storedLease(l *fleet.Lease) *fleet.Lease {
	bare := *l
	bare.GrantedAt = storedAt(bare.GrantedAt)
	bare.ExpiresAt = storedAt(bare.ExpiresAt)
	bare.AgentDoneAt = storedAtPtr(bare.AgentDoneAt)
	bare.SyncedSince = storedAtPtr(bare.SyncedSince)
	bare.SyncedUntil = storedAtPtr(bare.SyncedUntil)
	bare.NoWorseSince = storedAtPtr(bare.NoWorseSince)
	bare.NoWorseUntil = storedAtPtr(bare.NoWorseUntil)
	bare.EndedAt = storedAtPtr(bare.EndedAt)

	if l.Baseline != nil {
		b := *l.Baseline
		b.At = storedAt(b.At)
		bare.Baseline = &b
	}

	if l.Cancelled != nil {
		c := *l.Cancelled
		c.At = storedAt(c.At)
		bare.Cancelled = &c
	}

	if l.Operation != nil {
		op := *l.Operation
		op.StartedAt = storedAt(op.StartedAt)
		op.UpdatedAt = storedAt(op.UpdatedAt)
		bare.Operation = &op
	}

	return viaJSON(&bare)
}

// writable reports an injected failure. The caller holds mu.
func (s *fakeStore) writable() error {
	if s.failWrites {
		return errWrite
	}

	switch {
	case s.failNext > 0 && s.failSkip > 0:
		s.failSkip--
	case s.failNext > 0:
		s.failNext--

		return errWrite
	}

	return nil
}

// putHost stores a host as a sync would.
func (s *fakeStore) putHost(h *fleet.Host) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := *h
	s.hosts[h.Name] = &c
}

// recordSync stores a sync as store.RecordSync does: the host is created or
// its certificate data, status and last-seen time replaced.
func (s *fakeStore) recordSync(h *fleet.Host) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := *h

	if old := s.hosts[h.Name]; old != nil {
		c.FirstSeen = old.FirstSeen
		c.Broken = old.Broken
		c.Unproven = old.Unproven
	} else {
		c.FirstSeen = h.LastSeen
	}

	s.hosts[h.Name] = &c
}

// setFailing injects write failures: every write while all is set, else the
// next n writes.
func (s *fakeStore) setFailing(all bool, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.failWrites = all
	s.failNext = n
}

// failing reports whether a write failure is injected.
func (s *fakeStore) failing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failWrites || s.failNext > 0
}

func (s *fakeStore) host(name string) *fleet.Host {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := s.hosts[name]
	if h == nil {
		return nil
	}

	c := *h

	return &c
}

func (s *fakeStore) putLiveness(rows ...fleet.Liveness) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range rows {
		if s.liveness[r.Epoch] == nil {
			s.liveness[r.Epoch] = map[string]fleet.Liveness{}
		}

		s.liveness[r.Epoch][r.Host] = r
	}

	// Keep the newest epochs only, like PruneLiveness.
	if len(s.liveness) > 16 {
		epochs := make([]uint64, 0, len(s.liveness))
		for e := range s.liveness {
			epochs = append(epochs, e)
		}

		slices.Sort(epochs)

		for _, e := range epochs[:len(epochs)-16] {
			delete(s.liveness, e)
		}
	}
}

func (s *fakeStore) ListHosts(context.Context) ([]*fleet.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*fleet.Host, 0, len(s.hosts))

	for _, h := range s.hosts {
		c := *h
		out = append(out, &c)
	}

	slices.SortFunc(out, func(a, b *fleet.Host) int { return cmp.Compare(a.Name, b.Name) })

	return out, nil
}

func (s *fakeStore) GetHost(_ context.Context, name string) (*fleet.Host, error) {
	if h := s.host(name); h != nil {
		return h, nil
	}

	return nil, store.ErrNotFound
}

func (s *fakeStore) DeleteHost(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writable(); err != nil {
		return err
	}

	delete(s.hosts, name)

	for _, rows := range s.liveness {
		delete(rows, name)
	}

	return nil
}

// attest records an included attestation of the host's validators for slot.
func (s *fakeStore) attest(host string, slot uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	slots := s.attested[host]
	if len(slots) > 0 && slots[len(slots)-1] >= slot {
		return
	}

	next := append(slices.Clone(slots), slot)
	s.attested[host] = next[max(0, len(next)-fleet.MaxReadyAttestations):]
}

// attestedSlots stands in for the beacon service's AttestedSlots.
func (s *fakeStore) attestedSlots() map[string][]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return maps.Clone(s.attested)
}

// listLiveness stands in for the beacon service's Liveness.
func (s *fakeStore) listLiveness(epochs int) []fleet.Liveness {
	s.mu.Lock()
	defer s.mu.Unlock()

	have := make([]uint64, 0, len(s.liveness))
	for e := range s.liveness {
		have = append(have, e)
	}

	slices.Sort(have)
	slices.Reverse(have)

	if len(have) > epochs {
		have = have[:epochs]
	}

	var out []fleet.Liveness

	for _, e := range have {
		names := make([]string, 0, len(s.liveness[e]))
		for n := range s.liveness[e] {
			names = append(names, n)
		}

		slices.Sort(names)

		for _, n := range names {
			out = append(out, s.liveness[e][n])
		}
	}

	return out
}

func (s *fakeStore) ListRollouts(_ context.Context, f store.RolloutFilter) ([]*fleet.Rollout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if f.OpenOnly {
		s.reads = append(s.reads, "rollouts:open")
	} else {
		s.reads = append(s.reads, "rollouts:all")
	}

	var out []*fleet.Rollout

	for i := len(s.rolloutSeq) - 1; i >= 0; i-- {
		r := s.rollouts[s.rolloutSeq[i]]

		switch {
		case r.State.Terminal() && f.OpenOnly:
			continue
		case r.State.Terminal():
			out = append(out, r)
		default:
			out = append(out, cloneRollout(r))
		}

		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}

	return out, nil
}

func (s *fakeStore) GetRollout(_ context.Context, id string) (*fleet.Rollout, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.rollouts[id]
	if r == nil {
		return nil, store.ErrNotFound
	}

	return cloneRollout(r), nil
}

func (s *fakeStore) AbortedKeys(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []string

	for _, id := range s.rolloutSeq {
		if r := s.rollouts[id]; r.State == fleet.RolloutAborted && !slices.Contains(out, r.Key) {
			out = append(out, r.Key)
		}
	}

	return out, nil
}

func (s *fakeStore) ListLeases(_ context.Context, f store.LeaseFilter) ([]*fleet.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*fleet.Lease

	for i := len(s.leaseSeq) - 1; i >= 0; i-- {
		l := s.leases[s.leaseSeq[i]]
		if (f.OpenOnly && l.State.Terminal()) || (f.Host != "" && l.Host != f.Host) {
			continue
		}

		out = append(out, cloneLease(l))

		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}

	return out, nil
}

func (s *fakeStore) GetLease(_ context.Context, id string) (*fleet.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l := s.leases[id]
	if l == nil {
		return nil, store.ErrNotFound
	}

	return cloneLease(l), nil
}

func (s *fakeStore) ListDirectives(_ context.Context, f store.DirectiveFilter) ([]*fleet.Directive, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case f.Host != "":
	case len(f.Rollouts) > 0:
		s.reads = append(s.reads, "directives:rollouts")
	case f.PendingOnly:
		s.reads = append(s.reads, "directives:pending")
	default:
		s.reads = append(s.reads, "directives:all")
	}

	var out []*fleet.Directive

	for _, id := range s.dirSeq {
		d := s.directives[id]
		if (f.PendingOnly && d.State != fleet.DirectivePending) || (f.Host != "" && d.Host != f.Host) ||
			(len(f.Rollouts) > 0 && !slices.Contains(f.Rollouts, d.RolloutID)) {
			continue
		}

		c := *d
		out = append(out, &c)
	}

	return out, nil
}

func (s *fakeStore) Commit(_ context.Context, ch *store.Changes) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writable(); err != nil {
		return err
	}

	// All or nothing, like the transaction of the SQLite store.
	for _, off := range ch.Offline {
		if s.hosts[off.Host] == nil {
			return store.ErrNotFound
		}

		s.hosts[off.Host].Offline = off.Mark
	}

	for _, marks := range ch.Broken {
		if h := s.hosts[marks.Host]; h != nil {
			h.Broken = viaJSON(&marks).Marks
		}
	}

	for _, u := range ch.Unproven {
		if h := s.hosts[u.Host]; h != nil {
			h.Unproven = storedAtPtr(u.At)
		}
	}

	for _, r := range ch.Rollouts {
		if _, ok := s.rollouts[r.ID]; !ok {
			s.rolloutSeq = append(s.rolloutSeq, r.ID)
		}

		s.rollouts[r.ID] = storedRollout(r)
	}

	for _, id := range ch.Dropped {
		delete(s.rollouts, id)
		s.rolloutSeq = slices.DeleteFunc(s.rolloutSeq, func(seq string) bool { return seq == id })
	}

	for _, l := range ch.Leases {
		if _, ok := s.leases[l.ID]; !ok {
			s.leaseSeq = append(s.leaseSeq, l.ID)
		}

		s.leases[l.ID] = storedLease(l)
	}

	for _, d := range ch.Directives {
		if _, ok := s.directives[d.ID]; !ok {
			s.dirSeq = append(s.dirSeq, d.ID)
		}

		c := *d
		c.CreatedAt = storedAt(c.CreatedAt)
		c.UpdatedAt = storedAt(c.UpdatedAt)
		s.directives[d.ID] = &c
	}

	s.appendAudit(ch.Audit)

	return nil
}

func (s *fakeStore) appendAudit(entries []fleet.AuditEntry) {
	for _, e := range entries {
		e.ID = int64(len(s.audit) + 1)
		s.audit = append(s.audit, e)
	}
}

func (s *fakeStore) LoadSettings(_ context.Context, defaults fleet.Settings) (fleet.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.settings != nil {
		return *s.settings, nil
	}

	return defaults, nil
}

func (s *fakeStore) SaveSettings(_ context.Context, _, settings fleet.Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writable(); err != nil {
		return err
	}

	s.settings = &settings

	return nil
}

func (s *fakeStore) AppendAudit(_ context.Context, entries ...fleet.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writable(); err != nil {
		return err
	}

	s.appendAudit(entries)

	return nil
}

// auditActions lists the actions of the audit trail, oldest first.
func (s *fakeStore) auditActions() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.audit))
	for _, e := range s.audit {
		out = append(out, e.Action)
	}

	return out
}

func (s *fakeStore) lastAudit() fleet.AuditEntry {
	return s.lastAuditOf("")
}

// readKinds returns the distinct listings read since reads was cleared, in
// the order of their first use.
func (s *fakeStore) readKinds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	kinds := make([]string, 0, 4)

	for _, r := range s.reads {
		if !slices.Contains(kinds, r) {
			kinds = append(kinds, r)
		}
	}

	return kinds
}

// lastAuditOf returns the newest audit entry with the action; any action
// when it is empty.
func (s *fakeStore) lastAuditOf(action string) fleet.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.audit) - 1; i >= 0; i-- {
		if action == "" || s.audit[i].Action == action {
			return s.audit[i]
		}
	}

	return fleet.AuditEntry{}
}

// snapshot returns copies of all rollouts and leases in creation order and
// of all directives.
func (s *fakeStore) snapshot() ([]*fleet.Rollout, []*fleet.Lease, []*fleet.Directive) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rs := make([]*fleet.Rollout, 0, len(s.rolloutSeq))
	for _, id := range s.rolloutSeq {
		rs = append(rs, cloneRollout(s.rollouts[id]))
	}

	ls := make([]*fleet.Lease, 0, len(s.leaseSeq))
	for _, id := range s.leaseSeq {
		ls = append(ls, cloneLease(s.leases[id]))
	}

	ds := make([]*fleet.Directive, 0, len(s.dirSeq))

	for _, id := range s.dirSeq {
		c := *s.directives[id]
		ds = append(ds, &c)
	}

	return rs, ls, ds
}

// view returns all rollouts, leases and directives in creation order without
// copying them. A committed row is replaced, never changed in place, so the
// result stays what it was; the caller must only read it.
func (s *fakeStore) view() ([]*fleet.Rollout, []*fleet.Lease, []*fleet.Directive) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rs := make([]*fleet.Rollout, 0, len(s.rolloutSeq))
	for _, id := range s.rolloutSeq {
		rs = append(rs, s.rollouts[id])
	}

	ls := make([]*fleet.Lease, 0, len(s.leaseSeq))
	for _, id := range s.leaseSeq {
		ls = append(ls, s.leases[id])
	}

	ds := make([]*fleet.Directive, 0, len(s.dirSeq))
	for _, id := range s.dirSeq {
		ds = append(ds, s.directives[id])
	}

	return rs, ls, ds
}

// clone returns an independent copy of the store.
func (s *fakeStore) clone() *fakeStore {
	rs, ls, ds := s.snapshot()

	s.mu.Lock()
	defer s.mu.Unlock()

	c := newFakeStore()

	for n, h := range s.hosts {
		hc := *h
		c.hosts[n] = &hc
	}

	for e, rows := range s.liveness {
		c.liveness[e] = map[string]fleet.Liveness{}
		for n, r := range rows {
			c.liveness[e][n] = r
		}
	}

	for _, r := range rs {
		c.rollouts[r.ID] = r
		c.rolloutSeq = append(c.rolloutSeq, r.ID)
	}

	for _, l := range ls {
		c.leases[l.ID] = l
		c.leaseSeq = append(c.leaseSeq, l.ID)
	}

	for _, d := range ds {
		c.directives[d.ID] = d
		c.dirSeq = append(c.dirSeq, d.ID)
	}

	if s.settings != nil {
		set := *s.settings
		c.settings = &set
	}

	c.audit = slices.Clone(s.audit)

	return c
}

// dump renders the scheduler's rows for comparing two stores.
func (s *fakeStore) dump() string {
	rs, ls, ds := s.view()

	var b strings.Builder

	for _, v := range []any{rs, ls, ds} {
		raw, err := json.Marshal(v)
		if err != nil {
			panic(fmt.Sprintf("fakestore: dump: %v", err))
		}

		b.Write(raw)
		b.WriteByte('\n')
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// What the scheduler writes about the hosts.
	names := make([]string, 0, len(s.hosts))
	for name := range s.hosts {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if marks := s.hosts[name].Broken; len(marks) > 0 {
			raw, _ := json.Marshal(marks)
			fmt.Fprintf(&b, "%s broken %s\n", name, raw)
		}

		if at := s.hosts[name].Unproven; at != nil {
			fmt.Fprintf(&b, "%s unproven %s\n", name, at.Format(time.RFC3339Nano))
		}
	}

	for _, e := range s.audit {
		fmt.Fprintf(&b, "%s %s %s %s\n", e.Time.Format(time.RFC3339), e.Actor, e.Action, e.Target)
	}

	return b.String()
}

var testGenesis = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	testSecondsPerSlot = 12
	testSlotsPerEpoch  = 32
	testSlot           = testSecondsPerSlot * time.Second
	testEpoch          = testSlotsPerEpoch * testSlot

	imgLighthouse = "sigp/lighthouse:unstable"
	imgGeth       = "ethereum/client-go:latest"
	digestOld     = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	digestA       = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB       = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC       = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

var allVerbs = []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot}

// worldBuilder builds small hand-made worlds and carries the outcome of
// each reconcile into the next one, like the store would.
type worldBuilder struct {
	t             *testing.T
	w             *World
	ids           int
	nextValidator uint64
	// autoLive adds a fully live row in the newest complete epoch for every
	// validator host that has none, and has every validator host whose
	// liveOverride is not 0 attest in every slot up to the newest settled one.
	autoLive bool
	// liveOverride is what autoLive writes for a host instead of its weight;
	// dutyOverride is the number of its validators with a duty that live
	// writes instead of its weight.
	liveOverride map[string]uint64
	dutyOverride map[string]uint64
	// healthMod changes the beacon view after every refresh.
	healthMod func(n *fleet.NetworkHealth)
	// attestedUpTo is the newest slot fillAttested filled; attestedSome is
	// false before it first did.
	attestedUpTo uint64
	attestedSome bool
	// ended are the leases that ended, in the order in which they did;
	// closedRollouts the rollouts that closed, newest first.
	ended          []*fleet.Lease
	closedRollouts []*fleet.Rollout
}

func newWorldBuilder(t *testing.T) *worldBuilder {
	t.Helper()

	b := &worldBuilder{t: t, autoLive: true, liveOverride: map[string]uint64{}, dutyOverride: map[string]uint64{}}
	settings := fleet.DefaultSettings()
	settings.PolicyReboot = fleet.PolicyAuto

	b.w = &World{
		Now:      testGenesis.Add(100*testEpoch + 10*time.Second),
		Settings: settings,
		NewID: func() string {
			b.ids++

			return fmt.Sprintf("id%03d", b.ids)
		},
	}
	b.refreshHealth()

	return b
}

// refreshHealth makes the beacon view healthy and current.
func (b *worldBuilder) refreshHealth() {
	epoch := uint64(b.w.Now.Sub(testGenesis) / testEpoch)
	b.w.Health = fleet.NetworkHealth{
		UpdatedAt:         b.w.Now,
		GenesisTime:       testGenesis,
		SecondsPerSlot:    testSecondsPerSlot,
		Phases:            fleet.SinglePhase(testGenesis, testSecondsPerSlot*time.Second),
		SlotsPerEpoch:     testSlotsPerEpoch,
		CurrentEpoch:      epoch,
		FinalizedEpoch:    epoch - 2,
		FinalityLagEpochs: 2,
		LivenessKnown:     true,
		LivenessEpoch:     epoch - 1,
	}

	if b.healthMod != nil {
		b.healthMod(&b.w.Health)
	}
}

// host adds a host with weight validators that synced just now.
func (b *worldBuilder) host(name string, weight uint64, roles ...string) *fleet.Host {
	h := &fleet.Host{
		Name:      name,
		Roles:     roles,
		FirstSeen: b.w.Now.Add(-30 * 24 * time.Hour),
		LastSeen:  b.w.Now,
		Status:    &protocol.SyncRequest{ProtocolVersion: protocol.Version, Verbs: slices.Clone(allVerbs)},
	}

	if weight > 0 {
		h.Validators = []identity.ValidatorRange{{Start: b.nextValidator, End: b.nextValidator + weight}}
		b.nextValidator += weight
	}

	b.w.Hosts = append(b.w.Hosts, h)

	return h
}

func (b *worldBuilder) get(name string) *fleet.Host {
	for _, h := range b.w.Hosts {
		if h.Name == name {
			return h
		}
	}

	b.t.Fatalf("no host %s", name)

	return nil
}

// live sets the host's liveness row in an epoch.
func (b *worldBuilder) live(epoch uint64, name string, live uint64) {
	total := b.get(name).Weight()
	if duty, ok := b.dutyOverride[name]; ok {
		total = duty
	}

	b.w.Liveness = slices.DeleteFunc(b.w.Liveness, func(r fleet.Liveness) bool {
		return r.Host == name && r.Epoch == epoch
	})
	b.w.Liveness = append(b.w.Liveness, fleet.Liveness{
		Host: name, Epoch: epoch, Live: live, Total: total,
	})
}

// down makes the host established as offline: none of its validators
// attested in the two newest complete epochs, nor will they in the ones that
// autoLive fills in.
func (b *worldBuilder) down(name string) {
	b.liveOverride[name] = 0
	b.live(b.w.Health.LivenessEpoch, name, 0)
	b.live(b.w.Health.LivenessEpoch-1, name, 0)
}

// advance moves the clock; every host that is not in silent syncs at the new
// time, and has kept syncing with the same status in between: a run of
// healthy syncs that the scheduler has on record for its lease has no gap. (A
// host in silent sends nothing, and its run has the gap.)
func (b *worldBuilder) advance(d time.Duration, silent ...string) {
	b.w.Now = b.w.Now.Add(d)

	for _, h := range b.w.Hosts {
		if !slices.Contains(silent, h.Name) {
			h.LastSeen = b.w.Now
		}
	}

	for _, l := range b.w.Leases {
		if l.SyncedUntil != nil && !slices.Contains(silent, l.Host) {
			until := b.w.Now
			l.SyncedUntil = &until
		}
	}

	b.refreshHealth()
}

// step reconciles once and applies the outcome to the world.
func (b *worldBuilder) step() *Outcome {
	b.t.Helper()
	b.fillLiveness()

	out := Reconcile(b.w)
	applyOutcome(b.w, out)

	for _, l := range out.Leases {
		if l.State.Terminal() {
			b.ended = append(b.ended, l)
		}
	}

	for _, r := range out.Rollouts {
		if r.State.Terminal() {
			b.closedRollouts = append([]*fleet.Rollout{r}, b.closedRollouts...)
		}
	}

	return out
}

// fillLiveness adds the rows of the newest complete epoch that are missing,
// and the attested slots.
func (b *worldBuilder) fillLiveness() {
	if b.autoLive {
		b.fillAttested()
	}

	if b.autoLive && b.w.Health.LivenessKnown {
		epoch := b.w.Health.LivenessEpoch

		for _, h := range b.w.Hosts {
			if h.Weight() == 0 || slices.ContainsFunc(b.w.Liveness, func(r fleet.Liveness) bool {
				return r.Host == h.Name && r.Epoch == epoch
			}) {
				continue
			}

			live := h.Weight()
			if n, ok := b.liveOverride[h.Name]; ok {
				live = n
			}

			b.live(epoch, h.Name, live)
		}
	}
}

// fillAttested has every validator host that attests attest in every slot up
// to two slots before the clock's, as the beacon service settles them.
func (b *worldBuilder) fillAttested() {
	clock, ok := b.w.Health.SlotAt(b.w.Now)
	if !ok || clock < 2 {
		return
	}

	settled := clock - 2

	from := settled - min(settled, fleet.MaxReadyAttestations-1)
	if b.attestedSome {
		from = max(from, b.attestedUpTo+1)
	}

	b.attestedUpTo, b.attestedSome = settled, true

	for _, h := range b.w.Hosts {
		if live, set := b.liveOverride[h.Name]; h.Weight() == 0 || set && live == 0 {
			continue
		}

		for slot := from; slot <= settled; slot++ {
			b.attest(h.Name, slot)
		}
	}
}

// attest records an included attestation of the host's validators for slot,
// keeping the newest fleet.MaxReadyAttestations slots as the beacon service
// does.
func (b *worldBuilder) attest(name string, slot uint64) {
	if b.w.Attested == nil {
		b.w.Attested = map[string][]uint64{}
	}

	slots := slices.Clone(b.w.Attested[name])

	i, found := slices.BinarySearch(slots, slot)
	if found {
		return
	}

	slots = slices.Insert(slots, i, slot)
	if len(slots) > fleet.MaxReadyAttestations {
		slots = slots[len(slots)-fleet.MaxReadyAttestations:]
	}

	b.w.Attested[name] = slots
}

// applyOutcome writes an outcome back into a world.
func applyOutcome(w *World, out *Outcome) {
	for _, r := range out.Rollouts {
		w.Rollouts = slices.DeleteFunc(w.Rollouts, func(x *fleet.Rollout) bool { return x.ID == r.ID })

		if !r.State.Terminal() {
			w.Rollouts = append(w.Rollouts, r)
		}
	}

	for _, id := range out.Dropped {
		w.Rollouts = slices.DeleteFunc(w.Rollouts, func(x *fleet.Rollout) bool { return x.ID == id })
	}

	for _, l := range out.Leases {
		w.Leases = slices.DeleteFunc(w.Leases, func(x *fleet.Lease) bool { return x.ID == l.ID })

		if !l.State.Terminal() {
			w.Leases = append(w.Leases, l)
		}
	}

	for _, d := range out.Directives {
		w.Directives = slices.DeleteFunc(w.Directives, func(x *fleet.Directive) bool { return x.ID == d.ID })
		w.Directives = append(w.Directives, d)
	}

	// As the store does: the broken marks of a host are replaced. The host is
	// replaced, not changed: the pass may still read the old one.
	for _, marks := range out.Broken {
		for i, h := range w.Hosts {
			if h.Name == marks.Host {
				c := *h
				c.Broken = marks.Marks
				w.Hosts[i] = &c
			}
		}
	}

	for _, u := range out.Unproven {
		for i, h := range w.Hosts {
			if h.Name == u.Host {
				c := *h
				c.Unproven = u.At
				w.Hosts[i] = &c
			}
		}
	}
}

// rollout returns the open rollout with the key.
func (b *worldBuilder) rollout(key string) *fleet.Rollout {
	b.t.Helper()

	for _, r := range b.w.Rollouts {
		if r.Key == key {
			return r
		}
	}

	return nil
}

// mustRollout is rollout that fails the test when there is none.
func (b *worldBuilder) mustRollout(key string) *fleet.Rollout {
	b.t.Helper()

	r := b.rollout(key)
	require.NotNil(b.t, r, "no open rollout %s", key)

	return r
}

// closed returns the newest closed rollout with the key.
func (b *worldBuilder) closed(key string) *fleet.Rollout {
	for _, r := range b.closedRollouts {
		if r.Key == key {
			return r
		}
	}

	return nil
}

// lease returns the host's open lease.
func (b *worldBuilder) lease(host string) *fleet.Lease {
	for _, l := range b.w.Leases {
		if l.Host == host {
			return l
		}
	}

	return nil
}

func (b *worldBuilder) leasedHosts() []string {
	out := make([]string, 0, len(b.w.Leases))
	for _, l := range b.w.Leases {
		out = append(out, l.Host)
	}

	slices.Sort(out)

	return out
}

// report makes the host's agent report an operation for its open lease.
func (b *worldBuilder) report(host string, phase protocol.Phase, disrupted bool, statuses ...protocol.StepStatus) {
	b.t.Helper()

	l := b.lease(host)
	require.NotNil(b.t, l, "host %s holds no lease", host)

	op := &protocol.Operation{LeaseID: l.ID, Phase: phase, StartedAt: b.w.Now, UpdatedAt: b.w.Now, Disrupted: disrupted}

	for i, n := range l.Needs {
		status := protocol.StepRunning

		switch {
		case i < len(statuses):
			status = statuses[i]
		case phase == protocol.PhaseDone:
			status = protocol.StepDone
		case phase == protocol.PhaseFailed:
			status = protocol.StepFailed
		}

		op.Steps = append(op.Steps, protocol.Step{NeedID: n.ID, Status: status})
	}

	if phase == protocol.PhaseFailed {
		op.Error = "container did not start"
	}

	h := b.get(host)
	st := *h.Status
	st.Operation = op
	h.Status = &st
}

// finish makes the agent report done and drop the served needs, as an
// agent whose operation took effect does.
func (b *worldBuilder) finish(host string) {
	b.t.Helper()
	b.report(host, protocol.PhaseDone, true)

	h := b.get(host)
	l := b.lease(host)
	h.Status.Needs = slices.DeleteFunc(slices.Clone(h.Status.Needs), func(n protocol.Need) bool {
		return slices.ContainsFunc(l.Needs, func(g protocol.Need) bool { return g.ID == n.ID })
	})
}

// serve walks the host's lease to released: done, a first sync after it,
// then syncs every minute until two epochs have passed (the validators live,
// the node synced throughout).
func (b *worldBuilder) serve(host string) {
	b.t.Helper()
	b.finish(host)
	b.step()
	b.advance(5 * time.Second)
	b.step()
	b.advance(2 * testEpoch)
	b.step()
	require.Nil(b.t, b.lease(host), "lease of %s was not released", host)
}

// lastLease returns the lease of the host that ended last.
func (b *worldBuilder) lastLease(host string) *fleet.Lease {
	b.t.Helper()

	for i := len(b.ended) - 1; i >= 0; i-- {
		if b.ended[i].Host == host {
			return b.ended[i]
		}
	}

	b.t.Fatalf("no lease of %s has ended", host)

	return nil
}

// broke marks the host broken for the group of the need, as a lease that
// failed on it long ago did.
func (b *worldBuilder) broke(host string, need protocol.Need) {
	h := b.get(host)
	marks := maps.Clone(h.Broken)

	if marks == nil {
		marks = make(map[string]string, 1)
	}

	marks[needGroup(&need)] = "broke-" + host
	h.Broken = marks
}

// queue returns the waiting reason per host of a plan.
func queueReasons(p *Plan) map[string]string {
	out := make(map[string]string, len(p.Queue))
	for _, q := range p.Queue {
		out[q.Host] = q.Reason
	}

	return out
}

// testKind is the kind the tests label the containers of an image with:
// one rollout unit per image.
func testKind(image string) protocol.ContainerKind { return protocol.ContainerKind("g-" + image) }

// imageKey is the key of the group rollout of one tag that moved, for the
// needs of containers of a team (none: empty).
func imageKey(team, image, digest string) string {
	return groupKey(testKind(image), team, []fleet.RolloutTarget{{Image: image, Digest: digest}})
}

func imageNeed(container, image, to string) protocol.Need {
	return protocol.Need{
		ID:   protocol.ImageNeedID(container, to),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: container, Kind: testKind(image), Image: image, FromDigest: digestOld, ToDigest: to,
		},
	}
}

func packagesNeed(version string) protocol.Need {
	pkgs := []protocol.PackageChange{{Name: "docker-ce", From: "1.0", To: version}}

	return protocol.Need{
		ID:       protocol.PackagesNeedID(pkgs),
		Kind:     protocol.NeedPackages,
		Packages: &protocol.PackagesNeed{Packages: pkgs},
	}
}

func packagesKey(version string) string {
	return prefixPackages + protocol.PackagesKey(
		[]protocol.PackageChange{{Name: "docker-ce", From: "1.0", To: version}})
}

func rebootNeed() protocol.Need {
	return protocol.Need{
		ID:     protocol.RebootNeedID,
		Kind:   protocol.NeedReboot,
		Reboot: &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonFlag}},
	}
}

// container is a managed container whose registry check saw registryDigest
// at checked.
func container(name, image, running, registryDigest string, checked time.Time) protocol.Container {
	return protocol.Container{
		Name: name, Image: image, Digest: running, State: "running",
		Managed: true, ConfiguredImage: image, Kind: testKind(image), Impact: protocol.ImpactNode,
		Registry: &protocol.RegistryCheck{Digest: registryDigest, CheckedAt: checked, Pulled: true},
	}
}

// wantsImage makes the host run image at digestOld and report a need for
// the digest.
func (b *worldBuilder) wantsImage(host, image, to string) {
	h := b.get(host)
	name := "c-" + image
	h.Status.Containers = append(h.Status.Containers, container(name, image, digestOld, to, b.w.Now))
	h.Status.Needs = append(h.Status.Needs, imageNeed(name, image, to))
}

// wantsKind is wantsImage for a container of a real kind (el, cl, vc).
func (b *worldBuilder) wantsKind(host string, kind protocol.ContainerKind, image, to string) {
	h := b.get(host)
	name := "c-" + string(kind)
	c := container(name, image, digestOld, to, b.w.Now)
	c.Kind = kind
	n := imageNeed(name, image, to)
	n.Image.Kind = kind
	h.Status.Containers = append(h.Status.Containers, c)
	h.Status.Needs = append(h.Status.Needs, n)
}

// kindKey is the key of the group rollout of wantsKind.
func kindKey(kind protocol.ContainerKind, image, digest string) string {
	return groupKey(kind, "", []fleet.RolloutTarget{{Image: image, Digest: digest}})
}

func (b *worldBuilder) wants(host string, needs ...protocol.Need) {
	h := b.get(host)
	h.Status.Needs = append(h.Status.Needs, needs...)
}

// memberStates returns the member state per host of a rollout.
func memberStates(r *fleet.Rollout) map[string]fleet.HostState {
	out := make(map[string]fleet.HostState, len(r.Hosts))
	for i := range r.Hosts {
		out[r.Hosts[i].Host] = r.Hosts[i].State
	}

	return out
}

func auditActions(out *Outcome) []string {
	actions := make([]string, 0, len(out.Audit))
	for _, e := range out.Audit {
		actions = append(actions, e.Action)
	}

	return actions
}

// replaceSettings stores the settings as given, like a patch that names every
// field.
func replaceSettings(ctx context.Context, svc Service, actor string, settings fleet.Settings) error {
	_, _, err := svc.PatchSettings(ctx, actor, func(s *fleet.Settings) error {
		*s = settings

		return nil
	})

	return err
}

// failedOnDigestA builds a fleet in which the host failing rolled to digest A
// and failed, so that the rollout of digest A is halted with it.
func failedOnDigestA(t *testing.T, failing string, hosts map[string]uint64) *worldBuilder {
	t.Helper()

	b := newWorldBuilder(t)

	for _, name := range sortedKeys(hosts) {
		b.host(name, hosts[name])
		b.wantsImage(name, imgLighthouse, digestA)
		if name != failing {
			b.get(name).Status.Hold.Holds = []protocol.HostHold{{Holder: fleet.HolderStaff}}
		}
	}

	b.step()
	require.Equal(t, []string{failing}, b.leasedHosts())
	b.report(failing, protocol.PhaseFailed, true)

	for name := range hosts {
		b.get(name).Status.Hold.Holds = nil
	}

	b.step()
	require.Equal(t, fleet.RolloutHalted, b.mustRollout(imageKey("", imgLighthouse, digestA)).State)
	require.Empty(t, b.leasedHosts())

	return b
}

// seesDigestB makes the hosts report the tag at digest B.
func seesDigestB(b *worldBuilder, hosts ...string) {
	for _, name := range hosts {
		h := b.get(name)
		h.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digestB, b.w.Now)}
		h.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digestB)}
		h.Status.Operation = nil
	}
}

// seesDigest makes the hosts report the tag at a digest while they still run
// the old one.
func seesDigest(b *worldBuilder, digest string, hosts ...string) {
	for _, name := range hosts {
		h := b.get(name)
		h.Status.Containers = []protocol.Container{container("c", imgLighthouse, digestOld, digest, b.w.Now)}
		h.Status.Needs = []protocol.Need{imageNeed("c", imgLighthouse, digest)}
		h.Status.Operation = nil
	}
}
