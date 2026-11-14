package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
)

const (
	fakeRef      = "ethpandaops/geth:master"
	fakeOldImage = "sha256:old"
	fakeNewImage = "sha256:new"
)

// fakeEngine is an in-memory Docker daemon, just deep enough for the
// control flow of the runtime: containers by ID with a name, images by
// reference and ID, a log of the mutating calls and injectable failures.
type fakeEngine struct {
	version    string
	containers map[string]*container.InspectResponse
	// images maps references and IDs to images.
	images map[string]*image.InspectResponse
	// calls are the mutating calls in order, e.g. "stop old".
	calls []string
	// fail makes the call with that log entry fail.
	fail map[string]error
	// onCreate runs before a container is created.
	onCreate func()
	created  []client.ContainerCreateOptions
	pulls    []client.ImagePullOptions
	pullMsgs []jsonstream.Message
	nextID   int
}

func newFakeEngine() *fakeEngine {
	f := &fakeEngine{
		version:    "1.47",
		containers: make(map[string]*container.InspectResponse, 4),
		images:     make(map[string]*image.InspectResponse, 4),
		fail:       make(map[string]error, 2),
	}

	oldImage := &image.InspectResponse{
		ID:          fakeOldImage,
		RepoDigests: []string{"ethpandaops/geth@" + digestA},
		Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{
			Env:    []string{"PATH=/bin", "MODE=old"},
			Cmd:    []string{"serve"},
			Labels: map[string]string{labelImageRevision: "oldrev", labelImageVersion: "v1"},
		}},
	}
	newImage := &image.InspectResponse{
		ID:          fakeNewImage,
		RepoDigests: []string{"ethpandaops/geth@" + digestB},
		Config: &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{
			Env:    []string{"PATH=/bin", "MODE=new"},
			Cmd:    []string{"serve"},
			Labels: map[string]string{labelImageRevision: "newrev", labelImageVersion: "v2"},
		}},
	}

	f.images[fakeOldImage] = oldImage
	f.images[fakeNewImage] = newImage
	f.images[fakeRef] = newImage

	f.containers["old"] = &container.InspectResponse{
		ID:           "old",
		Name:         "/geth",
		Image:        fakeOldImage,
		RestartCount: 2,
		State: &container.State{
			Status:    container.StateRunning,
			Running:   true,
			StartedAt: "2026-11-14T10:00:00.5Z",
			Health:    &container.Health{Status: container.Healthy},
		},
		Config: &container.Config{
			Image:  fakeRef,
			Env:    []string{"OWN=1", "PATH=/bin", "MODE=old"},
			Cmd:    []string{"serve"},
			Labels: map[string]string{"role": "el", labelImageRevision: "oldrev", labelImageVersion: "v1"},
		},
		HostConfig: &container.HostConfig{NetworkMode: "net-a"},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"net-a": {Aliases: []string{"el"}},
			"net-b": {},
		}},
	}

	return f
}

func (f *fakeEngine) record(call string) error {
	f.calls = append(f.calls, call)

	return f.fail[call]
}

// find resolves a container by ID or name.
func (f *fakeEngine) find(idOrName string) *container.InspectResponse {
	if c, ok := f.containers[idOrName]; ok {
		return c
	}

	for _, c := range f.containers {
		if c.Name == "/"+idOrName {
			return c
		}
	}

	return nil
}

func notFound(what string) error {
	return fmt.Errorf("no such %s: %w", what, cerrdefs.ErrNotFound)
}

func (f *fakeEngine) ContainerList(
	context.Context, client.ContainerListOptions,
) (client.ContainerListResult, error) {
	var res client.ContainerListResult
	for id := range f.containers {
		res.Items = append(res.Items, container.Summary{ID: id})
	}

	// One that disappears before it can be inspected.
	res.Items = append(res.Items, container.Summary{ID: "vanished"})

	return res, nil
}

func (f *fakeEngine) ContainerInspect(
	_ context.Context, id string, _ client.ContainerInspectOptions,
) (client.ContainerInspectResult, error) {
	c := f.find(id)
	if c == nil {
		return client.ContainerInspectResult{}, notFound("container " + id)
	}

	raw, err := json.Marshal(c)
	if err != nil {
		return client.ContainerInspectResult{}, err
	}

	return client.ContainerInspectResult{Container: *c, Raw: raw}, nil
}

