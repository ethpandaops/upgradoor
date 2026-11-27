// Package controller wires the per-devnet controller: store, beacon watcher,
// scheduler, registry lookups, API and the embedded web UI.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"path/filepath"
	"strings"
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

// Config is the controller configuration. The controller holds one secret,
// the controller seed, from which it derives the key it signs its answers to
// agents with. Agents are verified against the signer public key, users
// against the authenticatoor's public keys.
type Config struct {
	// Listen is the HTTP listen address.
	Listen string
	// DataDir holds the SQLite database.
	DataDir string
	// Network is the network name certificates must be issued for.
	Network string
	// SignerPublicKey is the unpadded base64url ML-DSA-44 signer public key.
	SignerPublicKey string
	// ControllerSeedFile is the file holding the controller seed, from which
	// the controller derives the controller key at start. It is not the
	// devnet seed.
	// The seed is only ever read from a file: a flag or an environment
	// variable would show it in the process listing or the pod
	// specification.
	ControllerSeedFile string
	// AllowUnsignedAgentAnswers runs the controller without a controller
	// seed: its answers are not signed, and agents that pin a controller key
	// refuse them (development only).
	AllowUnsignedAgentAnswers bool
	// PublicURLs are the base URLs agents reach the controller under.
	PublicURLs []string
	// AllowInsecureAgentHTTP accepts agent requests over plain http when no
	// public URL is configured (development only).
	AllowInsecureAgentHTTP bool
	// TrustForwardedHeaders takes the client address from X-Forwarded-For.
	TrustForwardedHeaders bool
	// AuthProviderURL is the authenticatoor base URL; empty runs the API
	// open, which needs AllowOpenAPI.
	AuthProviderURL string
	// AllowOpenAPI runs the user API without an auth provider: every action
	// is anonymous and allowed (closed test networks only).
	AllowOpenAPI bool
	// AuthService is the name of this service in the token's "services"
	// claim; empty means api.DefaultAuthService.
	AuthService string
	// UIOrigins are extra origins the web UI may connect to.
	UIOrigins []string
	// UIScript is a script added to the web UI, e.g. a site menu, and
	// UIScriptIntegrity its subresource integrity hash.
	UIScript          string
	UIScriptIntegrity string
	// BeaconEndpoints are beacon API base URLs, tried in order.
	BeaconEndpoints []string
	// ValidatorRangesURL is the devnet's validator ranges inventory, from
	// which hosts get their validators; empty gives no host validators.
	ValidatorRangesURL string
	// RegistryAuthFile is a Docker config.json with the credentials for the
	// registry lookups; empty or missing means anonymous.
	RegistryAuthFile string
	// Registry is how the registries are reached for the lookups.
	Registry registry.Config
	// ImageSources name the GitHub repository an image is built from, as
	// "<image>=<owner>/<name>[@<ref>]", for images whose labels do not.
	ImageSources []string
	// SourceInterval is how often the source branches are read.
	SourceInterval time.Duration
	// BuildScheduleMinute is the minute of the hour at which the image
	// builder's scheduled run starts; negative when it has none.
	BuildScheduleMinute int
	// DefaultSettings are "key=value" overrides of the built-in setting
	// defaults; keys are the JSON names of fleet.Settings.
	DefaultSettings []string
	// Version is the build version.
	Version string
	// WebFS is the built web UI.
	WebFS fs.FS
}

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
	api        *api.Server
	server     *http.Server
	served     chan error
}

var _ Service = (*controller)(nil)

