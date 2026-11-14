package system

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
)

const (
	kernelPackagePrefix         = "linux-image-"
	kernelPackageUnsignedPrefix = "linux-image-unsigned-"
)

var (
	// kernelImagePrefixes are the names of kernel images in /boot.
	kernelImagePrefixes = []string{"vmlinuz-", "vmlinux-"}
	// kernelImageSkipSuffixes mark leftovers in /boot that are not an
	// installed kernel (vmlinuz-6.1.0-13-amd64.old).
	kernelImageSkipSuffixes = []string{".old", ".bak", ".dpkg-tmp", ".dpkg-new"}
	// kernelPackageSkipSuffixes mark linux-image packages that install no
	// kernel (debug symbols).
	kernelPackageSkipSuffixes = []string{"-dbg", "-dbgsym"}
)

// kernelReleaseFromPackage returns the kernel release a linux-image package
// installs, e.g. "6.12.48+deb13-amd64" for "linux-image-6.12.48+deb13-amd64".
// Metapackages (linux-image-amd64) and debug packages return false.
func kernelReleaseFromPackage(name string) (string, bool) {
	release, ok := strings.CutPrefix(name, kernelPackageUnsignedPrefix)
	if !ok {
		release, ok = strings.CutPrefix(name, kernelPackagePrefix)
	}

	if !ok || release == "" || !isDigit(release[0]) || hasAnySuffix(release, kernelPackageSkipSuffixes) {
		return "", false
	}

	// Debian's unsigned images: linux-image-<release>-unsigned.
	return strings.TrimSuffix(release, "-unsigned"), true
}

// kernelReleasesFromPackages returns the releases of the installed kernel
// image packages.
func kernelReleasesFromPackages(pkgs []installedPackage) []string {
	var releases []string

	for i := range pkgs {
		if release, ok := kernelReleaseFromPackage(pkgs[i].Name); ok {
			releases = append(releases, release)
		}
	}

	return releases
}

// bootKernelReleases returns the releases of the kernel images in /boot. A
// missing /boot is not an error.
func (s *System) bootKernelReleases() ([]string, error) {
	entries, err := os.ReadDir(s.path(pathBoot))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("list kernels in /boot: %w", err)
	}

	var releases []string

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		for _, prefix := range kernelImagePrefixes {
			release, ok := strings.CutPrefix(entry.Name(), prefix)
			if !ok || release == "" || !isDigit(release[0]) ||
				hasAnySuffix(release, kernelImageSkipSuffixes) {
				continue
			}

			releases = append(releases, release)
		}
	}

	return releases, nil
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}

	return false
}

// sortKernelReleases merges release lists into one, de-duplicated and sorted
// oldest first by Debian version order.
func sortKernelReleases(lists ...[]string) []string {
	merged := make([]string, 0, 8)
	for _, list := range lists {
		merged = append(merged, list...)
	}

	slices.SortFunc(merged, func(a, b string) int {
		if c := CompareVersions(a, b); c != 0 {
			return c
		}

		return strings.Compare(a, b)
	})

	return slices.Compact(merged)
}

// newestKernel returns the newest of the installed releases. A system with
// no kernel of its own (a container) has none installed; its newest kernel
// is the running one, so that it never looks like it needs a reboot.
func newestKernel(installed []string, running string) string {
	if len(installed) == 0 {
		return running
	}

	return installed[len(installed)-1]
}
