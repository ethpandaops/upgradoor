package system

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Paths below the filesystem root.
const (
	pathBootID         = "proc/sys/kernel/random/boot_id"
	pathUptime         = "proc/uptime"
	pathKernelRelease  = "proc/sys/kernel/osrelease"
	pathOSRelease      = "etc/os-release"
	pathOSReleaseLib   = "usr/lib/os-release"
	pathRebootRequired = "var/run/reboot-required"
	pathRebootPackages = "var/run/reboot-required.pkgs"
	pathBoot           = "boot"
)

// path resolves a path below the configured filesystem root.
func (s *System) path(rel string) string {
	return filepath.Join(s.root, rel)
}

// readBootID returns the kernel's boot id.
func (s *System) readBootID() (string, error) {
	raw, err := os.ReadFile(s.path(pathBootID))
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}

	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("read boot id: %s is empty", s.path(pathBootID))
	}

	return id, nil
}

// readUptime returns the time since boot from /proc/uptime.
func (s *System) readUptime() (time.Duration, error) {
	raw, err := os.ReadFile(s.path(pathUptime))
	if err != nil {
		return 0, fmt.Errorf("read uptime: %w", err)
	}

	return parseUptime(string(raw))
}

// parseUptime parses the content of /proc/uptime ("<seconds> <idle>").
func parseUptime(content string) (time.Duration, error) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return 0, errors.New("parse uptime: empty")
	}

	// "93784.52" is a decimal number of seconds; ParseDuration reads it
	// without a detour through floating point.
	uptime, err := time.ParseDuration(fields[0] + "s")
	if err != nil || uptime < 0 {
		return 0, fmt.Errorf("parse uptime: bad value %q", fields[0])
	}

	return uptime, nil
}

// readOSRelease reads /etc/os-release, falling back to /usr/lib/os-release.
// Arch is left empty.
func (s *System) readOSRelease() (protocol.OSInfo, error) {
	raw, err := os.ReadFile(s.path(pathOSRelease))
	if errors.Is(err, fs.ErrNotExist) {
		raw, err = os.ReadFile(s.path(pathOSReleaseLib))
	}

	if err != nil {
		return protocol.OSInfo{}, fmt.Errorf("read os-release: %w", err)
	}

	return parseOSRelease(raw), nil
}

// parseOSRelease parses os-release(5): KEY=value lines, values optionally
// quoted. Unknown keys and malformed lines are ignored.
func parseOSRelease(raw []byte) protocol.OSInfo {
	var info protocol.OSInfo

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		value = unquoteOSReleaseValue(strings.TrimSpace(value))

		switch strings.TrimSpace(key) {
		case "ID":
			info.ID = value
		case "VERSION_ID":
			info.VersionID = value
		case "VERSION_CODENAME":
			info.Codename = value
		case "PRETTY_NAME":
			info.PrettyName = value
		}
	}

	return info
}

// unquoteOSReleaseValue removes one level of shell quoting.
func unquoteOSReleaseValue(v string) string {
	if len(v) < 2 {
		return v
	}

	first, last := v[0], v[len(v)-1]

	switch {
	case first == '"' && last == '"':
		v = v[1 : len(v)-1]

		return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`").Replace(v)
	case first == '\'' && last == '\'':
		return v[1 : len(v)-1]
	default:
		return v
	}
}

// readRebootRequired reports whether /var/run/reboot-required exists and
// which packages asked for the reboot (reboot-required.pkgs, de-duplicated,
// in file order).
func (s *System) readRebootRequired() (required bool, pkgs []string, err error) {
	if _, err = os.Stat(s.path(pathRebootRequired)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil, nil
		}

		return false, nil, fmt.Errorf("stat reboot-required: %w", err)
	}

	raw, err := os.ReadFile(s.path(pathRebootPackages))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil, nil
		}

		return true, nil, fmt.Errorf("read reboot-required.pkgs: %w", err)
	}

	seen := make(map[string]struct{}, 8)

	for line := range strings.SplitSeq(string(raw), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}

		if _, dup := seen[name]; dup {
			continue
		}

		seen[name] = struct{}{}

		pkgs = append(pkgs, name)
	}

	return true, pkgs, nil
}

// runningKernel returns the running kernel release (uname -r).
func (s *System) runningKernel(ctx context.Context) (string, error) {
	if raw, err := os.ReadFile(s.path(pathKernelRelease)); err == nil {
		if release := strings.TrimSpace(string(raw)); release != "" {
			return release, nil
		}
	}

	out, err := s.runner.Run(ctx, cmdUname, "-r")
	if err != nil {
		return "", fmt.Errorf("running kernel: %w", err)
	}

	release := strings.TrimSpace(string(out))
	if release == "" {
		return "", errors.New("running kernel: uname -r printed nothing")
	}

	return release, nil
}

// architecture returns the dpkg architecture (amd64, arm64, ...), falling
// back to the machine name from uname. The value is cached once known.
func (s *System) architecture(ctx context.Context) (string, error) {
	s.mu.RLock()
	arch := s.arch
	s.mu.RUnlock()

	if arch != "" {
		return arch, nil
	}

	out, err := s.runner.Run(ctx, cmdDpkg, "--print-architecture")
	if err == nil {
		arch = strings.TrimSpace(string(out))
	}

	if arch == "" {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("architecture: %w", ctxErr)
		}

		machine, unameErr := s.runner.Run(ctx, cmdUname, "-m")
		if unameErr != nil {
			return "", fmt.Errorf("architecture: %w", errors.Join(err, unameErr))
		}

		arch = debianArch(strings.TrimSpace(string(machine)))
	}

	if arch == "" {
		return "", errors.New("architecture: dpkg and uname printed nothing")
	}

	s.mu.Lock()
	s.arch = arch
	s.mu.Unlock()

	return arch, nil
}

// debianArch maps a uname machine name to the Debian architecture name.
func debianArch(machine string) string {
	switch machine {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "armv7l":
		return "armhf"
	case "i686", "i586", "i386":
		return "i386"
	case "ppc64le":
		return "ppc64el"
	default:
		return machine
	}
}
