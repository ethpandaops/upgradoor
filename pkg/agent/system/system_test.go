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

	// containerd.io is on hold: apt-cache policy is not even asked, and
	// nothing ever runs apt-get update.
	for _, call := range runner.recorded() {
		assert.NotContains(t, call, "apt-cache")
		assert.NotContains(t, call, "update")
	}

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

// A blocked package the full upgrade leaves out (kept back) is found through
// apt-cache policy, unless a person put it on hold.
func TestBlockedPackageKeptBack(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, dockerBlocked)

	unheld := strings.Replace(string(capture(t, "dpkg-query.txt")),
		"containerd.io\t1.7.27-1\tamd64\thold ok installed",
		"containerd.io\t1.7.27-1\tamd64\tinstall ok installed", 1)
	runner.set(keyDpkgQuery, []byte(unheld), nil)
	runner.set("apt-cache policy containerd.io", capture(t, "apt-cache-policy.txt"), nil)

	status, err := sys.Status(t.Context())
	require.NoError(t, err)

	assert.Equal(t, protocol.Pending{Upgradable: 8, Security: 5, Blocked: 2}, status.Pending)
	assert.Equal(t, []protocol.PackageChange{
		{Name: "containerd.io", From: "1.7.27-1", To: "2.4.2-4~debian.13~trixie"},
		{
			Name: "docker-ce-cli",
			From: "5:28.1.0-1~debian.13~trixie",
			To:   "5:29.9.3-1~debian.13~trixie",
		},
	}, status.Blocked)

	inv, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.Equal(t, protocol.Upgradable{
		Name: "containerd.io", Arch: "amd64", Current: "1.7.27-1",
		Candidate: "2.4.2-4~debian.13~trixie", Origin: "trixie/stable", Blocked: true,
	}, inv.Upgradable[0])

	// docker-ce is blocked but not installed, docker-ce-cli is already in
	// the simulation: only containerd.io is looked up.
	assert.Equal(t, 1, runner.count("apt-cache policy containerd.io"))
}

// A kept-back blocked package whose candidate is published in a regular
// suite and in the security suite is a security upgrade, whichever archive
// the version table lists first.
func TestBlockedPackageKeptBackFromSecurity(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, []string{"bash"})
	runner.set("apt-cache policy bash", []byte("bash:\n"+
		"  Installed: 5.2.37-2+b10\n"+
		"  Candidate: 5.2.37-2+b11\n"+
		"  Version table:\n"+
		"     5.2.37-2+b11 500\n"+
		"        500 http://deb.debian.org/debian trixie/main amd64 Packages\n"+
		"        500 http://deb.debian.org/debian-security trixie-security/main amd64 Packages\n"+
		" *** 5.2.37-2+b10 100\n"+
		"        100 /var/lib/dpkg/status\n"), nil)

	inv, err := sys.Inventory(t.Context())
	require.NoError(t, err)

	var bash []protocol.Upgradable

	for _, up := range inv.Upgradable {
		if up.Name == "bash" {
			bash = append(bash, up)
		}
	}

	require.Len(t, bash, 1)
	assert.Equal(t, "5.2.37-2+b11", bash[0].Candidate)
	assert.Equal(t, "trixie/main,trixie-security/main", bash[0].Origin)
	assert.True(t, bash[0].Security)
	assert.True(t, bash[0].Blocked)
}

func TestBlockedPolicyCandidateNotNewer(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, []string{"bash"})

	runner.set("apt-cache policy bash", []byte("bash:\n"+
		"  Installed: 5.2.37-2+b10\n"+
		"  Candidate: 5.2.37-2+b10\n"+
		"  Version table:\n"+
		" *** 5.2.37-2+b10 500\n"+
		"        500 http://deb.debian.org/debian trixie/main amd64 Packages\n"+
		"        100 /var/lib/dpkg/status\n"), nil)

	status, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Empty(t, status.Blocked)
	assert.Zero(t, status.Pending.Blocked)
	assert.Equal(t, 1, runner.count("apt-cache policy bash"))
}

func TestInvalidBlockedNamesAreDropped(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, []string{"-oAPT::Update::Pre-Invoke::=reboot", "bash;id"})

	_, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sys.blocked)

	for _, call := range runner.recorded() {
		assert.NotContains(t, call, "apt-cache")
	}
}

