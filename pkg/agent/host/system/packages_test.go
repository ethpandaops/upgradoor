package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// testdata/debian13 holds outputs of a debian:13 container with a few packages
// downgraded, the Docker repository, containerd.io on hold and two kernels.
func capture(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", "debian13", name))
	require.NoError(t, err)

	return raw
}

func TestParseDpkgQueryCapture(t *testing.T) {
	pkgs, malformed := parseDpkgQuery(capture(t, "dpkg-query.txt"))

	assert.Zero(t, malformed)
	require.Len(t, pkgs, 100)

	byName := make(map[string]protocol.Package, len(pkgs))
	for _, p := range pkgs {
		byName[p.Name] = p
	}

	assert.Equal(t, protocol.Package{
		Name: "docker-ce-cli", Version: "5:28.1.0-1~debian.13~trixie", Arch: "amd64",
	}, byName["docker-ce-cli"])
	assert.Equal(t, protocol.Package{Name: "containerd.io", Version: "1.7.27-1", Arch: "amd64"},
		byName["containerd.io"], "a package on hold is installed")
	assert.Equal(t, "all", byName["tzdata"].Arch)

	for i := 1; i < len(pkgs); i++ {
		assert.Less(t, pkgs[i-1].Name, pkgs[i].Name, "sorted by name")
	}
}

func TestParseDpkgQuery(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      []protocol.Package
		malformed int
	}{
		{
			name: "only fully installed packages",
			in: "b\t1.0\tamd64\tinstall ok installed\n" +
				"removed\t1.0\tamd64\tdeinstall ok config-files\n" +
				"half\t1.0\tamd64\tinstall ok half-configured\n" +
				"unpacked\t1.0\tamd64\tinstall ok unpacked\n" +
				"broken\t1.0\tamd64\tinstall reinstreq installed\n" +
				"purged\t\t\tunknown ok not-installed\n" +
				"a\t2.0\tall\thold ok installed\n",
			want: []protocol.Package{
				{Name: "a", Version: "2.0", Arch: "all"},
				{Name: "b", Version: "1.0", Arch: "amd64"},
			},
		},
		{
			name: "multi-arch sorts by name then arch",
			in: "libc6\t2.41-12\ti386\tinstall ok installed\n" +
				"libc6\t2.41-12\tamd64\tinstall ok installed\n",
			want: []protocol.Package{
				{Name: "libc6", Version: "2.41-12", Arch: "amd64"},
				{Name: "libc6", Version: "2.41-12", Arch: "i386"},
			},
		},
		{
			name: "malformed lines are counted and skipped",
			in: "garbage\n" +
				"a\t1.0\tamd64\n" +
				"b\t1.0\tamd64\tinstall ok\n" +
				"\t1.0\tamd64\tinstall ok installed\n" +
				"\n" +
				"c\t1.0\tamd64\tinstall ok installed\n",
			want: []protocol.Package{
				{Name: "c", Version: "1.0", Arch: "amd64"},
			},
			malformed: 4,
		},
		{name: "empty", in: "", want: []protocol.Package{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, malformed := parseDpkgQuery([]byte(tt.in))
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.malformed, malformed)
		})
	}
}

func TestParseSimulatedUpgradeCapture(t *testing.T) {
	got := parseSimulatedUpgrade(capture(t, "apt-get-simulate.txt"))

	const (
		security = "Debian-Security:13/stable-security"
		amd64    = "amd64"
	)

	// In apt's order, without the two new installations.
	want := []protocol.Upgradable{
		{
			Name: "openssl-provider-legacy", Arch: amd64, Current: "3.5.7-1~deb13u2",
			Candidate: "3.5.7-1~deb13u3", Origin: security, Security: true,
		},
		{
			Name: "zlib1g", Arch: amd64, Current: "1:1.3.dfsg+really1.3.1-1+b1",
			Candidate: "1:1.3.dfsg+really1.3.1-1+deb13u1", Origin: security, Security: true,
		},
		{
			Name: "libssl3t64", Arch: amd64, Current: "3.5.7-1~deb13u2",
			Candidate: "3.5.7-1~deb13u3", Origin: security, Security: true,
		},
		{
			Name: "tzdata", Arch: "all", Current: "2026c-0+deb13u1",
			Candidate: "2026f-0+deb13u1", Origin: "Debian:13-updates/stable-updates",
		},
		{
			Name: "docker-ce-cli", Arch: amd64, Current: "5:28.1.0-1~debian.13~trixie",
			Candidate: "5:29.9.3-1~debian.13~trixie", Origin: "Docker CE:trixie",
		},
		{
			Name: "linux-image-cloud-amd64", Arch: amd64, Current: "6.12.107-1",
			Candidate: "6.12.117-1", Origin: security, Security: true,
		},
		{
			Name: "openssl", Arch: amd64, Current: "3.5.7-1~deb13u2",
			Candidate: "3.5.7-1~deb13u3", Origin: security, Security: true,
		},
	}

	assert.Equal(t, want, got)
}

