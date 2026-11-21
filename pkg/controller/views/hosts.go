package views

import (
	"context"
	"errors"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
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

	detail := &HostDetail{
		HostSummary: Summarize(host, v.opts.Scheduler.Plan(), v.now(),
			settings.HostStaleAfter.Std()),
		CertHash:   host.CertHash,
		FirstSeen:  host.FirstSeen,
		RemoteAddr: host.RemoteAddr,
	}

	if host.Validators != nil {
		start, end := host.Validators.Start, host.Validators.End
		detail.ValidatorStart, detail.ValidatorEnd = &start, &end
	}

	if host.ReportedValidators() != nil {
		all, listErr := v.opts.Store.ListHosts(ctx)
		if listErr != nil {
			return nil, listErr
		}

		detail.ValidatorOverlaps = host.ReportedOverlaps(all)
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
		HoldWanted: host.HoldWanted,
		HoldReason: host.HoldReason,
		HoldBy:     HoldBy(host),
		Offline:    offlineMark(host, now),
		Broken:     host.Broken,
		Containers: []protocol.Container{},
		Verbs:      []protocol.Verb{},
		Needs:      []protocol.Need{},
		Plan:       scheduler.HostPlan{Slot: scheduler.SlotIdle, Weight: host.Weight()},
	}

	if status := host.Status; status != nil {
		sum.ValidatorReport = status.Validators
		sum.AgentVersion = status.AgentVersion
		sum.Containers = NonNil(status.Containers)
		sum.Node = status.Node
		sum.OS = status.OS
		sum.Pending = status.Pending
		sum.UptimeSeconds = status.UptimeSeconds
		sum.Hold = status.Hold
		sum.Verbs = NonNil(status.Verbs)
		sum.Needs = NonNil(status.Needs)
	}

	if plan != nil {
		if hp, ok := plan.Hosts[host.Name]; ok {
			sum.Plan = hp
		}
	}

	return sum
}

// Listing is the row of a host in the host list.
func Listing(sum *HostSummary) HostListing {
	out := HostListing{
		Name: sum.Name, Roles: sum.Roles, Validators: sum.Validators, LastSeen: sum.LastSeen,
		Stale: sum.Stale, AgentVersion: sum.AgentVersion,
		Containers: make([]ContainerListing, 0, len(sum.Containers)),
		OS:         sum.OS, Pending: sum.Pending, UptimeSeconds: sum.UptimeSeconds, Hold: sum.Hold,
		HoldWanted: sum.HoldWanted, HoldReason: sum.HoldReason, HoldBy: sum.HoldBy, Offline: sum.Offline, Verbs: sum.Verbs,
		Needs: make([]NeedListing, 0, len(sum.Needs)), Plan: sum.Plan,
	}

	for i := range sum.Containers {
		c := &sum.Containers[i]
		out.Containers = append(out.Containers, ContainerListing{
			Name: ContainerName(c.Name), Image: c.Image, Digest: c.Digest, Revision: c.Revision,
			State: c.State, Managed: c.Managed, Kind: c.Kind, Impact: c.Impact, Owners: c.Owners, Group: c.Group,
		})
	}

	for i := range sum.Needs {
		out.Needs = append(out.Needs, NeedListing{Kind: sum.Needs[i].Kind})
	}

	return out
}

// offlineMark is the host's known-offline mark while it is in force.
func offlineMark(host *fleet.Host, now time.Time) *fleet.OfflineMark {
	if !host.KnownOffline(now) {
		return nil
	}

	return host.Offline
}

// HoldBy says for the host views who set the controller hold: "team:<teams>"
// for a hold set through a team feature, "staff" for every other hold,
// nothing for a host that is not held.
func HoldBy(host *fleet.Host) string {
	_, team := fleet.HoldTeams(host.HoldBy)

	switch {
	case !host.HoldWanted:
		return ""
	case team:
		return host.HoldBy
	default:
		return authz.HoldByStaff
	}
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

	out := &Rollouts{
		Open:         NonNil(open),
		Recent:       make([]*fleet.Rollout, 0, limit),
		Leases:       NonNil(openLeases),
		RecentLeases: make([]*fleet.Lease, 0, limit),
	}

	for _, rollout := range newest {
		if rollout.State.Terminal() && len(out.Recent) < limit {
			out.Recent = append(out.Recent, rollout)
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
// around the current gate before the first reconcile.
func (v *Views) Plan() scheduler.Plan {
	plan := v.opts.Scheduler.Plan()
	if plan == nil {
		return scheduler.Plan{
			Gate:  v.Gate(),
			Queue: []scheduler.QueueEntry{},
			Hosts: map[string]scheduler.HostPlan{},
		}
	}

	out := *plan
	out.Gate.Reasons = NonNil(out.Gate.Reasons)
	out.Queue = NonNil(out.Queue)

	if out.Hosts == nil {
		out.Hosts = map[string]scheduler.HostPlan{}
	}

	return out
}
