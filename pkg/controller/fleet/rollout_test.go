package fleet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The open states are the ones that are not terminal: the store's queries are
// built from the lists, the scheduler asks Terminal.
func TestOpenStatesAreTheOnesThatAreNotTerminal(t *testing.T) {
	leases := []LeaseState{
		LeaseGranted, LeaseRunning, LeaseVerifying, LeaseReleased, LeaseFailed, LeaseExpired,
	}
	for _, s := range leases {
		assert.NotEqual(t, s.Terminal(), contains(OpenLeaseStates(), s), s)
	}

	rollouts := []RolloutState{
		RolloutActive, RolloutHalted, RolloutCompleted, RolloutSuperseded, RolloutAborted,
	}
	for _, s := range rollouts {
		assert.NotEqual(t, s.Terminal(), contains(OpenRolloutStates(), s), s)
	}

	assert.False(t, LeaseState("no such state").Terminal())
	assert.False(t, RolloutState("no such state").Terminal())
}

func contains[S comparable](list []S, s S) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}