// New validates the configuration and builds the controller.
func New(log logrus.FieldLogger, cfg *Config) (Service, error) {
	if cfg.Network == "" {
		return nil, errors.New("controller: --network is required")
	}

	if err := identity.ValidateName(cfg.Network); err != nil {
		return nil, fmt.Errorf("controller: --network: %w", err)
	}

	signer, err := identity.DecodePublicKey(cfg.SignerPublicKey)
	if err != nil {
		return nil, fmt.Errorf("controller: --signer-public-key: %w", err)
	}

	answers, err := loadAnswerSigner(cfg)
	if err != nil {
		return nil, err
	}

	if authErr := checkAuth(cfg); authErr != nil {
		return nil, authErr
	}

	if len(cfg.BeaconEndpoints) == 0 {
		return nil, errors.New("controller: at least one --beacon-endpoint is required")
	}

	if cfg.BuildScheduleMinute > 59 {
		return nil, errors.New("controller: --build-schedule-minute is a minute of the hour (0-59), or -1 for none")
	}

	defaults, err := ParseDefaultSettings(cfg.DefaultSettings)
	if err != nil {
		return nil, fmt.Errorf("controller: --default-setting: %w", err)
	}

	c := &controller{
		log:    log.WithField("package", "controller"),
		cfg:    *cfg,
		served: make(chan error, 1),
	}

	c.store = store.New(log, filepath.Join(cfg.DataDir, DatabaseFile))
	c.beacon = beacon.New(log, beacon.Options{
		Endpoints: cfg.BeaconEndpoints,
		Store:     c.store,
	})
	c.validators = validators.New(log, validators.Options{
		URL:       cfg.ValidatorRangesURL,
		Store:     c.store,
		UserAgent: "upgradoor-controller/" + cfg.Version,
		Changed:   func() { c.api.Notify() },
	})
	c.scheduler = scheduler.New(log, scheduler.Options{
		Store:    c.store,
		Health:   c.health,
		Liveness: c.beacon.Liveness,
		Attested: c.beacon.AttestedSlots,
		Defaults: defaults,
		NewID:    newID,
		// The API is built after the scheduler, which is not running yet.
		Changed: func() { c.api.Notify() },
	})

	logins := registryauth.Logins{}
	if cfg.RegistryAuthFile != "" {
		if logins, err = registryauth.LoadLogins(log, cfg.RegistryAuthFile); err != nil {
			return nil, fmt.Errorf("controller: --registry-auth-file: %w", err)
		}
	}

	resolver, err := registry.NewResolver(log, &cfg.Registry, logins)
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}

	c.lookups = discovery.NewTracker(log, resolver, defaults.LookupInterval.Std(), time.Now)

	sources := make(map[string]upstream.Source, len(cfg.ImageSources))

	for _, value := range cfg.ImageSources {
		image, source, serr := upstream.ParseSource(value)
		if serr != nil {
			return nil, fmt.Errorf("controller: --image-source: %w", serr)
		}

		sources[image] = source
	}

	c.upstream = upstream.New(log, upstream.Options{
		Lookups:  c.lookups,
		Registry: resolver,
		NotFound: registry.IsNotFound,
		Remote:   upstream.NewGitHub(cfg.Version),
		Sources:  sources,
		Interval: cfg.SourceInterval,
		Changed:  func() { c.api.Notify() },
		Now:      time.Now,
	})

	var buildMinute *int
	if cfg.BuildScheduleMinute >= 0 {
		buildMinute = &cfg.BuildScheduleMinute
	}

	c.api, err = api.New(log, api.Options{
		Network:                cfg.Network,
		Version:                cfg.Version,
		Verifier:               identity.NewVerifier(signer, cfg.Network),
		AnswerSigner:           answers,
		PublicURLs:             cfg.PublicURLs,
		AllowInsecureAgentHTTP: cfg.AllowInsecureAgentHTTP,
		TrustForwardedHeaders:  cfg.TrustForwardedHeaders,
		AuthProviderURL:        cfg.AuthProviderURL,
		AuthService:            cfg.AuthService,
		UIOrigins:              cfg.UIOrigins,
		UIScript:               cfg.UIScript,
		UIScriptIntegrity:      cfg.UIScriptIntegrity,
		Store:                  c.store,
		Scheduler:              c.scheduler,
		Beacon:                 c.beacon,
		Endpoints:              c.beacon,
		Validators:             c.validators,
		Lookups:                c.lookups,
		Upstream:               c.upstream,
		BuildMinute:            buildMinute,
		DefaultSettings:        defaults,
		WebFS:                  cfg.WebFS,
		Registry:               prometheus.NewRegistry(),
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}

	if answers == nil {
		c.log.Warn("INSECURE: --allow-unsigned-agent-answers is set and there is no " +
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

	if cfg.AllowOpenAPI {
		c.log.Warn("INSECURE: --allow-open-api is set and there is no --auth-provider-url. " +
			"The API is open to everyone who can reach it: every action is anonymous " +
			"and allowed. Closed test networks only.")
	}

	return c, nil
}

// checkAuth accepts a configuration with an auth provider, or without one
// with the explicit opt-out, not both.
func checkAuth(cfg *Config) error {
	switch {
	case cfg.AuthProviderURL == "" && !cfg.AllowOpenAPI:
		return errors.New("controller: --auth-provider-url is required: without it every " +
			"action is anonymous and allowed (--allow-open-api runs without, " +
			"for closed test networks)")
	case cfg.AuthProviderURL != "" && cfg.AllowOpenAPI:
		return errors.New("controller: --auth-provider-url and --allow-open-api exclude each other")
	}

	return nil
}

// loadAnswerSigner reads the controller seed and derives the controller key
// of this network from it. Without a seed it returns nil,
// which is only accepted with the explicit opt-out.
func loadAnswerSigner(cfg *Config) (*identity.AnswerSigner, error) {
	switch {
	case cfg.ControllerSeedFile == "" && cfg.AllowUnsignedAgentAnswers:
		return nil, nil //nolint:nilnil // no key and no error is the opt-out
	case cfg.ControllerSeedFile == "":
		return nil, errors.New("controller: --controller-seed-file is required: agents act " +
			"only on signed answers (--allow-unsigned-agent-answers runs without, " +
			"for development)")
	case cfg.AllowUnsignedAgentAnswers:
		return nil, errors.New("controller: --controller-seed-file and " +
			"--allow-unsigned-agent-answers exclude each other")
	}

	// Neither error carries anything of the file's content.
	seed, err := identity.ReadSeedFile(cfg.ControllerSeedFile)
	if err != nil {
		return nil, fmt.Errorf("controller: --controller-seed-file: %w", err)
	}

	key, err := identity.DeriveControllerKey(seed)
	if err != nil {
		return nil, fmt.Errorf("controller: derive the controller key: %w", err)
	}

	signer, err := identity.NewAnswerSigner(key, cfg.Network)
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

	if err := c.validators.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("validator ranges: %w", err), c.store.Stop())
	}

	if err := c.beacon.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("beacon: %w", err), c.validators.Stop(), c.store.Stop())
	}

	if err := c.scheduler.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("scheduler: %w", err),
			c.beacon.Stop(), c.validators.Stop(), c.store.Stop())
	}

	if err := c.lookups.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("lookups: %w", err),
			c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.store.Stop())
	}

	if err := c.upstream.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("upstream: %w", err), c.lookups.Stop(),
			c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.store.Stop())
	}

	if err := c.api.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("api: %w", err), c.upstream.Stop(),
			c.lookups.Stop(), c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.store.Stop())
	}

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", c.cfg.Listen)
	if err != nil {
		return errors.Join(fmt.Errorf("listen: %w", err), c.api.Stop(), c.upstream.Stop(), c.lookups.Stop(),
			c.scheduler.Stop(), c.beacon.Stop(), c.validators.Stop(), c.store.Stop())
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
		"network": c.cfg.Network,
		"auth":    c.cfg.AuthProviderURL != "",
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
		c.validators.Stop(), c.store.Stop())

	return errors.Join(errs...)
}

