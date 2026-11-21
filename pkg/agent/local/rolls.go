package local

import (
	"cmp"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Rolls is the in-memory progress of the rolls requested on the host, for
// `roll` and `status`. Safe for concurrent use.
type Rolls interface {
	// Add takes a roll in state RollResolving; a refusal when one of the
	// container is already under way. The caller checks plan.RollRefusal.
	Add(user, container string) (Roll, error)
	// Take hands the loop the rolls added since the last call.
	Take() []Roll
	// Set moves a roll to a state with a detail.
	Set(id, state, detail string)
	// Open reports whether a roll is under way; somebody waits for it.
	Open() bool
	// List returns the rolls, oldest first; never nil, so the status says
	// "rolls": [].
	List() []Roll
	// Follow brings every roll past RollResolving up to date from a cycle's
	// progress and drops old final rolls.
	Follow(p *Progress)
}

// Progress is what the loop knows after a cycle about the local rolls' needs.
type Progress struct {
	// Lease is the lease the agent holds ("" for none).
	Lease     string
	Started   bool
	Operation *protocol.Operation
	// Open are the IDs of the open manual needs.
	Open []string

	Hold protocol.Hold
	// Answered is false when the sync failed; Queue and Ended are then nil.
	Answered bool
	Queue    *protocol.Queue
	Ended    *protocol.EndedLease
}

const (
	maxRolls = 32
	// rollRetention is how long a final roll stays in the list.
	rollRetention = time.Hour
	// rollIDTime is the time part of a roll's ID.
	rollIDTime = "20060102T150405"
	// waitingForController is the detail of a queued roll without a reason.
	waitingForController = "waiting for the controller"
)

type localRoll struct {
	Roll

	// taken: the loop took the roll.
	taken bool
	// seenLease: the roll's need was a step of a lease.
	seenLease bool
}

type rolls struct {
	now func() time.Time

	mu   sync.Mutex
	list []*localRoll
	next int
}

var _ Rolls = (*rolls)(nil)

// NewRolls returns an empty list.
func NewRolls(now func() time.Time) Rolls {
	return &rolls{now: now}
}

// Add implements Rolls.
func (r *rolls) Add(user, container string) (Roll, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, o := range r.list {
		if o.Container == container && !o.Final() {
			return Roll{}, plan.Refusal("a roll of %s is already under way (%s, %s)", container, o.ID, o.State)
		}
	}

	now := r.now().UTC()
	r.next++
	roll := &localRoll{Roll: Roll{
		ID:        fmt.Sprintf("%s-%d", now.Format(rollIDTime), r.next),
		Container: container, RequestedBy: user, RequestedAt: now, UpdatedAt: now,
		State: RollResolving, Detail: "asking the registry and pulling the image",
	}}
	r.list = append(r.list, roll)

	return roll.Roll, nil
}

// Take implements Rolls.
func (r *rolls) Take() []Roll {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []Roll

	for _, o := range r.list {
		if !o.taken {
			o.taken = true
			out = append(out, o.Roll)
		}
	}

	return out
}

// Set implements Rolls.
func (r *rolls) Set(id, state, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, o := range r.list {
		if o.ID == id {
			r.update(o, state, detail, r.now().UTC())
		}
	}
}

// update moves a roll; UpdatedAt changes only with the state or detail.
func (r *rolls) update(o *localRoll, state, detail string, now time.Time) {
	if o.State != state || o.Detail != detail {
		o.State, o.Detail, o.UpdatedAt = state, detail, now
	}
}

// Open implements Rolls.
func (r *rolls) Open() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.ContainsFunc(r.list, func(o *localRoll) bool { return !o.Final() })
}

// List implements Rolls.
func (r *rolls) List() []Roll {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Roll, 0, len(r.list))
	for _, o := range r.list {
		out = append(out, o.Roll)
	}

	return out
}

// Follow implements Rolls.
func (r *rolls) Follow(p *Progress) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now().UTC()

	for _, o := range r.list {
		if !o.taken || o.Final() || o.State == RollResolving {
			continue
		}

		state, detail := rollState(o, p)
		r.update(o, state, detail, now)

		o.Position = 0
		if o.State == RollQueued && p.Queue != nil {
			o.Position = p.Queue.Position
		}
	}

	r.list = slices.DeleteFunc(r.list, func(o *localRoll) bool {
		return o.Final() && now.Sub(o.UpdatedAt) > rollRetention
	})

	// The rolls not yet taken are the newest; the oldest go.
	for len(r.list) > maxRolls && r.list[0].taken {
		r.list = r.list[1:]
	}
}

// rollState says where a roll stands after a cycle.
func rollState(o *localRoll, p *Progress) (state, detail string) {
	needID := protocol.LocalNeedID(o.ID)

	if p.Lease != "" && p.Operation != nil {
		for i := range p.Operation.Steps {
			if step := &p.Operation.Steps[i]; step.NeedID == needID {
				o.seenLease, o.Lease = true, p.Lease

				return leasedRollState(p, step)
			}
		}
	}

	ended := p.Ended != nil && p.Ended.ID == o.Lease

	switch open := slices.Contains(p.Open, needID); {
	case open && p.Queue != nil:
		return RollQueued, p.Queue.Reason
	case open && p.Hold.Active():
		return RollQueued, plan.HoldReason(p.Hold)
	case open:
		return RollQueued, waitingForController
	case !o.seenLease:
		return RollSuperseded, "the roll was dropped: the container no longer qualifies"
	case ended && p.Ended.State != "released":
		return RollFailed, cmp.Or(p.Ended.Reason, "the controller ended the lease as "+p.Ended.State)
	case o.State == RollVerified || ended:
		return RollDone, "the controller released the slot"
	default:
		// The operation is forgotten and was not seen verified: as it was.
		return o.State, o.Detail
	}
}

// leasedRollState is the state of a roll whose need is a step of the lease.
func leasedRollState(p *Progress, step *protocol.Step) (state, detail string) {
	op := p.Operation

	switch step.Status {
	case protocol.StepFailed:
		return RollFailed, step.Error
	case protocol.StepRefused:
		return RollRejected, step.Error
	case protocol.StepSuperseded, protocol.StepCancelled:
		return RollSuperseded, step.Error
	case protocol.StepDeferred:
		// The need stays open: the roll is tried again.
		return RollQueued, "not begun, it is tried again: " + step.Error
	case protocol.StepDone:
		switch op.Phase {
		case protocol.PhaseDone:
			return RollVerified, "the container stayed up; the controller now checks that the node is back"
		case protocol.PhaseFailed:
			return RollFailed, op.Error
		default:
			return RollRecreated, "watching that the container stays up"
		}
	case protocol.StepRunning:
		return RollRecreating, ""
	default:
	}

	switch {
	case op.Phase == protocol.PhaseFailed:
		return RollFailed, cmp.Or(step.Error, op.Error)
	case !p.Started:
		return RollGranted, "lease " + p.Lease + " granted"
	default:
		return RollRecreating, "lease " + p.Lease + ": " + string(op.Phase)
	}
}
