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
	// ErrForbidden is returned when the authorization the request carries
	// (WithAuthorization) does not allow what the action would do to the
	// state it finds under the lock; the message says what, and the API
	// answers 403 with it.
	ErrForbidden = errors.New("scheduler: forbidden")
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
	// PatchSettings changes the settings in one step under the scheduler's
	// lock: apply is given a copy of the effective settings and changes what
	// the caller names; the result is validated, stored and audited with the
	// values before and after. Nothing a concurrent action or patch wrote in
	// the meantime is lost: apply sees it. An error of apply is returned as it
	// is and changes nothing. When apply changes nothing, nothing is stored
	// and nothing is audited; changed says which.
	PatchSettings(
		ctx context.Context, actor string, apply func(settings *fleet.Settings) error,
	) (settings fleet.Settings, changed bool, err error)

	// Roll requests a manual image roll.
	Roll(ctx context.Context, actor string, req RollRequest) (*ActionResult, error)
	// Reboot requests a manual reboot.
	Reboot(ctx context.Context, actor string, req RebootRequest) (*ActionResult, error)
	// Hold puts hosts on controller hold; Unhold lifts it.
	Hold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error)
	Unhold(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error)
	// MarkOffline marks hosts as known offline; UnmarkOffline lifts the mark.
	MarkOffline(ctx context.Context, actor string, req OfflineRequest) (*ActionResult, error)
	UnmarkOffline(ctx context.Context, actor string, req OfflineRequest) (*ActionResult, error)
	// ImageOverride and FlagOverride send a host the instruction to set or
	// take away the image, or the flags, of one container's override. The
	// host's agent decides whether it takes it; the container follows under
	// a lease of the host's own rollout.
	ImageOverride(ctx context.Context, actor string, req ImageOverrideRequest) (*ActionResult, error)
	FlagOverride(ctx context.Context, actor string, req FlagOverrideRequest) (*ActionResult, error)
	ClientOverride(ctx context.Context, actor string, req ClientOverrideRequest) (*ActionResult, error)
	// SetClientPolicy sets the image policy of one client, or returns the
	// client to the global default (policyImage).
	SetClientPolicy(ctx context.Context, actor string, req ClientPolicyRequest) (*ActionResult, error)
	// Pause pauses everything (no Rollout) or one rollout; Resume lifts the
	// pause and approves a rollout that waits on a manual policy.
	Pause(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error)
	Resume(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error)
	// Skip marks hosts as skipped in a rollout. In a rollout of an observed
	// cause the rollout stays open while a skipped host still has the need.
	Skip(ctx context.Context, actor string, req RolloutHostsRequest) (*ActionResult, error)
	// Retry returns the failed hosts of an open rollout (all, or the listed
	// ones) to pending and so lifts its halt. It also takes back skips of the
	// hosts it names.
	Retry(ctx context.Context, actor string, req RolloutHostsRequest) (*ActionResult, error)
	// Abort closes a manual rollout. A rollout of an observed cause cannot be
	// closed while hosts report the cause: it is paused instead, until a
	// person resumes it or the cause changes. Open leases finish.
	Abort(ctx context.Context, actor string, req ScopeRequest) (*ActionResult, error)
	// EndLease asks for the end of one open lease: the lease is marked
	// cancelled. It stays open, with its place in the budget, until the agent
	// reports that it has stopped or goes silent, and then ends as failed.
	EndLease(ctx context.Context, actor string, req EndLeaseRequest) (*ActionResult, error)
	// Forget deletes hosts that are gone. Refused while a host holds a lease.
	Forget(ctx context.Context, actor string, req HoldRequest) (*ActionResult, error)
}

// Assignment is the scheduler's part of a sync response.
type Assignment struct {
	Lease      *protocol.Lease
	Directives []protocol.Directive
	Queue      *protocol.Queue
	// Ended is the host's newest lease when it is over and the host holds
	// none: how it ended, for the host's own status.
	Ended *protocol.EndedLease
	// Pull is the host's pull window, if it has one.
	Pull *protocol.PullWindow
	// LookupInterval is the setting of the same name: how often one host of
	// those on a tag looks it up.
	LookupInterval time.Duration
}

