package system

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	testBootID  = "3f1c2a9e-5b7d-4e21-9c0a-8d6f4b2e1a70"
	testRunning = "6.12.94+deb13-cloud-amd64"

	keyDpkgQuery = "dpkg-query -W -f " + dpkgQueryFormat
	keySimulate  = "apt-get -s -o Debug::NoLocking=true dist-upgrade"
	keyArch      = "dpkg --print-architecture"
	keyUpdate    = "apt-get update -o DPkg::Lock::Timeout=300"
	keyInstall   = "apt-get install --only-upgrade -y --no-remove -o DPkg::Lock::Timeout=300 " +
		"-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold "
)

// fakeRunner answers commands from a table and records every call.
type fakeRunner struct {
	mu        sync.Mutex
	responses map[string]fakeResponse
	calls     []string
	// hook, when set, runs before a command is answered.
	hook func(ctx context.Context, key string)
}

type fakeResponse struct {
	out []byte
	err error
}

var _ CommandRunner = (*fakeRunner)(nil)

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")

	f.mu.Lock()
	f.calls = append(f.calls, key)
	resp, ok := f.responses[key]
	hook := f.hook
	f.mu.Unlock()

	if hook != nil {
		hook(ctx, key)
	}

	if err := ctx.Err(); err != nil {
		return nil, &CommandError{Name: name, Args: args, ExitCode: -1, Err: err}
	}

	if !ok {
		return nil, &CommandError{
			Name: name, Args: args, ExitCode: 127, Err: errors.New("no canned answer"),
		}
	}

	return resp.out, resp.err
}

func (f *fakeRunner) set(key string, out []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.responses[key] = fakeResponse{out: out, err: err}
}

func (f *fakeRunner) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakeRunner) count(key string) int {
	n := 0

	for _, call := range f.recorded() {
		if call == key {
			n++
		}
	}

	return n
}

func (f *fakeRunner) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = nil
}

// captureRunner is a fakeRunner loaded with the debian:13 captures.
func captureRunner(t *testing.T) *fakeRunner {
	t.Helper()

	return &fakeRunner{responses: map[string]fakeResponse{
		keyDpkgQuery: {out: capture(t, "dpkg-query.txt")},
		keySimulate:  {out: capture(t, "apt-get-simulate.txt")},
		keyArch:      {out: capture(t, "dpkg-print-architecture.txt")},
	}}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()

	path := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// captureRoot builds a fake filesystem root from the debian:13 captures.
func captureRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	writeFile(t, root, pathBootID, testBootID+"\n")
	writeFile(t, root, pathUptime, "93784.52 371822.11\n")
	writeFile(t, root, pathKernelRelease, testRunning+"\n")
	writeFile(t, root, pathOSRelease, string(capture(t, "os-release")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "var", "run"), 0o755))

	for name := range strings.FieldsSeq(string(capture(t, "boot.txt"))) {
		writeFile(t, root, filepath.Join(pathBoot, name), "")
	}

	return root
}

func testLogger() logrus.FieldLogger {
	log := logrus.New()
	log.SetOutput(io.Discard)

	return log
}

var dockerBlocked = []string{"docker-ce", "docker-ce-cli", "containerd.io"}

func newCaptureSystem(t *testing.T, blocked []string) (*System, *fakeRunner, string) {
	t.Helper()

	runner, root := captureRunner(t), captureRoot(t)

	return New(testLogger(), Options{Blocked: blocked, Root: root, Runner: runner}), runner, root
}

