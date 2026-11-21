// Package system reads the operating system (OS release, boot id, uptime,
// kernels, the reboot flag, the dpkg/apt package inventory), upgrades
// packages and reboots. It decides nothing and never runs "apt-get update"
// outside UpgradePackages.
//
// Only the closed set of programs runs (dpkg-query, dpkg, apt-get,
// systemctl), by absolute path, with a fixed environment, no shell and
// bounded output; only packages of the configured blocked set are upgraded.
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

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// System is the operating system of the host.
type System interface {
	// Status returns the cheap, frequently read part of the OS state; the
	// inventory parts are cached.
	Status(ctx context.Context) (Status, error)
	// Inventory returns the full package inventory with its hash set.
	Inventory(ctx context.Context) (*protocol.Inventory, error)
	// UpgradePackages upgrades exactly the named packages; a name outside
	// the blocked set is an error before anything runs.
	UpgradePackages(ctx context.Context, names []string) error
	// Reboot issues systemctl reboot and returns once it is accepted.
	Reboot(ctx context.Context) error
}

// Status is the cheap, frequently read part of the OS state.
type Status struct {
	BootID  string
	Uptime  time.Duration
	OS      protocol.OSSummary
	Pending protocol.Pending
	// Blocked are the pending upgrades of configured blocked packages.
	Blocked []protocol.PackageChange
	// InventoryHash is the hash of the inventory Inventory would return now.
	InventoryHash string
}

// New returns the operating system of this host. blocked are the packages
// UpgradePackages may upgrade; an invalid name is dropped with a warning.
func New(log logrus.FieldLogger, blocked []string) System {
	return newOSSystem(log, blocked, "/", execRunner{})
}

var _ System = (*osSystem)(nil)

const (
	// inventoryInterval is how long a collected inventory is served.
	inventoryInterval = 15 * time.Minute
	// inventoryTimeout bounds one collection of the inventory.
	inventoryTimeout = 5 * time.Minute
	// inventoryRetryInterval is the pause after a failed refresh; the
	// previous inventory is served meanwhile.
	inventoryRetryInterval = time.Minute
)

// osSystem is the System of a Debian-like host. Refreshing the package lists
// outside UpgradePackages is left to the system's apt timers.
type osSystem struct {
	log     logrus.FieldLogger
	root    string
	runner  commandRunner
	blocked map[string]struct{}
	now     func() time.Time
	// listsLockRetry is the pause between apt-get update attempts while the lists are locked.
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

// snapshot is one collected inventory with what Status derives from it;
// immutable once stored.
type snapshot struct {
	inventory *protocol.Inventory
	pending   protocol.Pending
	blocked   []protocol.PackageChange
	// kernelPackages are the releases of the installed linux-image packages.
	kernelPackages []string
}

// newOSSystem returns the system whose files are below root and whose
// programs runner runs. It does no I/O.
func newOSSystem(
	log logrus.FieldLogger, blocked []string, root string, runner commandRunner,
) *osSystem {
	log = log.WithField("package", "system")

	s := &osSystem{
		log:     log,
		root:    root,
		runner:  runner,
		blocked: make(map[string]struct{}, len(blocked)),
		now:     time.Now,

		listsLockRetry: aptListsLockRetryInterval,
	}

	for _, name := range blocked {
		if !validPackageName(name) {
			log.WithField("name", name).Warn("Ignoring blocked package: not a package name")

			continue
		}

		s.blocked[name] = struct{}{}
	}

	return s
}

// Status implements System. Package counts, blocked packages and the hash
// come from the cached inventory; everything else is read on every call.
func (s *osSystem) Status(ctx context.Context) (Status, error) {
	status, err := s.status(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("system status: %w", err)
	}

	return status, nil
}

func (s *osSystem) status(ctx context.Context) (Status, error) {
	bootID, err := s.readBootID()
	if err != nil {
		return Status{}, err
	}

	uptime, err := s.readUptime()
	if err != nil {
		return Status{}, err
	}

	osInfo, err := s.readOSRelease()
	if err != nil {
		return Status{}, err
	}

	arch, err := s.architecture(ctx)
	if err != nil {
		return Status{}, err
	}

	running, err := s.runningKernel()
	if err != nil {
		return Status{}, err
	}

	rebootRequired, rebootPackages, err := s.readRebootRequired()
	if err != nil {
		return Status{}, err
	}

	snap, err := s.current(ctx)
	if err != nil {
		return Status{}, err
	}

	bootKernels, err := s.bootKernelReleases()
	if err != nil {
		return Status{}, err
	}

	installed := sortKernelReleases(bootKernels, snap.kernelPackages)

	return Status{
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

// Inventory implements System; it returns a copy of the cached inventory.
func (s *osSystem) Inventory(ctx context.Context) (*protocol.Inventory, error) {
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

// Reboot implements System.
func (s *osSystem) Reboot(ctx context.Context) error {
	s.log.Warn("Rebooting the host")

	_, err := s.runner.Run(ctx, cmdSystemctl, "reboot")
	if err == nil {
		return nil
	}

	// systemctl itself can be terminated by the shutdown it started.
	var cmdErr *commandError
	if errors.As(err, &cmdErr) && cmdErr.Signal == syscall.SIGTERM && ctx.Err() == nil {
		return nil
	}

	return fmt.Errorf("reboot: %w", err)
}

// current returns the cached snapshot, collecting one when there is none or
// it is due. While another caller collects, or after a failed collection,
// the previous snapshot is served.
func (s *osSystem) current(ctx context.Context) (*snapshot, error) {
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
func (s *osSystem) refresh(ctx context.Context) (*snapshot, error) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	return s.refreshLocked(ctx)
}

// refreshLocked collects and stores a snapshot. The caller holds refreshMu.
func (s *osSystem) refreshLocked(ctx context.Context) (*snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, inventoryTimeout)
	defer cancel()

	snap, err := s.collect(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err != nil {
		s.nextRefresh = s.now().Add(inventoryRetryInterval)

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
	s.nextRefresh = s.now().Add(inventoryInterval)

	return snap, nil
}

// collect reads the whole inventory. A failed installed list fails it; a
// failed upgradable list (broken dependencies, damaged lists) is reported as
// empty so the host stays visible.
func (s *osSystem) collect(ctx context.Context) (*snapshot, error) {
	osInfo, err := s.readOSRelease()
	if err != nil {
		return nil, err
	}

	if osInfo.Arch, err = s.architecture(ctx); err != nil {
		return nil, err
	}

	running, err := s.runningKernel()
	if err != nil {
		return nil, err
	}

	installed, err := s.listInstalled(ctx)
	if err != nil {
		return nil, err
	}

	upgradable, err := s.collectUpgradable(ctx)
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
		Packages:   installed,
		Upgradable: make([]protocol.Upgradable, 0, len(upgradable)),
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
