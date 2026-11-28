package controllertest

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Store is an in-memory store with what the controller's API reads and
// writes. Lock it to change its fields while a test runs.
type Store struct {
	sync.Mutex

	Hosts       map[string]*fleet.Host
	Inventories map[string]*protocol.Inventory
	Slots       []fleet.SlotParticipation // oldest first
	Rollouts    []*fleet.Rollout          // newest first
	Leases      []*fleet.Lease            // newest first
	Directives  []*fleet.Directive        // oldest first
	Audit       []fleet.AuditEntry
	Packages    []store.PackageMatch
	// Ranges is the validator ranges inventory; SetValidatorRanges assigns
	// its ranges to the hosts, RecordSync to a new host.
	Ranges fleet.ValidatorRanges

	// Err, when set, fails every call.
	Err error
	// AuditErr fails AppendAudit only.
	AuditErr error
	// LastSearchQuery and LastSearchLimit record the last ListPackages
	// arguments.
	LastSearchQuery string
	LastSearchLimit int
	// LastPackageFilter is the last ListPackages filter in full.
	LastPackageFilter store.PackageFilter
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		Hosts:       make(map[string]*fleet.Host, 4),
		Inventories: make(map[string]*protocol.Inventory, 4),
	}
}

func (f *Store) RecordSync(_ context.Context, sync *store.HostSync) (*store.SyncResult, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	name := sync.Cert.Subject
	res := &store.SyncResult{}

	host, ok := f.Hosts[name]
	if !ok {
		host = &fleet.Host{Name: name, FirstSeen: sync.At, Validators: f.assigned(name)}
		f.Hosts[name] = host
		res.Created = true
	} else if host.CertHash != sync.Cert.Hash {
		res.PreviousCertHash = host.CertHash
	}

	host.CertHash = sync.Cert.Hash

	// Like the real store: a sync without a request keeps the roles.
	switch {
	case sync.Request != nil:
		host.Roles = append([]string{}, sync.Request.Roles...)
	case host.Roles == nil:
		host.Roles = []string{}
	}

	host.LastSeen = sync.At
	host.RemoteAddr = sync.RemoteAddr
	host.Status = sync.Request

	cp := *host
	res.Host = &cp

	return res, nil
}

// ValidatorRanges returns Ranges.
func (f *Store) ValidatorRanges(context.Context) (*fleet.ValidatorRanges, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	out := f.Ranges

	return &out, nil
}

// SetValidatorRanges replaces Ranges and the validators of every host.
func (f *Store) SetValidatorRanges(_ context.Context, ranges *fleet.ValidatorRanges) error {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return f.Err
	}

	f.Ranges = *ranges

	for name, host := range f.Hosts {
		host.Validators = f.assigned(name)
	}

	return nil
}

// assigned returns the ranges of Ranges that name the host.
func (f *Store) assigned(name string) []identity.ValidatorRange {
	var out []identity.ValidatorRange

	for _, r := range f.Ranges.Ranges {
		if r.Host == name {
			out = append(out, r.ValidatorRange)
		}
	}

	return out
}

func (f *Store) GetHost(_ context.Context, name string) (*fleet.Host, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	host, ok := f.Hosts[name]
	if !ok {
		return nil, store.ErrNotFound
	}

	cp := *host

	return &cp, nil
}