func TestKernelSelection(t *testing.T) {
	tests := []struct {
		name          string
		boot          []string
		packages      []string
		running       string
		wantInstalled []string
		wantNewest    string
	}{
		{
			name:          "numeric order, not lexical",
			boot:          []string{"vmlinuz-6.12.94+deb13-amd64", "vmlinuz-6.12.107+deb13-amd64"},
			running:       "6.12.94+deb13-amd64",
			wantInstalled: []string{"6.12.94+deb13-amd64", "6.12.107+deb13-amd64"},
			wantNewest:    "6.12.107+deb13-amd64",
		},
		{
			name: "boot and packages are merged",
			boot: []string{"vmlinuz-6.1.0-9-amd64", "initrd.img-6.1.0-9-amd64", "config-6.1.0-9-amd64"},
			packages: []string{
				"linux-image-6.1.0-9-amd64", "linux-image-6.1.0-13-amd64", "linux-image-amd64",
				"linux-image-6.1.0-21-amd64-dbg", "linux-image-6.1.0-10-amd64-unsigned",
				"linux-headers-6.1.0-30-amd64", "linux-image-unsigned-6.1.0-11-generic",
			},
			running: "6.1.0-9-amd64",
			wantInstalled: []string{
				"6.1.0-9-amd64", "6.1.0-10-amd64", "6.1.0-11-generic", "6.1.0-13-amd64",
			},
			wantNewest: "6.1.0-13-amd64",
		},
		{
			name: "leftovers in /boot are not kernels",
			boot: []string{
				"vmlinuz-6.1.0-9-amd64", "vmlinuz-6.1.0-99-amd64.old", "vmlinuz-6.1.0-98-amd64.dpkg-tmp",
				"vmlinuz", "vmlinuz-rescue", "System.map-6.1.0-97-amd64", "vmlinux-6.1.0-10-arm64",
			},
			running:       "6.1.0-9-amd64",
			wantInstalled: []string{"6.1.0-9-amd64", "6.1.0-10-arm64"},
			wantNewest:    "6.1.0-10-arm64",
		},
		{
			name:          "ubuntu flavours",
			boot:          []string{"vmlinuz-6.8.0-45-generic", "vmlinuz-6.8.0-100-generic"},
			running:       "6.8.0-100-generic",
			wantInstalled: []string{"6.8.0-45-generic", "6.8.0-100-generic"},
			wantNewest:    "6.8.0-100-generic",
		},
		{
			name:          "running kernel newer than anything installed",
			boot:          []string{"vmlinuz-6.1.0-9-amd64"},
			running:       "6.5.0-1-amd64",
			wantInstalled: []string{"6.1.0-9-amd64"},
			wantNewest:    "6.1.0-9-amd64",
		},
		{
			name:          "no kernel of its own (container): the running one",
			running:       "6.12.113+deb13-amd64",
			wantInstalled: []string{},
			wantNewest:    "6.12.113+deb13-amd64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, pathBootID, testBootID+"\n")
			writeFile(t, root, pathUptime, "1.00 1.00\n")
			writeFile(t, root, pathKernelRelease, tt.running+"\n")
			writeFile(t, root, pathOSRelease, "ID=debian\n")

			for _, name := range tt.boot {
				writeFile(t, root, filepath.Join(pathBoot, name), "")
			}

			var listing strings.Builder

			listing.WriteString("base-files\t13\tamd64\tinstall ok installed\n")

			for _, name := range tt.packages {
				listing.WriteString(name + "\t1.0\tamd64\tinstall ok installed\n")
			}

			runner := &fakeRunner{responses: map[string]fakeResponse{
				keyDpkgQuery: {out: []byte(listing.String())},
				keySimulate:  {},
				keyArch:      {out: []byte("amd64\n")},
			}}
			sys := New(testLogger(), Options{Root: root, Runner: runner})

			inv, err := sys.Inventory(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.wantInstalled, inv.Kernels.Installed)
			assert.Equal(t, tt.wantNewest, inv.Kernels.Newest)
			assert.Equal(t, tt.running, inv.Kernels.Running)

			status, err := sys.Status(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.wantNewest, status.OS.NewestKernel)
			assert.Equal(t, tt.running, status.OS.RunningKernel)
		})
	}
}

func TestKernelInstalledAfterCollectionShowsInStatus(t *testing.T) {
	sys, _, root := newCaptureSystem(t, nil)

	status, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "6.12.107+deb13-cloud-amd64", status.OS.NewestKernel)

	// unattended-upgrades installs a kernel between two inventory runs.
	writeFile(t, root, filepath.Join(pathBoot, "vmlinuz-6.12.117+deb13-cloud-amd64"), "")

	status, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "6.12.117+deb13-cloud-amd64", status.OS.NewestKernel)
}

