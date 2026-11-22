package protocol

import "time"

// Lease is the controller's permission for a host to go down once and serve
// the listed needs.
//
// Rules for the agent:
//   - Persist the lease before acting on it.
//   - Do not start a lease whose deadline has passed. The deadline is the
//     earlier of ExpiresAt and (time of first receipt + TTLSeconds), which
//     tolerates clock skew in either direction.
//   - Once started, finish, whatever happens to the lease afterwards, with one
//     exception: a lease that is restated with Cancel set. The agent then
//     begins no further step, completes the step in progress, brings the
//     containers back and reports the remaining steps as cancelled. A lease
//     that it has not started is never started.
//   - Validate every need locally (verb allowed, no hold, container managed
//     and existing, tag unchanged, pulled digest equals ToDigest, package in
//     the configured blocked set). The lease grants a slot; it cannot widen
//     what the agent is willing to do.
type Lease struct {
	ID        string    `json:"id"`
	GrantedAt time.Time `json:"grantedAt"`
	ExpiresAt time.Time `json:"expiresAt"`
	// TTLSeconds is the time left until expiry when the response was built.
	TTLSeconds int64 `json:"ttlSeconds"`
	// Needs are the granted needs: copies of needs the agent reported.
	Needs []Need `json:"needs"`
	// Cancel is set while a person has ended the lease and it is still open.
	// It is restated with the lease on every sync.
	Cancel bool `json:"cancel,omitempty"`
}

// Phase is the phase of an operation.
type Phase string

// Operation phases, in order. Done and Failed are terminal.
const (
	PhasePulling   Phase = "pulling"   // pulling the images; nothing is stopped yet
	PhaseDraining  Phase = "draining"  // stopping containers ahead of a reboot
	PhaseApplying  Phase = "applying"  // upgrading packages, recreating containers
	PhaseAction    Phase = "action"    // running a configured action
	PhaseRebooting Phase = "rebooting" // reboot issued; persisted before it
	PhaseStarting  Phase = "starting"  // containers starting (after a reboot too)
	PhaseVerifying Phase = "verifying" // containers must stay up, probes answer
	PhaseDone      Phase = "done"
	PhaseFailed    Phase = "failed"
)

// Terminal reports whether the phase is Done or Failed.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

// StepStatus is the outcome of one granted need.
type StepStatus string

// Step statuses.
const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	// StepSuperseded: the need no longer applied when its turn came (the tag
	// moved on, the package was already upgraded, agent.yaml names another
	// spec now). Nothing was disrupted for it and it is not a
	// failure.
	StepSuperseded StepStatus = "superseded"
	// StepRefused: local policy forbids the need (verb not allowed, hold,
	// unmanaged container). Nothing was disrupted for it.
	StepRefused StepStatus = "refused"
	// StepDeferred: an image of the lease could not be pulled, so nothing was
	// begun. Nothing was disrupted for it, it is not a failure and the need
	// stays open.
	StepDeferred StepStatus = "deferred"
	// StepCancelled: the lease was cancelled (Lease.Cancel) before the step
	// had begun. Nothing was disrupted for it and it is not a failure.
	StepCancelled StepStatus = "cancelled"
)

// Step is the per-need result inside an operation.
type Step struct {
	NeedID string     `json:"needId"`
	Status StepStatus `json:"status"`
	Error  string     `json:"error,omitempty"`
	// Action is how the run of an action need ended.
	Action *ActionResult `json:"action,omitempty"`
}

