package agenttest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const scenarioYAML = `
bootId: test-boot
os: {id: debian, versionId: "13", prettyName: Debian GNU/Linux 13, arch: amd64}
runningKernel: 6.12.41-amd64
newestKernel: 6.12.48-amd64
rebootRequired: true
containers:
  - {name: geth, image: "ethpandaops/geth:master", digest: "sha256:aaa", revision: r1,
     command: [--http]}
  - {name: lighthouse, image: "sigp/lighthouse:latest", digest: "sha256:bbb"}
  - {name: stopped, image: "busybox", digest: "sha256:ccc", state: exited}
registry:
  "docker.io/ethpandaops/geth:master": {digest: "sha256:ddd", revision: r2}
  "ethpandaops/geth:fix": {digest: "sha256:fff", revision: rf}
packages:
  blocked: [docker-ce]
  installed:
    - {name: openssl, version: "3.5.1", arch: amd64}
    - {name: docker-ce, version: "28.0", arch: amd64}
  upgradable:
    - {name: docker-ce, arch: amd64, current: "28.0", candidate: "28.1", origin: "Docker:trixie"}
    - {name: openssl, arch: amd64, current: "3.5.1", candidate: "3.5.2", security: true}
`

func loadHost(t *testing.T) *Host {
	t.Helper()

	path := filepath.Join(t.TempDir(), "scenario.yaml")
	require.NoError(t, os.WriteFile(path, []byte(scenarioYAML), 0o600))

	sc, err := LoadScenario(path)
	require.NoError(t, err)

	h, err := NewHost(sc, nil)
	require.NoError(t, err)

	return h
}