func TestFallbacksWithoutProcFiles(t *testing.T) {
	sys, runner, root := newCaptureSystem(t, nil)

	require.NoError(t, os.Remove(filepath.Join(root, pathKernelRelease)))
	require.NoError(t, os.Rename(filepath.Join(root, pathOSRelease),
		filepath.Join(root, "os-release.moved")))

	moved, err := os.ReadFile(filepath.Join(root, "os-release.moved"))
	require.NoError(t, err)
	writeFile(t, root, pathOSReleaseLib, string(moved))

	runner.set("uname -r", []byte("6.12.107+deb13-cloud-amd64\n"), nil)
	runner.set(keyArch, nil, &CommandError{Name: cmdDpkg, ExitCode: 127, Err: errors.New("gone")})
	runner.set("uname -m", []byte("aarch64\n"), nil)

	status, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "6.12.107+deb13-cloud-amd64", status.OS.RunningKernel)
	assert.Equal(t, "arm64", status.OS.Arch)
	assert.Equal(t, "debian", status.OS.ID)

	// The architecture is asked once.
	_, err = sys.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, runner.count("uname -m"))
}

func TestStatusErrors(t *testing.T) {
	for _, missing := range []string{pathBootID, pathUptime, pathOSRelease} {
		t.Run("missing "+missing, func(t *testing.T) {
			sys, _, root := newCaptureSystem(t, nil)
			require.NoError(t, os.Remove(filepath.Join(root, missing)))

			_, err := sys.Status(t.Context())
			require.Error(t, err)
		})
	}

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
	runner, root := captureRunner(t), captureRoot(t)
	sys := New(testLogger(), Options{Root: root, Runner: runner, InventoryInterval: 10 * time.Minute})

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

	now = now.Add(10*time.Minute - time.Second)

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

	now = now.Add(defaultInventoryInterval)

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
	now = now.Add(defaultInventoryInterval)
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

func TestUpgradePackagesRefusals(t *testing.T) {
	tests := []struct {
		name  string
		names []string
	}{
		{"not blocked", []string{"bash"}},
		{"one of several not blocked", []string{"docker-ce", "openssl"}},
		{"nothing named", nil},
		{"empty name", []string{""}},
		{"option", []string{"--allow-downgrades"}},
		{"short option", []string{"-y"}},
		{"version selector", []string{"docker-ce=5:27.0.0-1"}},
		{"release selector", []string{"docker-ce/trixie-backports"}},
		{"architecture qualifier", []string{"docker-ce:i386"}},
		{"removal suffix", []string{"docker-ce-"}},
		{"shell metacharacters", []string{"docker-ce; reboot"}},
		{"whitespace", []string{"docker-ce bash"}},
		{"upper case", []string{"Docker-CE"}},
		{"glob", []string{"docker-*"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys, runner, _ := newCaptureSystem(t, dockerBlocked)

			err := sys.UpgradePackages(t.Context(), tt.names)
			require.ErrorIs(t, err, ErrPackageNotAllowed)
			assert.Empty(t, runner.recorded(), "nothing may be executed")
		})
	}
}

func TestUpgradePackages(t *testing.T) {
	sys, runner, _ := newCaptureSystem(t, dockerBlocked)

	before, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	runner.reset()

	install := keyInstall + "containerd.io docker-ce-cli"
	runner.set(keyUpdate, []byte("Reading package lists...\n"), nil)
	runner.set("apt-cache policy docker-ce-cli", capture(t, "apt-cache-policy.txt"), nil)
	runner.set(install, []byte("1 upgraded, 0 newly installed, 0 to remove and 6 not upgraded.\n"), nil)

	// After the upgrade dpkg and apt report the new state.
	upgraded := strings.Replace(string(capture(t, "dpkg-query.txt")),
		"docker-ce-cli\t5:28.1.0-1~debian.13~trixie", "docker-ce-cli\t5:29.9.3-1~debian.13~trixie", 1)
	simulated := strings.ReplaceAll(string(capture(t, "apt-get-simulate.txt")),
		"Inst docker-ce-cli", "Skip docker-ce-cli")

	runner.mu.Lock()
	runner.hook = func(_ context.Context, key string) {
		if key == install {
			// Called with the runner unlocked.
			runner.set(keyDpkgQuery, []byte(upgraded), nil)
			runner.set(keySimulate, []byte(simulated), nil)
		}
	}
	runner.mu.Unlock()

	// Duplicates collapse, order is normalised.
	require.NoError(t, sys.UpgradePackages(t.Context(),
		[]string{"docker-ce-cli", "containerd.io", "docker-ce-cli"}))

	calls := runner.recorded()
	require.GreaterOrEqual(t, len(calls), 5)
	assert.Equal(t, []string{keyDpkgQuery, keyUpdate, install}, calls[:3],
		"installed check, update, then install, nothing else")
	assert.Equal(t, 2, runner.count(keyDpkgQuery), "inventory refreshed right after")

	// The refreshed inventory is served without another collection.
	runner.reset()

	after, err := sys.Inventory(t.Context())
	require.NoError(t, err)
	assert.Empty(t, runner.recorded())
	assert.NotEqual(t, before.Hash, after.Hash)
	assert.Contains(t, after.Packages, protocol.Package{
		Name: "docker-ce-cli", Version: "5:29.9.3-1~debian.13~trixie", Arch: "amd64",
	})

	status, err := sys.Status(t.Context())
	require.NoError(t, err)
	assert.Empty(t, status.Blocked)
	assert.Equal(t, protocol.Pending{Upgradable: 6, Security: 5}, status.Pending)
}

// apt-get install takes a name it does not know for a regular expression
// ("containerd.io" selects "containerd-io" too), so names of packages that
// are not installed never reach apt.
func TestUpgradePackagesNotInstalled(t *testing.T) {
	t.Run("only not installed: apt does not run", func(t *testing.T) {
		// docker-ce is blocked and known to apt, but not installed.
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)

		require.NoError(t, sys.UpgradePackages(t.Context(), []string{"docker-ce"}))
		assert.Equal(t, []string{keyDpkgQuery}, runner.recorded())
	})

	t.Run("regex-like name of a package nobody knows", func(t *testing.T) {
		// As a regular expression "docker.ce" matches the installed
		// "docker-ce-cli".
		sys, runner, _ := newCaptureSystem(t, []string{"docker.ce", "docker-ce-cli"})

		require.NoError(t, sys.UpgradePackages(t.Context(), []string{"docker.ce"}))
		assert.Equal(t, []string{keyDpkgQuery}, runner.recorded())
	})

	t.Run("mixed: only the installed ones are passed to apt", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		install := keyInstall + "containerd.io docker-ce-cli"
		runner.set(keyUpdate, nil, nil)
		runner.set(install, []byte("done\n"), nil)

		require.NoError(t, sys.UpgradePackages(t.Context(), dockerBlocked))

		calls := runner.recorded()
		require.GreaterOrEqual(t, len(calls), 3)
		assert.Equal(t, []string{keyDpkgQuery, keyUpdate, install}, calls[:3])

		for _, call := range calls {
			if strings.HasPrefix(call, "apt-get install") {
				assert.Equal(t, install, call)
			}
		}
	})

	t.Run("installed list unavailable: apt does not run", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		runner.set(keyDpkgQuery, nil, &CommandError{
			Name: cmdDpkgQuery, ExitCode: 2, Err: errors.New("exit status 2"),
		})

		err := sys.UpgradePackages(t.Context(), []string{"docker-ce-cli"})
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrPackageNotAllowed)
		assert.Equal(t, []string{keyDpkgQuery}, runner.recorded())
	})
}