func TestStatus(t *testing.T) {
	sys, runner, root := newCaptureSystem(t, dockerBlocked)

	status, err := sys.Status(t.Context())
	require.NoError(t, err)

	assert.Equal(t, testBootID, status.BootID)
	assert.Equal(t, 93784520*time.Millisecond, status.Uptime)
	assert.Equal(t, protocol.OSSummary{
		ID:            "debian",
		VersionID:     "13",
		PrettyName:    "Debian GNU/Linux 13 (trixie)",
		Arch:          "amd64",
		RunningKernel: testRunning,
		NewestKernel:  "6.12.107+deb13-cloud-amd64",
	}, status.OS)
	assert.Equal(t, protocol.Pending{Upgradable: 7, Security: 5, Blocked: 1}, status.Pending)
	assert.Equal(t, []protocol.PackageChange{{
		Name: "docker-ce-cli",
		From: "5:28.1.0-1~debian.13~trixie",
		To:   "5:29.9.3-1~debian.13~trixie",
	}}, status.Blocked)

	inv, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.Equal(t, inv.Hash, status.InventoryHash)

	// containerd.io is blocked and installed, but on hold: a full upgrade
	// leaves it where it is, so it is no need. And nothing ever runs
	// apt-get update for a status.
	assert.Equal(t, []string{keyArch, keyDpkgQuery, keySimulate}, runner.recorded())

	// The reboot flag is read on every call, not cached.
	writeFile(t, root, pathRebootRequired, "*** System restart required ***\n")
	writeFile(t, root, pathRebootPackages, "linux-image-6.12.107+deb13-cloud-amd64\nlibc6\n\nlibc6\n")

	status, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.OS.RebootRequired)
	assert.Equal(t, []string{"linux-image-6.12.107+deb13-cloud-amd64", "libc6"},
		status.OS.RebootPackages)

	// The flag without the package list.
	require.NoError(t, os.Remove(filepath.Join(root, pathRebootPackages)))

	status, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.True(t, status.OS.RebootRequired)
	assert.Empty(t, status.OS.RebootPackages)
}

func TestInventory(t *testing.T) {
	sys, _, _ := newCaptureSystem(t, dockerBlocked)

	inv, err := sys.Inventory(t.Context())
	require.NoError(t, err)

	assert.Equal(t, protocol.OSInfo{
		ID: "debian", VersionID: "13", Codename: "trixie",
		PrettyName: "Debian GNU/Linux 13 (trixie)", Arch: "amd64",
	}, inv.OS)
	assert.Equal(t, protocol.KernelInfo{
		Running:   testRunning,
		Newest:    "6.12.107+deb13-cloud-amd64",
		Installed: []string{testRunning, "6.12.107+deb13-cloud-amd64"},
	}, inv.Kernels)
	assert.WithinDuration(t, time.Now(), inv.CollectedAt, time.Minute)

	require.Len(t, inv.Packages, 100)
	assert.Equal(t, protocol.Package{Name: "apt", Version: "3.0.4", Arch: "amd64"}, inv.Packages[0])
	assert.Contains(t, inv.Packages,
		protocol.Package{Name: "containerd.io", Version: "1.7.27-1", Arch: "amd64"})

	names := make([]string, 0, len(inv.Upgradable))
	for _, up := range inv.Upgradable {
		names = append(names, up.Name)

		assert.Equal(t, up.Name == "docker-ce-cli", up.Blocked, up.Name)
		assert.Equal(t, strings.HasPrefix(up.Origin, "Debian-Security:"), up.Security, up.Name)
	}

	assert.Equal(t, []string{
		"docker-ce-cli", "libssl3t64", "linux-image-cloud-amd64", "openssl",
		"openssl-provider-legacy", "tzdata", "zlib1g",
	}, names)

	hash, err := inv.ComputeHash()
	require.NoError(t, err)
	assert.Equal(t, hash, inv.Hash)

	// The result is a copy: changing it does not change the cache.
	inv.Packages[0].Name = "tampered"
	inv.Upgradable[0].Blocked = false
	inv.Kernels.Installed[0] = "tampered"

	again, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "apt", again.Packages[0].Name)
	assert.True(t, again.Upgradable[0].Blocked)
	assert.Equal(t, testRunning, again.Kernels.Installed[0])
}

func TestInvalidBlockedNamesAreDropped(t *testing.T) {
	sys, _, _ := newCaptureSystem(t, []string{"-oAPT::Update::Pre-Invoke::=reboot", "bash;id"})

	_, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sys.blocked)
}

// /etc/os-release may be missing on a host that only has the vendor's file.
func TestOSReleaseFallback(t *testing.T) {
	sys, runner, root := newCaptureSystem(t, nil)

	content, err := os.ReadFile(filepath.Join(root, pathOSRelease))
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(root, pathOSRelease)))
	writeFile(t, root, pathOSReleaseLib, string(content))

	status, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "debian", status.OS.ID)

	// The architecture is asked once.
	_, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, runner.count(keyArch))
}

