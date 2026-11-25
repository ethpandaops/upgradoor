package views

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// hostLeases and hostHistory bound the host detail.
const (
	hostLeases  = 20
	hostHistory = 50
)

// HostDetail builds the detail view of one host. An unknown host is
// store.ErrNotFound.
func (v *Views) HostDetail(ctx context.Context, name string) (*HostDetail, error) {
	host, err := v.opts.Store.GetHost(ctx, name)
	if err != nil {
		return nil, err
	}

	settings, err := v.opts.Scheduler.Settings(ctx)
	if err != nil {
		return nil, err
	}

	plan := v.Plan()
	detail := &HostDetail{
		HostSummary: Summarize(host, &plan, v.now(), settings.HostStaleAfter.Std()),
		CertHash:    host.CertHash,
		FirstSeen:   host.FirstSeen,
		RemoteAddr:  host.RemoteAddr,
		Assigned:    host.Weight(),
	}

	inv, err := v.opts.Store.GetInventory(ctx, name)

	switch {
	case err == nil:
		detail.Inventory = inv
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	detail.Leases, err = v.opts.Store.ListLeases(ctx, store.LeaseFilter{
		Host: name, Limit: hostLeases,
	})
	if err != nil {
		return nil, err
	}

	detail.History, err = v.opts.Store.ListAudit(ctx, store.AuditFilter{
		Target: name, Limit: hostHistory,
	})
	if err != nil {
		return nil, err
	}

	if detail.Leases == nil {
		detail.Leases = []*fleet.Lease{}
	}

	if detail.History == nil {
		detail.History = []fleet.AuditEntry{}
	}

	return detail, nil
}

// Summarize builds one row of the authenticated host table.
func Summarize(
	host *fleet.Host, plan *scheduler.Plan, now time.Time, staleAfter time.Duration,
) HostSummary {
	sum := HostSummary{
		Name:       host.Name,
		Roles:      NonNil(host.Roles),
		Validators: host.Weight(),
		LastSeen:   host.LastSeen,
		Stale:      host.Stale(now, staleAfter),
		Offline:    offlineMark(host, now),
		Broken:     host.Broken,
		Containers: []protocol.Container{},
		Verbs:      []protocol.Verb{},
		Needs:      []protocol.Need{},
		Actions:    []protocol.ActionInfo{},
		Plan:       scheduler.HostPlan{Slot: scheduler.SlotIdle, Weight: host.Weight()},
	}

	if status := host.Status; status != nil {
		sum.AgentVersion = status.AgentVersion
		sum.AgentProtocol = status.ProtocolVersion
		sum.Incompatible = host.Incompatible()
		sum.Containers = NonNil(status.Containers)
		sum.Node = status.Node
		sum.OS = status.OS
		sum.Pending = status.Pending
		sum.UptimeSeconds = status.UptimeSeconds
		res := status.Resources
		sum.Resources = &res
		sum.Hold = status.Hold
		sum.Verbs = NonNil(status.Verbs)
		sum.Needs = NonNil(status.Needs)
		sum.Actions = NonNil(status.Actions)
	}

	if plan != nil {
		if hp, ok := plan.Hosts[host.Name]; ok {
			sum.Plan = hp
			sum.Validators = hp.Weight
		}
	}

	return sum
}

// Listing is the row of a host in the host list.
func Listing(sum *HostSummary) HostListing {
	out := HostListing{
		Name: sum.Name, Roles: sum.Roles, Validators: sum.Validators, LastSeen: sum.LastSeen,
		Stale: sum.Stale, AgentVersion: sum.AgentVersion,
		AgentProtocol: sum.AgentProtocol, Incompatible: sum.Incompatible,
		Containers: make([]ContainerListing, 0, len(sum.Containers)),
		OS:         sum.OS, Pending: sum.Pending, UptimeSeconds: sum.UptimeSeconds,
		Usage: NewHostUsage(sum.Resources), Hold: sum.Hold,
		Holders: NonNil(sum.Hold.Holders()), Offline: sum.Offline, Verbs: sum.Verbs,
		Needs: make([]NeedListing, 0, len(sum.Needs)), Plan: sum.Plan, Actions: sum.Actions,
	}

	for i := range sum.Containers {
		c := &sum.Containers[i]
		out.Containers = append(out.Containers, ContainerListing{
			Name: ContainerName(c.Name), Image: c.Image, Digest: c.Digest, Revision: c.Revision,
			State: c.State, Managed: c.Managed, Kind: c.Kind, Impact: c.Impact, Team: c.Team,
		})
	}

	for i := range sum.Needs {
		out.Needs = append(out.Needs, NeedListing{Kind: sum.Needs[i].Kind})
	}

	return out
}

// NewHostUsage is the brief of a host's resource use; nil for nil or when
// nothing of it is known.
func NewHostUsage(r *protocol.Resources) *HostUsage {
	if r == nil {
		return nil
	}

	var out HostUsage

	if r.CPU.Busy != nil {
		out.CPU = wholePercent(*r.CPU.Busy)
	}

	if mem, ok := r.Memory.UsedPercent(); ok {
		out.Memory = wholePercent(mem)
	}

	if out == (HostUsage{}) {
		return nil
	}

	return &out
}

func wholePercent(v float64) *int {
	n := int(math.Round(v))

	return &n
}

// offlineMark is the host's known-offline mark while it is in force.
func offlineMark(host *fleet.Host, now time.Time) *fleet.OfflineMark {
	if !host.KnownOffline(now) {
		return nil
	}

	return host.Offline
}

// NonNil returns an empty slice for nil, so JSON carries [] and not null.
func NonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}

	return in
}

