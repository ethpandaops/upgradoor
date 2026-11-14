package fakehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestLoadScenarioRejectsBadInput(t *testing.T) {
	_, err := LoadScenario(filepath.Join(t.TempDir(), "missing.yaml"))
	require.Error(t, err)

	tests := map[string]string{
		"not yaml":          "containers: [",
		"nameless":          "containers: [{image: x}]",
		"imageless":         "containers: [{name: x}]",
		"duplicate":         "containers: [{name: x, image: a}, {name: x, image: b}]",
		"registry no digst": "registry: {x: {revision: r}}",
	}

	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "scenario.yaml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			_, loadErr := LoadScenario(path)
			require.Error(t, loadErr)
		})
	}

	_, err = New(nil)
	require.Error(t, err)
}

func TestRuntime(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)

	containers, err := h.Observe(ctx)
	require.NoError(t, err)
	require.Len(t, containers, 3)
	assert.Equal(t, "geth", containers[0].Name)
	assert.Equal(t, "ethpandaops/geth:master", containers[0].Image)
	assert.Equal(t, "sha256:aaa", containers[0].Digest)
	assert.Equal(t, StateRunning, containers[0].State)
	assert.Equal(t, StateExited, containers[2].State)
	assert.False(t, containers[0].Managed, "the caller fills what the configuration says")

	// The registry is ahead of the local image for geth, level for the rest.
	digest, revision, err := h.Resolve(ctx, "ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, "sha256:ddd", digest)
	assert.Equal(t, "r2", revision)

	digest, _, err = h.Resolve(ctx, "docker.io/sigp/lighthouse")
	require.NoError(t, err)
	assert.Equal(t, "sha256:bbb", digest, "an unlisted tag points at what the container runs")

	_, _, err = h.Resolve(ctx, "nobody/nothing")
	require.Error(t, err)

	local, err := h.LocalDigest(ctx, "docker.io/ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, "sha256:aaa", local)

	local, err = h.LocalDigest(ctx, "nobody/nothing")
	require.NoError(t, err)
	assert.Empty(t, local, "an absent image has no digest")

	// Recreate checks the digest before it touches anything.
	err = h.Recreate(ctx, "geth", "sha256:ddd", time.Second, true)
	require.ErrorIs(t, err, agent.ErrDigestMismatch)
	assert.Empty(t, h.Calls())
	assert.Equal(t, "sha256:aaa", h.Container("geth").Digest)

	// Pulling moves the local tag only.
	pulled, err := h.Pull(ctx, "ethpandaops/geth:master")
	require.NoError(t, err)
	assert.Equal(t, "sha256:ddd", pulled)
	assert.Equal(t, "sha256:aaa", h.Container("geth").Digest)

	_, err = h.Pull(ctx, "nobody/nothing")
	require.Error(t, err)

	before := h.Container("geth")
	require.NoError(t, h.Crash("geth"))
	assert.Equal(t, 1, h.Container("geth").RestartCount)

	require.NoError(t, h.Recreate(ctx, "geth", "sha256:ddd", time.Second, false))

	after := h.Container("geth")
	assert.Equal(t, "sha256:ddd", after.Digest)
	assert.Equal(t, "r2", after.Revision)
	assert.NotEqual(t, before.ID, after.ID)
	assert.Equal(t, StateCreated, after.State, "start=false leaves it stopped")
	assert.Zero(t, after.RestartCount)

	require.NoError(t, h.Start(ctx, "geth"))
	require.NoError(t, h.Start(ctx, "geth"), "starting a running container is fine")
	assert.Equal(t, StateRunning, h.Container("geth").State)

	require.NoError(t, h.Stop(ctx, "geth", time.Second))
	require.NoError(t, h.Stop(ctx, "geth", time.Second), "stopping a stopped container is fine")
	assert.Equal(t, StateExited, h.Container("geth").State)

	require.Error(t, h.Start(ctx, "nimbus"))
	require.Error(t, h.Stop(ctx, "nimbus", time.Second))
	require.Error(t, h.Recreate(ctx, "nimbus", "sha256:x", time.Second, true))
	require.Error(t, h.Crash("nimbus"))
	require.Error(t, h.SetContainerImage("nimbus", "x", "y"))
	assert.Nil(t, h.Container("nimbus"))

	assert.Equal(t, []string{
		"pull ethpandaops/geth:master", "pull nobody/nothing",
		"recreate geth sha256:ddd start=false",
		"start geth", "start geth", "stop geth", "stop geth",
		"start nimbus", "stop nimbus",
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
	assert.Equal(t, StateExited, h.Container("geth").State)
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
	assert.Equal(t, StateExited, h.Container("geth").State)
}

func TestSystem(t *testing.T) {
	ctx := context.Background()
	h := loadHost(t)

	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	h.SetClock(func() time.Time { return now })

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

	hash, err := inv.ComputeHash()
	require.NoError(t, err)
	assert.Equal(t, hash, inv.Hash)
	assert.Equal(t, st.InventoryHash, inv.Hash)
	assert.Equal(t, "docker-ce", inv.Packages[0].Name, "sorted by name")
	assert.True(t, inv.Upgradable[0].Blocked)
	assert.False(t, inv.Upgradable[1].Blocked)
	assert.Equal(t, []string{"6.12.41-amd64", "6.12.48-amd64"}, inv.Kernels.Installed)

	// Only the blocked set can be upgraded.
	require.Error(t, h.UpgradePackages(ctx, []string{"docker-ce", "openssl"}))
	require.NoError(t, h.UpgradePackages(ctx, []string{"docker-ce"}))

	st, err = h.Status(ctx)
	require.NoError(t, err)
	assert.Empty(t, st.Blocked)
	assert.Equal(t, protocol.Pending{Upgradable: 1, Security: 1}, st.Pending)
	assert.NotEqual(t, inv.Hash, st.InventoryHash, "the inventory changed")

	inv, err = h.Inventory(ctx)
	require.NoError(t, err)
	assert.Equal(t, protocol.Package{Name: "docker-ce", Version: "28.1", Arch: "amd64"}, inv.Packages[0])

	// A reboot: new boot id, newest kernel, flag gone, running containers
	// restarted, stopped ones still stopped.
	now = now.Add(time.Hour)

	require.NoError(t, h.Reboot(ctx))

	st, err = h.Status(ctx)
	require.NoError(t, err)
	assert.Equal(t, "test-boot-1", st.BootID)
	assert.Equal(t, "6.12.48-amd64", st.OS.RunningKernel)
	assert.False(t, st.OS.RebootRequired)
	assert.Zero(t, st.Uptime)
	assert.Equal(t, now, h.Container("geth").StartedAt)
	assert.Equal(t, StateRunning, h.Container("geth").State)
	assert.Equal(t, StateExited, h.Container("stopped").State)

	// An inert reboot is accepted and does nothing.
	h.SetInertReboot(true)
	require.NoError(t, h.Reboot(ctx))
	assert.Equal(t, "test-boot-1", h.BootID())

	h.SetInertReboot(false)
	require.NoError(t, h.Reboot(ctx))
	assert.Equal(t, "test-boot-2", h.BootID())

	h.SetRebootRequired(true)
	h.SetNewestKernel("6.13.0-amd64")
	h.SetUpgradable([]protocol.Upgradable{{Name: "docker-ce", Current: "28.1", Candidate: "28.2"}})

	st, err = h.Status(ctx)
	require.NoError(t, err)
	assert.True(t, st.OS.RebootRequired)
	assert.Equal(t, "6.13.0-amd64", st.OS.NewestKernel)
	assert.Equal(t, 1, st.Pending.Blocked)
}

func post(t *testing.T, url string, body any) (int, State) {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(raw))
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	var st State

	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	}

	return resp.StatusCode, st
}

func TestControlEndpoint(t *testing.T) {
	h := loadHost(t)

	srv := httptest.NewServer(h.ControlHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/state")
	require.NoError(t, err)

	var st State

	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, "test-boot", st.BootID)
	assert.Len(t, st.Containers, 3)
	assert.Equal(t, "sha256:ddd", st.Registry["docker.io/ethpandaops/geth:master"].Digest)
	assert.Equal(t, "sha256:aaa", st.Local["docker.io/ethpandaops/geth:master"])

	// Move a tag while the agent would be running.
	code, st := post(t, srv.URL+"/registry", map[string]string{
		"ref": "sigp/lighthouse", "digest": "sha256:eee", "revision": "r9",
	})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, Tag{Digest: "sha256:eee", Revision: "r9"}, st.Registry["docker.io/sigp/lighthouse:latest"])

	digest, _, err := h.Resolve(context.Background(), "sigp/lighthouse:latest")
	require.NoError(t, err)
	assert.Equal(t, "sha256:eee", digest)

	code, st = post(t, srv.URL+"/reboot-required", map[string]bool{"required": false})
	require.Equal(t, http.StatusOK, code)
	assert.False(t, st.RebootRequired)

	code, st = post(t, srv.URL+"/kernel", map[string]string{"newest": "6.13.0"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "6.13.0", st.NewestKernel)

	code, st = post(t, srv.URL+"/upgradable", []protocol.Upgradable{})
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, st.Upgradable)

	code, st = post(t, srv.URL+"/crash", map[string]string{"container": "geth"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, 1, st.Containers[0].RestartCount)

	// Bad requests change nothing.
	for path, body := range map[string]any{
		"/registry":        map[string]string{"ref": "x"},
		"/kernel":          map[string]string{},
		"/reboot-required": map[string]string{"unknown": "field"},
		"/upgradable":      "not a list",
	} {
		code, _ = post(t, srv.URL+path, body)
		assert.Equal(t, http.StatusBadRequest, code, path)
	}

	code, _ = post(t, srv.URL+"/crash", map[string]string{"container": "nimbus"})
	assert.Equal(t, http.StatusNotFound, code)

	code, _ = post(t, srv.URL+"/state", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, code)
}

func TestServeControl(t *testing.T) {
	h := loadHost(t)

	// The endpoint has no authentication: loopback only.
	for _, addr := range []string{"0.0.0.0:7777", "192.0.2.1:7777", "example.org:80", ":7777", "nonsense"} {
		require.Error(t, h.ServeControl(t.Context(), addr), addr)
	}

	// Find a free loopback port.
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)

	go func() { served <- h.ServeControl(ctx, addr) }()

	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/state")
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 5*time.Millisecond)

	cancel()

	select {
	case err := <-served:
		require.NoError(t, err, "ending the context is a clean stop")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the control endpoint did not stop")
	}
}