func TestStatusErrors(t *testing.T) {
	for _, missing := range []string{pathBootID, pathUptime, pathOSRelease, pathKernelRelease} {
		t.Run("missing "+missing, func(t *testing.T) {
			sys, _, root := newCaptureSystem(t, nil)
			require.NoError(t, os.Remove(filepath.Join(root, missing)))

			_, err := sys.Status(t.Context())
			require.Error(t, err)
		})
	}

	t.Run("dpkg cannot tell the architecture", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, nil)
		failure := &CommandError{Name: cmdDpkg, ExitCode: 127, Err: errors.New("gone")}
		runner.set(keyArch, nil, failure)

		_, err := sys.Status(t.Context())
		require.ErrorIs(t, err, failure)

		runner.set(keyArch, []byte("\n"), nil)

		_, err = sys.Status(t.Context())
		require.ErrorContains(t, err, "dpkg printed nothing")
	})

	t.Run("dpkg-query fails", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, nil)
		failure := &CommandError{Name: cmdDpkgQuery, ExitCode: 2, Err: errors.New("exit status 2")}
		runner.set(keyDpkgQuery, nil, failure)

		_, err := sys.Status(t.Context())
		require.ErrorIs(t, err, failure)

		_, err = sys.Inventory(t.Context())
		require.ErrorIs(t, err, failure)
	})

	t.Run("dpkg-query lists nothing", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, nil)
		runner.set(keyDpkgQuery, []byte("\n"), nil)

		_, err := sys.Inventory(t.Context())
		require.Error(t, err)
	})

	t.Run("cancelled context", func(t *testing.T) {
		sys, _, _ := newCaptureSystem(t, nil)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := sys.Inventory(ctx)
		require.ErrorIs(t, err, context.Canceled)
	})
}

// When apt cannot simulate the upgrade the host still reports its installed
// packages, with no upgradable ones.
func TestSimulationFailureDegrades(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, dockerBlocked)
	runner.set(keySimulate, nil, &CommandError{
		Name: cmdAptGet, ExitCode: 100, Err: errors.New("exit status 100"),
		Output: "E: Unmet dependencies. Try 'apt --fix-broken install'",
	})

	inv, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.Len(t, inv.Packages, 100)
	assert.Empty(t, inv.Upgradable)

	status, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Zero(t, status.Pending)
	assert.Empty(t, status.Blocked)
}

func TestInventoryHashStability(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, dockerBlocked)

	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	sys.now = func() time.Time { return now }

	first, err := sys.Inventory(t.Context())
	require.NoError(t, err)

	// A second collection later, with dpkg listing the same packages in
	// another order: same hash, newer timestamp.
	lines := strings.Split(strings.TrimSpace(string(capture(t, "dpkg-query.txt"))), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}

	runner.set(keyDpkgQuery, []byte(strings.Join(lines, "\n")+"\n"), nil)

	now = now.Add(time.Hour)

	second, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, runner.count(keyDpkgQuery), "collected again")
	assert.Equal(t, first.Hash, second.Hash)
	assert.True(t, second.CollectedAt.After(first.CollectedAt))
	assert.Equal(t, first.Packages, second.Packages)

	// A different system with the same content has the same hash.
	other, _, _ := newCaptureSystem(t, dockerBlocked)

	otherInv, err := other.Inventory(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first.Hash, otherInv.Hash)

	// Any change of content changes it: the blocked set ...
	unblocked, _, _ := newCaptureSystem(t, nil)

	unblockedInv, err := unblocked.Inventory(t.Context())
	require.NoError(t, err)
	assert.NotEqual(t, first.Hash, unblockedInv.Hash)

	// ... and a package version.
	runner.set(keyDpkgQuery, []byte(strings.Replace(string(capture(t, "dpkg-query.txt")),
		"apt\t3.0.4\t", "apt\t3.0.5\t", 1)), nil)

	now = now.Add(time.Hour)

	third, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.NotEqual(t, first.Hash, third.Hash)

	status, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, third.Hash, status.InventoryHash)
}

