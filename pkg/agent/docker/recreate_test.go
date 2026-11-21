package docker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
)

func TestRecreate(t *testing.T) {
	f := newFakeEngine()
	r := newTestRuntime(t, f)

	require.NoError(t, r.Recreate(t.Context(), "geth", digestB, 90*time.Second, true))

	assert.Equal(t, []string{
		"stop old t=90",
		"rename old geth" + replacedSuffix,
		"create geth",
		"connect net-b new1",
		"rename old geth" + retiredSuffix,
		"start new1",
		"remove old",
	}, f.calls)

	require.Len(t, f.created, 1)
	created := f.created[0]
	assert.Equal(t, fakeRef, created.Config.Image)
	assert.Equal(t, []string{"OWN=1"}, created.Config.Env, "image defaults are not pinned")
	assert.Nil(t, created.Config.Cmd)
	assert.Equal(t, map[string]string{"role": "el"}, created.Config.Labels)
	require.NotNil(t, created.NetworkingConfig)
	assert.Equal(t, map[string]*network.EndpointSettings{"net-a": {Aliases: []string{"el"}}},
		created.NetworkingConfig.EndpointsConfig, "one endpoint in the create request")

	now := f.find("geth")
	require.NotNil(t, now)
	assert.Equal(t, fakeNewImage, now.Image)
	assert.True(t, now.State.Running)
	assert.Len(t, f.containers, 1, "the old container is gone")
}

func TestRecreateWithoutStart(t *testing.T) {
	f := newFakeEngine()
	r := newTestRuntime(t, f)

	require.NoError(t, r.Recreate(t.Context(), "geth", digestB, 0, false))

	assert.Equal(t, []string{
		"stop old t=0",
		"rename old geth" + replacedSuffix,
		"create geth",
		"connect net-b new1",
		"rename old geth" + retiredSuffix,
		"remove old",
	}, f.calls)
	assert.Equal(t, container.StateCreated, f.find("geth").State.Status)
}

func TestRecreateRefusesBeforeStopping(t *testing.T) {
	tests := []struct {
		name         string
		digest       string
		prepare      func(*fakeEngine)
		wantMismatch bool
		wantErr      string
	}{
		{name: "another digest", digest: digestA, wantMismatch: true},
		{name: "digest of another image of the repository", digest: digestC, wantMismatch: true},
		{name: "empty digest", digest: "", wantMismatch: true},
		{
			name: "no local image for the reference", digest: digestB,
			prepare:      func(f *fakeEngine) { delete(f.images, fakeRef) },
			wantMismatch: true,
		},
		{
			name: "local image never came from a registry", digest: digestB,
			prepare:      func(f *fakeEngine) { f.images[fakeRef].RepoDigests = nil },
			wantMismatch: true,
		},
		{
			name: "old image gone", digest: digestB,
			prepare: func(f *fakeEngine) { delete(f.images, fakeOldImage) },
			wantErr: "is gone",
		},
		{
			name: "auto-remove container", digest: digestB,
			prepare: func(f *fakeEngine) { f.containers["old"].HostConfig.AutoRemove = true },
			wantErr: "auto-remove",
		},
		{
			name: "container with links", digest: digestB,
			prepare: func(f *fakeEngine) {
				f.containers["old"].HostConfig.Links = []string{"/db:/geth/database"}
			},
			wantErr: "has links (--link)",
		},
		{
			name: "incomplete inspect data", digest: digestB,
			prepare: func(f *fakeEngine) { f.containers["old"].HostConfig = nil },
			wantErr: "incomplete container inspect data",
		},
		{
			name: "unknown container", digest: digestB,
			prepare: func(f *fakeEngine) { f.containers["old"].Name = "/other" },
			wantErr: "no such container",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeEngine()
			if tt.prepare != nil {
				tt.prepare(f)
			}

			err := newTestRuntime(t, f).Recreate(t.Context(), "geth", tt.digest, time.Second, true)
			require.Error(t, err)
			assert.Equal(t, tt.wantMismatch, errors.Is(err, agent.ErrDigestMismatch), err.Error())

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
			}

			assert.Empty(t, f.calls, "nothing may be touched")
			assert.True(t, f.containers["old"].State.Running)
		})
	}
}

