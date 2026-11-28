// Package controller wires the per-devnet controller: store, beacon watcher,
// scheduler, registry lookups, API and the embedded web UI.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/api"
	"github.com/ethpandaops/upgradoor/pkg/controller/beacon"
	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/registry"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/upstream"
	"github.com/ethpandaops/upgradoor/pkg/controller/validators"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/registryauth"
)

// DatabaseFile is the SQLite file name inside the data directory.
const DatabaseFile = "upgradoor.db"

// Service is the running controller.
type Service interface {
	Start(ctx context.Context) error
	Stop() error
}

type controller struct {
	log logrus.FieldLogger
	cfg Config

	store      store.Store
	beacon     beacon.Service
	validators validators.Service
	scheduler  scheduler.Service
	lookups    discovery.Tracker
	upstream   upstream.Tracker
	retention  *auditRetention
	api        *api.Server
	server     *http.Server
	served     chan error
}

var _ Service = (*controller)(nil)

// New validates the configuration and builds the controller.
func New(log logrus.FieldLogger, cfg *Config) (Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("controller config: %w", err)
	}

	signer, err := identity.DecodePublicKey(cfg.Agents.SignerPublicKey)
	if err != nil {
		return nil, fmt.Errorf("controller: agents.signerPublicKey: %w", err)
	}

	answers, err := loadAnswerSigner(cfg)
	if err != nil {
		return nil, err
	}

	defaults, err := cfg.Settings()
	if err != nil {
		return nil, fmt.Errorf("controller: settingDefaults: %w", err)
	}

	c := &controller{
		log:    log.WithField("package", "controller"),
		cfg:    *cfg,
		served: make(chan error, 1),
	}

	c.store = store.New(log, filepath.Join(cfg.Store.DataDir, DatabaseFile))
	c.retention = newAuditRetention(log, c.store, cfg.Store.AuditRetention.Std())
	c.beacon = beacon.New(log, beacon.Options{
		Endpoints: cfg.Network.BeaconEndpoints,
		Store:     c.store,
	})
	c.validators = validators.New(log, validators.Options{
		URL:       cfg.Network.ValidatorRangesURL,
		Store:     c.store,
		UserAgent: "upgradoor-controller/" + cfg.Version,
		Changed:   func() { c.api.Notify() },
	})

	logins := registryauth.Logins{}
	if cfg.Images.RegistryAuthFile != "" {
		if logins, err = registryauth.LoadLogins(log, cfg.Images.RegistryAuthFile); err != nil {
			return nil, fmt.Errorf("controller: images.registryAuthFile: %w", err)
		}
	}

	resolver, err := registry.NewResolver(log, &registry.Config{
		PlainHTTP: cfg.Images.RegistryPlainHTTP, CAFile: cfg.Images.RegistryCAFile,
		AllowPrivate: cfg.Images.RegistryAllowPrivate,
	}, logins)
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}

	c.lookups = discovery.NewTracker(log, resolver, defaults.LookupInterval.Std(), time.Now)

	c.scheduler = scheduler.New(log, scheduler.Options{
		Store:        c.store,
		Health:       c.health,
		Liveness:     c.beacon.Liveness,
		Attested:     c.beacon.AttestedSlots,
		Defaults:     defaults,
		DeployWindow: cfg.DeployWindow.Std(),
		NewID:        newID,
		Lookups:      c.lookups,
		// The API is built after the scheduler, which is not running yet.
		Changed: func() { c.api.Notify() },
	})

	sources := make(map[string]upstream.Source, len(cfg.Images.Sources))

	for _, value := range cfg.Images.Sources {
		image, source, serr := upstream.ParseSource(value)
		if serr != nil {
			return nil, fmt.Errorf("controller: images.sources: %w", serr)
		}

		sources[image] = source
	}

	c.upstream = upstream.New(log, upstream.Options{
		Lookups:  c.lookups,
		Registry: resolver,
		NotFound: registry.IsNotFound,
		Remote:   upstream.NewGitHub(cfg.Version),
		Sources:  sources,
		Interval: cfg.Images.SourceInterval.Std(),
		Changed:  func() { c.api.Notify() },
		Now:      time.Now,
	})

	c.api, err = api.New(log, api.Options{
		Network:                cfg.Network.Name,
		Version:                cfg.Version,
		Verifier:               identity.NewVerifier(signer, cfg.Network.Name),
		AnswerSigner:           answers,
		PublicURLs:             cfg.Server.PublicURLs,
		AllowInsecureAgentHTTP: cfg.Agents.AllowInsecureHTTP,
		TrustForwardedHeaders:  cfg.Server.TrustForwardedHeaders,
		AuthProviderURL:        cfg.Auth.ProviderURL,
		AuthService:            cfg.Auth.Service,
		UIOrigins:              cfg.UI.Origins,
		UIScript:               cfg.UI.Script,
		UIScriptIntegrity:      cfg.UI.ScriptIntegrity,
		Pprof:                  cfg.UI.Pprof,
		Store:                  c.store,
		Scheduler:              c.scheduler,
		Beacon:                 c.beacon,
		Endpoints:              c.beacon,
		Validators:             c.validators,
		Lookups:                c.lookups,
		Upstream:               c.upstream,
		DefaultSettings:        defaults,
		WebFS:                  cfg.WebFS,
		Registry:               prometheus.NewRegistry(),
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}

	if answers == nil {
		c.log.Warn("INSECURE: agents.allowUnsignedAnswers is set and there is no " +
			"controller seed. Answers to agents are NOT signed: agents that pin a " +
			"controller key refuse them, and the others trust whoever answers under " +
			"the controller's name. Development only.")
	} else {
		// The fingerprint names the public key; it is no secret. It is what
		// the agents log as controller_key_sha256.
		c.log.WithField("controller_key_sha256", identity.KeyFingerprint(answers.PublicKey())).
			Info("Signing answers to agents with the controller key derived " +
				"from the controller seed")
	}

	if cfg.Auth.AllowOpenAPI {
		c.log.Warn("INSECURE: auth.allowOpenApi is set and there is no auth.providerUrl. " +
			"The API is open to everyone who can reach it: every action is anonymous " +
			"and allowed. Closed test networks only.")
	}

	return c, nil
}

