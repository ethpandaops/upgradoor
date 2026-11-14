package scheduler

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
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

	hosts      map[string]*fleet.Host
	liveness   map[uint64]map[string]fleet.Liveness
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
	writes     int

	// reads names the rollout and directive listings since it was last
	// cleared, e.g. "rollouts:open" or "directives:pending".
	reads []string
}

var _ Store = (*fakeStore)(nil)

func newFakeStore() *fakeStore {
	return &fakeStore{
		hosts:      map[string]*fleet.Host{},
		liveness:   map[uint64]map[string]fleet.Liveness{},
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
	bare.EndedAt = storedAtPtr(bare.EndedAt)

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

	s.writes++

	return nil
}

// putHost stores a host as a sync would.
func (s *fakeStore) putHost(h *fleet.Host) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := *h
	s.hosts[h.Name] = &c
}

// put stores rollouts directly, whatever failures are injected: rows that
// somebody else wrote (another build).
func (s *fakeStore) put(rollouts ...*fleet.Rollout) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range rollouts {
		if _, ok := s.rollouts[r.ID]; !ok {
			s.rolloutSeq = append(s.rolloutSeq, r.ID)
		}

		s.rollouts[r.ID] = storedRollout(r)
	}
}

// recordSync stores a sync as store.RecordSync does: the host is created or
// its certificate data, status and last-seen time replaced; the hold switch
// stays. A new host that reports a controller hold is created with the hold
// wanted; adopted says so.
func (s *fakeStore) recordSync(h *fleet.Host) (adopted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := *h

	if old := s.hosts[h.Name]; old != nil {
		c.FirstSeen = old.FirstSeen
		c.HoldWanted = old.HoldWanted
		c.HoldReason = old.HoldReason
	} else {
		c.FirstSeen = h.LastSeen

		if h.Status != nil && h.Status.Hold.Controller {
			adopted = true
			c.HoldWanted = true
			c.HoldReason = store.HoldReasonAdopted
		}
	}

	s.hosts[h.Name] = &c

	return adopted
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

// writeCount is the number of writes that succeeded.
func (s *fakeStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.writes
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

func (s *fakeStore) SetHostHold(_ context.Context, name string, wanted bool, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writable(); err != nil {
		return err
	}

	h := s.hosts[name]
	if h == nil {
		return store.ErrNotFound
	}

	h.HoldWanted = wanted
	h.HoldReason = reason

	return nil
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

func (s *fakeStore) ListLiveness(_ context.Context, epochs int) ([]fleet.Liveness, error) {
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

	return out, nil
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
	case f.PendingOnly:
		s.reads = append(s.reads, "directives:pending")
	default:
		s.reads = append(s.reads, "directives:all")
	}

	var out []*fleet.Directive

	for _, id := range s.dirSeq {
		d := s.directives[id]
		if (f.PendingOnly && d.State != fleet.DirectivePending) || (f.Host != "" && d.Host != f.Host) {
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

	for _, r := range ch.Rollouts {
		if _, ok := s.rollouts[r.ID]; !ok {
			s.rolloutSeq = append(s.rolloutSeq, r.ID)
		}

		s.rollouts[r.ID] = storedRollout(r)
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

	for _, e := range s.audit {
		fmt.Fprintf(&b, "%s %s %s %s\n", e.Time.Format(time.RFC3339), e.Actor, e.Action, e.Target)
	}

	return b.String()
}
