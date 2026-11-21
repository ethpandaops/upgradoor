package system

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// dpkgQueryFormat prints one tab-separated line per package.
	dpkgQueryFormat = "${Package}\t${Version}\t${Architecture}\t${Status}\n"

	// aptLockTimeout is how long apt waits for the dpkg locks.
	aptLockTimeout = 5 * time.Minute
	// aptUpdateTimeout bounds one apt-get update.
	aptUpdateTimeout = 10 * time.Minute
	// aptListsLockRetryInterval is the pause before retrying apt-get update on locked lists.
	aptListsLockRetryInterval = 5 * time.Second
	// aptInstallTimeout bounds apt-get install, which the caller's context does not interrupt.
	aptInstallTimeout = 30 * time.Minute
)

// dpkgLockOption makes apt wait for the dpkg locks.
var dpkgLockOption = "DPkg::Lock::Timeout=" + strconv.Itoa(int(aptLockTimeout/time.Second))

var (
	// packageNamePattern is a Debian policy package name without qualifier or
	// selector; no leading dash (an apt option) or trailing dash ("remove").
	packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,126}[a-z0-9+.]$`)

	// simulatedInstPattern matches an "Inst" line of apt-get -s:
	//
	//	Inst name [current] (candidate Label:Version/Archive, ... [arch]) []
	simulatedInstPattern = regexp.MustCompile(
		`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+)(?: (.*?))?(?:\[([^\]\s]+)\])?\)`)

	// errPackageNotAllowed rejects a name outside the blocked set or not a package name.
	errPackageNotAllowed = errors.New("system: package not allowed")
)

// validPackageName reports whether name is a plain Debian package name.
func validPackageName(name string) bool {
	return packageNamePattern.MatchString(name)
}

// listInstalled returns the installed packages, sorted by name then arch.
func (s *osSystem) listInstalled(ctx context.Context) ([]protocol.Package, error) {
	out, err := s.runner.Run(ctx, cmdDpkgQuery, "-W", "-f", dpkgQueryFormat)
	if err != nil {
		return nil, fmt.Errorf("list installed packages: %w", err)
	}

	pkgs, malformed := parseDpkgQuery(out)
	if malformed > 0 {
		s.log.WithField("lines", malformed).Warn("Ignored malformed dpkg-query lines")
	}

	if len(pkgs) == 0 {
		return nil, errors.New("list installed packages: dpkg-query listed no installed package")
	}

	return pkgs, nil
}

// parseDpkgQuery keeps the fully installed packages ("install ok installed"
// or "hold ok installed") and counts the lines it could not read.
func parseDpkgQuery(out []byte) (pkgs []protocol.Package, malformed int) {
	pkgs = make([]protocol.Package, 0, bytes.Count(out, []byte{'\n'}))

	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			malformed++

			continue
		}

		status := strings.Fields(fields[3])
		if len(status) != 3 {
			malformed++

			continue
		}

		if status[1] != "ok" || status[2] != "installed" {
			continue
		}

		if status[0] != "install" && status[0] != "hold" {
			continue
		}

		name, version, arch := fields[0], fields[1], fields[2]
		if name == "" || version == "" {
			malformed++

			continue
		}

		pkgs = append(pkgs, protocol.Package{Name: name, Version: version, Arch: arch})
	}

	slices.SortFunc(pkgs, func(a, b protocol.Package) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}

		return strings.Compare(a.Arch, b.Arch)
	})

	return pkgs, malformed
}

// simulateUpgrade asks apt, without locks or changes, which packages a full
// upgrade would move to a newer version.
func (s *osSystem) simulateUpgrade(ctx context.Context) ([]protocol.Upgradable, error) {
	out, err := s.runner.Run(ctx, cmdAptGet, "-s", "-o", "Debug::NoLocking=true", "dist-upgrade")
	if err != nil {
		return nil, fmt.Errorf("simulate upgrade: %w", err)
	}

	return parseSimulatedUpgrade(out), nil
}

// parseSimulatedUpgrade reads the "Inst" lines of apt-get -s dist-upgrade,
// skipping new installations and lines it does not understand.
func parseSimulatedUpgrade(out []byte) []protocol.Upgradable {
	var ups []protocol.Upgradable

	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		m := simulatedInstPattern.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}

		name, qualifier, _ := strings.Cut(m[1], ":")
		current := strings.Fields(m[2])

		if name == "" || len(current) == 0 {
			continue
		}

		arch := m[5]
		if arch == "" {
			arch = qualifier
		}

		origin := strings.TrimSpace(m[4])

		ups = append(ups, protocol.Upgradable{
			Name:      name,
			Arch:      arch,
			Current:   current[0],
			Candidate: m[3],
			Origin:    origin,
			Security:  isSecurityOrigin(origin),
		})
	}

	return ups
}

// isSecurityOrigin reports whether apt-get -s origins name a security archive:
// label "Debian-Security" or a suite ending in "-security".
func isSecurityOrigin(origin string) bool {
	for entry := range strings.SplitSeq(origin, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))

		label, archive, hasLabel := strings.Cut(entry, ":")
		if !hasLabel {
			label, archive = "", entry
		}

		if label == "debian-security" {
			return true
		}

		// Archive is "version/suite", "suite" or "suite/component".
		for part := range strings.SplitSeq(archive, "/") {
			if strings.HasSuffix(part, "-security") {
				return true
			}
		}
	}

	return false
}

// collectUpgradable returns what a full upgrade would change, sorted, with the
// blocked packages marked; held or kept-back packages are not listed.
func (s *osSystem) collectUpgradable(ctx context.Context) ([]protocol.Upgradable, error) {
	ups, err := s.simulateUpgrade(ctx)
	if err != nil {
		return nil, err
	}

	for i := range ups {
		_, ups[i].Blocked = s.blocked[ups[i].Name]
	}

	slices.SortStableFunc(ups, func(a, b protocol.Upgradable) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Arch, b.Arch))
	})

	return ups, nil
}

// UpgradePackages implements System: apt-get update, then apt-get install
// --only-upgrade --no-remove keeping configuration files. Names that are not
// installed are left out. The install is not interrupted by the context, as a
// killed dpkg leaves packages half configured; it has its own timeout.
func (s *osSystem) UpgradePackages(ctx context.Context, names []string) error {
	names, err := s.checkUpgradeNames(names)
	if err != nil {
		return err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("upgrade packages: %w", ctxErr)
	}

	s.upgradeMu.Lock()
	defer s.upgradeMu.Unlock()

	names, err = s.installedOnly(ctx, names)
	if err != nil {
		return fmt.Errorf("upgrade packages: %w", err)
	}

	if len(names) == 0 {
		s.log.Info("None of the named packages is installed, nothing to upgrade")

		return nil
	}

	log := s.log.WithField("packages", strings.Join(names, " "))
	log.Info("Upgrading packages")

	err = s.runUpgrade(ctx, names)

	// Refresh even when the caller is gone or apt failed: some packages may
	// have been upgraded.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), inventoryTimeout)
	defer cancel()

	if _, refreshErr := s.refresh(refreshCtx); refreshErr != nil {
		log.WithError(refreshErr).Warn("Could not refresh the inventory after the upgrade")
	}

	if err != nil {
		return fmt.Errorf("upgrade packages: %w", err)
	}

	log.Info("Packages upgraded")

	return nil
}

// checkUpgradeNames validates the names and returns them sorted and unique.
func (s *osSystem) checkUpgradeNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no package named", errPackageNotAllowed)
	}

	checked := make([]string, 0, len(names))

	for _, name := range names {
		if !validPackageName(name) {
			return nil, fmt.Errorf("%w: %q is not a package name", errPackageNotAllowed, name)
		}

		if _, ok := s.blocked[name]; !ok {
			return nil, fmt.Errorf("%w: %q is not in the blocked set", errPackageNotAllowed, name)
		}

		checked = append(checked, name)
	}

	slices.Sort(checked)

	return slices.Compact(checked), nil
}

// installedOnly returns the installed names. apt-get takes an unknown name
// as an unanchored regular expression ("containerd.io" matches
// "containerd-io") and would upgrade other packages.
func (s *osSystem) installedOnly(ctx context.Context, names []string) ([]string, error) {
	installed, err := s.listInstalled(ctx)
	if err != nil {
		return nil, err
	}

	known := make(map[string]struct{}, len(installed))
	for i := range installed {
		known[installed[i].Name] = struct{}{}
	}

	kept := make([]string, 0, len(names))

	for _, name := range names {
		if _, ok := known[name]; !ok {
			s.log.WithField("name", name).Warn("Not upgrading a package that is not installed")

			continue
		}

		kept = append(kept, name)
	}

	return kept, nil
}

// updateLists runs apt-get update, retrying for aptLockTimeout while the
// package lists are locked, since apt does not wait for that lock itself.
func (s *osSystem) updateLists(ctx context.Context) error {
	deadline := s.now().Add(aptLockTimeout)

	for {
		updateCtx, cancel := context.WithTimeout(ctx, aptUpdateTimeout)
		_, err := s.runner.Run(updateCtx, cmdAptGet, "update", "-o", dpkgLockOption)

		cancel()

		if err == nil {
			return nil
		}

		if ctx.Err() != nil || !isListsLockError(err) || !s.now().Before(deadline) {
			return err
		}

		s.log.WithField("retryIn", s.listsLockRetry.String()).
			Info("The apt package lists are locked by another process, retrying apt-get update")

		timer := time.NewTimer(s.listsLockRetry)

		select {
		case <-ctx.Done():
			timer.Stop()

			return fmt.Errorf("%w (%w)", ctx.Err(), err)
		case <-timer.C:
		}
	}
}

// isListsLockError reports whether apt-get update failed on the lists lock
// (messages of LC_ALL=C):
//
//	E: Could not get lock /var/lib/apt/lists/lock. It is held by process 14 (apt-get)
//	E: Unable to lock directory /var/lib/apt/lists/
func isListsLockError(err error) bool {
	var cmdErr *commandError
	if !errors.As(err, &cmdErr) {
		return false
	}

	return strings.Contains(cmdErr.Output, "E: Could not get lock ") ||
		strings.Contains(cmdErr.Output, "E: Unable to lock directory ")
}

// runUpgrade runs apt-get update and apt-get install.
func (s *osSystem) runUpgrade(ctx context.Context, names []string) error {
	if err := s.updateLists(ctx); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	args := make([]string, 0, 10+len(names))
	args = append(args,
		"install", "--only-upgrade", "-y", "--no-remove",
		"-o", dpkgLockOption,
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
	)
	args = append(args, names...)

	installCtx, cancelInstall := context.WithTimeout(context.WithoutCancel(ctx), aptInstallTimeout)
	defer cancelInstall()

	out, err := s.runner.Run(installCtx, cmdAptGet, args...)
	if err != nil {
		return err
	}

	s.log.WithFields(logrus.Fields{
		"packages": strings.Join(names, " "),
		"lastLine": lastLine(out),
	}).Debug("apt-get install finished")

	return nil
}

// lastLine returns the last non-empty line of a command's output.
func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}
