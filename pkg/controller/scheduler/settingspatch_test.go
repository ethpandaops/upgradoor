package scheduler

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// TestPatchSettings: a patch is applied to the settings as they are under the
// lock, validated, stored and audited with before and after; a patch that
// changes nothing writes nothing; a failing patch changes nothing.
func TestPatchSettings(t *testing.T) {
	h := newHarness(t)

	base, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)

	audits := func() int { return len(h.store.auditActions()) }

	// Nothing named: no write, no entry.
	got, changed, err := h.svc.PatchSettings(h.ctx, "alice", func(*fleet.Settings) error { return nil })
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, base, got)
	assert.Zero(t, audits())

	// What is set already: the same.
	got, changed, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = base.MinPeers
		s.Paused = base.Paused

		return nil
	})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, base, got)
	assert.Zero(t, audits())

	// A change, by a request with an authorization.
	ctx := WithAuthorization(h.ctx, Authorization{By: "admin", Teams: []string{"panda"}})

	got, changed, err = h.svc.PatchSettings(ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = 7

		return nil
	})
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 7, got.MinPeers)

	entry := h.store.lastAudit()
	assert.Equal(t, "settings.update", entry.Action)
	assert.Equal(t, "alice", entry.Actor)
	assert.Equal(t, base, entry.Detail[keyBefore])
	assert.Equal(t, got, entry.Detail[keyAfter])
	assert.Equal(t, "admin", entry.Detail[keyAuthorizedBy])
	assert.Equal(t, 1, audits())

	// An error of the patch is the caller's and changes nothing, also when
	// the patch wrote into what it was given.
	_, changed, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = 9

		return assert.AnError
	})
	require.ErrorIs(t, err, assert.AnError)
	assert.False(t, changed)

	// A result that is not valid is refused.
	_, changed, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.MinPeers = 0

		return nil
	})
	require.ErrorIs(t, err, ErrInvalidAction)
	assert.False(t, changed)

	now, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, got, now)
	assert.Equal(t, 1, audits())

	// A patch sees what an action wrote before it: the global pause is not
	// overwritten by a patch that does not name it, with a restart between.
	_, err = h.svc.Pause(h.ctx, "bob", ScopeRequest{})
	require.NoError(t, err)

	h.restart()

	got, _, err = h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
		s.ReadyPeerShare = 0.25

		return nil
	})
	require.NoError(t, err)
	assert.True(t, got.Paused)
	assert.Equal(t, 7, got.MinPeers)
	assert.InDelta(t, 0.25, got.ReadyPeerShare, 0)
}

// TestPatchSettingsOverlapping: patches that run at once each change what
// they name; none writes back what it read before another one wrote.
func TestPatchSettingsOverlapping(t *testing.T) {
	h := newHarness(t)

	const rounds = 50

	var wg sync.WaitGroup

	patch := func(apply func(i int, s *fleet.Settings)) {
		defer wg.Done()

		for i := range rounds {
			_, _, err := h.svc.PatchSettings(h.ctx, "alice", func(s *fleet.Settings) error {
				apply(i, s)

				return nil
			})
			assert.NoError(t, err)
		}
	}

	wg.Add(3)

	go patch(func(i int, s *fleet.Settings) { s.MinPeers = 4 + i })
	go patch(func(i int, s *fleet.Settings) { s.ReadyEpochs = uint64(2 + i) })
	go patch(func(i int, s *fleet.Settings) { s.PauseAfterCanaryImage = i%2 == 1 })

	wg.Wait()

	got, err := h.svc.Settings(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, 4+rounds-1, got.MinPeers)
	assert.Equal(t, uint64(2+rounds-1), got.ReadyEpochs)
	assert.True(t, got.PauseAfterCanaryImage)
}
