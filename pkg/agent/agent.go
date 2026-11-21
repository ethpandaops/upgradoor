// Package agent is the per-host upgradoor agent: it observes the host, syncs
// with the controller and serves leases with a closed set of verbs.
//
// The agent opens no port. It can do exactly three things, each only under a
// lease and only when the local configuration allows the verb and no hold is
// in effect:
//
//  1. roll: recreate an existing managed container (it never creates one) on
//     the digest its own tag points at, or with the image and command line
//     agent.yaml and the overrides file say, after verifying that the pulled digest equals the
//     digest in the lease;
//  2. packages: apt-get install --only-upgrade for the configured blocked
//     packages;
//  3. reboot: stop VC, CL, EL in that order, then systemctl reboot.
//
// # Operation order
//
// One lease serves all granted needs in one downtime:
//
//	draining   only with a reboot need: stop managed containers, VC then CL
//	           then EL (then other), each with its stop timeout
//	applying   upgrade packages (if granted), then for every image need
//	           recreate the container, EL then CL then VC then other; with a
//	           reboot need the new containers are created but left stopped
//	rebooting  only with a reboot need: persist the phase, then reboot. After
//	           the boot the agent finds the phase in its state file, checks
//	           that the boot id changed and continues with starting
//	starting   make sure every managed container that should be running is
//	           running again, EL then CL then VC then other. That is every
//	           container that ran before the operation and every one that had
//	           stopped by itself with an error (protocol.Container.ShouldRun):
//	           a newer image repairs a container that crashed on a broken one.
//	           A container somebody stopped stays down, also when it is rolled
//	verifying  every touched container stays running without a restart for
//	           verify.window; bounded by verify.timeout
//	done / failed
//
// A step whose need no longer applies is "superseded", one that local policy
// forbids is "refused"; neither disrupts anything nor fails the operation.
// If any step fails the agent still runs starting so the host comes back as
// far as it can, then reports failed. The agent never rolls back.
//
// # Layout
//
// This package wires the parts and runs the sync loop (service.go). The
// parts depend on each other only downwards, never on this package:
//
//	config      agent.yaml
//	controller  signed requests, verified answers, the pace of the syncs
//	needs       what the host needs, from one observation
//	imagewatch  registry lookups, relayed digests, pull windows
//	journal     the state file, state.json
//	overrides   the overrides file with its holds
//	directives  the controller's directives, each applied once
//	operation   the execution of a lease, by phase
//	local       the host socket of the 'upgradoor host' commands
package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/controller"
	"github.com/ethpandaops/upgradoor/pkg/agent/directives"
	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/imagewatch"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Service is the agent's main loop.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
}

