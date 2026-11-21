package local

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestRollsAddAndTake(t *testing.T) {
	now := fileTime
	r := NewRolls(func() time.Time { return now })

	assert.NotNil(t, r.List())
	assert.Empty(t, r.List())
	assert.False(t, r.Open())

	roll, err := r.Add("local:sam", "geth")
	require.NoError(t, err)
	assert.Equal(t, Roll{
		ID: "20261120T160000-1", Container: "geth", RequestedBy: "local:sam", RequestedAt: now, UpdatedAt: now,
		State: RollResolving, Detail: "asking the registry and pulling the image",
	}, roll)
	assert.True(t, r.Open())

	_, err = r.Add("local:sam", "geth")
	require.ErrorIs(t, err, plan.ErrRefused)
	require.ErrorContains(t, err, "a roll of geth is already under way (20261120T160000-1, resolving)")

	other, err := r.Add("local:sam", "beacon")
	require.NoError(t, err)

	assert.Equal(t, []Roll{roll, other}, r.Take())
	assert.Empty(t, r.Take())

	r.Set(roll.ID, RollRejected, "no")
	assert.Equal(t, RollRejected, r.List()[0].State)

	_, err = r.Add("local:sam", "geth")
	require.NoError(t, err, "a final roll is not under way")
}

// lease is the progress of a lease whose one step serves the roll.
func lease(id string, started bool, phase protocol.Phase, step protocol.Step) *Progress {
	return &Progress{
		Lease: "l1", Started: started, Answered: true,
		Operation: &protocol.Operation{Phase: phase, Steps: []protocol.Step{
			{NeedID: protocol.LocalNeedID(id), Status: step.Status, Error: step.Error},
		}},
	}
}

func TestRollsFollow(t *testing.T) {
	now := fileTime
	r := NewRolls(func() time.Time { return now })
	roll, err := r.Add("local:sam", "geth")
	require.NoError(t, err)

	need := protocol.LocalNeedID(roll.ID)
	state := func(p *Progress) Roll {
		r.Follow(p)

		return r.List()[0]
	}

	// Not taken, or resolving: Follow leaves it.
	assert.Equal(t, RollResolving, state(&Progress{}).State)
	r.Take()
	assert.Equal(t, RollResolving, state(&Progress{}).State)

	r.Set(roll.ID, RollQueued, waitingForController)

	got := state(&Progress{Open: []string{need}, Answered: true, Queue: &protocol.Queue{Position: 3, Reason: "budget"}})
	assert.Equal(t, RollQueued, got.State)
	assert.Equal(t, "budget", got.Detail)
	assert.Equal(t, 3, got.Position)

	got = state(&Progress{Open: []string{need}, Hold: protocol.Hold{Local: true, LocalReason: "disk"}})
	assert.Equal(t, "host is on local hold: disk", got.Detail)
	assert.Zero(t, got.Position)

	assert.Equal(t, waitingForController, state(&Progress{Open: []string{need}}).Detail)

	for _, tt := range []struct {
		p      *Progress
		state  string
		detail string
	}{
		{lease(roll.ID, false, protocol.PhaseDraining, protocol.Step{Status: protocol.StepPending}), RollGranted, "lease l1 granted"},
		{lease(roll.ID, true, protocol.PhaseApplying, protocol.Step{Status: protocol.StepPending}), RollRecreating, "lease l1: applying"},
		{lease(roll.ID, true, protocol.PhaseApplying, protocol.Step{Status: protocol.StepRunning}), RollRecreating, ""},
		{lease(roll.ID, true, protocol.PhaseVerifying, protocol.Step{Status: protocol.StepDone}), RollRecreated, "watching that the container stays up"},
		{lease(roll.ID, true, protocol.PhaseDone, protocol.Step{Status: protocol.StepDone}), RollVerified,
			"the container stayed up; the controller now checks that the node is back"},
	} {
		got = state(tt.p)
		assert.Equal(t, tt.state, got.State)
		assert.Equal(t, tt.detail, got.Detail)
		assert.Equal(t, "l1", got.Lease)
	}

	got = state(&Progress{Answered: true, Ended: &protocol.EndedLease{ID: "l1", State: "released"}})
	assert.Equal(t, RollDone, got.State)
	assert.Equal(t, "the controller released the slot", got.Detail)
	assert.False(t, r.Open())

	// A final roll goes after an hour.
	now = now.Add(2 * time.Hour)
	r.Follow(&Progress{})
	assert.Empty(t, r.List())
}

func TestRollsFollowEnds(t *testing.T) {
	r := NewRolls(func() time.Time { return fileTime })

	start := func(container string) string {
		roll, err := r.Add("local:sam", container)
		require.NoError(t, err)
		r.Take()
		r.Set(roll.ID, RollQueued, waitingForController)

		return roll.ID
	}

	last := func() Roll {
		l := r.List()

		return l[len(l)-1]
	}

	// Gone without a lease.
	start("a")
	r.Follow(&Progress{})
	assert.Equal(t, Roll{State: RollSuperseded, Detail: "the roll was dropped: the container no longer qualifies"},
		Roll{State: last().State, Detail: last().Detail})

	for _, tt := range []struct {
		step   protocol.Step
		phase  protocol.Phase
		state  string
		detail string
	}{
		{protocol.Step{Status: protocol.StepFailed, Error: "boom"}, protocol.PhaseFailed, RollFailed, "boom"},
		{protocol.Step{Status: protocol.StepRefused, Error: "hold"}, protocol.PhaseDone, RollRejected, "hold"},
		{
			protocol.Step{Status: protocol.StepDeferred, Error: "pull x: timeout"}, protocol.PhaseDone,
			RollQueued, "not begun, it is tried again: pull x: timeout",
		},
		{protocol.Step{Status: protocol.StepCancelled, Error: "c"}, protocol.PhaseDone, RollSuperseded, "c"},
		{protocol.Step{Status: protocol.StepPending}, protocol.PhaseFailed, RollFailed, "op"},
	} {
		id := start("c-" + tt.state + tt.detail)
		p := lease(id, true, tt.phase, tt.step)
		p.Operation.Error = "op"
		r.Follow(p)
		assert.Equal(t, tt.state, last().State)
		assert.Equal(t, tt.detail, last().Detail)
	}

	// The controller fails the lease after the agent's part.
	id := start("x")
	r.Follow(lease(id, true, protocol.PhaseVerifying, protocol.Step{Status: protocol.StepDone}))
	r.Follow(&Progress{Answered: true, Ended: &protocol.EndedLease{ID: "l1", State: "failed"}})
	assert.Equal(t, RollFailed, last().State)
	assert.Equal(t, "the controller ended the lease as failed", last().Detail)
}

func TestRollsAreBounded(t *testing.T) {
	r := NewRolls(func() time.Time { return fileTime })

	for i := range maxRolls + 5 {
		roll, err := r.Add("local:sam", string(rune('a'+i)))
		require.NoError(t, err)
		r.Set(roll.ID, RollRejected, "no")
	}

	r.Take()
	r.Follow(&Progress{})
	assert.Len(t, r.List(), maxRolls)
	assert.Equal(t, string(rune('a'+5)), r.List()[0].Container, "the oldest go")
}
