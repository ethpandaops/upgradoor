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

// recordSync stores a sync as store.RecordSync does: the host is created or
// its certificate data, status and last-seen time replaced; the hold switch
// stays.
func (s *fakeStore) recordSync(h *fleet.Host) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := *h

	if old := s.hosts[h.Name]; old != nil {
		c.FirstSeen = old.FirstSeen
		c.HoldWanted = old.HoldWanted
		c.HoldReason = old.HoldReason
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

		s.rollouts[r.ID] = cloneRollout(r)
	}

	for _, l := range ch.Leases {
		if _, ok := s.leases[l.ID]; !ok {
			s.leaseSeq = append(s.leaseSeq, l.ID)
		}

		s.leases[l.ID] = cloneLease(l)
	}

	for _, d := range ch.Directives {
		if _, ok := s.directives[d.ID]; !ok {
			s.dirSeq = append(s.dirSeq, d.ID)
		}

		c := *d
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
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.audit) == 0 {
		return fleet.AuditEntry{}
	}

	return s.audit[len(s.audit)-1]
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
