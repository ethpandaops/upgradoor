//go:build system_integration

package systemtest

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// guardFile exists only in the image built from this directory.
	guardFile = "/usr/local/sbin/upgradoor-boot"
	// guardEnv is set by run.sh for every test it starts.
	guardEnv = "UPGRADOOR_SYSTEM_TEST"

	stateDirEnv     = "UPGRADOOR_STATE_DIR"
	defaultStateDir = "/var/lib/upgradoor"

	bootIDFile        = "boot-id-before-reboot"
	rebootMarkerFile  = "reboot-accepted"
	hashAfterUpgrades = "inventory-hash-before-reboot"

	pkgDummy  = "upgradoor-dummy"
	pkgLate   = "upgradoor-late"
	pkgBroken = "upgradoor-broken"
	pkgPlain  = "upgradoor-plain"
	pkgSecfix = "upgradoor-secfix"
	// pkgRegex is in the blocked set but no such package exists anywhere.
	// Read as a regular expression, which is what apt-get install does with
	// a name it does not know, it matches upgradoor-dummy.
	pkgRegex = "upgradoor.d"

	aptListsLock = "/var/lib/apt/lists/lock"

	originMain     = "Upgradoor-Test:1/upgradoor"
	originSecurity = "Debian-Security:13/stable-security"

	rebootRequired     = "/var/run/reboot-required"
	rebootRequiredPkgs = "/var/run/reboot-required.pkgs"
	dummyConf          = "/etc/upgradoor-test/upgradoor-dummy.conf"
)

var (
	// blocked is the agent's configured blocked set in these tests.
	blocked = []string{pkgDummy, pkgLate, pkgBroken, pkgRegex}

	bootIDPattern = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
)

// requireThrowawayHost stops a test that is not running inside the harness
// container. These tests upgrade packages and reboot.
func requireThrowawayHost(t *testing.T) {
	t.Helper()

	if os.Getenv(guardEnv) != "1" {
		t.Fatalf("%s is not set: run these tests through test/system/run.sh only", guardEnv)
	}

	if _, err := os.Stat(guardFile); err != nil {
		t.Fatalf("not inside the system test container (%s): %v", guardFile, err)
	}

	source, err := os.ReadFile("/etc/apt/sources.list.d/upgradoor-test.list")
	if err != nil || !strings.Contains(string(source), "/opt/upgradoor-repo/") {
		t.Fatalf("not inside the system test container: apt sources are not the test's")
	}
}

func stateDir() string {
	if dir := os.Getenv(stateDirEnv); dir != "" {
		return dir
	}

	return defaultStateDir
}

// newSystem returns the machine as the agent sees it. Only its OS part is
// used: the Docker client is created without connecting.
func newSystem(t *testing.T) host.Host {
	t.Helper()

	log := logrus.New()
	log.SetOutput(os.Stderr)
	log.SetLevel(logrus.DebugLevel)

	cfg := &host.Config{}
	cfg.ApplyDefaults()

	h, err := host.New(log, cfg, blocked, nil)
	require.NoError(t, err)

	t.Cleanup(func() { _ = h.Close() })

	return h
}

func testContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	return ctx
}

// debianArch is the dpkg name of the architecture this binary was built
// for, which is the container's.
func debianArch() string {
	switch runtime.GOARCH {
	case "arm":
		return "armhf"
	case "386":
		return "i386"
	case "ppc64le":
		return "ppc64el"
	default:
		return runtime.GOARCH
	}
}

func installedVersions(t *testing.T, inv *protocol.Inventory) map[string]string {
	t.Helper()

	versions := make(map[string]string, len(inv.Packages))

	for _, pkg := range inv.Packages {
		if strings.HasPrefix(pkg.Name, "upgradoor-") {
			assert.Equal(t, "all", pkg.Arch, pkg.Name)
		}

		versions[pkg.Name] = pkg.Version
	}

	return versions
}

