package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
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

	host    host.Host
	link    link.Client
	tracker registry.Tracker
	files   local.Files
	audit   local.Audit
	rolls   local.Rolls
	server  local.Server
	// state and runner are set by Start.
	state  lease.State
	runner lease.Runner

	// kick asks the loop for a sync now.
	kick chan struct{}
	// inventoryTimeout bounds the collection of the inventory.
	inventoryTimeout time.Duration

	// managedMu guards managed.yaml as last read; managedErr says why it
	// cannot be read.
	managedMu  sync.Mutex
	managed    map[string]plan.Deployed
	managedErr string

	// ranMu guards actionsRan: the inventory hash after the last run of each
	// action, by name. Kept in memory: after a restart a due action may run
	// once more.
	ranMu      sync.Mutex
	actionsRan map[string]string

	// mu guards what the socket reads of the last cycle.
	mu     sync.Mutex
	view   *plan.View
	status local.Status
	// seen are the facts of view; Status classifies them again.
	seen *plan.Facts

	// Owned by the loop goroutine.
	directives directives
	// tags are the followed tags as last reported.
	tags        []string
	facts       plan.Facts
	queue       protocol.Queue
	syncFailing bool
	// lastAnswer is when the controller last answered; the start before that.
	lastAnswer time.Time
	// fallbackTried are the needs the fallback has given a lease during this
	// silence; nil while the fallback is off.
	fallbackTried map[string]struct{}

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

	audit := local.NewAudit(log, &cfg.Local.AuditLog, now)

	a := &agent{
		log: log, cfg: cfg, now: now, version: version,
		hostName: claims.Subject, network: claims.Network,
		host: deps.Host, link: client,
		// Without the roll verb nothing is pulled.
		tracker:          registry.NewTracker(log, deps.Host, cfg.Policy.Allows(protocol.VerbRoll), now),
		files:            local.NewFiles(log, cfg.Local.OverridesFile, audit, now),
		audit:            audit,
		rolls:            local.NewRolls(now),
		kick:             make(chan struct{}, 1),
		inventoryTimeout: inventoryTimeout,
		directives:       directives{pending: make(map[string]pendingRoll, 4)},
		lastAnswer:       now(),
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

	a.reloadManaged()

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
		"defaultPolicy": a.cfg.Policy.DefaultPolicy, "version": a.version,
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
		Host: a.host, Policy: &a.cfg.Policy, Overrides: inputs{a}, Registry: a.tracker,
		Audit: a.audit, Notify: a.wake, Now: a.now, ActionRan: a.noteActionRan,
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

// inputs is lease.Overrides over agent.inputs.
type inputs struct{ a *agent }

// Load implements lease.Overrides.
func (i inputs) Load() plan.Overrides { return i.a.inputs() }

// inputs is what the host's files say now: the overrides file as it reads,
// managed.yaml as last read and the holds of the state file.
func (a *agent) inputs() plan.Overrides {
	out := a.files.Load()

	a.managedMu.Lock()
	out.Deployed = maps.Clone(a.managed)
	managedErr := a.managedErr
	a.managedMu.Unlock()

	if managedErr != "" {
		out.Known = false
		out.Err = strings.TrimPrefix(out.Err+"; "+managedErr, "; ")
	}

	out.Holds = a.state.Holds()

	return out
}

// reloadManaged reads managed.yaml again; one that cannot be read keeps the
// last content and holds the host until it reads.
func (a *agent) reloadManaged() {
	m, err := local.ReadManaged(a.cfg.Local.ManagedFile)

	a.managedMu.Lock()
	defer a.managedMu.Unlock()

	if err != nil {
		text := plan.Line("managed.yaml cannot be read: " + err.Error())
		if text != a.managedErr {
			a.log.WithField("file", a.cfg.Local.ManagedFile).Error(text)
		}

		a.managedErr = text

		return
	}

	if a.managedErr != "" {
		a.log.WithField("file", a.cfg.Local.ManagedFile).Info("managed.yaml reads again")
	}

	a.managed, a.managedErr = m, ""
}

// noteActionRan records the inventory hash after a run of an action.
func (a *agent) noteActionRan(name, inventoryHash string) {
	a.ranMu.Lock()
	defer a.ranMu.Unlock()

	if a.actionsRan == nil {
		a.actionsRan = make(map[string]string, len(a.cfg.Policy.Actions))
	}

	a.actionsRan[name] = inventoryHash
}

// ranActions returns a copy of the inventory hashes after the last runs.
func (a *agent) ranActions() map[string]string {
	a.ranMu.Lock()
	defer a.ranMu.Unlock()

	return maps.Clone(a.actionsRan)
}

// wake asks the loop for a sync now.
func (a *agent) wake() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}
