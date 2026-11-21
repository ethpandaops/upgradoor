package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/docker"
	"github.com/ethpandaops/upgradoor/pkg/agent/fakehost"
	"github.com/ethpandaops/upgradoor/pkg/agent/probe"
	"github.com/ethpandaops/upgradoor/pkg/agent/registry"
	"github.com/ethpandaops/upgradoor/pkg/agent/system"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func newAgentCmd(log *logrus.Logger) *cobra.Command {
	var (
		configFile string
		fakeFile   string
	)

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the per-host agent",
		Long: "Run the per-host agent.\n\n" +
			"With --fake the agent runs against an in-memory host described by a scenario\n" +
			"file instead of Docker, apt and systemd. That is for local development only:\n" +
			"nothing on the machine is touched.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgent(cmd.Context(), log, configFile, fakeFile)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", config.DefaultFile, "agent configuration file")
	cmd.Flags().StringVar(&fakeFile, "fake", "", "fake host scenario file (development only)")

	return cmd
}

func runAgent(ctx context.Context, log *logrus.Logger, configFile, fakeFile string) error {
	cfg, err := config.Load(configFile)
	if err != nil {
		return err
	}

	creds, err := loadCredentials(cfg)
	if err != nil {
		return err
	}

	deps := agent.Deps{
		Prober:  probe.New(log, cfg.Probes.EL, cfg.Probes.CL),
		Version: version,
	}

	var cleanup func()

	if fakeFile != "" {
		cleanup, err = fakeDeps(ctx, log, fakeFile, &deps)
	} else {
		cleanup, err = realDeps(log, cfg, &deps)
	}

	if err != nil {
		return err
	}

	defer cleanup()

	svc, err := agent.New(log, cfg, creds, deps)
	if err != nil {
		return err
	}

	if err := svc.Start(ctx); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	<-ctx.Done()

	return svc.Stop()
}

func loadCredentials(cfg *config.Config) (*identity.Credentials, error) {
	rawKey, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read host key: %w", err)
	}

	key, err := identity.DecodePrivateKey(string(rawKey))
	if err != nil {
		return nil, fmt.Errorf("host key %s: %w", cfg.KeyFile, err)
	}

	rawCert, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		return nil, fmt.Errorf("read host certificate: %w", err)
	}

	creds, err := identity.NewCredentials(key, string(bytes.TrimSpace(rawCert)))
	if err != nil {
		return nil, fmt.Errorf("host certificate %s: %w", cfg.CertFile, err)
	}

	return creds, nil
}

// realDeps wires Docker, the registry resolver and the OS layer.
func realDeps(log *logrus.Logger, cfg *config.Config, deps *agent.Deps) (func(), error) {
	// One set of logins for the registry lookups and the pulls.
	logins, err := registry.LoadLogins(log, cfg.Registry.AuthFile)
	if err != nil {
		return nil, err
	}

	runtime, err := docker.New(log, cfg.Docker.Host, logins.Lookup)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}

	resolver, err := registry.New(log, registry.Options{
		Logins:    logins,
		PlainHTTP: cfg.Registry.PlainHTTP,
		CAFile:    cfg.Registry.CAFile,
	})
	if err != nil {
		_ = runtime.Close()

		return nil, fmt.Errorf("registry: %w", err)
	}

	deps.Runtime = runtime
	deps.Registry = resolver
	deps.System = system.New(log, system.Options{Blocked: cfg.Packages.Blocked})

	return func() {
		if err := runtime.Close(); err != nil {
			log.WithError(err).Warn("closing docker client")
		}
	}, nil
}

// fakeDeps wires the in-memory host.
func fakeDeps(
	ctx context.Context, log *logrus.Logger, file string, deps *agent.Deps,
) (func(), error) {
	sc, err := fakehost.LoadScenario(file)
	if err != nil {
		return nil, err
	}

	host, err := fakehost.New(sc)
	if err != nil {
		return nil, err
	}

	deps.Runtime = host
	deps.Registry = host
	deps.System = host

	if sc.Control == "" {
		return func() {}, nil
	}

	ctlCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		if err := host.ServeControl(ctlCtx, sc.Control); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.WithError(err).Error("fake host control endpoint")
		}
	}()

	return func() {
		cancel()
		<-done
	}, nil
}