func TestRecreateAcceptsAnyDigestOfTheImage(t *testing.T) {
	f := newFakeEngine()
	f.images[fakeRef].RepoDigests = []string{
		"ethpandaops/geth@" + digestC, "ethpandaops/geth@" + digestB,
	}

	require.NoError(t, newTestRuntime(t, f).Recreate(t.Context(), "geth", digestB, time.Second, true))
}

func TestRecreateRollsBack(t *testing.T) {
	boom := errors.New("boom")
	tmp := "geth" + replacedSuffix
	done := "geth" + retiredSuffix

	tests := []struct {
		name       string
		prepare    func(*fakeEngine)
		wantCalls  []string
		wantErr    error
		wantInText string
		oldStopped bool
	}{
		{
			name:    "stop fails",
			prepare: func(f *fakeEngine) { f.fail["stop old t=1"] = boom },
			wantCalls: []string{
				"stop old t=1", "start old",
			},
			wantErr: boom,
		},
		{
			name:    "rename fails",
			prepare: func(f *fakeEngine) { f.fail["rename old "+tmp] = boom },
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "start old",
			},
			wantErr: boom,
		},
		{
			name:    "create fails",
			prepare: func(f *fakeEngine) { f.fail["create geth"] = boom },
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "create geth", "rename old geth", "start old",
			},
			wantErr: boom,
		},
		{
			name: "tag moved between check and create",
			prepare: func(f *fakeEngine) {
				f.onCreate = func() { f.images[fakeRef] = f.images[fakeOldImage] }
			},
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "create geth",
				"remove new1 with volumes", "rename old geth", "start old",
			},
			wantErr: agent.ErrDigestMismatch,
		},
		{
			name:    "network connect fails",
			prepare: func(f *fakeEngine) { f.fail["connect net-b new1"] = boom },
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "create geth", "connect net-b new1",
				"remove new1 with volumes", "rename old geth", "start old",
			},
			wantErr: boom,
		},
		{
			name:    "retiring the old container fails",
			prepare: func(f *fakeEngine) { f.fail["rename old "+done] = boom },
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "create geth", "connect net-b new1",
				"rename old " + done,
				"remove new1 with volumes", "rename old geth", "start old",
			},
			wantErr: boom,
		},
		{
			name:    "start fails",
			prepare: func(f *fakeEngine) { f.fail["start new1"] = boom },
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "create geth", "connect net-b new1",
				"rename old " + done, "start new1",
				"rename old " + tmp, "remove new1 with volumes", "rename old geth", "start old",
			},
			wantErr: boom,
		},
		{
			name: "a stopped container is not started by the rollback",
			prepare: func(f *fakeEngine) {
				f.containers["old"].State = &container.State{Status: container.StateExited}
				f.fail["create geth"] = boom
			},
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "create geth", "rename old geth",
			},
			wantErr:    boom,
			oldStopped: true,
		},
		{
			name: "rollback problems are reported with the cause",
			prepare: func(f *fakeEngine) {
				f.fail["create geth"] = boom
				f.fail["start old"] = errors.New("cannot start")
			},
			wantCalls: []string{
				"stop old t=1", "rename old " + tmp, "create geth", "rename old geth", "start old",
			},
			wantErr:    boom,
			wantInText: "rollback incomplete: start old container: cannot start",
			oldStopped: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeEngine()
			tt.prepare(f)

			err := newTestRuntime(t, f).Recreate(t.Context(), "geth", digestB, time.Second, true)
			require.ErrorIs(t, err, tt.wantErr)
			assert.ErrorContains(t, err, tt.wantInText)
			assert.Equal(t, tt.wantCalls, f.calls)

			old := f.find("geth")
			require.NotNil(t, old, "the old container has its name back")
			assert.Equal(t, "old", old.ID)
			assert.Equal(t, !tt.oldStopped, old.State.Running)
			assert.Len(t, f.containers, 1, "no half-created container is left")
		})
	}
}

func TestRecreateRollsBackWithCancelledContext(t *testing.T) {
	f := newFakeEngine()
	ctx, cancel := context.WithCancel(t.Context())

	f.onCreate = cancel
	f.fail["create geth"] = context.Canceled

	err := newTestRuntime(t, f).Recreate(ctx, "geth", digestB, time.Second, true)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, f.find("geth").State.Running, "the rollback does not depend on the caller's context")
}