func (f *fakeEngine) ContainerCreate(
	_ context.Context, options client.ContainerCreateOptions,
) (client.ContainerCreateResult, error) {
	if f.onCreate != nil {
		f.onCreate()
	}

	if err := f.record("create " + options.Name); err != nil {
		return client.ContainerCreateResult{}, err
	}

	if f.find(options.Name) != nil {
		return client.ContainerCreateResult{}, fmt.Errorf("name in use: %w", cerrdefs.ErrConflict)
	}

	img, ok := f.images[options.Config.Image]
	if !ok {
		return client.ContainerCreateResult{}, notFound("image")
	}

	f.nextID++
	id := fmt.Sprintf("new%d", f.nextID)
	f.containers[id] = &container.InspectResponse{
		ID:         id,
		Name:       "/" + options.Name,
		Image:      img.ID,
		State:      &container.State{Status: container.StateCreated},
		Config:     options.Config,
		HostConfig: options.HostConfig,
	}
	f.created = append(f.created, options)

	return client.ContainerCreateResult{ID: id, Warnings: []string{"a warning"}}, nil
}

func (f *fakeEngine) ContainerStart(
	_ context.Context, id string, _ client.ContainerStartOptions,
) (client.ContainerStartResult, error) {
	c := f.find(id)
	if c == nil {
		return client.ContainerStartResult{}, notFound("container " + id)
	}

	if err := f.record("start " + c.ID); err != nil {
		return client.ContainerStartResult{}, err
	}

	c.State = &container.State{Status: container.StateRunning, Running: true}

	return client.ContainerStartResult{}, nil
}

func (f *fakeEngine) ContainerStop(
	_ context.Context, id string, options client.ContainerStopOptions,
) (client.ContainerStopResult, error) {
	c := f.find(id)
	if c == nil {
		return client.ContainerStopResult{}, notFound("container " + id)
	}

	call := "stop " + c.ID
	if options.Timeout != nil {
		call += fmt.Sprintf(" t=%d", *options.Timeout)
	}

	if err := f.record(call); err != nil {
		return client.ContainerStopResult{}, err
	}

	c.State = &container.State{Status: container.StateExited}

	return client.ContainerStopResult{}, nil
}

func (f *fakeEngine) ContainerRename(
	_ context.Context, id string, options client.ContainerRenameOptions,
) (client.ContainerRenameResult, error) {
	c := f.find(id)
	if c == nil {
		return client.ContainerRenameResult{}, notFound("container " + id)
	}

	if err := f.record("rename " + c.ID + " " + options.NewName); err != nil {
		return client.ContainerRenameResult{}, err
	}

	if f.find(options.NewName) != nil {
		return client.ContainerRenameResult{}, fmt.Errorf("name in use: %w", cerrdefs.ErrConflict)
	}

	c.Name = "/" + options.NewName

	return client.ContainerRenameResult{}, nil
}

func (f *fakeEngine) ContainerRemove(
	_ context.Context, id string, options client.ContainerRemoveOptions,
) (client.ContainerRemoveResult, error) {
	c := f.find(id)
	if c == nil {
		return client.ContainerRemoveResult{}, notFound("container " + id)
	}

	call := "remove " + c.ID
	if options.RemoveVolumes {
		call += " with volumes"
	}

	if err := f.record(call); err != nil {
		return client.ContainerRemoveResult{}, err
	}

	delete(f.containers, c.ID)

	return client.ContainerRemoveResult{}, nil
}

func (f *fakeEngine) ContainerUpdate(
	_ context.Context, id string, options client.ContainerUpdateOptions,
) (client.ContainerUpdateResult, error) {
	return client.ContainerUpdateResult{},
		f.record("update " + id + " restart=" + string(options.RestartPolicy.Name))
}

func (f *fakeEngine) ImageInspect(
	_ context.Context, ref string, _ ...client.ImageInspectOption,
) (client.ImageInspectResult, error) {
	img, ok := f.images[ref]
	if !ok {
		return client.ImageInspectResult{}, notFound("image " + ref)
	}

	return client.ImageInspectResult{InspectResponse: *img}, nil
}

func (f *fakeEngine) ImagePull(
	_ context.Context, ref string, options client.ImagePullOptions,
) (client.ImagePullResponse, error) {
	if err := f.record("pull " + ref); err != nil {
		return nil, err
	}

	f.pulls = append(f.pulls, options)

	return fakePullStream{msgs: f.pullMsgs}, nil
}

func (f *fakeEngine) NetworkConnect(
	_ context.Context, networkID string, options client.NetworkConnectOptions,
) (client.NetworkConnectResult, error) {
	return client.NetworkConnectResult{}, f.record("connect " + networkID + " " + options.Container)
}

func (f *fakeEngine) ClientVersion() string { return f.version }

func (f *fakeEngine) Close() error { return f.record("close") }

// fakePullStream replays pull progress messages.
type fakePullStream struct {
	msgs []jsonstream.Message
}

func (s fakePullStream) Read([]byte) (int, error) { return 0, io.EOF }
func (s fakePullStream) Close() error             { return nil }

func (s fakePullStream) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {
		for _, msg := range s.msgs {
			if !yield(msg, nil) {
				return
			}
		}
	}
}

