package system

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestParseOSRelease(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want protocol.OSInfo
	}{
		{
			name: "debian 13 capture",
			in:   string(capture(t, "os-release")),
			want: protocol.OSInfo{
				ID: "debian", VersionID: "13", Codename: "trixie",
				PrettyName: "Debian GNU/Linux 13 (trixie)",
			},
		},
		{
			name: "ubuntu",
			in: "PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\n" +
				"VERSION=\"24.04.1 LTS (Noble Numbat)\"\nVERSION_CODENAME=noble\nID=ubuntu\n" +
				"ID_LIKE=debian\nUBUNTU_CODENAME=noble\n",
			want: protocol.OSInfo{
				ID: "ubuntu", VersionID: "24.04", Codename: "noble",
				PrettyName: "Ubuntu 24.04.1 LTS",
			},
		},
		{
			name: "quoting, comments and junk",
			in: "# a comment\n\n  ID='debian'  \nPRETTY_NAME=\"A \\\"quoted\\\" \\$name\"\n" +
				"no equals sign\nVERSION_ID=\n=nokey\nVERSION_CODENAME=\"\n",
			want: protocol.OSInfo{
				ID: "debian", PrettyName: `A "quoted" $name`, Codename: `"`,
			},
		},
		{name: "empty", in: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseOSRelease([]byte(tt.in)))
		})
	}
}

func TestParseUptime(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "93784.52 371822.11\n", want: 93784520 * time.Millisecond},
		{in: "0.00 0.00\n", want: 0},
		{in: "12\n", want: 12 * time.Second},
		{in: "8640000.99 1.0", want: 8640000990 * time.Millisecond},
		{in: "", wantErr: true},
		{in: "  \n", wantErr: true},
		{in: "soon 1.0", wantErr: true},
		{in: "-5.0 1.0", wantErr: true},
		{in: "1h 1.0", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseUptime(tt.in)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDebianArch(t *testing.T) {
	for machine, want := range map[string]string{
		"x86_64": "amd64", "aarch64": "arm64", "armv7l": "armhf", "i686": "i386",
		"ppc64le": "ppc64el", "riscv64": "riscv64", "s390x": "s390x",
	} {
		assert.Equal(t, want, debianArch(machine), machine)
	}
}

func TestKernelReleaseFromPackage(t *testing.T) {
	tests := []struct {
		name    string
		release string
		ok      bool
	}{
		{"linux-image-6.12.94+deb13-cloud-amd64", "6.12.94+deb13-cloud-amd64", true},
		{"linux-image-6.1.0-13-amd64", "6.1.0-13-amd64", true},
		{"linux-image-6.1.0-13-amd64-unsigned", "6.1.0-13-amd64", true},
		{"linux-image-unsigned-6.8.0-45-generic", "6.8.0-45-generic", true},
		{"linux-image-6.8.0-45-generic", "6.8.0-45-generic", true},
		{"linux-image-cloud-amd64", "", false},
		{"linux-image-amd64", "", false},
		{"linux-image-generic", "", false},
		{"linux-image-6.1.0-13-amd64-dbg", "", false},
		{"linux-image-unsigned-6.8.0-45-generic-dbgsym", "", false},
		{"linux-image-", "", false},
		{"linux-headers-6.1.0-13-amd64", "", false},
		{"linux-base", "", false},
		{"bash", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release, ok := kernelReleaseFromPackage(tt.name)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.release, release)
		})
	}
}