// Deps are the host-facing parts of the agent.
type Deps struct {
	// Runtime, Registry and System are required.
	Runtime  hostapi.ContainerRuntime
	Registry hostapi.Registry
	System   hostapi.System
	// Prober checks the local nodes; nil reports no node health.
	Prober hostapi.Prober
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Version is the agent version reported to the controller (default
	// "dev").
	Version string
}

// service is the agent: a sync loop, an image watcher and, while a lease is
// open, one operation.
type service struct {
	log     logrus.FieldLogger
	cfg     *config.Config
	deps    Deps
	now     func() time.Time
	version string

	// host and network are the names of the certificate.
	host    string
	network string

	client controller.Client
	// directives applies the controller's directives; it belongs to the
	// loop.
	directives directives.Handler
	watcher    imagewatch.Watcher
	// audit is the host's audit log.
	audit auditlog.Log
	// overrides is the host's overrides file with its holds.
	overrides overrides.Keeper
	// local is the agent's side of the host socket; it is built by Start.
	local local.Host
	// store is opened by Start.
	store journal.Store

	mu      sync.Mutex
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// kick asks the loop for a sync now (an operation changed phase).
	kick chan struct{}
	// inventoryTimeout bounds one hostapi.System.Inventory call.
	inventoryTimeout time.Duration
	// opActive is true while the operation goroutine runs.
	opActive atomic.Bool

	// The fields below belong to the loop goroutine.

	// lastContainers and lastSys are the last good observations.
	lastContainers []protocol.Container
	lastSys        *hostapi.SystemStatus
	// lastQueue and ignoredLease only keep the log quiet.
	lastQueue    protocol.Queue
	ignoredLease string
	syncFailing  bool
	// lastAnswer is when the controller last answered; lookupFallback how
	// long the agent goes without an answer before it looks its tags up on
	// its own schedule again.
	lastAnswer     time.Time
	lookupFallback time.Duration
	// answerRefused is true while the controller's answers fail
	// verification; it keeps that to one log line per episode.
	answerRefused bool
	// lastValidators is the validator range last reported, to log changes.
	lastValidators *protocol.Validators
}

var _ Service = (*service)(nil)

// New returns the agent. Start opens the state directory and begins to
// observe and sync.
//
//nolint:gocritic // hugeParam: Deps is passed by value once, at construction
func New(
	log logrus.FieldLogger, cfg *config.Config, creds *identity.Credentials, deps Deps,
) (Service, error) {
	switch {
	case log == nil:
		return nil, errors.New("agent: no logger")
	case cfg == nil:
		return nil, errors.New("agent: no configuration")
	case creds == nil:
		return nil, errors.New("agent: no credentials")
	case deps.Runtime == nil || deps.Registry == nil || deps.System == nil:
		return nil, errors.New("agent: container runtime, registry and system are required")
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("agent: configuration: %w", err)
	}

	if cfg.StateDir == "" || cfg.Controller.SyncInterval <= 0 ||
		cfg.Controller.LeaseSyncInterval <= 0 || cfg.Controller.MinSyncInterval <= 0 ||
		cfg.Controller.MaxSyncInterval < cfg.Controller.MinSyncInterval || cfg.Registry.PollInterval <= 0 ||
		cfg.Verify.Window <= 0 || cfg.Verify.Timeout <= 0 {
		return nil, errors.New("agent: configuration without defaults (call ApplyDefaults)")
	}

	if deps.Now == nil {
		deps.Now = time.Now
	}

	if deps.Version == "" {
		deps.Version = "dev"
	}

	log = log.WithField("package", "agent")

	claims, err := identity.UnverifiedClaims(creds.Certificate())
	if err != nil {
		return nil, fmt.Errorf("agent: host certificate: %w", err)
	}

	log = log.WithFields(logrus.Fields{"host": claims.Subject, "network": claims.Network})

	client, err := controller.New(log, &cfg.Controller, creds, claims, deps.Now)
	if err != nil {
		return nil, err
	}

	s := &service{
		log:     log,
		cfg:     cfg,
		deps:    deps,
		now:     deps.Now,
		version: deps.Version,
		host:    claims.Subject,
		network: claims.Network,
		client:  client,
		audit:   auditlog.New(log, cfg.AuditLog, deps.Now),
		// Without the roll verb the tags are still looked up, for the report,
		// but nothing is pulled: this host never rolls, and a pulled image
		// would be picked up by whoever recreates the container next.
		watcher: imagewatch.New(
			log, deps.Runtime, deps.Registry, cfg.Registry.PollInterval, deps.Now,
			cfg.Allows(protocol.VerbRoll)),
		kick:             make(chan struct{}, 1),
		inventoryTimeout: inventoryTimeout,
		lookupFallback:   3 * cfg.Registry.PollInterval,
	}
	// What the overrides of a container are checked against is what the last
	// cycle saw managed, as the host socket shows it.
	s.overrides = overrides.NewKeeper(log, cfg, s.audit, deps.Now,
		func(name string) (string, bool) { return s.local.ManagedGroup(name) }, s.requestSync)

	return s, nil
}

// Start opens the state file and starts the sync loop and the image
// watcher. An operation found in the state file is continued. The loop and
// the watcher stop when ctx ends or Stop is called; an operation first
// finishes its disruptive steps (see Stop, which waits for it).
func (s *service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("agent: already started")
	}

	store, err := journal.Open(s.log, s.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	s.store = store
	s.local = local.NewHost(&local.HostDeps{
		Log: s.log, Config: s.cfg, Overrides: s.overrides, Store: store, Audit: s.audit, Now: s.now,
		Host: s.host, Network: s.network, Version: s.version, Changed: s.requestSync,
	})
	s.directives = directives.New(&directives.Deps{
		Log: s.log, Config: s.cfg, Store: store, Overrides: s.overrides, Watcher: s.watcher,
		Audit: s.audit, ConcludeLocalRoll: s.local.ConcludeRoll,
	})
	s.started = true

	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	s.log.WithFields(logrus.Fields{
		"controller": s.cfg.Controller.URL,
		"verbs":      s.cfg.Verbs,
		"groups":     len(s.cfg.Groups),
		"version":    s.version,
	}).Info("agent started")

	if err := s.local.Start(runCtx); err == nil {
		s.wg.Go(func() {
			<-runCtx.Done()

			_ = s.local.Stop()
		})
	}

	s.wg.Go(func() { s.watcher.Run(runCtx) })
	s.wg.Go(func() { s.loop(runCtx) })

	return nil
}

// Stop ends the loop and the watcher and waits for them and for a running
// operation.
//
// An operation is not cut short in its draining and applying phases: a
// recreate or a package upgrade in flight, the steps after it and the reboot
// are carried through first, each bounded by its own timeout, and Stop
// returns only then. That can take as long as the stop timeouts of the
// containers plus a package upgrade, so the unit's TimeoutStopSec should
// allow for it; a process killed before that resumes the operation as
// failed. An operation that is waiting (for the reboot, for containers to
// start or to stay up) ends at once, stays in the state file and is
// continued by the next Start.
func (s *service) Stop() error {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel == nil {
		return nil
	}

	cancel()

	if s.opActive.Load() {
		s.log.Info("stopping: waiting for the open operation to finish its disruptive steps")
	}

	s.wg.Wait()

	s.log.Info("agent stopped")

	return nil
}
