package system

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