// Rollouts builds the rollout view: everything open, and the newest limit
// closed rollouts and leases.
func (v *Views) Rollouts(ctx context.Context, limit int) (*Rollouts, error) {
	settings, err := v.opts.Scheduler.Settings(ctx)
	if err != nil {
		return nil, err
	}

	open, err := v.opts.Store.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
	if err != nil {
		return nil, err
	}

	// The store filters open or all. The newest limit+len(open) rollouts
	// hold at least the newest `limit` closed ones.
	newest, err := v.opts.Store.ListRollouts(ctx, store.RolloutFilter{Limit: limit + len(open)})
	if err != nil {
		return nil, err
	}

	openLeases, err := v.opts.Store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return nil, err
	}

	newestLeases, err := v.opts.Store.ListLeases(ctx, store.LeaseFilter{
		Limit: limit + len(openLeases),
	})
	if err != nil {
		return nil, err
	}

	view := func(r *fleet.Rollout) *RolloutView {
		out := &RolloutView{Rollout: *r, Policy: settings.RolloutPolicy(r)}
		if r.Kind == fleet.RolloutGroup {
			_, out.PolicySource = settings.GroupPolicy(r)
		}

		return out
	}

	out := &Rollouts{
		Open:         make([]*RolloutView, 0, len(open)),
		Recent:       make([]*RolloutView, 0, limit),
		Leases:       NonNil(openLeases),
		RecentLeases: make([]*fleet.Lease, 0, limit),
	}

	for _, rollout := range open {
		out.Open = append(out.Open, view(rollout))
	}

	for _, rollout := range newest {
		if rollout.State.Terminal() && len(out.Recent) < limit {
			out.Recent = append(out.Recent, view(rollout))
		}
	}

	for _, lease := range newestLeases {
		if lease.State.Terminal() && len(out.RecentLeases) < limit {
			out.RecentLeases = append(out.RecentLeases, lease)
		}
	}

	return out, nil
}

// Plan returns the last plan in the form the API serves it, or an empty one
// around the current gate before the first reconcile. The error texts of
// the beacon service are out of it, as out of the public gate (scrubGate):
// from the gate and from the waiting reasons that quote it.
func (v *Views) Plan() scheduler.Plan {
	plan := v.opts.Scheduler.Plan()
	if plan == nil {
		gate := v.Gate()
		scrubGate(&gate)

		return scheduler.Plan{
			Gate:  gate,
			Queue: []scheduler.QueueEntry{},
			Hosts: map[string]scheduler.HostPlan{},
		}
	}

	out := *plan
	scrub := newScrubber(&plan.Gate.Network)
	out.Gate.Reasons = append(make([]string, 0, len(plan.Gate.Reasons)), plan.Gate.Reasons...)
	scrubGate(&out.Gate)

	if scrub.none() {
		out.Queue = NonNil(out.Queue)
	} else {
		out.Queue = make([]scheduler.QueueEntry, 0, len(plan.Queue))

		for _, entry := range plan.Queue {
			entry.Reason = scrub.waitReason(entry.Reason)
			out.Queue = append(out.Queue, entry)
		}
	}

	switch {
	case plan.Hosts == nil:
		out.Hosts = map[string]scheduler.HostPlan{}
	case !scrub.none():
		out.Hosts = make(map[string]scheduler.HostPlan, len(plan.Hosts))

		for name, hp := range plan.Hosts {
			hp.Reason = scrub.waitReason(hp.Reason)
			out.Hosts[name] = hp
		}
	}

	return out
}