func TestRuntime(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)

	containers, err := h.Containers(ctx)
	require.NoError(t, err)
	require.Len(t, containers, 3)
	assert.Equal(t, "sha256:aaa", containers[0].Digest)
	assert.Equal(t, protocol.StateExited, containers[2].State)

	// The registry is ahead of the local image for geth, level for the rest.
	digest, err := h.Resolve(ctx, "ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, "sha256:ddd", digest)

	digest, err = h.Resolve(ctx, "docker.io/sigp/lighthouse")
	require.NoError(t, err)
	assert.Equal(t, "sha256:bbb", digest, "an unlisted tag points at what the container runs")

	_, err = h.Resolve(ctx, "nobody/nothing")
	require.Error(t, err)
	assert.Equal(t, 3, h.Resolves())

	local, revision, err := h.LocalImage(ctx, "docker.io/ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, "sha256:aaa", local)
	assert.Equal(t, "r1", revision)

	local, _, err = h.LocalImage(ctx, "nobody/nothing")
	require.NoError(t, err)
	assert.Empty(t, local, "an absent image has no digest")

	// Recreate checks the digest before it touches anything.
	err = h.Recreate(ctx, "geth", "sha256:ddd", time.Second, true, nil)
	require.ErrorIs(t, err, host.ErrDigestMismatch)
	require.ErrorIs(t, h.Recreate(ctx, "lighthouse", "", time.Second, true, nil),
		host.ErrDigestMismatch, "an empty digest never passes")
	assert.Empty(t, h.Calls())
	assert.Equal(t, "sha256:aaa", h.Container("geth").Digest)

	// Pulling moves the local tag only.
	pulled, revision, err := h.Pull(ctx, "ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, "sha256:ddd", pulled)
	assert.Equal(t, "r2", revision, "the revision comes with the pulled image")
	assert.Equal(t, "sha256:aaa", h.Container("geth").Digest)

	_, _, err = h.Pull(ctx, "nobody/nothing")
	require.Error(t, err)

	before := h.Container("geth")

	require.NoError(t, h.Recreate(ctx, "geth", "sha256:ddd", time.Second, false, nil))

	after := h.Container("geth")
	assert.Equal(t, "sha256:ddd", after.Digest)
	assert.Equal(t, "r2", after.Revision)
	assert.NotEqual(t, before.ID, after.ID)
	assert.Equal(t, stateCreated, after.State, "start=false leaves it stopped")

	require.NoError(t, h.Start(ctx, "geth"))
	require.NoError(t, h.Start(ctx, "geth"), "starting a running container is fine")
	require.NoError(t, h.Stop(ctx, "geth", time.Second))
	require.NoError(t, h.Stop(ctx, "geth", time.Second), "stopping a stopped container is fine")
	assert.Equal(t, protocol.StateExited, h.Container("geth").State)

	require.Error(t, h.Start(ctx, "nimbus"))
	assert.Nil(t, h.Container("nimbus"))

	// A recreate onto an override writes image, command, override and group labels.
	_, _, err = h.Pull(ctx, "ethpandaops/geth:fix")
	require.NoError(t, err)
	require.NoError(t, h.Recreate(ctx, "geth", "sha256:fff", time.Second, true, &host.Change{
		Image: "ethpandaops/geth:fix", Command: []string{"--http", "--v"}, SetCommand: true,
		Base: &protocol.OverrideBase{
			Image: "ethpandaops/geth:master", Command: []string{"--http"}, HasCommand: true,
		},
		SetBase: true,
		Group:   "execution", Kind: protocol.KindEL,
	}))

	geth := h.Container("geth")
	assert.Equal(t, "ethpandaops/geth:fix", geth.Image)
	assert.Equal(t, []string{"--http", "--v"}, geth.Command)
	assert.Equal(t, &protocol.OverrideBase{
		Image: "ethpandaops/geth:master", Command: []string{"--http"}, HasCommand: true,
	}, geth.Base)
	assert.Equal(t, "execution", geth.Group)
	assert.Equal(t, protocol.KindEL, geth.Kind)
	assert.Equal(t, protocol.StateRunning, geth.State)

	// The restore removes the labels.
	require.NoError(t, h.Recreate(ctx, "geth", "sha256:ddd", time.Second, true, &host.Change{
		Image: "ethpandaops/geth:master", Command: []string{"--http"}, SetCommand: true, SetBase: true,
	}))
	assert.Nil(t, h.Container("geth").Base)

	assert.Equal(t, []string{
		"pull ethpandaops/geth:master", "pull nobody/nothing",
		"recreate geth sha256:ddd start=false",
		"start geth", "start geth", "stop geth", "stop geth",
		"start nimbus",
		"pull ethpandaops/geth:fix",
		"recreate geth sha256:fff start=true image=ethpandaops/geth:fix command=--http --v",
		"recreate geth sha256:ddd start=true image=ethpandaops/geth:master command=--http",
	}, h.Calls())

	fixRecreate := "recreate geth sha256:fff start=true image=ethpandaops/geth:fix command=--http --v"
	assert.Equal(t, []string{
		"stop geth", "start nimbus", fixRecreate,
		"recreate geth sha256:ddd start=true image=ethpandaops/geth:master command=--http",
	}, Disruptive(CallsAfter(h.Calls(), "stop geth")), "the pull is left out")
}

func TestInjectedFailures(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)
	boom := errors.New("boom")

	var hooked []string

	h.OnCall(func(call string) { hooked = append(hooked, call) })

	h.Fail("observe", boom)
	_, err := h.Containers(ctx)
	require.ErrorIs(t, err, boom)

	h.Fail("observe", nil)
	_, err = h.Containers(ctx)
	require.NoError(t, err)

	// A recreate that breaks leaves the container stopped.
	h.Fail("recreate:geth", boom)
	require.ErrorIs(t, h.Recreate(ctx, "geth", "sha256:aaa", time.Second, true, nil), boom)
	assert.Equal(t, protocol.StateExited, h.Container("geth").State)
	assert.Equal(t, "sha256:aaa", h.Container("geth").Digest)

	h.Fail("status", boom)
	_, err = h.System(ctx)
	require.ErrorIs(t, err, boom)

	h.Fail("reboot", boom)
	require.ErrorIs(t, h.Reboot(ctx), boom)
	assert.Equal(t, "test-boot", h.BootID())

	assert.Equal(t, []string{"recreate geth sha256:aaa start=true", "reboot"}, hooked)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	require.ErrorIs(t, h.Start(cancelled, "geth"), context.Canceled)
	assert.Equal(t, protocol.StateExited, h.Container("geth").State)

	// BlockIn parks the call until it is released.
	entered, release := h.BlockIn(t, "stop ")
	done := make(chan error, 1)

	go func() { done <- h.Stop(ctx, "lighthouse", time.Second) }()

	AwaitParked(t, entered, "stop lighthouse")
	assert.Equal(t, protocol.StateRunning, h.Container("lighthouse").State)

	release()
	require.NoError(t, <-done)
	assert.Equal(t, protocol.StateExited, h.Container("lighthouse").State)
}

func TestSystem(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)

	st, err := h.System(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-boot", st.BootID)
	assert.Equal(t, "6.12.41-amd64", st.OS.RunningKernel)
	assert.Equal(t, "6.12.48-amd64", st.OS.NewestKernel)
	assert.True(t, st.OS.RebootRequired)
	assert.Equal(t, protocol.Pending{Upgradable: 2, Security: 1, Blocked: 1}, st.Pending)
	assert.Equal(t, []protocol.PackageChange{{Name: "docker-ce", From: "28.0", To: "28.1"}}, st.Blocked)

	inv, err := h.Inventory(ctx)
	require.NoError(t, err)
	assert.Equal(t, st.InventoryHash, inv.Hash)
	assert.Equal(t, []string{"6.12.41-amd64", "6.12.48-amd64"}, inv.Kernels.Installed)

	// Only the blocked set can be upgraded.
	require.Error(t, h.UpgradePackages(ctx, []string{"docker-ce", "openssl"}))
	assert.Empty(t, h.Calls(), "a refused upgrade runs nothing")
	require.NoError(t, h.UpgradePackages(ctx, []string{"docker-ce"}))
	assert.Equal(t, []string{"upgrade docker-ce"}, h.Calls())

	st, err = h.System(ctx)
	require.NoError(t, err)
	assert.Empty(t, st.Blocked)
	assert.NotEqual(t, inv.Hash, st.InventoryHash, "the inventory changed")

	// A reboot: new boot id, newest kernel, flag gone, stopped containers stay stopped.
	started := h.Container("geth").StartedAt

	require.NoError(t, h.Reboot(ctx))

	st, err = h.System(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-boot-1", st.BootID)
	assert.Equal(t, "6.12.48-amd64", st.OS.RunningKernel)
	assert.False(t, st.OS.RebootRequired)
	assert.True(t, h.Container("geth").StartedAt.After(started))
	assert.Equal(t, protocol.StateRunning, h.Container("geth").State)
	assert.Equal(t, protocol.StateExited, h.Container("stopped").State)

	// An inert reboot is accepted and does nothing.
	h.SetInertReboot(true)
	require.NoError(t, h.Reboot(ctx))
	assert.Equal(t, "test-boot-1", h.BootID())
}

func TestRecreate(t *testing.T) {
	ctx := context.Background()
	overridden := &protocol.OverrideBase{
		Image: GethImage, Command: []string{"--a"}, HasCommand: true,
	}

	tests := []struct {
		name string
		// before is a recreate that runs first, to give the container labels.
		before   *host.Change
		change   *host.Change
		wantCall string
		wantBase *protocol.OverrideBase
		wantKind protocol.ContainerKind
	}{
		{
			name:     "no change",
			wantCall: "recreate geth sha256:geth-1 start=true",
			wantKind: "el",
		},
		{
			name:     "relabel only",
			change:   &host.Change{Group: "renamed", Kind: "weird"},
			wantCall: "recreate geth sha256:geth-1 start=true image=" + GethImage,
			wantKind: "weird",
		},
		{
			name:     "SetBase false keeps the base",
			before:   &host.Change{Base: overridden, SetBase: true},
			change:   &host.Change{Command: []string{"--b"}, SetCommand: true},
			wantCall: "recreate geth sha256:geth-1 start=true image=" + GethImage + " command=--b",
			wantBase: overridden,
			wantKind: "el",
		},
		{
			name:     "an empty base reads back as nil",
			before:   &host.Change{Base: overridden, SetBase: true},
			change:   &host.Change{Base: &protocol.OverrideBase{}, SetBase: true},
			wantCall: "recreate geth sha256:geth-1 start=true image=" + GethImage,
			wantKind: "el",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := BaseScenario()
			sc.Containers[1].Command = []string{"--http"}

			h, err := NewHost(sc, nil)
			require.NoError(t, err)

			require.NoError(t, h.Stop(ctx, "geth", time.Second))
			assert.Equal(t, 143, h.Container("geth").ExitCode, "what docker stop leaves")

			if tt.before != nil {
				require.NoError(t, h.Recreate(ctx, "geth", GethV1, time.Second, true, tt.before))
			}

			require.NoError(t, h.Recreate(ctx, "geth", GethV1, time.Second, true, tt.change))

			calls := h.Calls()
			assert.Equal(t, tt.wantCall, calls[len(calls)-1])

			geth := h.Container("geth")
			assert.Equal(t, tt.wantBase, geth.Base)
			assert.Equal(t, tt.wantKind, geth.Kind, "the kind is kept raw")
			assert.Zero(t, geth.ExitCode, "a new container")
			assert.Equal(t, protocol.StateRunning, geth.State)

			// What the host hands out is a copy.
			geth.Command[0] = "--changed"
			if geth.Base != nil {
				geth.Base.Command[0] = "--changed"
			}

			assert.Equal(t, tt.wantBase, h.Container("geth").Base)
			assert.NotContains(t, h.Container("geth").Command, "--changed")
		})
	}
}