func TestInventoryIsCachedForTheInterval(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, nil)

	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	sys.now = func() time.Time { return now }

	for range 5 {
		_, err := sys.Status(t.Context())
		require.NoError(t, err)

		_, err = sys.Inventory(t.Context())
		require.NoError(t, err)
	}

	assert.Equal(t, 1, runner.count(keyDpkgQuery))
	assert.Equal(t, 1, runner.count(keySimulate))
	assert.Equal(t, 1, runner.count(keyArch))

	now = now.Add(inventoryInterval - time.Second)

	_, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, runner.count(keyDpkgQuery))

	now = now.Add(time.Second)

	_, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, runner.count(keyDpkgQuery))
}

func TestStaleInventoryIsServedWhenRefreshFails(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, dockerBlocked)

	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	sys.now = func() time.Time { return now }

	first, err := sys.Inventory(t.Context())
	require.NoError(t, err)

	runner.set(keyDpkgQuery, nil, &CommandError{
		Name: cmdDpkgQuery, ExitCode: 2, Err: errors.New("exit status 2"),
	})

	now = now.Add(inventoryInterval)

	stale, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.Equal(t, first.Hash, stale.Hash)
	assert.Equal(t, 2, runner.count(keyDpkgQuery))

	// No retry storm: the next attempt waits for the retry interval.
	_, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, runner.count(keyDpkgQuery))

	runner.set(keyDpkgQuery, capture(t, "dpkg-query.txt"), nil)

	now = now.Add(inventoryRetryInterval)

	_, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 3, runner.count(keyDpkgQuery))
}

// While a refresh is running, other callers get the previous inventory
// instead of queueing behind apt.
func TestConcurrentCallsDuringRefresh(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, nil)

	var clock sync.Mutex

	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	sys.now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()

		return now
	}

	_, err := sys.Inventory(t.Context())
	require.NoError(t, err)

	clock.Lock()
	now = now.Add(inventoryInterval)
	clock.Unlock()

	entered, release := make(chan struct{}), make(chan struct{})

	var once sync.Once

	runner.mu.Lock()
	runner.hook = func(_ context.Context, key string) {
		if key == keyDpkgQuery {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
	}
	runner.mu.Unlock()

	var wg sync.WaitGroup

	wg.Go(func() {
		_, refreshErr := sys.Inventory(t.Context())
		assert.NoError(t, refreshErr)
	})

	<-entered

	// Status calls return while the refresh is still blocked.
	served := make(chan struct{})

	go func() {
		defer close(served)

		for range 8 {
			_, statusErr := sys.Status(t.Context())
			assert.NoError(t, statusErr)
		}
	}()

	select {
	case <-served:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("Status queued behind the running refresh")
	}

	close(release)
	wg.Wait()

	assert.Equal(t, 2, runner.count(keyDpkgQuery))
}

func TestReboot(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, nil)
		runner.set("systemctl reboot", nil, nil)

		require.NoError(t, sys.Reboot(t.Context()))
		assert.Equal(t, []string{"systemctl reboot"}, runner.recorded())
	})

	t.Run("systemctl terminated by the shutdown it started", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, nil)
		runner.set("systemctl reboot", nil, &CommandError{
			Name: cmdSystemctl, Args: []string{"reboot"}, ExitCode: -1,
			Signal: syscall.SIGTERM, Err: errors.New("signal: terminated"),
		})

		require.NoError(t, sys.Reboot(t.Context()))
	})

	t.Run("refused", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, nil)
		runner.set("systemctl reboot", nil, &CommandError{
			Name: cmdSystemctl, Args: []string{"reboot"}, ExitCode: 1,
			Err: errors.New("exit status 1"), Output: "Failed to connect to bus: No such file",
		})

		err := sys.Reboot(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Failed to connect to bus")
	})

	t.Run("killed for another reason", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, nil)
		runner.set("systemctl reboot", nil, &CommandError{
			Name: cmdSystemctl, Args: []string{"reboot"}, ExitCode: -1,
			Signal: syscall.SIGKILL, Err: errors.New("signal: killed"),
		})

		require.Error(t, sys.Reboot(t.Context()))
	})
}