// startDeployWindow opens the initial deployment window on the first start
// on a fresh store and logs its state.
func (c *controller) startDeployWindow(ctx context.Context) error {
	w, err := c.store.StartDeployWindow(ctx, time.Now())
	if err != nil {
		return fmt.Errorf("deploy window: %w", err)
	}

	log := c.log.WithField("started_at", w.StartedAt)
	if w.ArmedAt != nil {
		log.WithField("armed_at", *w.ArmedAt).Info("Initial deployment window is armed")

		return nil
	}

	log.Info("Initial deployment window not armed: only Ansible deploys are leased " +
		"until it ends (deployWindow) or is armed")

	return nil
}

// loadAnswerSigner reads the controller seed and derives the controller key
// of this network from it; nil without a seed (agents.allowUnsignedAnswers,
// checked by Validate).
func loadAnswerSigner(cfg *Config) (*identity.AnswerSigner, error) {
	if cfg.Agents.ControllerSeedFile == "" {
		return nil, nil //nolint:nilnil // no key and no error is the opt-out
	}

	// Neither error carries anything of the file's content.
	seed, err := identity.ReadSeedFile(cfg.Agents.ControllerSeedFile)
	if err != nil {
		return nil, fmt.Errorf("controller: agents.controllerSeedFile: %w", err)
	}

	key, err := identity.DeriveControllerKey(seed)
	if err != nil {
		return nil, fmt.Errorf("controller: derive the controller key: %w", err)
	}

	signer, err := identity.NewAnswerSigner(key, cfg.Network.Name)
	if err != nil {
		return nil, fmt.Errorf("controller: controller key: %w", err)
	}

	return signer, nil
}

// Start opens the store and starts every part, then serves HTTP.
func (c *controller) Start(ctx context.Context) error {
	if err := c.store.Start(ctx); err != nil {
		return fmt.Errorf("store: %w", err)
	}

	if err := c.startDeployWindow(ctx); err != nil {
		return errors.Join(err, c.store.Stop())
	}

	if err := c.retention.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("audit retention: %w", err), c.store.Stop())
	}

	if err := c.validators.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("validator ranges: %w", err), c.retention.Stop(), c.store.Stop())
	}

	if err := c.beacon.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("beacon: %w", err), c.validators.Stop(), c.retention.Stop(), c.store.Stop())
	}

	if err := c.scheduler.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("scheduler: %w", err),
			c.beacon.Stop(), c.validators.Stop(), c.retention.Stop(), c.store.Stop())
	}

	if err := c.lookups.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("lookups: %w", err),
			c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.retention.Stop(), c.store.Stop())
	}

	if err := c.upstream.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("upstream: %w", err), c.lookups.Stop(),
			c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.retention.Stop(), c.store.Stop())
	}

	if err := c.api.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("api: %w", err), c.upstream.Stop(),
			c.lookups.Stop(), c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.retention.Stop(), c.store.Stop())
	}

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", c.cfg.Server.Listen)
	if err != nil {
		return errors.Join(fmt.Errorf("listen: %w", err), c.api.Stop(), c.upstream.Stop(), c.lookups.Stop(),
			c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.retention.Stop(), c.store.Stop())
	}

	// WriteTimeout is that of a request. An event stream sets its own
	// deadline with every write.
	c.server = &http.Server{
		Handler:           c.api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	go func() {
		err := c.server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		c.served <- err
	}()

	c.log.WithFields(logrus.Fields{
		"listen":  listener.Addr().String(),
		"network": c.cfg.Network.Name,
		"auth":    c.cfg.Auth.ProviderURL != "",
	}).Info("controller started")

	return nil
}

// Stop shuts everything down in reverse order.
func (c *controller) Stop() error {
	var errs []error

	if c.server != nil {
		// An event stream stays open for as long as its page: the server
		// would wait for it until the timeout.
		c.api.CloseStreams()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		errs = append(errs, c.server.Shutdown(ctx), <-c.served)
	}

	errs = append(errs, c.api.Stop(), c.upstream.Stop(), c.lookups.Stop(), c.scheduler.Stop(), c.beacon.Stop(),
		c.validators.Stop(), c.retention.Stop(), c.store.Stop())

	return errors.Join(errs...)
}

// health is the beacon service's view with the state of the validator ranges
// inventory.
func (c *controller) health() fleet.NetworkHealth {
	h := c.beacon.Health()
	h.ValidatorRangesError = c.validators.Error()

	return h
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}

	return hex.EncodeToString(b[:])
}
