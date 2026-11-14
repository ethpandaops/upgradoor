// Package scheduler plans rollouts and grants leases under the availability
// budget, the network gate and the order inside a rollout (hosts the change
// already broke first, then a canary, then the rest). docs/scheduler.md states
// the rules; the simulation tests assert them after every step.
package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Errors returned by actions.
var (
	// ErrUnknownHost is returned when an action names a host that does not
	// exist.
	ErrUnknownHost = errors.New("scheduler: unknown host")
	// ErrUnknownRollout is returned when an action names a rollout that does
	// not exist.
	ErrUnknownRollout = errors.New("scheduler: unknown rollout")
	// ErrUnknownLease is returned when an action names a lease that does not
	// exist.
	ErrUnknownLease = errors.New("scheduler: unknown lease")
	// ErrInvalidAction is returned when an action cannot apply in the
	// current state; the message says why.
	ErrInvalidAction = errors.New("scheduler: invalid action")
)

// Service is the scheduler. It reconciles on a timer and whenever Nudge is
// called; every decision is committed to the store before it is visible.
type Service interface {
	Start(ctx context.Context) error
	Stop() error

	// Nudge asks for a reconcile soon (after a sync that changed something).
	Nudge()
	// Assignment returns what a sync response for the host must carry: its
	// open lease, outstanding directives and queue position. It reads only
	// committed state.
	Assignment(ctx context.Context, host string) (*Assignment, error)
	// Plan returns the result of the last reconcile.
	Plan() *Plan

	// Settings returns the effective settings.
	Settings(ctx context.Context) (fleet.Settings, error)
	// UpdateSettings validates, stores and audits new settings.
	UpdateSettings(ctx context.Context, actor string, settings fleet.Settings) error

	// Roll requests a manual image roll.
	Roll(ctx context.Context, actor string, req RollRequest) (*ActionResult, error)
	// Reboot requests a manual reboot.
	Reboot(ctx context.Context, actor string, req RebootRequest) (*ActionResult, error)
	// Hold puts hosts on controller hold; Unhold lifts it.
	Hold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error)
	Unhold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error)
	// Pause pauses everything (no Rollout) or one rollout; Resume lifts the
	// pause and approves a rollout that waits on a manual policy.
	Pause(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error)
	Resume(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error)
	// Skip marks hosts as skipped in a rollout.
	Skip(ctx context.Context, actor string, req RolloutHostsRequest) (*ActionResult, error)
	// Retry reopens a halted or aborted rollout (all failed hosts, or the
	// listed ones) and so lifts the digest block. It also takes back skips
	// of the hosts it applies to.
	Retry(ctx context.Context, actor string, req RolloutHostsRequest) (*ActionResult, error)
	// Abort closes a rollout. Open leases finish.
	Abort(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error)
	// EndLease ends one open lease that the agent has started as failed
	// ("ended by operator"). The host is broken from then on and goes first;
	// a lease that held back the rollouts of its group no longer does.
	EndLease(ctx context.Context, actor string, req EndLeaseRequest) (*ActionResult, error)
	// Forget deletes hosts that are gone. Refused while a host holds a lease.
	Forget(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error)
}

// Assignment is the scheduler's part of a sync response.
type Assignment struct {
	Lease      *protocol.Lease
	Directives []protocol.Directive
	Queue      *protocol.Queue
}

// RollRequest is the body of POST /api/v1/actions/roll.
type RollRequest struct {
	// Hosts to roll; empty with All selects every host.
	Hosts []string `json:"hosts"`
	All   bool     `json:"all"`
	// Containers limits the roll to these container names; empty means
	// every managed, non-custom container of each host.
	Containers []string `json:"containers"`
	// Staggered runs the selection as a manual rollout through the normal
	// order, budget, gate and readiness check. Not staggered means "now":
	// each host is granted as soon as the budget and gate allow, in no order
	// and without waiting for readiness between hosts.
	Staggered bool `json:"staggered"`
	// Force skips ordering and readiness for a staggered rollout as well.
	Force bool `json:"force"`
	// IgnoreBudget also skips the budget and the network gate. Audited.
	IgnoreBudget bool   `json:"ignoreBudget"`
	Reason       string `json:"reason"`
}

// RebootRequest is the body of POST /api/v1/actions/reboot.
type RebootRequest struct {
	Hosts        []string `json:"hosts"`
	All          bool     `json:"all"`
	Staggered    bool     `json:"staggered"`
	Force        bool     `json:"force"`
	IgnoreBudget bool     `json:"ignoreBudget"`
	Reason       string   `json:"reason"`
}

// HoldRequest is the body of the hold, unhold and forget actions.
type HoldRequest struct {
	Hosts  []string `json:"hosts"`
	Reason string   `json:"reason"`
}

// ScopeRequest is the body of pause, resume and abort. An empty Rollout
// means the global scope (pause and resume only).
type ScopeRequest struct {
	Rollout string `json:"rollout"`
	Reason  string `json:"reason"`
}

// RolloutHostsRequest is the body of skip and retry.
type RolloutHostsRequest struct {
	Rollout string   `json:"rollout"`
	Hosts   []string `json:"hosts"`
	Reason  string   `json:"reason"`
}

// EndLeaseRequest is the body of POST /api/v1/actions/end-lease.
type EndLeaseRequest struct {
	// Lease is the ID of an open lease.
	Lease  string `json:"lease"`
	Reason string `json:"reason"`
}