// health is the beacon service's view with the state of the validator ranges
// inventory.
func (c *controller) health() fleet.NetworkHealth {
	h := c.beacon.Health()
	h.ValidatorRangesError = c.validators.Error()

	return h
}

// ParseDefaultSettings applies "key=value" overrides to the built-in
// defaults. A value is read as JSON when it parses (numbers, booleans,
// quoted strings) and as a plain string otherwise ("30m", "manual").
func ParseDefaultSettings(overrides []string) (fleet.Settings, error) {
	settings := fleet.DefaultSettings()

	raw, err := json.Marshal(settings)
	if err != nil {
		return settings, err
	}

	fields := make(map[string]json.RawMessage, 16)

	err = json.Unmarshal(raw, &fields)
	if err != nil {
		return settings, err
	}

	for _, kv := range overrides {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return settings, fmt.Errorf("%q is not key=value", kv)
		}

		if _, known := fields[key]; !known {
			return settings, fmt.Errorf("unknown setting %q", key)
		}

		if json.Valid([]byte(value)) {
			fields[key] = json.RawMessage(value)

			continue
		}

		quoted, qerr := json.Marshal(value)
		if qerr != nil {
			return settings, qerr
		}

		fields[key] = quoted
	}

	merged, err := json.Marshal(fields)
	if err != nil {
		return settings, err
	}

	// A bare number for a duration ("leaseTtl=30") must not be accepted
	// silently, so decode strictly into the typed struct.
	if err := json.Unmarshal(merged, &settings); err != nil {
		return settings, err
	}

	if err := settings.Validate(); err != nil {
		return settings, err
	}

	return settings, nil
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}

	return hex.EncodeToString(b[:])
}
