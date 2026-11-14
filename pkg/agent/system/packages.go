package system

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// dpkgQueryFormat prints one tab-separated line per package.
	dpkgQueryFormat = "${Package}\t${Version}\t${Architecture}\t${Status}\n"

	// aptLockTimeout is how long apt waits for the dpkg locks, which the
	// system's own apt timers or a person may hold.
	aptLockTimeout = 5 * time.Minute
	// aptUpdateTimeout bounds one apt-get update inside a packages slot.
	aptUpdateTimeout = 10 * time.Minute
	// aptListsLockRetryInterval is the pause before apt-get update is tried
	// again after it found the package lists locked.
	aptListsLockRetryInterval = 5 * time.Second
	// aptInstallTimeout bounds apt-get install. Once started it is not
	// interrupted by the caller's context, see UpgradePackages.
	aptInstallTimeout = 30 * time.Minute

	policyNone = "(none)"
)

var (
	// packageNamePattern is the Debian policy package name, nothing more:
	// no architecture qualifier, no version or release selector, no leading
	// dash that apt would read as an option and no trailing dash that apt
	// would read as "remove".
	packageNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{0,126}[a-z0-9+.]$`)

	// simulatedInstPattern matches an "Inst" line of apt-get -s:
	//
	//	Inst name [current] (candidate Label:Version/Archive, ... [arch]) []
	//
	// Groups: name, current (empty for a new installation), candidate,
	// origins, arch.
	simulatedInstPattern = regexp.MustCompile(
		`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+)(?: (.*?))?(?:\[([^\]\s]+)\])?\)`)

	// ErrPackageNotAllowed is returned by UpgradePackages for a package that
	// is not in the configured blocked set or is not a plain package name.
	ErrPackageNotAllowed = errors.New("system: package not allowed")
)

// installedPackage is one line of the dpkg-query listing.
type installedPackage struct {
	protocol.Package

	// Held is true when the package's selection state is "hold".
	Held bool
}

// validPackageName reports whether name is a plain Debian package name.
func validPackageName(name string) bool {
	return packageNamePattern.MatchString(name)
}

// listInstalled returns the installed packages, sorted by name then arch.
func (s *System) listInstalled(ctx context.Context) ([]installedPackage, error) {
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

// parseDpkgQuery parses the output of dpkg-query -W -f dpkgQueryFormat. Only
// fully installed packages are kept: status "install ok installed", or
// "hold ok installed" for a package a person put on hold. It returns the
// number of lines it could not read.
func parseDpkgQuery(out []byte) (pkgs []installedPackage, malformed int) {
	pkgs = make([]installedPackage, 0, bytes.Count(out, []byte{'\n'}))

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

		pkgs = append(pkgs, installedPackage{
			Package: protocol.Package{Name: name, Version: version, Arch: arch},
			Held:    status[0] == "hold",
		})
	}

	slices.SortFunc(pkgs, func(a, b installedPackage) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}

		return strings.Compare(a.Arch, b.Arch)
	})

	return pkgs, malformed
}

// simulateUpgrade asks apt what a full upgrade would change, without taking
// locks or changing anything, and returns the packages that would move to a
// newer version.
func (s *System) simulateUpgrade(ctx context.Context) ([]protocol.Upgradable, error) {
	out, err := s.runner.Run(ctx, cmdAptGet, "-s", "-o", "Debug::NoLocking=true", "dist-upgrade")
	if err != nil {
		return nil, fmt.Errorf("simulate upgrade: %w", err)
	}

	return parseSimulatedUpgrade(out), nil
}

// parseSimulatedUpgrade reads the "Inst" lines of apt-get -s dist-upgrade.
// New installations (no current version) and lines it does not understand
// are skipped. Arch falls back to the name's qualifier ("libc6:i386").
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

// isSecurityOrigin reports whether an origin string names a security
// archive. It understands the "Label:Version/Archive" form of apt-get -s
// (several, comma separated) and the "suite/component" form of apt-cache
// policy: Debian's label is "Debian-Security"; Debian, Ubuntu and Ubuntu ESM
// security suites end in "-security".
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

// policyEntry is one package of apt-cache policy.
type policyEntry struct {
	Name      string
	Installed string
	Candidate string
	// Origin is the "suite/component" of every archive that carries the
	// candidate version, comma separated, in the order of the version table
	// and without repeats.
	Origin string
}

// blockedCandidates returns the candidate versions of the named packages
// from apt-cache policy.
func (s *System) blockedCandidates(ctx context.Context, names []string) ([]policyEntry, error) {
	out, err := s.runner.Run(ctx, cmdAptCache, append([]string{"policy"}, names...)...)
	if err != nil {
		return nil, fmt.Errorf("read apt policy: %w", err)
	}

	return parseAptPolicy(out), nil
}

// parseAptPolicy parses apt-cache policy <names...>:
//
//	name:
//	  Installed: 1.0
//	  Candidate: 2.0
//	  Version table:
//	     2.0 500
//	        500 http://host/debian suite/component amd64 Packages
//	 *** 1.0 100
//	        100 /var/lib/dpkg/status
func parseAptPolicy(out []byte) []policyEntry {
	var (
		entries []policyEntry
		cur     *policyEntry
		// inCandidate is true while reading the source lines below the
		// candidate's line in the version table.
		inCandidate bool
		// origins are the archives of the current entry's candidate.
		origins []string
	)

	// The same version can be published in several archives, for instance a
	// regular suite and its security suite: all of them count.
	flush := func() {
		if cur != nil {
			cur.Origin = strings.Join(origins, ",")
		}

		origins = origins[:0]
	}

	for line := range strings.SplitSeq(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if line[0] != ' ' && line[0] != '\t' {
			flush()

			cur, inCandidate = nil, false

			if header, ok := strings.CutSuffix(trimmed, ":"); ok {
				name, _, _ := strings.Cut(header, ":")
				entries = append(entries, policyEntry{Name: name})
				cur = &entries[len(entries)-1]
			}

			continue
		}

		if cur == nil {
			continue
		}

		if v, ok := strings.CutPrefix(trimmed, "Installed:"); ok {
			cur.Installed = policyVersion(v)

			continue
		}

		if v, ok := strings.CutPrefix(trimmed, "Candidate:"); ok {
			cur.Candidate = policyVersion(v)

			continue
		}

		fields := strings.Fields(strings.TrimPrefix(trimmed, "*** "))

		switch {
		case len(fields) == 2 && isPolicyPriority(fields[1]):
			// "<version> <priority>": a version of the table.
			inCandidate = cur.Candidate != "" && fields[0] == cur.Candidate
		case inCandidate && len(fields) >= 3 && isPolicyPriority(fields[0]):
			// "<priority> <uri> <suite/component> [<arch>] Packages"
			if !slices.Contains(origins, fields[2]) {
				origins = append(origins, fields[2])
			}
		}
	}

	flush()

	return entries
}

func policyVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == policyNone {
		return ""
	}

	return v
}

func isPolicyPriority(s string) bool {
	_, err := strconv.Atoi(s)

	return err == nil
}

// collectUpgradable builds the upgradable list: what a full upgrade would
// change, plus configured blocked packages that have a newer candidate but
// are left out of the full upgrade (kept back by the resolver). Packages on
// hold are never offered through that second path: a person put them on
// hold and apt would refuse to upgrade them.
func (s *System) collectUpgradable(
	ctx context.Context,
	installed []installedPackage,
) ([]protocol.Upgradable, error) {
	ups, err := s.simulateUpgrade(ctx)
	if err != nil {
		return nil, err
	}

	listed := make(map[string]struct{}, len(ups))
	for i := range ups {
		listed[ups[i].Name] = struct{}{}
	}

	byName := make(map[string]*installedPackage, len(s.blocked))

	missing := make([]string, 0, len(s.blocked))

	for i := range installed {
		pkg := &installed[i]

		if _, blocked := s.blocked[pkg.Name]; !blocked {
			continue
		}

		if _, seen := byName[pkg.Name]; seen {
			continue
		}

		byName[pkg.Name] = pkg

		if _, ok := listed[pkg.Name]; !ok && !pkg.Held {
			missing = append(missing, pkg.Name)
		}
	}

	if len(missing) > 0 {
		entries, policyErr := s.blockedCandidates(ctx, missing)
		if policyErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("read apt policy: %w", ctxErr)
			}

			// The simulation's list still stands.
			s.log.WithError(policyErr).Warn("Could not read the apt policy of blocked packages")
		}

		for i := range entries {
			entry := &entries[i]

			pkg, ok := byName[entry.Name]
			if !ok || !slices.Contains(missing, entry.Name) || entry.Candidate == "" {
				continue
			}

			if CompareVersions(entry.Candidate, pkg.Version) <= 0 {
				continue
			}

			ups = append(ups, protocol.Upgradable{
				Name:      pkg.Name,
				Arch:      pkg.Arch,
				Current:   pkg.Version,
				Candidate: entry.Candidate,
				Origin:    entry.Origin,
				Security:  isSecurityOrigin(entry.Origin),
			})
		}
	}

	for i := range ups {
		_, ups[i].Blocked = s.blocked[ups[i].Name]
	}

	return sortUpgradable(ups), nil
}

// sortUpgradable sorts by name then arch and drops repeated entries.
func sortUpgradable(ups []protocol.Upgradable) []protocol.Upgradable {
	sort.SliceStable(ups, func(i, j int) bool {
		if ups[i].Name != ups[j].Name {
			return ups[i].Name < ups[j].Name
		}

		return ups[i].Arch < ups[j].Arch
	})

	out := ups[:0]

	for i := range ups {
		if n := len(out); n > 0 && out[n-1].Name == ups[i].Name && out[n-1].Arch == ups[i].Arch {
			continue
		}

		out = append(out, ups[i])
	}

	return out
}

// UpgradePackages runs apt-get update and then
// apt-get install --only-upgrade for exactly the named packages.
//
// Every name must be a plain package name from the configured blocked set;
// otherwise nothing at all is executed and the error wraps
// ErrPackageNotAllowed. Names of packages that are not installed are left
// out, and without any installed one apt is not run at all: there is nothing
// to upgrade, and apt-get would take a name it does not know for a regular
// expression and select other packages with it. Existing configuration
// files are kept, apt waits for the package lists lock and the dpkg lock,
// and apt fails rather than remove a package. On failure the error carries
// the tail of apt's output.
//
// The context is honoured until apt-get install starts. The installation
// itself is not interrupted by a cancelled context, because a killed dpkg
// leaves packages half configured; it is bounded by its own timeout. The
// cached inventory is refreshed afterwards, also after a failure.
func (s *System) UpgradePackages(ctx context.Context, names []string) error {
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

// checkUpgradeNames validates the names given to UpgradePackages and returns
// them sorted and de-duplicated.
func (s *System) checkUpgradeNames(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no package named", ErrPackageNotAllowed)
	}

	checked := make([]string, 0, len(names))

	for _, name := range names {
		if !validPackageName(name) {
			return nil, fmt.Errorf("%w: %q is not a package name", ErrPackageNotAllowed, name)
		}

		if _, ok := s.blocked[name]; !ok {
			return nil, fmt.Errorf("%w: %q is not in the blocked set", ErrPackageNotAllowed, name)
		}

		checked = append(checked, name)
	}

	slices.Sort(checked)

	return slices.Compact(checked), nil
}

// installedOnly returns those of the names that dpkg has installed. apt-get
// install looks a name up exactly first, so an installed package is always
// selected by its own name. A name apt does not know at all would instead be
// tried as an unanchored regular expression over all package names
// ("containerd.io" also matches "containerd-io"), and other packages would
// be upgraded.
func (s *System) installedOnly(ctx context.Context, names []string) ([]string, error) {
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

// updateLists runs apt-get update. Unlike the dpkg lock, apt does not wait
// for the lock of the package lists, which apt-daily.service or a person
// running apt-get update holds for a while: it fails at once. That is
// retried for as long as apt would wait for the dpkg lock.
func (s *System) updateLists(ctx context.Context, lockTimeout string) error {
	deadline := s.now().Add(aptLockTimeout)

	for {
		updateCtx, cancel := context.WithTimeout(ctx, aptUpdateTimeout)
		_, err := s.runner.Run(updateCtx, cmdAptGet, "update", "-o", lockTimeout)

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

// isListsLockError reports whether apt-get update failed because another
// process holds the lock of the package lists (messages of LC_ALL=C):
//
//	E: Could not get lock /var/lib/apt/lists/lock. It is held by process 14 (apt-get)
//	E: Unable to lock directory /var/lib/apt/lists/
func isListsLockError(err error) bool {
	var cmdErr *CommandError
	if !errors.As(err, &cmdErr) {
		return false
	}

	return strings.Contains(cmdErr.Output, "E: Could not get lock ") ||
		strings.Contains(cmdErr.Output, "E: Unable to lock directory ")
}

// runUpgrade runs the two apt commands of a packages slot.
func (s *System) runUpgrade(ctx context.Context, names []string) error {
	lockTimeout := "DPkg::Lock::Timeout=" + strconv.Itoa(int(aptLockTimeout/time.Second))

	if err := s.updateLists(ctx, lockTimeout); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	args := make([]string, 0, 10+len(names))
	args = append(args,
		"install", "--only-upgrade", "-y", "--no-remove",
		"-o", lockTimeout,
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