// apt-get update does not wait for the lock of the package lists, which
// apt-daily.service holds while it runs.
func TestUpgradePackagesListsLocked(t *testing.T) {
	locked := func(t *testing.T) *CommandError {
		t.Helper()

		return &CommandError{
			Name: cmdAptGet, Args: []string{"update"}, ExitCode: 100,
			Err:    errors.New("exit status 100"),
			Output: string(capture(t, "apt-get-update-locked.txt")),
		}
	}

	install := keyInstall + "docker-ce-cli"

	t.Run("retried until the lock is free", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		sys.listsLockRetry = time.Millisecond

		runner.set(keyUpdate, nil, locked(t))
		runner.set(install, []byte("done\n"), nil)

		attempts := 0
		runner.hook = func(_ context.Context, key string) {
			if key != keyUpdate {
				return
			}

			// The third attempt is the last one that finds the lock held.
			if attempts++; attempts == 3 {
				runner.set(keyUpdate, []byte("Reading package lists...\n"), nil)
			}
		}

		require.NoError(t, sys.UpgradePackages(t.Context(), []string{"docker-ce-cli"}))
		assert.Equal(t, 4, runner.count(keyUpdate))
		assert.Equal(t, 1, runner.count(install))
	})

	t.Run("gives up after the lock wait", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		sys.listsLockRetry = time.Millisecond

		var (
			mu    sync.Mutex
			clock = time.Now()
		)

		sys.now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()

			return clock
		}

		runner.set(keyUpdate, nil, locked(t))
		runner.hook = func(_ context.Context, key string) {
			if key == keyUpdate {
				mu.Lock()
				clock = clock.Add(aptLockTimeout / 2)
				mu.Unlock()
			}
		}

		err := sys.UpgradePackages(t.Context(), []string{"docker-ce-cli"})

		var cmdErr *CommandError

		require.ErrorAs(t, err, &cmdErr)
		assert.Contains(t, err.Error(), "Could not get lock /var/lib/apt/lists/lock")
		assert.Equal(t, 2, runner.count(keyUpdate))
		assert.Zero(t, runner.count(install))
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		sys.listsLockRetry = time.Hour

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		runner.set(keyUpdate, nil, locked(t))
		runner.hook = func(_ context.Context, key string) {
			if key == keyUpdate {
				// The answer is still "locked"; the wait is interrupted.
				time.AfterFunc(10*time.Millisecond, cancel)
			}
		}

		err := sys.UpgradePackages(ctx, []string{"docker-ce-cli"})
		require.ErrorIs(t, err, context.Canceled)
		assert.Contains(t, err.Error(), "Could not get lock")
		assert.Equal(t, 1, runner.count(keyUpdate))
		assert.Zero(t, runner.count(install))
	})

	t.Run("other update failures are not retried", func(t *testing.T) {
		assert.False(t, isListsLockError(errors.New("plain")))
		assert.False(t, isListsLockError(&CommandError{
			Output: "E: The repository 'http://x stable Release' is not signed.",
		}))
		assert.True(t, isListsLockError(locked(t)))
	})
}