func (f *Store) ListHosts(_ context.Context) ([]*fleet.Host, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	out := make([]*fleet.Host, 0, len(f.Hosts))

	for _, host := range f.Hosts {
		cp := *host
		out = append(out, &cp)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

func (f *Store) SaveInventory(_ context.Context, host string, inv *protocol.Inventory) error {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return f.Err
	}

	f.Inventories[host] = inv

	if h, ok := f.Hosts[host]; ok {
		h.InventoryHash = inv.Hash
	}

	return nil
}

func (f *Store) GetInventory(_ context.Context, host string) (*protocol.Inventory, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	inv, ok := f.Inventories[host]
	if !ok {
		return nil, store.ErrNotFound
	}

	return inv, nil
}

// ListPackages pages through the fake's packages in their order; the cursor
// is "2" and the name of the last entry of the page before.
func (f *Store) ListPackages(_ context.Context, filter *store.PackageFilter) (
	*store.PackagePage, error,
) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	f.LastSearchQuery, f.LastSearchLimit = filter.Query, filter.Limit
	f.LastPackageFilter = *filter

	if filter.After != "" && !strings.HasPrefix(filter.After, "2") {
		return nil, store.ErrBadCursor
	}

	page := &store.PackagePage{Matches: make([]store.PackageMatch, 0, len(f.Packages)), Total: -1}
	skipping := filter.After != ""
	total := 0

	for _, m := range f.Packages {
		if !strings.Contains(strings.ToLower(m.Name), strings.ToLower(filter.Query)) {
			continue
		}

		total++

		switch {
		case skipping:
			skipping = "2"+m.Name != filter.After
		case len(page.Matches) == filter.Limit:
			page.Next = "2" + page.Matches[len(page.Matches)-1].Name
		default:
			if m.Upgrades == nil {
				m.Upgrades = []store.PackageUpgrade{}
			}

			page.Matches = append(page.Matches, m)
		}
	}

	if filter.After == "" {
		page.Total = total
		page.Counts = &store.PackageCounts{All: total}
	}

	return page, nil
}

func (f *Store) ListSlotParticipation(
	_ context.Context, fromSlot uint64,
) ([]fleet.SlotParticipation, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	out := make([]fleet.SlotParticipation, 0, len(f.Slots))

	for i := range f.Slots {
		if row := f.Slots[i]; row.Slot >= fromSlot {
			row.Hosts = nil
			out = append(out, row)
		}
	}

	return out, nil
}

// SumHostSlotRuns joins the per-host rows into runs like the store.
func (f *Store) SumHostSlotRuns(
	_ context.Context, fromSlot, toSlot, gap, minSlots uint64, perSlot []string,
) ([]fleet.HostSlotRun, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	rows := make(map[string][]fleet.HostSlotRun, 8)
	hosts := make([]string, 0, 8)

	for i := range f.Slots {
		slot := f.Slots[i].Slot
		if slot < fromSlot || slot > toSlot {
			continue
		}

		for _, part := range f.Slots[i].Hosts {
			if part.Included < part.Assigned {
				if _, seen := rows[part.Host]; !seen {
					hosts = append(hosts, part.Host)
				}

				rows[part.Host] = append(rows[part.Host], fleet.HostSlotRun{
					Host: part.Host, First: slot, Last: slot, Slots: 1,
					Assigned: part.Assigned, Included: part.Included,
				})
			}
		}
	}

	slices.Sort(hosts)

	out := make([]fleet.HostSlotRun, 0, len(hosts))

	for _, host := range hosts {
		list := rows[host]
		slices.SortFunc(list, func(a, b fleet.HostSlotRun) int { return cmp.Compare(a.First, b.First) })

		for _, row := range list {
			last := len(out) - 1
			if last >= 0 && out[last].Host == host && !slices.Contains(perSlot, host) && row.First-out[last].Last <= gap {
				out[last].Last = row.First
				out[last].Slots++
				out[last].Assigned += row.Assigned
				out[last].Included += row.Included

				continue
			}

			out = append(out, row)
		}
	}

	kept := out[:0]

	for _, run := range out {
		if run.Slots >= minSlots || slices.Contains(perSlot, run.Host) {
			kept = append(kept, run)
		}
	}

	return kept, nil
}

