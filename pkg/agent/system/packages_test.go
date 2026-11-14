package system

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// testdata/debian13 holds outputs captured from a debian:13 container with a
// few packages downgraded, the Docker repository added, containerd.io on
// hold and two cloud kernels installed.
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

	byName := make(map[string]installedPackage, len(pkgs))
	for _, p := range pkgs {
		byName[p.Name] = p
	}

	assert.Equal(t, installedPackage{
		Package: protocol.Package{
			Name: "docker-ce-cli", Version: "5:28.1.0-1~debian.13~trixie", Arch: "amd64",
		},
	}, byName["docker-ce-cli"])
	assert.Equal(t, installedPackage{
		Package: protocol.Package{Name: "containerd.io", Version: "1.7.27-1", Arch: "amd64"},
		Held:    true,
	}, byName["containerd.io"])
	assert.Equal(t, "all", byName["tzdata"].Arch)

	for i := 1; i < len(pkgs); i++ {
		assert.Less(t, pkgs[i-1].Name, pkgs[i].Name, "sorted by name")
	}
}

func TestParseDpkgQuery(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      []installedPackage
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
			want: []installedPackage{
				{Package: protocol.Package{Name: "a", Version: "2.0", Arch: "all"}, Held: true},
				{Package: protocol.Package{Name: "b", Version: "1.0", Arch: "amd64"}},
			},
		},
		{
			name: "multi-arch sorts by name then arch",
			in: "libc6\t2.41-12\ti386\tinstall ok installed\n" +
				"libc6\t2.41-12\tamd64\tinstall ok installed\n",
			want: []installedPackage{
				{Package: protocol.Package{Name: "libc6", Version: "2.41-12", Arch: "amd64"}},
				{Package: protocol.Package{Name: "libc6", Version: "2.41-12", Arch: "i386"}},
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
			want: []installedPackage{
				{Package: protocol.Package{Name: "c", Version: "1.0", Arch: "amd64"}},
			},
			malformed: 4,
		},
		{name: "empty", in: "", want: []installedPackage{}},
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

	// In apt's order. The two new installations (apparmor and the new
	// kernel image) are not upgrades and are left out.
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
		{"trixie-security/main", true},
		{"trixie/main,trixie-security/main", true},
		{"noble-security/universe", true},

		{"Debian:13.7/stable", false},
		{"Debian:13-updates/stable-updates", false},
		{"Docker CE:trixie", false},
		{"Ubuntu:24.04/noble-updates", false},
		{"trixie/stable", false},
		{"trixie/main,trixie-updates/main", false},
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

func TestParseAptPolicyCapture(t *testing.T) {
	got := parseAptPolicy(capture(t, "apt-cache-policy.txt"))

	want := []policyEntry{
		{
			Name:      "docker-ce",
			Candidate: "5:29.9.3-1~debian.13~trixie",
			Origin:    "trixie/stable",
		},
		{
			Name:      "docker-ce-cli",
			Installed: "5:28.1.0-1~debian.13~trixie",
			Candidate: "5:29.9.3-1~debian.13~trixie",
			Origin:    "trixie/stable",
		},
		{
			Name:      "containerd.io",
			Installed: "1.7.27-1",
			Candidate: "2.4.2-4~debian.13~trixie",
			Origin:    "trixie/stable",
		},
	}

	assert.Equal(t, want, got)
}

func TestParseAptPolicy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []policyEntry
	}{
		{
			name: "candidate is the installed version",
			in: "bash:\n" +
				"  Installed: 5.2.37-2+b10\n" +
				"  Candidate: 5.2.37-2+b10\n" +
				"  Version table:\n" +
				" *** 5.2.37-2+b10 500\n" +
				"        500 http://deb.debian.org/debian trixie/main amd64 Packages\n" +
				"        100 /var/lib/dpkg/status\n",
			want: []policyEntry{{
				Name: "bash", Installed: "5.2.37-2+b10", Candidate: "5.2.37-2+b10",
				Origin: "trixie/main",
			}},
		},
		{
			name: "candidate from a security suite, not the first of the table",
			in: "openssl:\n" +
				"  Installed: 3.5.7-1~deb13u2\n" +
				"  Candidate: 3.5.7-1~deb13u3\n" +
				"  Version table:\n" +
				"     3.6.0-1 100\n" +
				"        100 http://deb.debian.org/debian trixie-backports/main amd64 Packages\n" +
				"     3.5.7-1~deb13u3 500\n" +
				"        500 http://deb.debian.org/debian-security trixie-security/main amd64 Packages\n" +
				" *** 3.5.7-1~deb13u2 500\n" +
				"        500 http://deb.debian.org/debian trixie/main amd64 Packages\n" +
				"        100 /var/lib/dpkg/status\n",
			want: []policyEntry{{
				Name: "openssl", Installed: "3.5.7-1~deb13u2", Candidate: "3.5.7-1~deb13u3",
				Origin: "trixie-security/main",
			}},
		},
		{
			name: "candidate in several archives, the security one not first",
			in: "openssl:\n" +
				"  Installed: 3.5.7-1~deb13u2\n" +
				"  Candidate: 3.5.7-1~deb13u3\n" +
				"  Version table:\n" +
				"     3.5.7-1~deb13u3 500\n" +
				"        500 http://deb.debian.org/debian trixie/main amd64 Packages\n" +
				"        500 http://deb.debian.org/debian trixie/main i386 Packages\n" +
				"        500 http://deb.debian.org/debian-security trixie-security/main amd64 Packages\n" +
				" *** 3.5.7-1~deb13u2 500\n" +
				"        500 http://deb.debian.org/debian trixie-updates/main amd64 Packages\n" +
				"        100 /var/lib/dpkg/status\n" +
				"zlib1g:\n" +
				"  Installed: 1\n" +
				"  Candidate: 2\n" +
				"  Version table:\n" +
				"     2 500\n" +
				"        500 http://deb.debian.org/debian trixie/main amd64 Packages\n",
			want: []policyEntry{
				{
					Name: "openssl", Installed: "3.5.7-1~deb13u2", Candidate: "3.5.7-1~deb13u3",
					Origin: "trixie/main,trixie-security/main",
				},
				{Name: "zlib1g", Installed: "1", Candidate: "2", Origin: "trixie/main"},
			},
		},
		{
			name: "flat repository, not installed, architecture qualifier",
			in: "upgradoor-dummy:amd64:\n" +
				"  Installed: (none)\n" +
				"  Candidate: 2.0\n" +
				"  Version table:\n" +
				"     2.0 500\n" +
				"        500 file:/opt/repo/main ./ Packages\n",
			want: []policyEntry{{Name: "upgradoor-dummy", Candidate: "2.0", Origin: "./"}},
		},
		{
			name: "unknown to apt",
			in: "ghost:\n" +
				"  Installed: (none)\n" +
				"  Candidate: (none)\n" +
				"  Version table:\n",
			want: []policyEntry{{Name: "ghost"}},
		},
		{
			name: "noise before the first package",
			in: "N: Unable to locate package nope\n" +
				"  Installed: 9\n" +
				"a:\n" +
				"  Installed: 1\n" +
				"  Candidate: 1\n",
			want: []policyEntry{{Name: "a", Installed: "1", Candidate: "1"}},
		},
		{name: "empty", in: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseAptPolicy([]byte(tt.in)))
		})
	}
}

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
