package fleet

import (
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// RolloutKind is the kind of cause a rollout groups needs by.
type RolloutKind string

// Rollout kinds.
const (
	// RolloutGroup: the containers of one roll group of one client moving to
	// one target: the new digests of their tags and what a soft deploy
	// changed (the deployed block of the overrides file). One lease per host
	// serves the whole group.
	RolloutGroup RolloutKind = "group"
	// RolloutPackages: one set of blocked package target versions.
	RolloutPackages RolloutKind = "packages"
	// RolloutReboot: hosts that report a reboot need.
	RolloutReboot RolloutKind = "reboot"
	// RolloutManual: a staggered roll or reboot of a selection requested by
	// a person.
	RolloutManual RolloutKind = "manual"
	// RolloutLocal: what one host asks for itself: its overrides file says
	// something else than a container runs, or a roll was requested on the
	// host. It has one member, the host.
	RolloutLocal RolloutKind = "local"
)

// RolloutTarget is one part of the target of a group rollout: a tag that
// moved to a digest, or a container that a soft deploy moves to another
// image or command line.
type RolloutTarget struct {
	// Image is the tag reference; Digest and Revision what it moves to.
	Image    string `json:"image"`
	Digest   string `json:"digest"`
	Revision string `json:"revision,omitempty"`
	// Spec is the key of a soft deploy (protocol.SpecNeed.Key); empty for a
	// tag that moved. Container names the container it was first reported
	// for, Flags that its command line changes.
	Spec      string `json:"spec,omitempty"`
	Container string `json:"container,omitempty"`
	Flags     bool   `json:"flags,omitempty"`
}

// RolloutState is the lifecycle state of a rollout.
type RolloutState string

// Rollout states. Completed, Superseded and Aborted are terminal.
const (
	// RolloutActive: hosts are being served (or waiting for budget, gate,
	// approval or a pause to lift; see the plan for the reason).
	RolloutActive RolloutState = "active"
	// RolloutHalted: a host failed. Nothing more is granted for this cause
	// until a person retries or the cause is superseded (a newer digest).
	// For image rollouts this is what "the digest is blocked" means.
	RolloutHalted RolloutState = "halted"
	// RolloutCompleted: no member host has the need any more.
	RolloutCompleted RolloutState = "completed"
	// RolloutSuperseded: the cause changed (the tag moved to a newer digest,
	// newer package versions appeared) before the rollout finished.
	RolloutSuperseded RolloutState = "superseded"
	// RolloutAborted: a manual rollout that a person closed.
	RolloutAborted RolloutState = "aborted"
)

// OpenRolloutStates lists the states of a rollout that is not Terminal.
func OpenRolloutStates() []RolloutState {
	return []RolloutState{RolloutActive, RolloutHalted}
}

// Terminal reports whether no further transition is possible.
func (s RolloutState) Terminal() bool {
	return s == RolloutCompleted || s == RolloutSuperseded || s == RolloutAborted
}

// Rollout is a group of hosts that share one cause.
type Rollout struct {
	ID   string      `json:"id"`
	Kind RolloutKind `json:"kind"`
	// Key identifies the cause. At most one non-terminal rollout exists per
	// key:
	//   group:<group>#<client>@<digest of the targets>
	//   packages:<protocol.PackagesKey>
	//   reboot
	//   manual:<rollout id>
	//   local:<host>:<digest of the IDs of the host's local needs>
	Key string `json:"key"`
	// Title is a one-line description for people.
	Title string `json:"title"`

	// Group is the roll group a group rollout moves; Targets what it moves
	// the group's containers to, sorted.
	Group   string          `json:"group,omitempty"`
	Targets []RolloutTarget `json:"targets,omitempty"`
	// Client is the client whose image policy a group rollout answers to:
	// the client of the group. Empty when the group has none; the rollout
	// then answers to policyImage.
	Client string `json:"client,omitempty"`
	// Packages is set for packages rollouts.
	Packages []protocol.PackageChange `json:"packages,omitempty"`

	State RolloutState `json:"state"`
	// Reason explains State halted / aborted / superseded.
	Reason string `json:"reason,omitempty"`
	// Paused stops new grants for this rollout only.
	Paused bool `json:"paused"`
	// PauseReason says why the rollout is paused when it is not simply a
	// person's pause: the scheduler paused it after its canary, or a person
	// aborted it. Empty once it is resumed.
	PauseReason string `json:"pauseReason,omitempty"`
	// Approved is set by a person for a rollout whose policy is manual.
	Approved bool `json:"approved"`
	// Force skips the order inside the rollout and the readiness check (never
	// the budget).
	Force bool `json:"force"`
	// IgnoreBudget additionally skips the budget and the network gate. It
	// can only be set explicitly by a person and is audited.
	IgnoreBudget bool `json:"ignoreBudget"`
	// Canary is the host that goes alone until the rollout has its first
	// success. Empty for a forced rollout, for a rollout with a single member
	// and for a rollout whose first success came without one (a broken host
	// that was repaired).
	Canary string `json:"canary,omitempty"`

	CreatedBy string     `json:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`

	Hosts []RolloutHost `json:"hosts"`
}

// Host returns the member entry for a host name, or nil.
func (r *Rollout) Host(name string) *RolloutHost {
	for i := range r.Hosts {
		if r.Hosts[i].Host == name {
			return &r.Hosts[i]
		}
	}

	return nil
}

// HostState is the state of one host inside a rollout.
type HostState string

// Host states inside a rollout.
const (
	// HostExpected: the controller expects the host to report the need (it
	// runs the image at another digest) but it has not yet.
	HostExpected HostState = "expected"
	// HostPending: the need is reported; waiting for a slot.
	HostPending HostState = "pending"
	// HostLeased: a lease that includes this rollout's need is open.
	HostLeased HostState = "leased"
	// HostDone: served and ready (LeaseID names the released lease), or the
	// need disappeared by itself, or its step was done in a lease that failed
	// on another step (both without LeaseID).
	HostDone HostState = "done"
	// HostFailed: the lease failed.
	HostFailed HostState = "failed"
	// HostSkipped: the host is left out of this rollout: a person skipped it
	// (RolloutHost.SkippedBy), the host was forgotten, or its agent rejected
	// the directive of a manual rollout.
	HostSkipped HostState = "skipped"
)

// Settled reports whether the host is no longer waited for.
func (s HostState) Settled() bool {
	return s == HostDone || s == HostSkipped
}

// Stage is the place of a host inside a rollout: hosts the change already
// broke go first, then one canary alone, then the rest. It is set anew in
// every reconcile of an open rollout for the members that wait, from the
// hosts' marks (Host.Broken) and Rollout.Canary; a member that was served
// keeps the stage it was served in.
type Stage int

// Stages, in serving order. The zero value is no stage.
const (
	// StageRepair: a member whose host is broken for the rollout's group: its
	// last lease for the same thing (the image reference, the packages)
	// failed. It waits for no canary, and the other members wait for it.
	StageRepair Stage = iota + 1
	// StageCanary: the one host that goes first of the others, alone.
	StageCanary
	// StageRest: every other member, as the budget and the host limit allow.
	StageRest
)

// RolloutHost is one member host of a rollout.
type RolloutHost struct {
	Host  string    `json:"host"`
	State HostState `json:"state"`
	Stage Stage     `json:"stage"`
	// NeedID is the host's need served by this rollout, once reported.
	NeedID  string `json:"needId,omitempty"`
	LeaseID string `json:"leaseId,omitempty"`
	// Error is the failure reason for HostFailed, or why a skipped host is
	// left out.
	Error string `json:"error,omitempty"`
	// SkippedBy names the person who skipped the host; empty for a host the
	// scheduler left out by itself. In a rollout of an observed cause such a
	// skip keeps the rollout open while the host still has the need.
	SkippedBy string    `json:"skippedBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// LeaseState is the lifecycle state of a lease.
type LeaseState string

// Lease states. Released, Failed and Expired are terminal. A host is
// "holding a lease" in every non-terminal state.
const (
	// LeaseGranted: offered; the agent has not reported an operation yet.
	LeaseGranted LeaseState = "granted"
	// LeaseRunning: the agent reports a non-terminal phase.
	LeaseRunning LeaseState = "running"
	// LeaseVerifying: the agent reported done; the controller is waiting
	// for its own readiness check (the node back at head with peers and
	// the host's validators attesting again).
	LeaseVerifying LeaseState = "verifying"
	// LeaseReleased: ready; the slot is free again.
	LeaseReleased LeaseState = "released"
	// LeaseFailed: the agent reported failed, went silent past the expiry,
	// or readiness did not arrive in time.
	LeaseFailed LeaseState = "failed"
	// LeaseExpired: the agent never started and the lease ran out, or it
	// finished without disrupting anything. Not a failure.
	LeaseExpired LeaseState = "expired"
)

// OpenLeaseStates lists the states in which a lease holds a slot: the ones
// that are not Terminal.
func OpenLeaseStates() []LeaseState {
	return []LeaseState{LeaseGranted, LeaseRunning, LeaseVerifying}
}

// Terminal reports whether the lease no longer holds a slot.
func (s LeaseState) Terminal() bool {
	return s == LeaseReleased || s == LeaseFailed || s == LeaseExpired
}

// LeaseCancel is the mark of a lease that a person ended while it was open
// (the end-lease action). The lease stays open and keeps its slot until its
// agent reports or goes silent, and then ends as failed.
type LeaseCancel struct {
	At time.Time `json:"at"`
	// By is the person, as recorded in the audit log.
	By string `json:"by"`
	// State is the state the lease was in when it was cancelled. It is for
	// people (audit, UI): no decision reads it.
	State  LeaseState `json:"state"`
	Reason string     `json:"reason,omitempty"`
}

// PeerCheck is the comparison of one client's peer count with the fleet in
// the readiness check of a lease.
type PeerCheck struct {
	// Layer is "EL" or "CL".
	Layer string `json:"layer"`
	// Peers is what the host's client reported, Median the median of the
	// reference hosts, Needs the share of it that the host must have.
	Peers  int `json:"peers"`
	Median int `json:"median"`
	Needs  int `json:"needs"`
}

// Lease is a slot granted to one host.
type Lease struct {
	ID    string     `json:"id"`
	Host  string     `json:"host"`
	State LeaseState `json:"state"`
	// Needs are the granted needs; Rollouts maps each need ID to the
	// rollout it is served for.
	Needs    []protocol.Need   `json:"needs"`
	Rollouts map[string]string `json:"rollouts"`

	// Weight is the host's validator weight at grant time; Charged is the
	// part of it that was live at grant time, i.e. what the grant cost the
	// budget. Both are for people: no decision reads them.
	Weight  uint64 `json:"weight"`
	Charged uint64 `json:"charged"`
	// Impact is the class of the lease: the heaviest impact class of the
	// containers its needs are about, node for packages and reboots. It
	// decides which limit the lease counts for and how readiness is judged.
	Impact protocol.Impact `json:"impact,omitempty"`
	// Force: readiness is not awaited; the lease is released as soon as the
	// agent reports done.
	Force bool `json:"force"`
	// GrantedBy is "scheduler" or the person behind a manual action.
	GrantedBy string `json:"grantedBy"`

	GrantedAt time.Time `json:"grantedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// AgentDoneAt is when the agent first reported a terminal phase.
	AgentDoneAt *time.Time `json:"agentDoneAt,omitempty"`
	// SyncedSince is the start of the current unbroken run of syncs, after
	// the agent's done report, in which the host's node probes reported the
	// node at head with enough peers. It is what the node part of the
	// readiness check measures; nil while there is no such run.
	SyncedSince *time.Time `json:"syncedSince,omitempty"`
	// SyncedUntil is the newest sync of that run. A sync that comes more than
	// hostStaleAfter after it starts a new run.
	SyncedUntil *time.Time `json:"syncedUntil,omitempty"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`

	// Operation is the agent's last report for this lease.
	Operation *protocol.Operation `json:"operation,omitempty"`
	// Readiness is a one-line progress note while verifying.
	Readiness string `json:"readiness,omitempty"`
	// PeerChecks is the comparison with the fleet that the last readiness
	// check made, per client it applied to. It is for people: no decision
	// reads it.
	PeerChecks []PeerCheck `json:"peerChecks,omitempty"`
	// Error is the failure reason for LeaseFailed.
	Error string `json:"error,omitempty"`
	// Cancelled is set once a person ended the lease. While the lease is
	// open the agent is told with every sync (protocol.Lease.Cancel) and
	// stops after the step in progress.
	Cancelled *LeaseCancel `json:"cancelled,omitempty"`
}

// DirectiveState is the controller-side state of a directive.
type DirectiveState string

// Directive states.
const (
	DirectivePending   DirectiveState = "pending"  // sent until acknowledged
	DirectiveAccepted  DirectiveState = "accepted" // agent took it
	DirectiveRejected  DirectiveState = "rejected" // agent refused it
	DirectiveCancelled DirectiveState = "cancelled"
)

// Directive is an instruction queued for one host.
type Directive struct {
	protocol.Directive

	Host  string         `json:"host"`
	State DirectiveState `json:"state"`
	// RolloutID links a roll / reboot directive to its manual rollout.
	RolloutID string    `json:"rolloutId,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AuditEntry is one line of the audit trail.
type AuditEntry struct {
	ID   int64     `json:"id"`
	Time time.Time `json:"time"`
	// Actor is the person's email (or token subject), "scheduler",
	// "agent:<host>" or "anonymous" when the API runs open.
	Actor string `json:"actor"`
	// Action is a dotted verb: settings.update, action.roll, action.reboot,
	// action.hold, action.unhold, action.pause, action.resume, action.skip,
	// action.retry, action.abort, action.forget, host.registered,
	// host.certificate-changed, lease.granted, lease.released, lease.failed,
	// rollout.created, rollout.halted, rollout.completed, ...
	Action string `json:"action"`
	// Target is the host, rollout or setting the entry is about.
	Target string `json:"target,omitempty"`
	// Detail is free-form JSON; for changes it has "before" and "after".
	Detail map[string]any `json:"detail,omitempty"`
}
