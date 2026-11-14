// Package system is the agent's view of the operating system: OS release,
// kernels, the reboot flag, the dpkg/apt package inventory, and the two
// things the agent may do to the OS under a lease: upgrade the configured
// blocked packages and reboot.
//
// It runs a closed set of programs (dpkg-query, dpkg, apt-get, apt-cache,
// uname and "systemctl reboot"), always with argument vectors and never
// through a shell. It never runs "apt-get update" outside UpgradePackages;
// refreshing the package lists is left to the system's apt timers.
package system

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	defaultInventoryInterval = 15 * time.Minute
	// inventoryTimeout bounds one collection of the inventory.
	inventoryTimeout = 5 * time.Minute
	// inventoryRetryInterval is the pause after a failed refresh during
	// which the previous inventory keeps being served.
	inventoryRetryInterval = time.Minute
)

// Options configures a System.
type Options struct {
	// Blocked are the packages UpgradePackages may upgrade. They are marked
	// in the inventory. Entries that are not plain package names are
	// dropped with a warning.
	Blocked []string
	// InventoryInterval is how long a collected inventory is served before
	// it is collected again (default 15m).
	InventoryInterval time.Duration
	// Root is the filesystem root for the files this package reads
	// (default "/"). Tests point it at a fake tree.
	Root string
	// Runner executes the programs. The default runs the real ones; tests
	// substitute canned outputs.
	Runner CommandRunner
}

// System implements agent.System on a Debian-like host.
type System struct {
	log      logrus.FieldLogger
	root     string
	runner   CommandRunner
	interval time.Duration
	blocked  map[string]struct{}
	now      func() time.Time
	// listsLockRetry is the pause between attempts of apt-get update while
	// the package lists are locked.
	listsLockRetry time.Duration

	// upgradeMu serialises UpgradePackages.
	upgradeMu sync.Mutex
	// refreshMu serialises inventory collection.
	refreshMu sync.Mutex

	// mu guards the fields below.
	mu          sync.RWMutex
	arch        string
	snap        *snapshot
	nextRefresh time.Time
}

var _ agent.System = (*System)(nil)

// snapshot is one collected inventory with what Status derives from it. It
// is immutable once stored.
type snapshot struct {
	inventory *protocol.Inventory
	pending   protocol.Pending
	blocked   []protocol.PackageChange
	// kernelPackages are the kernel releases of the installed linux-image
	// packages.
	kernelPackages []string
}

// New returns a System. It does no I/O; the first Status or Inventory call
// collects the inventory.
func New(log logrus.FieldLogger, opts Options) *System {
	log = log.WithField("package", "system")

	s := &System{
		log:      log,
		root:     opts.Root,
		runner:   opts.Runner,
		interval: opts.InventoryInterval,
		blocked:  make(map[string]struct{}, len(opts.Blocked)),
		now:      time.Now,

		listsLockRetry: aptListsLockRetryInterval,
	}

	if s.root == "" {
		s.root = "/"
	}

	if s.runner == nil {
		s.runner = execRunner{}
	}

	if s.interval <= 0 {
		s.interval = defaultInventoryInterval
	}

	for _, name := range opts.Blocked {
		if !validPackageName(name) {
			log.WithField("name", name).Warn("Ignoring blocked package: not a package name")

			continue
		}

		s.blocked[name] = struct{}{}
	}

	return s
}

// Status returns the cheap, frequently read part of the OS state. Package
// counts, blocked packages and the inventory hash come from the cached
// inventory, which is collected on the first call and again once it is older
// than the inventory interval; everything else is read on every call.
func (s *System) Status(ctx context.Context) (*agent.SystemStatus, error) {
	bootID, err := s.readBootID()
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	uptime, err := s.readUptime()
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	osInfo, err := s.readOSRelease()
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	arch, err := s.architecture(ctx)
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	running, err := s.runningKernel(ctx)
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	rebootRequired, rebootPackages, err := s.readRebootRequired()
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	snap, err := s.current(ctx)
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	bootKernels, err := s.bootKernelReleases()
	if err != nil {
		return nil, fmt.Errorf("system status: %w", err)
	}

	installed := sortKernelReleases(bootKernels, snap.kernelPackages)

	return &agent.SystemStatus{
		BootID: bootID,
		Uptime: uptime,
		OS: protocol.OSSummary{
			ID:             osInfo.ID,
			VersionID:      osInfo.VersionID,
			PrettyName:     osInfo.PrettyName,
			Arch:           arch,
			RunningKernel:  running,
			NewestKernel:   newestKernel(installed, running),
			RebootRequired: rebootRequired,
			RebootPackages: rebootPackages,
		},
		Pending:       snap.pending,
		Blocked:       slices.Clone(snap.blocked),
		InventoryHash: snap.inventory.Hash,
	}, nil
}

// Inventory returns the full package inventory with its hash set. The
// result is the caller's to keep; it is a copy of the cached inventory.
func (s *System) Inventory(ctx context.Context) (*protocol.Inventory, error) {
	snap, err := s.current(ctx)
	if err != nil {
		return nil, fmt.Errorf("system inventory: %w", err)
	}

	inv := *snap.inventory
	inv.Kernels.Installed = slices.Clone(inv.Kernels.Installed)
	inv.Packages = slices.Clone(inv.Packages)
	inv.Upgradable = slices.Clone(inv.Upgradable)

	return &inv, nil
}