// leftoverEngine is a fake after a Recreate whose old container could not
// be removed: the replacement runs under the name, the old container is
// left under the retired name.
func leftoverEngine(t *testing.T, start bool) (*fakeEngine, *Runtime) {
	t.Helper()

	f := newFakeEngine()
	f.fail["remove old"] = errors.New("device busy")
	r := newTestRuntime(t, f)

	require.NoError(t, r.Recreate(t.Context(), "geth", digestB, time.Second, start),
		"the container is replaced, the roll succeeded")
	require.Equal(t, []string{"remove old", "remove old", "update old restart=no"},
		f.calls[len(f.calls)-3:], "retried once, then kept from restarting next to its replacement")
	require.Equal(t, "new1", f.find("geth").ID)
	require.Equal(t, "old", f.find("geth"+retiredSuffix).ID)

	f.calls = nil

	return f, r
}

func TestRecreateCannotRemoveOld(t *testing.T) {
	t.Run("the leftover is not observed", func(t *testing.T) {
		_, r := leftoverEngine(t, true)

		got, err := r.Observe(t.Context())
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "geth", got[0].Name)
		assert.Equal(t, "new1", got[0].ID)
	})

	t.Run("a refused recreate touches neither container", func(t *testing.T) {
		f, r := leftoverEngine(t, true)
		delete(f.fail, "remove old")

		err := r.Recreate(t.Context(), "geth", digestA, time.Second, true)
		require.ErrorIs(t, err, agent.ErrDigestMismatch)
		assert.Empty(t, f.calls, "nothing may be touched")
		assert.Equal(t, "new1", f.find("geth").ID)
		assert.True(t, f.find("geth").State.Running, "the replacement keeps running")
	})

	t.Run("the next recreate removes the leftover and replaces the replacement", func(t *testing.T) {
		f, r := leftoverEngine(t, true)
		delete(f.fail, "remove old")

		require.NoError(t, r.Recreate(t.Context(), "geth", digestB, time.Second, true))
		assert.Equal(t, []string{"remove old", "stop new1 t=1"}, f.calls[:2])
		assert.Equal(t, "new2", f.find("geth").ID)
		assert.Len(t, f.containers, 1)
	})

	t.Run("a recreate does not stop anything while the leftover is stuck", func(t *testing.T) {
		f, r := leftoverEngine(t, true)

		err := r.Recreate(t.Context(), "geth", digestB, time.Second, true)
		require.ErrorIs(t, err, errLeftover)
		assert.Equal(t, []string{"remove old", "remove old", "update old restart=no"}, f.calls)
		assert.True(t, f.find("geth").State.Running)
	})

	t.Run("start and stop retry the removal and work on the replacement", func(t *testing.T) {
		f, r := leftoverEngine(t, false)
		assert.Equal(t, container.StateCreated, f.find("geth").State.Status)

		// Still stuck: the unstarted replacement is started all the same.
		require.NoError(t, r.Start(t.Context(), "geth"))
		assert.Equal(t, []string{"remove old", "remove old", "update old restart=no", "start new1"}, f.calls)

		f.calls = nil
		delete(f.fail, "remove old")

		require.NoError(t, r.Stop(t.Context(), "geth", time.Second))
		assert.Equal(t, []string{"remove old", "stop new1 t=1"}, f.calls)
		assert.Len(t, f.containers, 1)
	})

	t.Run("a leftover the daemon restarted is stopped before it is removed", func(t *testing.T) {
		f, r := leftoverEngine(t, true)
		delete(f.fail, "remove old")
		f.containers["old"].State = &container.State{Status: container.StateRunning, Running: true}

		require.NoError(t, r.Start(t.Context(), "geth"))
		assert.Equal(t, []string{"stop old", "remove old", "start new1"}, f.calls)
	})

	t.Run("a leftover whose replacement is gone gets its name back", func(t *testing.T) {
		f, r := leftoverEngine(t, true)
		delete(f.containers, "new1")

		require.NoError(t, r.Start(t.Context(), "geth"))
		assert.Equal(t, []string{"rename old geth", "start old"}, f.calls)
	})
}