func TestParseSimulatedUpgrade(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []protocol.Upgradable
	}{
		{
			name: "several origins",
			line: "Inst bash [5.2.15-2+b2] (5.2.15-2+b7 Debian:12.5/stable, " +
				"Debian-Security:12/stable-security [amd64])",
			want: []protocol.Upgradable{{
				Name: "bash", Arch: "amd64", Current: "5.2.15-2+b2", Candidate: "5.2.15-2+b7",
				Origin:   "Debian:12.5/stable, Debian-Security:12/stable-security",
				Security: true,
			}},
		},
		{
			name: "foreign architecture and trailing dependency list",
			line: "Inst libc6:i386 [2.36-9+deb12u3] (2.36-9+deb12u4 Debian:12.5/stable [i386]) " +
				"[libc6:amd64 on libc6:i386] []",
			want: []protocol.Upgradable{{
				Name: "libc6", Arch: "i386", Current: "2.36-9+deb12u3",
				Candidate: "2.36-9+deb12u4", Origin: "Debian:12.5/stable",
			}},
		},
		{
			name: "ubuntu security pocket",
			line: "Inst openssl [3.0.13-0ubuntu3.1] (3.0.13-0ubuntu3.4 " +
				"Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])",
			want: []protocol.Upgradable{{
				Name: "openssl", Arch: "amd64", Current: "3.0.13-0ubuntu3.1",
				Candidate: "3.0.13-0ubuntu3.4",
				Origin:    "Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security",
				Security:  true,
			}},
		},
		{
			name: "no origin and no architecture: qualifier is the fallback",
			line: "Inst local:arm64 [1.0] (2.0)",
			want: []protocol.Upgradable{{
				Name: "local", Arch: "arm64", Current: "1.0", Candidate: "2.0",
			}},
		},
		{
			name: "new installation is not an upgrade",
			line: "Inst apparmor (4.1.0-1 Debian:13.7/stable [amd64])",
		},
		{name: "conf line", line: "Conf bash (5.2.15-2+b7 Debian:12.5/stable [amd64])"},
		{name: "remove line", line: "Remv foo [1.0]"},
		{name: "truncated line", line: "Inst bash [5.2.15-2+b2] (5.2.15-2+b7 Debian:12.5/sta"},
		{name: "prose", line: "The following packages will be upgraded:"},
		{name: "empty brackets", line: "Inst bash [] (1.0 Debian:12/stable [amd64])"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseSimulatedUpgrade([]byte(tt.line+"\n")))
		})
	}
}

func TestIsSecurityOrigin(t *testing.T) {
	tests := []struct {
		origin string
		want   bool
	}{
		{"Debian-Security:13/stable-security", true},
		{"Debian-Security:12/oldstable-security", true},
		{"debian-security:13/whatever", true},
		{"Debian:13.7/stable, Debian-Security:13/stable-security", true},
		{"Ubuntu:24.04/noble-security", true},
		{"Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security", true},
		{"UbuntuESMApps:22.04/jammy-apps-security", true},

		{"Debian:13.7/stable", false},
		{"Debian:13-updates/stable-updates", false},
		{"Docker CE:trixie", false},
		{"Ubuntu:24.04/noble-updates", false},
		{"Upgradoor-Test:1/upgradoor", false},
		{"Security-Tools:1/stable", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.origin, func(t *testing.T) {
			assert.Equal(t, tt.want, isSecurityOrigin(tt.origin))
		})
	}
}