// RollRequest is the body of POST /api/v1/actions/roll.
type RollRequest struct {
	// Hosts to roll; empty with All selects every host.
	Hosts []string `json:"hosts"`
	All   bool     `json:"all"`
	// Containers limits the roll to these container names; empty means
	// every managed container of each host.
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

// OfflineRequest is the body of the mark-offline and unmark-offline actions.
// Reason is required to mark; Until, when set, ends the mark by itself.
type OfflineRequest struct {
	Hosts  []string   `json:"hosts"`
	Reason string     `json:"reason"`
	Until  *time.Time `json:"until"`
}

// ImageOverrideRequest is the body of POST /api/v1/actions/image-override.
type ImageOverrideRequest struct {
	Host      string `json:"host"`
	Container string `json:"container"`
	// Image is the image reference to run; empty takes the image override
	// away.
	Image string `json:"image"`
	// Until is when the override ends: by default DefaultOverrideDuration
	// from now, at most MaxOverrideDuration. Not read when Image is empty.
	Until  *time.Time `json:"until"`
	Reason string     `json:"reason"`
}

// FlagOverrideRequest is the body of POST /api/v1/actions/flag-override.
type FlagOverrideRequest struct {
	Host      string `json:"host"`
	Container string `json:"container"`
	// AddFlags and RemoveFlags are the flags to add and to remove; both
	// empty takes the flag override away.
	AddFlags    []string   `json:"addFlags"`
	RemoveFlags []string   `json:"removeFlags"`
	Until       *time.Time `json:"until"`
	Reason      string     `json:"reason"`
}

// ClientOverrideRequest is the body of POST /api/v1/actions/client-override:
// one part of an override for the container of one kind of a client, on
// each of the named hosts. Every host gets a directive of its own, as if it
// had been asked for alone; a host that cannot take it is refused with the
// reason and the others go ahead.
type ClientOverrideRequest struct {
	// Client is the client name, as the agents report it; Kind the kind of
	// its container (a consensus client has a beacon and a validator).
	Client string                 `json:"client"`
	Kind   protocol.ContainerKind `json:"kind"`
	Hosts  []string               `json:"hosts"`
	// Part is protocol.OverrideImage or protocol.OverrideFlags. An empty
	// Image, or no flags, takes that part of the override away.
	Part        string     `json:"part"`
	Image       string     `json:"image"`
	AddFlags    []string   `json:"addFlags"`
	RemoveFlags []string   `json:"removeFlags"`
	Until       *time.Time `json:"until"`
	Reason      string     `json:"reason"`
}

// Sets reports whether the request sets its part of the override; false
// takes it away.
func (r *ClientOverrideRequest) Sets() bool {
	if r.Part == protocol.OverrideImage {
		return r.Image != ""
	}

	return len(r.AddFlags)+len(r.RemoveFlags) > 0
}

// ClientContainer names the managed container of the client and kind on the
// host, by what its agent last reported; empty when there is none.
func ClientContainer(h *fleet.Host, client string, kind protocol.ContainerKind) string {
	if h.Status == nil {
		return ""
	}

	for i := range h.Status.Containers {
		if c := &h.Status.Containers[i]; c.Managed && c.Client == client && c.Kind == kind {
			return c.Name
		}
	}

	return ""
}

// Bounds of an override that is set through the controller: it always has
// an end. The agent enforces the maximum as well.
const (
	DefaultOverrideDuration = 24 * time.Hour
	MaxOverrideDuration     = 7 * 24 * time.Hour
	// overrideAnswerTimeout is how long an override directive waits for the
	// agent's answer before it is withdrawn: an instruction that was not
	// delivered soon is not delivered late.
	overrideAnswerTimeout = 15 * time.Minute
)

// ClientPolicyRequest is the body of POST /api/v1/actions/client-policy.
type ClientPolicyRequest struct {
	// Client is the client name, as the agents report it.
	Client string `json:"client"`
	// Policy is "auto" or "manual"; empty returns the client to the global
	// default (policyImage).
	Policy fleet.Policy `json:"policy"`
	Reason string       `json:"reason"`
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

// Gate is the network gate. No new lease is granted while it fails, with one
// exception (a host none of whose validators attests, while nothing else is
// leased); open leases finish.
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
	// UnavailableWeight is the full weight of every host that holds a lease
	// or works on an operation without one, plus the validators not attesting
	// on every other host.
	UnavailableWeight uint64 `json:"unavailableWeight"`
	// LeasedHosts and MaxLeasedHosts are the host-count dimension:
	// max(1, floor(maxUnavailable * hosts)) hosts may hold a lease (or work
	// on an operation without one) at once.
	LeasedHosts    int `json:"leasedHosts"`
	MaxLeasedHosts int `json:"maxLeasedHosts"`
	// IndependentHosts and MaxIndependent are the independent limit: hosts
	// whose lease is about independent containers alone, and
	// max(1, floor(maxIndependentPercent * hosts)). They count for nothing
	// above.
	IndependentHosts int `json:"independentHosts"`
	MaxIndependent   int `json:"maxIndependent"`
	// OfflineHosts are the hosts marked known offline that are not at work;
	// their OfflineWeight is left out of TotalWeight and UnavailableWeight,
	// and out of both sides of the gate's participation.
	OfflineHosts  int    `json:"offlineHosts,omitempty"`
	OfflineWeight uint64 `json:"offlineWeight,omitempty"`
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
	// of a rollout waits for: a member whose host is broken, or the canary.
	// That host may not be in the queue itself: it holds a lease, or it is
	// expected to report its need.
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

// Wait kinds. Budget, host limit and order clear by themselves as the
// rollout proceeds; working clears when the operation the host works on
// without a lease is over; gate clears when the network recovers; the others
// need somebody to act.
const (
	WaitBudget        WaitKind = "budget"
	WaitPull          WaitKind = "pull"
	WaitHostLimit     WaitKind = "host-limit"
	WaitOrder         WaitKind = "order"
	WaitGate          WaitKind = "gate"
	WaitVerb          WaitKind = "verb"
	WaitHalted        WaitKind = "halted"
	WaitApproval      WaitKind = "approval"
	WaitRolloutPaused WaitKind = "rollout-paused"
	WaitPaused        WaitKind = "paused"
	WaitSilent        WaitKind = "silent"
	WaitWorking       WaitKind = "working"
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
	// LeaseID is the open lease, if any. A host that works on an operation
	// without a lease has the slot leased and no lease ID.
	LeaseID string `json:"leaseId,omitempty"`
	// Reason is the waiting reason when queued.
	Reason string `json:"reason,omitempty"`
	// Pull is the host's pull state while it waits in a rollout: waiting
	// (for a pull window), pulling (it has one) or pulled; empty otherwise.
	Pull string `json:"pull,omitempty"`
}
