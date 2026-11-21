package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/lease"
	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/agent/registry"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Service is the per-host agent.
type Service interface {
	// Start opens the state file and starts the lease runner (which resumes
	// an operation of the state file), the tracker, the socket and the loop,
	// until ctx ends or Stop. A socket that cannot be opened is logged only.
	Start(ctx context.Context) error
	// Stop ends the loop, the tracker and the socket and waits for them and
	// for the runner's draining, applying and issuing of a reboot.
	Stop() error
}

// Deps are the parts of the agent that touch the outside world.
type Deps struct {
	// Host is the machine. Required.
	Host host.Host
	// Logins are the registry logins of cfg.Registry.AuthFile.
	Logins registry.Logins
	// Resolver looks tags up; nil uses registry.NewResolver with Logins.
	Resolver registry.Resolver
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Version is the reported agent version (default "dev").
	Version string
}

// agent is the Service.
type agent struct {
	log     logrus.FieldLogger
	cfg     *Config
	now     func() time.Time
	version string
	// hostName and network are the certificate's.
	hostName string
	network  string

	host     host.Host
	resolver registry.Resolver
	link     link.Client
	tracker  registry.Tracker
	files    local.Files
	audit    local.Audit
	rolls    local.Rolls
	server   local.Server
	// state and runner are set by Start.
	state  lease.State
	runner lease.Runner

	// kick asks the loop for a sync now.
	kick chan struct{}
	// inventoryTimeout bounds the collection of the inventory.
	inventoryTimeout time.Duration

	// mu guards what the socket reads of the last cycle.
	mu     sync.Mutex
	view   *plan.View
	status local.Status

	// Owned by the loop goroutine.
	directives     directives
	facts          plan.Facts
	lastValidators *protocol.Validators
	queue          protocol.Queue
	syncFailing    bool

	lifeMu   sync.Mutex
	started  bool
	cancel   context.CancelFunc
	loopDone chan struct{}
}

var _ Service = (*agent)(nil)

// New builds the agent from a configuration with the defaults applied and
// valid; Start opens the state file.
func New(log logrus.FieldLogger, cfg *Config, creds *identity.Credentials, deps *Deps) (Service, error) {
	switch {
	case log == nil, cfg == nil, creds == nil, deps == nil:
		return nil, errors.New("agent: logger, configuration, credentials and deps are required")
	case deps.Host == nil:
		return nil, errors.New("agent: no host")
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("agent: configuration: %w", err)
	}

	if !cfg.defaulted() {
		return nil, errors.New("agent: configuration without defaults (call ApplyDefaults)")
	}

	now, version := deps.Now, deps.Version
	if now == nil {
		now = time.Now
	}

	if version == "" {
		version = "dev"
	}

	claims, err := identity.UnverifiedClaims(creds.Certificate())
	if err != nil {
		return nil, fmt.Errorf("agent: host certificate: %w", err)
	}

	log = log.WithFields(logrus.Fields{
		"package": "agent", "host": claims.Subject, "network": claims.Network,
	})

	client, err := link.New(log, &cfg.Controller, creds, now)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}

	resolver := deps.Resolver
	if resolver == nil {
		if resolver, err = registry.NewResolver(log, &cfg.Registry, deps.Logins); err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
	}

	audit := local.NewAudit(log, &cfg.Local.AuditLog, now)

	a := &agent{
		log: log, cfg: cfg, now: now, version: version,
		hostName: claims.Subject, network: claims.Network,
		host: deps.Host, resolver: resolver, link: client,
		// Without the roll verb nothing is pulled.
		tracker: registry.NewTracker(log, deps.Host, resolver, cfg.Registry.PollInterval,
			cfg.Policy.Allows(protocol.VerbRoll), now),
		files:            local.NewFiles(log, cfg.Local.OverridesFile, audit, now),
		audit:            audit,
		rolls:            local.NewRolls(now),
		kick:             make(chan struct{}, 1),
		inventoryTimeout: inventoryTimeout,
		directives:       directives{pending: make(map[string]pendingRoll, 4)},
	}
	a.server = local.NewServer(log, &cfg.Local, localAPI{a})

	return a, nil
}

// Start implements Service. An agent starts once. When the runner or the
// tracker cannot start, what was started is stopped again.
func (a *agent) Start(ctx context.Context) error {
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()

	if a.started {
		return errors.New("agent: already started")
	}

	if err := a.open(); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)

	if err := a.runner.Start(runCtx); err != nil {
		cancel()

		return fmt.Errorf("agent: %w", err)
	}

	if err := a.tracker.Start(runCtx); err != nil {
		cancel()

		return errors.Join(fmt.Errorf("agent: %w", err), a.runner.Stop())
	}

	a.log.WithFields(logrus.Fields{
		"controller": a.cfg.Controller.URL, "verbs": a.cfg.Policy.Verbs,
		"groups": len(a.cfg.Policy.Groups), "version": a.version,
	}).Info("agent started")

	if err := a.server.Start(runCtx); err != nil {
		a.log.WithError(err).Error("cannot open the local socket; the on-host commands " +
			"do not work on this host")
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		a.loop(runCtx)
	}()

	a.cancel, a.loopDone, a.started = cancel, done, true

	return nil
}

// open opens the state file and builds the runner on it.
func (a *agent) open() error {
	state, err := lease.OpenState(a.log, a.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	a.state = state
	a.runner = lease.New(a.log, &a.cfg.Verify, state, &lease.Deps{
		Host: a.host, Policy: &a.cfg.Policy, Overrides: a.files, Registry: a.resolver,
		Audit: a.audit, Notify: a.wake, Now: a.now,
	})

	return nil
}

// Stop implements Service. It is a no-op on an agent that is not started.
func (a *agent) Stop() error {
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()

	if a.cancel == nil {
		return nil
	}

	a.cancel()
	<-a.loopDone

	a.cancel, a.loopDone = nil, nil

	serverErr, trackerErr := a.server.Stop(), a.tracker.Stop()

	if cur := a.runner.Current(); cur != nil && !cur.Operation.Phase.Terminal() {
		a.log.Info("stopping: waiting for the open operation to finish its disruptive steps")
	}

	runnerErr := a.runner.Stop()

	a.log.Info("agent stopped")

	return errors.Join(serverErr, trackerErr, runnerErr)
}

// wake asks the loop for a sync now.
func (a *agent) wake() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}
