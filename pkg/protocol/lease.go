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
//     and not custom, tag unchanged, pulled digest equals ToDigest, package in
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
	// It is restated with the lease on every sync. An agent that does not
	// know the field finishes its operation.
	Cancel bool `json:"cancel,omitempty"`
}

// Phase is the phase of an operation.
type Phase string

// Operation phases, in order. Done and Failed are terminal.
const (
	PhaseDraining  Phase = "draining"  // stopping containers ahead of a reboot
	PhaseApplying  Phase = "applying"  // upgrading packages, recreating containers
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
	// moved on, the package was already upgraded, the container was swapped
	// for a custom image). Nothing was disrupted for it and it is not a
	// failure.
	StepSuperseded StepStatus = "superseded"
	// StepRefused: local policy forbids the need (verb not allowed, hold,
	// unmanaged container). Nothing was disrupted for it.
	StepRefused StepStatus = "refused"
	// StepCancelled: the lease was cancelled (Lease.Cancel) before the step
	// had begun. Nothing was disrupted for it and it is not a failure.
	StepCancelled StepStatus = "cancelled"
)

// Step is the per-need result inside an operation.
type Step struct {
	NeedID string     `json:"needId"`
	Status StepStatus `json:"status"`
	Error  string     `json:"error,omitempty"`
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
	// hold file.
	DirectiveReleaseHold DirectiveKind = "release-hold"
	// DirectiveRoll creates an image need for Container at the digest its
	// tag currently points at, even if the container already runs it.
	DirectiveRoll DirectiveKind = "roll"
	// DirectiveReboot creates a reboot need.
	DirectiveReboot DirectiveKind = "reboot"
	// DirectiveRecheck asks the agent to check its registries and package
	// state now instead of at the next poll. It has no other effect.
	DirectiveRecheck DirectiveKind = "recheck"
)

// Directive is one outstanding instruction. The controller repeats it in
// every response until the agent acknowledges its ID.
type Directive struct {
	ID   string        `json:"id"`
	Kind DirectiveKind `json:"kind"`
	// Container is the target of a roll directive.
	Container string `json:"container,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// DirectiveStatus is the agent's answer to a directive.
type DirectiveStatus string

// Directive statuses.
const (
	// DirectiveAccepted: the directive took effect (hold set or lifted,
	// recheck started, manual need created and now listed in Needs).
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