// Operation is the agent's work on one lease. It is persisted in the agent's
// state file and survives a reboot.
type Operation struct {
	LeaseID   string    `json:"leaseId"`
	Phase     Phase     `json:"phase"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Error is the reason for PhaseFailed.
	Error string `json:"error,omitempty"`
	Steps []Step `json:"steps"`
	// Disrupted is true once the agent has stopped anything. An operation
	// that ends without it (every step superseded or refused, or the lease
	// expired before the start) cost no availability.
	Disrupted bool `json:"disrupted"`
}

// DirectiveKind is the kind of a controller directive.
type DirectiveKind string

// Directive kinds. Directives never widen the verb set: roll and reboot only
// create a need, which is then served under a lease like any other.
const (
	// DirectiveHold puts the host on controller hold.
	DirectiveHold DirectiveKind = "hold"
	// DirectiveReleaseHold lifts a controller hold. It never lifts a local
	// hold.
	DirectiveReleaseHold DirectiveKind = "release-hold"
	// DirectiveRoll creates an image need for Container at the digest its
	// tag currently points at, even if the container already runs it.
	DirectiveRoll DirectiveKind = "roll"
	// DirectiveReboot creates a reboot need.
	DirectiveReboot DirectiveKind = "reboot"
	// DirectiveOverride asks the agent to write (or take out) the image or
	// the flags of Container's entry in the host's overrides file. Like a
	// roll it only creates a need: the container follows under a lease. The
	// agent accepts it only within what its own configuration allows the
	// controller to override on that container.
	DirectiveOverride DirectiveKind = "override"
	// DirectiveWithdraw takes back the manual need that the roll or reboot
	// directive Withdraws created: its rollout was aborted or the host left
	// it. A need that is not there (served, dropped) is nothing to withdraw.
	DirectiveWithdraw DirectiveKind = "withdraw"
)

// Parts of an override that an instruction can set.
const (
	OverrideImage = "image"
	OverrideFlags = "flags"
)

// Actor is who asked for a directive, as the controller states it: the user
// it authenticated, the teams of the login and the feature that allowed the
// request. The agent has verified that the answer is the controller's; it
// cannot verify the user. It writes the actor into the overrides file and
// its audit log as what the controller said.
type Actor struct {
	User    string   `json:"user"`
	Teams   []string `json:"teams,omitempty"`
	Feature string   `json:"feature,omitempty"`
}

// OverrideInstruction is the content of an override directive: one part of
// a container's override, as it is to be from now on.
type OverrideInstruction struct {
	// Part is OverrideImage or OverrideFlags. The other part of the entry
	// stays as it is.
	Part string `json:"part"`
	// Image is the image reference to run; empty takes the image override
	// away. For OverrideImage.
	Image string `json:"image,omitempty"`
	// AddFlags and RemoveFlags are the flags to add and to remove; both
	// empty takes the flag override away. For OverrideFlags.
	AddFlags    []string `json:"addFlags,omitempty"`
	RemoveFlags []string `json:"removeFlags,omitempty"`
	// Until is when the entry ends. Required when the instruction sets
	// something; the agent refuses an end further away than it allows.
	Until *time.Time `json:"until,omitempty"`
}

// Directive is one outstanding instruction. The controller repeats it in
// every response until the agent acknowledges its ID.
type Directive struct {
	ID   string        `json:"id"`
	Kind DirectiveKind `json:"kind"`
	// Container is the target of a roll or override directive.
	Container string `json:"container,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// By is who asked, for a directive that a person's action created.
	By *Actor `json:"by,omitempty"`
	// Override is the content of an override directive.
	Override *OverrideInstruction `json:"override,omitempty"`
	// Withdraws is the ID of the directive whose need a withdraw directive
	// takes back.
	Withdraws string `json:"withdraws,omitempty"`
	// Action names the configured action of an action directive.
	Action string `json:"action,omitempty"`
}

// DirectiveStatus is the agent's answer to a directive.
type DirectiveStatus string

// Directive statuses.
const (
	// DirectiveAccepted: the directive took effect (hold set or lifted,
	// manual need created and now listed in Needs).
	DirectiveAccepted DirectiveStatus = "accepted"
	// DirectiveRejected: local policy refused it; Error says why.
	DirectiveRejected DirectiveStatus = "rejected"
)

// DirectiveAck acknowledges a directive. The agent repeats an ack for as
// long as the controller repeats the directive, and remembers handled IDs so
// a repeated directive is not applied twice.
type DirectiveAck struct {
	ID     string          `json:"id"`
	Status DirectiveStatus `json:"status"`
	Error  string          `json:"error,omitempty"`
}
