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
func (s *osSystem) path(rel string) string {
	return filepath.Join(s.root, rel)
}

// readBootID returns the kernel's boot id.
func (s *osSystem) readBootID() (string, error) {
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
func (s *osSystem) readUptime() (time.Duration, error) {
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

	// ParseDuration reads the decimal seconds without floating point.
	uptime, err := time.ParseDuration(fields[0] + "s")
	if err != nil || uptime < 0 {
		return 0, fmt.Errorf("parse uptime: bad value %q", fields[0])
	}

	return uptime, nil
}

// readOSRelease reads /etc/os-release, falling back to /usr/lib/os-release.
// Arch is left empty.
func (s *osSystem) readOSRelease() (protocol.OSInfo, error) {
	raw, err := os.ReadFile(s.path(pathOSRelease))
	if errors.Is(err, fs.ErrNotExist) {
		raw, err = os.ReadFile(s.path(pathOSReleaseLib))
	}

	if err != nil {
		return protocol.OSInfo{}, fmt.Errorf("read os-release: %w", err)
	}

	return parseOSRelease(raw), nil
}

// parseOSRelease parses os-release(5); unknown keys and malformed lines are ignored.
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

// readRebootRequired reports whether a reboot is required and which packages
// asked for it, de-duplicated in file order.
func (s *osSystem) readRebootRequired() (required bool, pkgs []string, err error) {
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

// runningKernel returns the running kernel release (what uname -r prints).
func (s *osSystem) runningKernel() (string, error) {
	raw, err := os.ReadFile(s.path(pathKernelRelease))
	if err != nil {
		return "", fmt.Errorf("running kernel: %w", err)
	}

	release := strings.TrimSpace(string(raw))
	if release == "" {
		return "", fmt.Errorf("running kernel: %s is empty", s.path(pathKernelRelease))
	}

	return release, nil
}

// architecture returns the dpkg architecture, cached once known.
func (s *osSystem) architecture(ctx context.Context) (string, error) {
	s.mu.RLock()
	arch := s.arch
	s.mu.RUnlock()

	if arch != "" {
		return arch, nil
	}

	out, err := s.runner.Run(ctx, cmdDpkg, "--print-architecture")
	if err != nil {
		return "", fmt.Errorf("architecture: %w", err)
	}

	arch = strings.TrimSpace(string(out))
	if arch == "" {
		return "", errors.New("architecture: dpkg printed nothing")
	}

	s.mu.Lock()
	s.arch = arch
	s.mu.Unlock()

	return arch, nil
}