func (s fakePullStream) Wait(context.Context) error { return nil }

func newTestRuntime(t *testing.T, f *fakeEngine) *Runtime {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	return &Runtime{
		log:      log,
		engine:   f,
		authFile: filepath.Join(t.TempDir(), "absent.json"),
		pulled:   make(map[string]string, 1),
	}
}

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

func TestStartStop(t *testing.T) {
	f := newFakeEngine()
	r := newTestRuntime(t, f)

	require.NoError(t, r.Stop(t.Context(), "geth", 1500*time.Millisecond))
	require.NoError(t, r.Stop(t.Context(), "geth", -time.Second))
	require.NoError(t, r.Start(t.Context(), "geth"))
	require.NoError(t, r.Start(t.Context(), "geth"))
	assert.Equal(t, []string{"stop old t=2", "stop old t=0", "start old", "start old"}, f.calls,
		"the timeout is rounded up to seconds")

	require.Error(t, r.Stop(t.Context(), "absent", time.Second))
	require.Error(t, r.Start(t.Context(), "absent"))

	// A container left under the temporary name by an interrupted recreate
	// is found again.
	f.calls = nil
	f.containers["old"].Name = "/geth" + replacedSuffix

	require.NoError(t, r.Start(t.Context(), "geth"))
	assert.Equal(t, []string{"rename old geth", "start old"}, f.calls)
}

func TestObserve(t *testing.T) {
	f := newFakeEngine()
	f.containers["zzz"] = &container.InspectResponse{
		ID:    "zzz",
		Name:  "/aaa-local",
		Image: "sha256:gone",
		State: &container.State{Status: container.StateCreated, StartedAt: "0001-01-01T00:00:00Z"},
		Config: &container.Config{
			Image: "local-build:dev",
		},
	}

	got, err := newTestRuntime(t, f).Observe(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2, "the vanished container is skipped")

	assert.Equal(t, "aaa-local", got[0].Name, "sorted by name")
	assert.Equal(t, "local-build:dev", got[0].Image)
	assert.Equal(t, "sha256:gone", got[0].ImageID)
	assert.Empty(t, got[0].Digest)
	assert.Equal(t, "created", got[0].State)
	assert.True(t, got[0].StartedAt.IsZero())

	geth := got[1]
	assert.Equal(t, "geth", geth.Name)
	assert.Equal(t, "old", geth.ID)
	assert.Equal(t, fakeRef, geth.Image)
	assert.Equal(t, fakeOldImage, geth.ImageID)
	assert.Equal(t, digestA, geth.Digest)
	assert.Equal(t, "oldrev", geth.Revision)
	assert.Equal(t, "v1", geth.Version)
	assert.Equal(t, "running", geth.State)
	assert.Equal(t, "healthy", geth.Health)
	assert.Equal(t, 2, geth.RestartCount)
	assert.Equal(t, time.Date(2026, 11, 14, 10, 0, 0, 500_000_000, time.UTC), geth.StartedAt)
	assert.False(t, geth.Managed, "left for the caller")

	// A container that merely has such a name, with nothing under the
	// name it would be the leftover of, is reported.
	f.containers["zzz"].Name = "/other" + retiredSuffix

	got, err = newTestRuntime(t, f).Observe(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "other"+retiredSuffix, got[1].Name)
}

func TestLocalDigest(t *testing.T) {
	f := newFakeEngine()
	r := newTestRuntime(t, f)

	got, err := r.LocalDigest(t.Context(), fakeRef)
	require.NoError(t, err)
	assert.Equal(t, digestB, got)

	got, err = r.LocalDigest(t.Context(), "ethpandaops/absent:latest")
	require.NoError(t, err)
	assert.Empty(t, got)

	_, err = r.LocalDigest(t.Context(), "Not A Reference")
	require.Error(t, err)

	// The daemon's descriptor of the image breaks a tie between digests.
	f.images[fakeRef].RepoDigests = []string{"ethpandaops/geth@" + digestC, "ethpandaops/geth@" + digestB}
	f.images[fakeRef].Descriptor = &ocispec.Descriptor{Digest: digestB}

	got, err = r.LocalDigest(t.Context(), fakeRef)
	require.NoError(t, err)
	assert.Equal(t, digestB, got)
}