// Reboot issues "systemctl reboot" and returns once systemd accepted it.
// The caller is expected to be killed by the shutdown soon after.
func (s *System) Reboot(ctx context.Context) error {
	s.log.Warn("Rebooting the host")

	_, err := s.runner.Run(ctx, cmdSystemctl, "reboot")
	if err == nil {
		return nil
	}

	// systemctl itself can be terminated by the shutdown it started.
	var cmdErr *CommandError
	if errors.As(err, &cmdErr) && cmdErr.Signal == syscall.SIGTERM && ctx.Err() == nil {
		return nil
	}

	return fmt.Errorf("reboot: %w", err)
}

// current returns the cached snapshot, collecting a new one when there is
// none or the cached one is due. While another caller is collecting, or
// after a failed collection, the previous snapshot is served.
func (s *System) current(ctx context.Context) (*snapshot, error) {
	s.mu.RLock()
	snap, due := s.snap, !s.now().Before(s.nextRefresh)
	s.mu.RUnlock()

	if snap != nil && !due {
		return snap, nil
	}

	if snap != nil {
		// Stale: refresh unless somebody else already is.
		if !s.refreshMu.TryLock() {
			return snap, nil
		}

		fresh, err := s.refreshLocked(ctx)
		s.refreshMu.Unlock()

		if err != nil {
			s.log.WithError(err).Warn("Could not refresh the inventory, serving the previous one")

			return snap, nil
		}

		return fresh, nil
	}

	// Nothing cached yet: wait for a collection.
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	s.mu.RLock()
	snap = s.snap
	s.mu.RUnlock()

	if snap != nil {
		return snap, nil
	}

	return s.refreshLocked(ctx)
}

// refresh collects the inventory now, whatever the age of the cached one.
func (s *System) refresh(ctx context.Context) (*snapshot, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	return s.refreshLocked(ctx)
}

// refreshLocked collects and stores a snapshot. The caller holds refreshMu.
func (s *System) refreshLocked(ctx context.Context) (*snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()

	snap, err := s.collect(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		s.nextRefresh = s.now().Add(min(s.interval, inventoryRetryInterval))

		return nil, err
	}

	if s.snap == nil || s.snap.inventory.Hash != snap.inventory.Hash {
		s.log.WithFields(logrus.Fields{
			"hash":       snap.inventory.Hash,
			"packages":   len(snap.inventory.Packages),
			"upgradable": snap.pending.Upgradable,
			"security":   snap.pending.Security,
			"blocked":    snap.pending.Blocked,
		}).Info("Package inventory changed")
	}

	s.snap = snap
	s.nextRefresh = s.now().Add(s.interval)

	return snap, nil
}

// collect reads the whole inventory from the system.
//
// The installed list is essential and its failure fails the collection. The
// upgradable list is best effort: when apt cannot compute it (broken
// dependencies, damaged lists) the inventory is still produced, with an
// empty upgradable list, so that the host stays visible; no package need can
// arise from such an inventory.
func (s *System) collect(ctx context.Context) (*snapshot, error) {
	osInfo, err := s.readOSRelease()
	if err != nil {
		return nil, err
	}

	if osInfo.Arch, err = s.architecture(ctx); err != nil {
		return nil, err
	}

	running, err := s.runningKernel(ctx)
	if err != nil {
		return nil, err
	}

	installed, err := s.listInstalled(ctx)
	if err != nil {
		return nil, err
	}

	upgradable, err := s.collectUpgradable(ctx, installed)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("collect inventory: %w", ctxErr)
		}

		s.log.WithError(err).Warn("Could not compute the upgradable packages, reporting none")

		upgradable = nil
	}

	bootKernels, err := s.bootKernelReleases()
	if err != nil {
		return nil, err
	}

	snap := &snapshot{kernelPackages: kernelReleasesFromPackages(installed)}
	kernels := sortKernelReleases(bootKernels, snap.kernelPackages)

	inv := &protocol.Inventory{
		CollectedAt: s.now().UTC(),
		OS:          osInfo,
		Kernels: protocol.KernelInfo{
			Running:   running,
			Newest:    newestKernel(kernels, running),
			Installed: kernels,
		},
		Packages:   make([]protocol.Package, 0, len(installed)),
		Upgradable: make([]protocol.Upgradable, 0, len(upgradable)),
	}

	for i := range installed {
		inv.Packages = append(inv.Packages, installed[i].Package)
	}

	for i := range upgradable {
		up := &upgradable[i]
		inv.Upgradable = append(inv.Upgradable, *up)

		snap.pending.Upgradable++

		if up.Security {
			snap.pending.Security++
		}

		if !up.Blocked {
			continue
		}

		snap.pending.Blocked++

		// One change per package name, whatever its architectures.
		if n := len(snap.blocked); n > 0 && snap.blocked[n-1].Name == up.Name {
			continue
		}

		snap.blocked = append(snap.blocked, protocol.PackageChange{
			Name: up.Name,
			From: up.Current,
			To:   up.Candidate,
		})
	}

	if inv.Hash, err = inv.ComputeHash(); err != nil {
		return nil, fmt.Errorf("collect inventory: %w", err)
	}

	snap.inventory = inv

	return snap, nil
}