// ActionResult is the answer to every action.
type ActionResult struct {
	// Message is a one-line summary for the UI.
	Message string `json:"message"`
	// Rollout is the rollout created or changed, if any.
	Rollout string `json:"rollout,omitempty"`
	// Hosts are the hosts the action applied to.
	Hosts []string `json:"hosts,omitempty"`
	// Rejected maps hosts the action could not apply to, to the reason.
	Rejected map[string]string `json:"rejected,omitempty"`
}

// Plan is the outcome of one reconcile: what is running, what waits and why.
type Plan struct {
	ComputedAt time.Time `json:"computedAt"`
	// Paused is the global pause.
	Paused bool   `json:"paused"`
	Gate   Gate   `json:"gate"`
	Budget Budget `json:"budget"`
	// Queue lists every host with an open need that holds no lease, in the
	// order the scheduler would serve them.
	Queue []QueueEntry `json:"queue"`
	// Hosts is the per-host availability view, keyed by host name.
	Hosts map[string]HostPlan `json:"hosts"`
}

// Gate is the network gate. No new lease is granted while it fails; open
// leases finish.
type Gate struct {
	OK bool `json:"ok"`
	// Reasons lists every failing condition, e.g.
	// "participation 88% < 90%", "finality lag 5 epochs > 3".
	Reasons []string            `json:"reasons"`
	Network fleet.NetworkHealth `json:"network"`
}

// Budget is the availability budget in validator weight.
type Budget struct {
	TotalWeight uint64 `json:"totalWeight"`
	// AllowedWeight is maxUnavailable applied to TotalWeight.
	AllowedWeight float64 `json:"allowedWeight"`
	// UnavailableWeight is the full weight of every host holding a lease
	// plus the validators not attesting on every other host.
	UnavailableWeight uint64 `json:"unavailableWeight"`
	// LeasedHosts and MaxLeasedHosts are the host-count dimension:
	// max(1, floor(maxUnavailable * hosts)) hosts may hold a lease at once.
	LeasedHosts    int `json:"leasedHosts"`
	MaxLeasedHosts int `json:"maxLeasedHosts"`
}

// QueueEntry is one waiting host.
type QueueEntry struct {
	Host     string `json:"host"`
	Position int    `json:"position"`
	// Reason is the one-line waiting reason, e.g. "budget 8% of 10%",
	// "network gate: participation 88% < 90%", "paused",
	// "waiting for approval (policy manual)", "after canary lighthouse-geth-1".
	Reason string `json:"reason"`
	// Wait classifies Reason, so a client can tell a host that will be served
	// by itself from one that waits for a person.
	Wait WaitKind `json:"wait"`
	// After names the host that a host waiting for its place in the order
	// of a rollout waits for: a member whose host is broken, the host of a
	// lease in flight, or the canary. That host may not be in the queue
	// itself: it holds a lease, or it is expected to report its need.
	After string `json:"after,omitempty"`
	// Rollout is the ID of the rollout the reason is about: the one that is
	// paused, halted, unapproved or whose order the host waits in. Empty when
	// the reason is about the host or the fleet (hold, global pause, budget).
	Rollout string `json:"rollout,omitempty"`
	// Rollouts are the IDs of the rollouts the host is a pending member of.
	Rollouts []string `json:"rollouts"`
}

// WaitKind is the class of a queued host's waiting reason.
type WaitKind string

// Wait kinds. Budget, host limit, order and next clear by themselves as the
// rollout proceeds; gate clears when the network recovers; the others need
// somebody to act.
const (
	WaitNext          WaitKind = "next"
	WaitBudget        WaitKind = "budget"
	WaitHostLimit     WaitKind = "host-limit"
	WaitOrder         WaitKind = "order"
	WaitGate          WaitKind = "gate"
	WaitVerb          WaitKind = "verb"
	WaitHalted        WaitKind = "halted"
	WaitPolicyPaused  WaitKind = "policy-paused"
	WaitApproval      WaitKind = "approval"
	WaitRolloutPaused WaitKind = "rollout-paused"
	WaitPaused        WaitKind = "paused"
	WaitSilent        WaitKind = "silent"
	WaitHeld          WaitKind = "held"
)

// SlotState is a host's state in the availability view.
type SlotState string

// Slot states.
const (
	SlotIdle      SlotState = "idle"      // no need, no lease
	SlotQueued    SlotState = "queued"    // open need, waiting
	SlotLeased    SlotState = "leased"    // lease granted or running
	SlotVerifying SlotState = "verifying" // agent done, readiness pending
	SlotFailed    SlotState = "failed"    // last lease failed, not retried yet
	SlotHeld      SlotState = "held"      // hold in effect
	SlotStale     SlotState = "stale"     // agent silent
)

// HostPlan is one host's availability view.
type HostPlan struct {
	Slot SlotState `json:"slot"`
	// Weight is the host's validator count; Live how many attested in the
	// last complete epoch (LiveKnown false when there is no data yet).
	Weight    uint64 `json:"weight"`
	Live      uint64 `json:"live"`
	LiveKnown bool   `json:"liveKnown"`
	// LeaseID is the open lease, if any.
	LeaseID string `json:"leaseId,omitempty"`
	// Reason is the waiting reason when queued.
	Reason string `json:"reason,omitempty"`
}
