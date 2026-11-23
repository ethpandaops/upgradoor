package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fakeRef      = "ethpandaops/geth:master"
	fakeOldImage = "sha256:old"
	fakeNewImage = "sha256:new"
)

// fakeEngine is an in-memory Docker daemon with a call log and injectable failures.
type fakeEngine struct {
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
	listed   []client.ContainerListOptions
	pulls    []client.ImagePullOptions
	pullMsgs []jsonstream.Message
	nextID   int
}

func newFakeEngine() *fakeEngine {
	f := &fakeEngine{
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
	_ context.Context, opts client.ContainerListOptions,
) (client.ContainerListResult, error) {
	f.listed = append(f.listed, opts)

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

func (f *fakeEngine) Close() error { return f.record("close") }

// fakePullStream replays pull progress messages.
type fakePullStream struct {
	msgs []jsonstream.Message
}

func (s fakePullStream) Read([]byte) (int, error) { return 0, io.EOF }

func (s fakePullStream) Close() error { return nil }

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

func newTestRuntime(t *testing.T, f *fakeEngine) *dockerRuntime {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	return &dockerRuntime{
		log:    log,
		engine: f,
		pulled: make(map[string]string, 1),
	}
}

func TestContainers(t *testing.T) {
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

	got, err := newTestRuntime(t, f).Containers(t.Context())
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

	// A retired-looking name with no container under the base name is reported.
	f.containers["zzz"].Name = "/other" + retiredSuffix

	got, err = newTestRuntime(t, f).Containers(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "other"+retiredSuffix, got[1].Name)
}

func TestContainersOnlyWithTheConfiguredLabels(t *testing.T) {
	f := newFakeEngine()
	r := newTestRuntime(t, f)
	r.only = labelFilter(map[string]string{"e2e.node": "1"})

	_, err := r.Containers(context.Background())
	require.NoError(t, err)
	require.Len(t, f.listed, 1)
	assert.True(t, f.listed[0].All)
	assert.Equal(t, client.Filters{"label": {"e2e.node=1": true}}, f.listed[0].Filters)

	assert.Nil(t, labelFilter(nil), "no labels: every container")
}

func TestConfigValidate(t *testing.T) {
	require.NoError(t, (&Config{Labels: map[string]string{"e2e.node": ""}}).Validate())
	require.Error(t, (&Config{Labels: map[string]string{" ": "1"}}).Validate())
}

func TestContainersLabels(t *testing.T) {
	f := newFakeEngine()

	for id, labels := range map[string]map[string]string{
		"labelled": {
			protocol.LabelKind: "Whatever", protocol.LabelTeam: "geth",
			labelOverrideImage: "x", labelOverrideCommand: "not a list",
		},
		"team-only": {protocol.LabelTeam: "geth"},
	} {
		f.containers[id] = &container.InspectResponse{
			ID: id, Name: "/" + id, Image: fakeNewImage,
			State:  &container.State{Status: container.StateRunning, Running: true},
			Config: &container.Config{Image: fakeRef, Labels: labels},
		}
	}

	got, err := newTestRuntime(t, f).Containers(t.Context())
	require.NoError(t, err)

	byName := make(map[string]protocol.Container, len(got))
	for _, c := range got {
		byName[c.Name] = c
	}

	labelled := byName["labelled"]
	assert.Equal(t, protocol.ContainerKind("Whatever"), labelled.Kind, "raw: plan.Classify checks it")
	assert.Equal(t, "geth", labelled.Team)
	assert.Equal(t, &protocol.OverrideBase{Image: "x"}, labelled.Base,
		"a command label that does not parse is no record")

	teamOnly := byName["team-only"]
	assert.Empty(t, teamOnly.Kind)
	assert.Equal(t, "geth", teamOnly.Team, "raw: without a kind plan.Classify drops it")
	assert.Nil(t, teamOnly.Base)
}

func TestLocalImage(t *testing.T) {
	f := newFakeEngine()
	r := newTestRuntime(t, f)

	got, revision, err := r.LocalImage(t.Context(), fakeRef)
	require.NoError(t, err)
	assert.Equal(t, digestB, got)
	assert.Equal(t, "newrev", revision, "the image's revision label")

	got, revision, err = r.LocalImage(t.Context(), "ethpandaops/absent:latest")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Empty(t, revision)

	// An image without labels has no revision.
	f.images[fakeRef].Config = nil

	got, revision, err = r.LocalImage(t.Context(), fakeRef)
	require.NoError(t, err)
	assert.Equal(t, digestB, got)
	assert.Empty(t, revision)

	_, _, err = r.LocalImage(t.Context(), "Not A Reference")
	require.Error(t, err)

	// The daemon's descriptor of the image breaks a tie between digests.
	f.images[fakeRef].RepoDigests = []string{"ethpandaops/geth@" + digestC, "ethpandaops/geth@" + digestB}
	f.images[fakeRef].Descriptor = &ocispec.Descriptor{Digest: digestB}

	got, _, err = r.LocalImage(t.Context(), fakeRef)
	require.NoError(t, err)
	assert.Equal(t, digestB, got)

	// A nameless image (containerd store, tag moved on) has only its descriptor.
	f.images[fakeRef].RepoDigests = nil

	got, _, err = r.LocalImage(t.Context(), fakeRef)
	require.NoError(t, err)
	assert.Equal(t, digestB, got)

	// A named image without registry digest was never pulled.
	f.images[fakeRef].RepoTags = []string{fakeRef}

	got, _, err = r.LocalImage(t.Context(), fakeRef)
	require.NoError(t, err)
	assert.Empty(t, got)
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

	// A container left under the temporary name is found again.
	f.calls = nil
	f.containers["old"].Name = "/geth" + replacedSuffix

	require.NoError(t, r.Start(t.Context(), "geth"))
	assert.Equal(t, []string{"rename old geth", "start old"}, f.calls)
}