func (f *Store) SumHostSlotParticipation(
	_ context.Context, fromSlot, bucketSlots uint64,
) ([]fleet.HostSlotBucket, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	type key struct {
		bucket uint64
		host   string
	}

	sums := make(map[key]fleet.HostSlotBucket, len(f.Slots))

	for i := range f.Slots {
		if f.Slots[i].Slot < fromSlot {
			continue
		}

		for _, part := range f.Slots[i].Hosts {
			if part.Included >= part.Assigned {
				// Like the store: a host that missed nothing has no row.
				continue
			}

			k := key{bucket: f.Slots[i].Slot / bucketSlots, host: part.Host}

			sum := sums[k]
			sum.Bucket, sum.Host = k.bucket, k.host
			sum.Assigned += part.Assigned
			sum.Included += part.Included
			sums[k] = sum
		}
	}

	out := make([]fleet.HostSlotBucket, 0, len(sums))
	for _, sum := range sums {
		out = append(out, sum)
	}

	slices.SortFunc(out, func(a, b fleet.HostSlotBucket) int {
		return cmp.Or(cmp.Compare(a.Bucket, b.Bucket), cmp.Compare(a.Host, b.Host))
	})

	return out, nil
}

func (f *Store) ListRollouts(_ context.Context, filter store.RolloutFilter) (
	[]*fleet.Rollout, error,
) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	out := make([]*fleet.Rollout, 0, len(f.Rollouts))

	for _, r := range f.Rollouts {
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

func (f *Store) ListLeases(_ context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	out := make([]*fleet.Lease, 0, len(f.Leases))

	for _, l := range f.Leases {
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

func (f *Store) GetRollout(_ context.Context, id string) (*fleet.Rollout, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	for _, r := range f.Rollouts {
		if r.ID == id {
			return r, nil
		}
	}

	return nil, store.ErrNotFound
}

func (f *Store) GetLease(_ context.Context, id string) (*fleet.Lease, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	for _, l := range f.Leases {
		if l.ID == id {
			return l, nil
		}
	}

	return nil, store.ErrNotFound
}

func (f *Store) ListDirectives(_ context.Context, filter store.DirectiveFilter) ([]*fleet.Directive, error) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	out := make([]*fleet.Directive, 0, len(f.Directives))

	for _, d := range f.Directives {
		switch {
		case filter.Host != "" && d.Host != filter.Host:
		case filter.PendingOnly && d.State != fleet.DirectivePending:
		case len(filter.Rollouts) > 0 && !slices.Contains(filter.Rollouts, d.RolloutID):
		default:
			out = append(out, d)
		}
	}

	return out, nil
}

func (f *Store) AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error {
	f.Lock()
	defer f.Unlock()

	// Like a database driver: a cancelled context fails the write.
	if err := ctx.Err(); err != nil {
		return err
	}

	if _, ok := ctx.Deadline(); !ok {
		return errors.New("audit write without deadline")
	}

	if f.Err != nil {
		return f.Err
	}

	if f.AuditErr != nil {
		return f.AuditErr
	}

	for _, e := range entries {
		e.ID = int64(len(f.Audit) + 1)
		f.Audit = append(f.Audit, e)
	}

	return nil
}

func (f *Store) ListAudit(_ context.Context, filter store.AuditFilter) (
	[]fleet.AuditEntry, error,
) {
	f.Lock()
	defer f.Unlock()

	if f.Err != nil {
		return nil, f.Err
	}

	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}

	out := make([]fleet.AuditEntry, 0, limit)

	for i := len(f.Audit) - 1; i >= 0 && len(out) < limit; i-- {
		e := f.Audit[i]

		if (filter.Target != "" && e.Target != filter.Target) ||
			!strings.HasPrefix(e.Action, filter.ActionPrefix) ||
			(filter.BeforeID > 0 && e.ID >= filter.BeforeID) {
			continue
		}

		out = append(out, e)
	}

	return out, nil
}

func (f *Store) AuditEntries() []fleet.AuditEntry {
	f.Lock()
	defer f.Unlock()

	return slices.Clone(f.Audit)
}

func (f *Store) Host(name string) *fleet.Host {
	f.Lock()
	defer f.Unlock()

	return f.Hosts[name]
}

func (f *Store) SetErr(err error) {
	f.Lock()
	defer f.Unlock()

	f.Err = err
}