// Only blocked packages are upgraded, by validated name.
func TestValidPackageName(t *testing.T) {
	valid := []string{
		"docker-ce", "containerd.io", "libstdc++6", "g++", "0ad", "docker-ce+",
		"linux-image-6.12.94+deb13-amd64",
	}
	invalid := []string{
		"", "a", "-y", "--allow-downgrades", "Docker-CE", "docker-ce=1.0", "docker-ce/stable",
		"docker-ce:amd64", "docker ce", "docker-ce\n", "docker-ce;reboot", "$(reboot)", "*",
		".hidden", "+plus", "docker_ce", "-o", "docker-ce-", "-oDpkg::Options::=--force-all",
	}

	for _, name := range valid {
		assert.True(t, validPackageName(name), "%q", name)
	}

	for _, name := range invalid {
		assert.False(t, validPackageName(name), "%q", name)
	}
}

// Only blocked packages are upgraded, by validated name.
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
			require.ErrorIs(t, err, errPackageNotAllowed)
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

// The upgrade command runs in place of apt-get, to its end, with the names.
func TestUpgradePackagesCommand(t *testing.T) {
	const upgrade = "exec /usr/bin/sudo -n /usr/local/lib/upgradoor/upgrade-packages containerd.io docker-ce-cli"

	sys, runner, _ := newCaptureSystem(t, dockerBlocked)
	sys.commands.UpgradePackages = []string{"/usr/bin/sudo", "-n", "/usr/local/lib/upgradoor/upgrade-packages"}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	runner.mu.Lock()
	runner.hook = func(hookCtx context.Context, key string) {
		if key == upgrade {
			cancel()
			assert.NoError(t, hookCtx.Err(), "the command is not interrupted")
		}
	}
	runner.mu.Unlock()

	runner.set(upgrade, []byte("1 upgraded.\n"), nil)

	require.NoError(t, sys.UpgradePackages(ctx, []string{"docker-ce-cli", "containerd.io"}))
	assert.Equal(t, []string{keyDpkgQuery, upgrade}, runner.recorded()[:2])
	assert.Zero(t, runner.count(keyUpdate))

	err := sys.UpgradePackages(t.Context(), []string{"bash"})
	require.ErrorIs(t, err, errPackageNotAllowed)
}

// Names of packages that are not installed never reach apt, which reads them as regular expressions.
func TestUpgradePackagesNotInstalled(t *testing.T) {
	t.Run("only not installed: apt does not run", func(t *testing.T) {
		// docker-ce is blocked and known to apt, but not installed.
		sys, runner, _ := newCaptureSystem(t, dockerBlocked)

		require.NoError(t, sys.UpgradePackages(t.Context(), []string{"docker-ce"}))
		assert.Equal(t, []string{keyDpkgQuery}, runner.recorded())
	})

	t.Run("regex-like name of a package nobody knows", func(t *testing.T) {
		// As a regular expression "docker.ce" matches "docker-ce-cli".
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
		runner.set(keyDpkgQuery, nil, &commandError{
			Name: cmdDpkgQuery, ExitCode: 2, Err: errors.New("exit status 2"),
		})

		err := sys.UpgradePackages(t.Context(), []string{"docker-ce-cli"})
		require.Error(t, err)
		assert.NotErrorIs(t, err, errPackageNotAllowed)
		assert.Equal(t, []string{keyDpkgQuery}, runner.recorded())
	})
}

// apt-get update is retried while the package lists are locked.
func TestUpgradePackagesListsLocked(t *testing.T) {
	locked := func(t *testing.T) *commandError {
		t.Helper()

		return &commandError{
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

		var cmdErr *commandError

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
		assert.False(t, isListsLockError(&commandError{
			Output: "E: The repository 'http://x stable Release' is not signed.",
		}))
		assert.True(t, isListsLockError(locked(t)))
	})
}

func TestUpgradePackagesFailures(t *testing.T) {
	aptFailure := func(output string) *commandError {
		return &commandError{
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

		var cmdErr *commandError

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