func TestUpgradePackagesFailures(t *testing.T) {
	aptFailure := func(output string) *CommandError {
		return &CommandError{
			Name: cmdAptGet, ExitCode: 100, Err: errors.New("exit status 100"), Output: output,
		}
	}

	t.Run("update fails: install is not attempted", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		runner.set(keyUpdate, nil, aptFailure("E: The repository 'http://x stable Release' is not signed."))

		err := sys.UpgradePackages(t.Context(), []string{"docker-ce-cli"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is not signed")
		assert.Equal(t, 1, runner.count(keyUpdate), "not a lock failure: not retried")
		assert.Zero(t, runner.count(keyInstall+"docker-ce-cli"))
	})

	t.Run("install fails: output in the error, inventory refreshed", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		runner.set(keyUpdate, nil, nil)
		runner.set(keyInstall+"docker-ce-cli", nil,
			aptFailure("dpkg: error processing package docker-ce-cli (--configure)"))

		err := sys.UpgradePackages(t.Context(), []string{"docker-ce-cli"})

		var cmdErr *CommandError

		require.ErrorAs(t, err, &cmdErr)
		assert.Equal(t, 100, cmdErr.ExitCode)
		assert.Contains(t, err.Error(), "dpkg: error processing package docker-ce-cli")
		assert.Equal(t, 2, runner.count(keyDpkgQuery))
	})

	t.Run("cancelled before start: nothing runs", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := sys.UpgradePackages(ctx, []string{"docker-ce-cli"})
		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, runner.recorded())
	})

	t.Run("cancelled during update: install is not attempted", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		runner.set(keyUpdate, nil, nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		runner.hook = func(_ context.Context, key string) {
			if key == keyUpdate {
				cancel()
			}
		}

		err := sys.UpgradePackages(ctx, []string{"docker-ce-cli"})
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, runner.count(keyInstall+"docker-ce-cli"))
	})

	t.Run("cancelled during install: install is not interrupted", func(t *testing.T) {
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)
		install := keyInstall + "docker-ce-cli"
		runner.set(keyUpdate, nil, nil)
		runner.set(install, []byte("done\n"), nil)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var installCtxErr error

		runner.hook = func(runCtx context.Context, key string) {
			if key == install {
				cancel()

				installCtxErr = runCtx.Err()
			}
		}

		require.NoError(t, sys.UpgradePackages(ctx, []string{"docker-ce-cli"}))
		require.NoError(t, installCtxErr, "the install context outlives the caller's")
		assert.Equal(t, 2, runner.count(keyDpkgQuery), "inventory refreshed although cancelled")
	})
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

func TestNewDefaults(t *testing.T) {
	sys := New(testLogger(), Options{})

	assert.Equal(t, "/", sys.root)
	assert.Equal(t, defaultInventoryInterval, sys.interval)
	assert.IsType(t, execRunner{}, sys.runner)
	assert.Empty(t, sys.blocked)
}
