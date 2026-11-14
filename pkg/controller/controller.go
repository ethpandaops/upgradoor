// Package controller wires the per-devnet controller: store, beacon watcher,
// scheduler, API and the embedded web UI.
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
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// DatabaseFile is the SQLite file name inside the data directory.
const DatabaseFile = "upgradoor.db"

// Config is the controller configuration. The controller holds no secret:
// agents are verified against the signer public key, users against the
// authenticatoor's public keys.
type Config struct {
	// Listen is the HTTP listen address.
	Listen string
	// DataDir holds the SQLite database.
	DataDir string
	// Network is the network name certificates must be issued for.
	Network string
	// SignerPublicKey is the unpadded base64url ML-DSA-44 signer public key.
	SignerPublicKey string
	// PublicURLs are the base URLs agents reach the controller under.
	PublicURLs []string
	// AllowInsecureAgentHTTP accepts agent requests over plain http when no
	// public URL is configured (development only).
	AllowInsecureAgentHTTP bool
	// TrustForwardedHeaders takes the client address from X-Forwarded-For.
	TrustForwardedHeaders bool
	// AuthProviderURL is the authenticatoor base URL; empty runs the API open.
	AuthProviderURL string
	// BeaconEndpoints are beacon API base URLs, tried in order.
	BeaconEndpoints []string
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

	store     store.Store
	beacon    beacon.Service
	scheduler scheduler.Service
	api       *api.Server
	server    *http.Server
	served    chan error
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

	if len(cfg.BeaconEndpoints) == 0 {
		return nil, errors.New("controller: at least one --beacon-endpoint is required")
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
	c.scheduler = scheduler.New(log, scheduler.Options{
		Store:    c.store,
		Health:   c.beacon.Health,
		Defaults: defaults,
		NewID:    newID,
	})

	c.api, err = api.New(log, api.Options{
		Network:                cfg.Network,
		Version:                cfg.Version,
		Verifier:               identity.NewVerifier(signer, cfg.Network),
		PublicURLs:             cfg.PublicURLs,
		AllowInsecureAgentHTTP: cfg.AllowInsecureAgentHTTP,
		TrustForwardedHeaders:  cfg.TrustForwardedHeaders,
		AuthProviderURL:        cfg.AuthProviderURL,
		Store:                  c.store,
		Scheduler:              c.scheduler,
		Beacon:                 c.beacon,
		DefaultSettings:        defaults,
		WebFS:                  cfg.WebFS,
		Registry:               prometheus.NewRegistry(),
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}

	return c, nil
}

// Start opens the store and starts every part, then serves HTTP.
func (c *controller) Start(ctx context.Context) error {
	if err := c.store.Start(ctx); err != nil {
		return fmt.Errorf("store: %w", err)
	}

	if err := c.beacon.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("beacon: %w", err), c.store.Stop())
	}

	if err := c.scheduler.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("scheduler: %w", err), c.beacon.Stop(), c.store.Stop())
	}

	if err := c.api.Start(ctx); err != nil {
		return errors.Join(fmt.Errorf("api: %w", err),
			c.scheduler.Stop(), c.beacon.Stop(), c.store.Stop())
	}

	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", c.cfg.Listen)
	if err != nil {
		return errors.Join(fmt.Errorf("listen: %w", err),
			c.api.Stop(), c.scheduler.Stop(), c.beacon.Stop(), c.store.Stop())
	}

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

	if c.cfg.AuthProviderURL == "" {
		c.log.Warn("no --auth-provider-url: the API is open to everyone who can reach it")
	}

	return nil
}

// Stop shuts everything down in reverse order.
func (c *controller) Stop() error {
	var errs []error

	if c.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		errs = append(errs, c.server.Shutdown(ctx), <-c.served)
	}

	errs = append(errs, c.api.Stop(), c.scheduler.Stop(), c.beacon.Stop(), c.store.Stop())

	return errors.Join(errs...)
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