func TestPull(t *testing.T) {
	t.Run("returns the digest the tag was pulled at", func(t *testing.T) {
		f := newFakeEngine()
		f.images[fakeRef].RepoDigests = []string{"ethpandaops/geth@" + digestC, "ethpandaops/geth@" + digestB}
		f.pullMsgs = []jsonstream.Message{
			{Status: "Pulling from ethpandaops/geth"},
			{Status: "Digest: " + digestB},
			{Status: "Status: Downloaded newer image"},
		}
		r := newTestRuntime(t, f)

		got, err := r.Pull(t.Context(), fakeRef)
		require.NoError(t, err)
		assert.Equal(t, digestB, got)

		local, err := r.LocalDigest(t.Context(), fakeRef)
		require.NoError(t, err)
		assert.Equal(t, digestB, local, "LocalDigest agrees with the last pull")

		require.Len(t, f.pulls, 1)
		assert.Empty(t, f.pulls[0].RegistryAuth, "no credentials file, anonymous pull")
	})

	t.Run("surfaces errors from the progress stream", func(t *testing.T) {
		f := newFakeEngine()
		f.pullMsgs = []jsonstream.Message{
			{Status: "Pulling from ethpandaops/geth"},
			{Error: &jsonstream.Error{Message: "manifest unknown"}},
		}

		_, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef)
		require.ErrorContains(t, err, "manifest unknown")
	})

	t.Run("request error", func(t *testing.T) {
		f := newFakeEngine()
		f.fail["pull "+fakeRef] = errors.New("daemon down")

		_, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef)
		require.ErrorContains(t, err, "daemon down")
	})

	t.Run("image absent after the pull", func(t *testing.T) {
		f := newFakeEngine()
		delete(f.images, fakeRef)

		_, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef)
		require.ErrorContains(t, err, "absent after the pull")
	})

	t.Run("sends credentials from the auth file", func(t *testing.T) {
		f := newFakeEngine()
		r := newTestRuntime(t, f)
		r.authFile = filepath.Join(t.TempDir(), "config.json")

		basic := base64.StdEncoding.EncodeToString([]byte("user:secret"))
		require.NoError(t, os.WriteFile(r.authFile,
			[]byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"`+basic+`"}}}`), 0o600))

		_, err := r.Pull(t.Context(), "registry-1.docker.io/"+fakeRef)
		require.ErrorContains(t, err, "absent after the pull", "the fake has no image under that spelling")

		require.Len(t, f.pulls, 1)

		raw, err := base64.URLEncoding.DecodeString(f.pulls[0].RegistryAuth)
		require.NoError(t, err)

		var sent registry.AuthConfig
		require.NoError(t, json.Unmarshal(raw, &sent))
		assert.Equal(t, "user", sent.Username)
		assert.Equal(t, "secret", sent.Password)
	})

	t.Run("a broken auth file means an anonymous pull", func(t *testing.T) {
		f := newFakeEngine()
		r := newTestRuntime(t, f)
		r.authFile = filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(r.authFile, []byte("{"), 0o600))

		_, err := r.Pull(t.Context(), fakeRef)
		require.NoError(t, err)
		require.Len(t, f.pulls, 1)
		assert.Empty(t, f.pulls[0].RegistryAuth)
	})
}

func TestFixedMAC(t *testing.T) {
	const mac = "02:aa:bb:cc:dd:01"

	old := &container.InspectResponse{
		ID: "old", Name: "/geth", HostConfig: &container.HostConfig{NetworkMode: "net-a"},
	}
	withMAC := []byte(`{"Config":{"MacAddress":"` + mac + `"}}`)
	without := []byte(`{"Config":{}}`)

	legacy := newFakeEngine()
	r := newTestRuntime(t, newFakeEngine())

	assert.Equal(t, mac, r.fixedMAC(t.Context(), old, withMAC), "old API: in the inspect itself")
	assert.Empty(t, r.fixedMAC(t.Context(), old, without))

	// A daemon that no longer reports the field is asked through the
	// legacy client.
	current := newFakeEngine()
	current.version = "1.52"
	r = newTestRuntime(t, current)
	assert.Empty(t, r.fixedMAC(t.Context(), old, without), "no legacy client")

	r.legacy = legacy
	assert.Empty(t, r.fixedMAC(t.Context(), old, without), "the fake reports no Config.MacAddress")

	delete(legacy.containers, "old")
	assert.Empty(t, r.fixedMAC(t.Context(), old, without), "a failing legacy inspect is not fatal")

	old.HostConfig.NetworkMode = "host"
	assert.Empty(t, r.fixedMAC(t.Context(), old, withMAC), "host networking has no MAC to fix")
}

func TestNewAndClose(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	_, err := New(nil, "unix:///var/run/docker.sock")
	require.Error(t, err)

	_, err = New(log, "")
	require.Error(t, err)

	// New does not connect: a socket that does not exist is fine.
	r, err := New(log, "unix:///nonexistent/upgradoor-test.sock", WithAuthFile("/etc/x.json"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/x.json", r.authFile)
	assert.NotNil(t, r.legacy)

	_, err = r.Observe(t.Context())
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "list containers"), err.Error())

	require.NoError(t, r.Close())
}