func dummyUpgradable(name, candidate, origin string, isBlocked, security bool) protocol.Upgradable {
	return protocol.Upgradable{
		Name: name, Arch: "all", Current: "1.0", Candidate: candidate,
		Origin: origin, Security: security, Blocked: isBlocked,
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(raw)
}

func writeState(t *testing.T, name, content string) {
	t.Helper()

	path := filepath.Join(stateDir(), name)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	require.NoError(t, err)

	_, err = f.WriteString(content)
	require.NoError(t, err)
	// The machine is about to go down: make sure it is on the volume.
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
}

// lockAptLists takes the lock of apt's package lists the way apt does (a
// POSIX write lock on the lock file), as apt-daily.service holds it during
// its own "apt-get update". The returned function releases it.
func lockAptLists(t *testing.T) (release func()) {
	t.Helper()

	f, err := os.OpenFile(aptListsLock, os.O_RDWR|os.O_CREATE, 0o640)
	require.NoError(t, err)

	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0}
	require.NoError(t, syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock))

	return func() {
		// Closing the file drops the lock.
		assert.NoError(t, f.Close())
	}
}

// TestPreReboot is everything up to, but not including, the reboot. Its
// subtests depend on each other and run in order on one System, as the
// agent would use it.
func TestPreReboot(t *testing.T) {
	requireThrowawayHost(t)

	ctx := testContext(t)
	sys := newSystem(t)

	var initialHash string

	t.Run("inventory of the fresh host", func(t *testing.T) {
		inv, err := sys.Inventory(ctx)
		require.NoError(t, err)

		assert.Equal(t, protocol.OSInfo{
			ID: "debian", VersionID: "13", Codename: "trixie",
			PrettyName: "Debian GNU/Linux 13 (trixie)", Arch: debianArch(),
		}, inv.OS)

		// A container has no kernel of its own.
		assert.Equal(t, strings.TrimSpace(readFile(t, "/proc/sys/kernel/osrelease")),
			inv.Kernels.Running)
		assert.Equal(t, inv.Kernels.Running, inv.Kernels.Newest)
		assert.Empty(t, inv.Kernels.Installed)

		versions := installedVersions(t, inv)
		for _, name := range []string{pkgDummy, pkgLate, pkgBroken, pkgPlain, pkgSecfix} {
			assert.Equal(t, "1.0", versions[name], name)
		}

		for _, name := range []string{"apt", "dpkg", "systemd", "base-files", "libc6"} {
			assert.NotEmpty(t, versions[name], "real package %s", name)
		}

		assert.Greater(t, len(inv.Packages), 80)

		for i := 1; i < len(inv.Packages); i++ {
			a, b := inv.Packages[i-1], inv.Packages[i]
			assert.True(t, a.Name < b.Name || (a.Name == b.Name && a.Arch < b.Arch),
				"sorted: %s before %s", a.Name, b.Name)
		}

		// upgradoor-late 2.0 is in the repository, but apt's lists are
		// older and nobody may run apt-get update for an inventory.
		assert.Equal(t, []protocol.Upgradable{
			dummyUpgradable(pkgBroken, "2.0", originMain, true, false),
			dummyUpgradable(pkgDummy, "2.0", originMain, true, false),
			dummyUpgradable(pkgPlain, "2.0", originMain, false, false),
			dummyUpgradable(pkgSecfix, "1.0+deb13u1", originSecurity, false, true),
		}, inv.Upgradable)

		hash, err := inv.ComputeHash()
		require.NoError(t, err)
		assert.Equal(t, hash, inv.Hash)
		assert.WithinDuration(t, time.Now(), inv.CollectedAt, time.Minute)

		initialHash = inv.Hash

		// A second, independent collection of the same system: same hash.
		again, err := newSystem(t).Inventory(ctx)
		require.NoError(t, err)
		assert.Equal(t, inv.Hash, again.Hash)
	})

	t.Run("status of the fresh host", func(t *testing.T) {
		status, err := sys.System(ctx)
		require.NoError(t, err)

		assert.Regexp(t, bootIDPattern, status.BootID)
		assert.Equal(t, strings.TrimSpace(readFile(t, "/proc/sys/kernel/random/boot_id")),
			status.BootID)
		assert.Positive(t, status.Uptime)

		assert.Equal(t, "debian", status.OS.ID)
		assert.Equal(t, "13", status.OS.VersionID)
		assert.Equal(t, debianArch(), status.OS.Arch)
		assert.NotEmpty(t, status.OS.RunningKernel)
		assert.Equal(t, status.OS.RunningKernel, status.OS.NewestKernel)
		assert.False(t, status.OS.RebootRequired)
		assert.Empty(t, status.OS.RebootPackages)

		assert.Equal(t, protocol.Pending{Upgradable: 4, Security: 1, Blocked: 2}, status.Pending)
		assert.Equal(t, []protocol.PackageChange{
			{Name: pkgBroken, From: "1.0", To: "2.0"},
			{Name: pkgDummy, From: "1.0", To: "2.0"},
		}, status.Blocked)
		assert.Equal(t, initialHash, status.InventoryHash)
	})

	t.Run("packages outside the blocked set are refused", func(t *testing.T) {
		for _, names := range [][]string{
			{pkgPlain},
			{pkgSecfix},
			{pkgDummy, pkgPlain},
			{"bash"},
			{pkgDummy + "=2.0"},
			{"--allow-downgrades", pkgDummy},
			{},
		} {
			err := sys.UpgradePackages(ctx, names)
			require.ErrorContains(t, err, "package not allowed", "%v", names)
		}

		// Nothing ran: not the upgrades, and not apt-get update either,
		// which would have made upgradoor-late 2.0 visible.
		inv, err := newSystem(t).Inventory(ctx)
		require.NoError(t, err)
		assert.Equal(t, initialHash, inv.Hash)

		versions := installedVersions(t, inv)
		assert.Equal(t, "1.0", versions[pkgPlain])
		assert.Equal(t, "1.0", versions[pkgDummy])
	})

	t.Run("a blocked name that is not installed never reaches apt", func(t *testing.T) {
		// apt-get install --only-upgrade upgradoor.d would upgrade
		// upgradoor-dummy through apt's regular expression fallback.
		require.NoError(t, sys.UpgradePackages(ctx, []string{pkgRegex}))

		// Nothing ran: no upgrade, and no apt-get update either.
		inv, err := newSystem(t).Inventory(ctx)
		require.NoError(t, err)
		assert.Equal(t, initialHash, inv.Hash)
		assert.Equal(t, "1.0", installedVersions(t, inv)[pkgDummy])
		assert.Equal(t, "1.0\n", readFile(t, "/usr/share/upgradoor-test/upgradoor-dummy.version"))
	})

	t.Run("blocked packages are upgraded", func(t *testing.T) {
		// Somebody else is updating the package lists when the slot starts:
		// apt-get update fails at once on that lock and has to be retried.
		release := lockAptLists(t)
		lockHeld := 2 * time.Second
		unlock := time.AfterFunc(lockHeld, release)

		t.Cleanup(func() {
			if unlock.Stop() {
				release()
			}
		})

		started := time.Now()

		// The name that is not installed is left out, the others upgraded.
		require.NoError(t, sys.UpgradePackages(ctx, []string{pkgDummy, pkgLate, pkgRegex}))
		assert.Greater(t, time.Since(started), lockHeld, "waited for the lists lock")

		// The same System serves the refreshed inventory at once.
		inv, err := sys.Inventory(ctx)
		require.NoError(t, err)
		assert.NotEqual(t, initialHash, inv.Hash)

		versions := installedVersions(t, inv)
		assert.Equal(t, "2.0", versions[pkgDummy])
		// Only apt-get update could have told apt about this one.
		assert.Equal(t, "2.0", versions[pkgLate])
		// Exactly the named packages were touched.
		assert.Equal(t, "1.0", versions[pkgBroken])
		assert.Equal(t, "1.0", versions[pkgPlain])
		assert.Equal(t, "1.0", versions[pkgSecfix])

		assert.Equal(t, "2.0\n", readFile(t, "/usr/share/upgradoor-test/upgradoor-dummy.version"))
		assert.Equal(t, "2.0\n", readFile(t, "/usr/share/upgradoor-test/upgradoor-late.version"))

		// The hand-edited configuration file was kept (--force-confold)
		// and the packaged one put next to it.
		conf := readFile(t, dummyConf)
		assert.Contains(t, conf, "setting=changed-by-hand")
		assert.Contains(t, conf, "setting=packaged-1.0")
		assert.Contains(t, readFile(t, dummyConf+".dpkg-dist"), "setting=packaged-2.0")
		// An untouched one was replaced (--force-confdef).
		assert.Contains(t, readFile(t, "/etc/upgradoor-test/upgradoor-late.conf"),
			"setting=packaged-2.0")

		assert.Equal(t, []protocol.Upgradable{
			dummyUpgradable(pkgBroken, "2.0", originMain, true, false),
			dummyUpgradable(pkgPlain, "2.0", originMain, false, false),
			dummyUpgradable(pkgSecfix, "1.0+deb13u1", originSecurity, false, true),
		}, inv.Upgradable)

		status, err := sys.System(ctx)
		require.NoError(t, err)
		assert.Equal(t, protocol.Pending{Upgradable: 3, Security: 1, Blocked: 1}, status.Pending)
		assert.Equal(t, []protocol.PackageChange{{Name: pkgBroken, From: "1.0", To: "2.0"}},
			status.Blocked)
		assert.Equal(t, inv.Hash, status.InventoryHash)
	})

	t.Run("a failing upgrade reports apt's output", func(t *testing.T) {
		err := sys.UpgradePackages(ctx, []string{pkgBroken})
		require.Error(t, err)

		assert.Contains(t, err.Error(), "apt-get")
		assert.Contains(t, err.Error(), "exit status 100")
		assert.Contains(t, err.Error(), "upgradoor-broken: preinst refuses the upgrade")

		inv, err := sys.Inventory(ctx)
		require.NoError(t, err)
		assert.Equal(t, "1.0", installedVersions(t, inv)[pkgBroken])
	})

	t.Run("reboot-required flag", func(t *testing.T) {
		require.NoError(t, os.WriteFile(rebootRequired,
			[]byte("*** System restart required ***\n"), 0o644))
		require.NoError(t, os.WriteFile(rebootRequiredPkgs,
			[]byte("libc6\nlinux-image-6.12.117+deb13-amd64\nlibc6\n"), 0o644))

		status, err := sys.System(ctx)
		require.NoError(t, err)
		assert.True(t, status.OS.RebootRequired)
		assert.Equal(t, []string{"libc6", "linux-image-6.12.117+deb13-amd64"},
			status.OS.RebootPackages)
	})
}

