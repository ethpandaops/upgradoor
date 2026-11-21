package fakehost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const scenarioYAML = `
bootId: test-boot
os: {id: debian, versionId: "13", prettyName: Debian GNU/Linux 13, arch: amd64}
runningKernel: 6.12.41-amd64
newestKernel: 6.12.48-amd64
rebootRequired: true
containers:
  - {name: geth, image: "ethpandaops/geth:master", digest: "sha256:aaa", revision: r1}
  - {name: lighthouse, image: "sigp/lighthouse:latest", digest: "sha256:bbb"}
  - {name: stopped, image: "busybox", digest: "sha256:ccc", state: exited}
registry:
  "docker.io/ethpandaops/geth:master": {digest: "sha256:ddd", revision: r2}
packages:
  blocked: [docker-ce]
  installed:
    - {name: openssl, version: "3.5.1", arch: amd64}
    - {name: docker-ce, version: "28.0", arch: amd64}
  upgradable:
    - {name: docker-ce, arch: amd64, current: "28.0", candidate: "28.1", origin: "Docker:trixie"}
    - {name: openssl, arch: amd64, current: "3.5.1", candidate: "3.5.2", security: true}
control: 127.0.0.1:0
`

func loadHost(t *testing.T) *Host {
	t.Helper()

	path := filepath.Join(t.TempDir(), "scenario.yaml")
	require.NoError(t, os.WriteFile(path, []byte(scenarioYAML), 0o600))

	sc, err := LoadScenario(path)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:0", sc.Control)

	h, err := New(sc)
	require.NoError(t, err)

	return h
}

func TestRuntime(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)

	containers, err := h.Observe(ctx)
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

	local, revision, err := h.LocalImage(ctx, "docker.io/ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, "sha256:aaa", local)
	assert.Equal(t, "r1", revision)

	local, _, err = h.LocalImage(ctx, "nobody/nothing")
	require.NoError(t, err)
	assert.Empty(t, local, "an absent image has no digest")

	// Recreate checks the digest before it touches anything.
	err = h.Recreate(ctx, "geth", "sha256:ddd", time.Second, true)
	require.ErrorIs(t, err, agent.ErrDigestMismatch)
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

	require.NoError(t, h.Recreate(ctx, "geth", "sha256:ddd", time.Second, false))

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

	assert.Equal(t, []string{
		"pull ethpandaops/geth:master", "pull nobody/nothing",
		"recreate geth sha256:ddd start=false",
		"start geth", "start geth", "stop geth", "stop geth",
		"start nimbus",
	}, h.Calls())
}

func TestInjectedFailures(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)
	boom := errors.New("boom")

	var hooked []string

	h.OnCall(func(call string) { hooked = append(hooked, call) })

	h.Fail("observe", boom)
	_, err := h.Observe(ctx)
	require.ErrorIs(t, err, boom)

	h.Fail("observe", nil)
	_, err = h.Observe(ctx)
	require.NoError(t, err)

	// A recreate that breaks leaves the container stopped.
	h.Fail("recreate:geth", boom)
	require.ErrorIs(t, h.Recreate(ctx, "geth", "sha256:aaa", time.Second, true), boom)
	assert.Equal(t, protocol.StateExited, h.Container("geth").State)
	assert.Equal(t, "sha256:aaa", h.Container("geth").Digest)

	h.Fail("status", boom)
	_, err = h.Status(ctx)
	require.ErrorIs(t, err, boom)

	h.Fail("reboot", boom)
	require.ErrorIs(t, h.Reboot(ctx), boom)
	assert.Equal(t, "test-boot", h.BootID())

	assert.Equal(t, []string{"recreate geth sha256:aaa start=true", "reboot"}, hooked)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	require.ErrorIs(t, h.Start(cancelled, "geth"), context.Canceled)
	assert.Equal(t, protocol.StateExited, h.Container("geth").State)
}

func TestSystem(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)

	st, err := h.Status(ctx)
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
	require.NoError(t, h.UpgradePackages(ctx, []string{"docker-ce"}))

	st, err = h.Status(ctx)
	require.NoError(t, err)
	assert.Empty(t, st.Blocked)
	assert.NotEqual(t, inv.Hash, st.InventoryHash, "the inventory changed")

	// A reboot: new boot id, newest kernel, flag gone, running containers
	// restarted, stopped ones still stopped.
	started := h.Container("geth").StartedAt

	require.NoError(t, h.Reboot(ctx))

	st, err = h.Status(ctx)
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