// interruptedEngine is a fake in the state a Recreate leaves when it dies
// after the create: the old container stopped under the temporary name, an
// unfinished replacement under the real one.
func interruptedEngine(running bool) *fakeEngine {
	f := newFakeEngine()
	f.containers["old"].Name = "/geth" + replacedSuffix
	f.containers["old"].State = &container.State{Status: container.StateExited}
	f.containers["half"] = &container.InspectResponse{
		ID: "half", Name: "/geth", Image: fakeNewImage,
		State:  &container.State{Status: container.StateCreated},
		Config: &container.Config{Image: fakeRef},
	}

	if running {
		f.containers["half"].State = &container.State{Status: container.StateRunning, Running: true}
	}

	return f
}

func TestRecreateRecoversInterruptedRun(t *testing.T) {
	t.Run("old container left under the temporary name", func(t *testing.T) {
		f := newFakeEngine()
		f.containers["old"].Name = "/geth" + replacedSuffix
		f.containers["old"].State = &container.State{Status: container.StateExited}

		require.NoError(t, newTestRuntime(t, f).Recreate(t.Context(), "geth", digestB, time.Second, true))
		assert.Equal(t, "rename old geth", f.calls[0])
		assert.Equal(t, fakeNewImage, f.find("geth").Image)
		assert.Len(t, f.containers, 1)
	})

	t.Run("unfinished replacement next to it", func(t *testing.T) {
		f := interruptedEngine(false)

		require.NoError(t, newTestRuntime(t, f).Recreate(t.Context(), "geth", digestB, time.Second, true))
		assert.Equal(t, []string{"remove half with volumes", "rename old geth"}, f.calls[:2])
		assert.Equal(t, "new1", f.find("geth").ID)
		assert.Equal(t, []string{"OWN=1"}, f.created[0].Config.Env, "built from the old container")
		assert.Len(t, f.containers, 1)
	})

	t.Run("a refused recreate leaves the interrupted run alone", func(t *testing.T) {
		for _, running := range []bool{false, true} {
			f := interruptedEngine(running)

			err := newTestRuntime(t, f).Recreate(t.Context(), "geth", digestA, time.Second, true)
			require.ErrorIs(t, err, agent.ErrDigestMismatch)
			assert.Empty(t, f.calls, "nothing may be touched")
			assert.Equal(t, running, f.containers["half"].State.Running)
		}
	})

	t.Run("an unfinished replacement the daemon started is stopped, not killed", func(t *testing.T) {
		f := interruptedEngine(true)
		// Keep the roll itself from going on, to see the recovered state.
		delete(f.images, fakeOldImage)

		err := newTestRuntime(t, f).Recreate(t.Context(), "geth", digestB, time.Second, true)
		require.ErrorContains(t, err, "is gone")
		assert.Equal(t, []string{
			"stop half", "remove half with volumes", "rename old geth", "start old",
		}, f.calls, "the old container takes over, running as the unfinished one was")
		assert.True(t, f.find("geth").State.Running)
	})

	t.Run("start restores the old container", func(t *testing.T) {
		f := interruptedEngine(true)

		require.NoError(t, newTestRuntime(t, f).Start(t.Context(), "geth"))
		assert.Equal(t, []string{
			"stop half", "remove half with volumes", "rename old geth", "start old",
		}, f.calls)
	})

	t.Run("stop restores the old container and leaves it stopped", func(t *testing.T) {
		f := interruptedEngine(true)

		require.NoError(t, newTestRuntime(t, f).Stop(t.Context(), "geth", time.Second))
		assert.Equal(t, []string{
			"stop half", "remove half with volumes", "rename old geth", "stop old t=1",
		}, f.calls)
	})

	t.Run("a complete replacement that was never started is kept", func(t *testing.T) {
		// Died between retiring the old container and removing it, with
		// start false.
		f := interruptedEngine(false)
		f.containers["old"].Name = "/geth" + retiredSuffix

		require.NoError(t, newTestRuntime(t, f).Start(t.Context(), "geth"))
		assert.Equal(t, []string{"remove old", "start half"}, f.calls)
	})

	t.Run("a failed recovery is reported", func(t *testing.T) {
		f := interruptedEngine(false)
		f.fail["rename old geth"] = errors.New("daemon down")

		require.ErrorContains(t, newTestRuntime(t, f).Start(t.Context(), "geth"), "daemon down")
	})
}