// TestReboot records the boot id on the state volume and reboots the host.
// The process may be killed by the shutdown at any point after Reboot
// returned; TestPostReboot checks what was recorded.
func TestReboot(t *testing.T) {
	requireThrowawayHost(t)

	ctx := testContext(t)
	sys := newSystem(t)

	status, err := sys.System(ctx)
	require.NoError(t, err)
	require.True(t, status.OS.RebootRequired, "TestPreReboot left the flag")

	writeState(t, bootIDFile, status.BootID+"\n")
	writeState(t, hashAfterUpgrades, status.InventoryHash+"\n")

	require.NoError(t, sys.Reboot(ctx))

	writeState(t, rebootMarkerFile, time.Now().UTC().Format(time.RFC3339Nano)+"\n")
}

// TestPostReboot runs after the host came back.
func TestPostReboot(t *testing.T) {
	requireThrowawayHost(t)

	ctx := testContext(t)
	sys := newSystem(t)

	before := strings.TrimSpace(readFile(t, filepath.Join(stateDir(), bootIDFile)))
	require.Regexp(t, bootIDPattern, before)

	accepted := strings.TrimSpace(readFile(t, filepath.Join(stateDir(), rebootMarkerFile)))
	_, err := time.Parse(time.RFC3339Nano, accepted)
	require.NoError(t, err, "Reboot returned without error before the host went down")

	status, err := sys.System(ctx)
	require.NoError(t, err)

	assert.Regexp(t, bootIDPattern, status.BootID)
	assert.NotEqual(t, before, status.BootID, "the boot id changes with a reboot")

	// /run did not survive, so the flag is gone, as on a real host.
	assert.False(t, status.OS.RebootRequired)
	assert.Empty(t, status.OS.RebootPackages)

	// The package state did survive.
	inv, err := sys.Inventory(ctx)
	require.NoError(t, err)

	versions := installedVersions(t, inv)
	assert.Equal(t, "2.0", versions[pkgDummy])
	assert.Equal(t, "2.0", versions[pkgLate])
	assert.Equal(t, "1.0", versions[pkgBroken])

	assert.Equal(t, strings.TrimSpace(readFile(t, filepath.Join(stateDir(), hashAfterUpgrades))),
		inv.Hash, "same packages, same kernel: same inventory hash across the reboot")
	assert.Equal(t, protocol.Pending{Upgradable: 3, Security: 1, Blocked: 1}, status.Pending)
}
