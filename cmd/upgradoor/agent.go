package main

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/registry"
)

func newAgentCmd(log *logrus.Logger) *cobra.Command {
	var configFile string

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the per-host agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgent(cmd.Context(), log, configFile)
		},
	}

	cmd.Flags().StringVar(&configFile, "config", agent.DefaultConfigFile, "agent configuration file")

	return cmd
}

// runAgent runs the agent on this machine until ctx ends.
func runAgent(ctx context.Context, log *logrus.Logger, configFile string) error {
	cfg, err := agent.LoadConfig(configFile)
	if err != nil {
		return err
	}

	creds, err := loadCredentials(cfg.KeyFile, cfg.CertFile)
	if err != nil {
		return err
	}

	// One set of logins for the registry lookups and the pulls.
	logins, err := registry.LoadLogins(log, cfg.Registry.AuthFile)
	if err != nil {
		return err
	}

	machine, err := host.New(log, &cfg.Host, cfg.Policy.Packages.Blocked, logins.Lookup)
	if err != nil {
		return fmt.Errorf("host: %w", err)
	}

	defer func() {
		if cerr := machine.Close(); cerr != nil {
			log.WithError(cerr).Warn("closing docker client")
		}
	}()

	svc, err := agent.New(log, cfg, creds, &agent.Deps{
		Host: machine, Logins: logins, Version: version,
	})
	if err != nil {
		return err
	}

	if err := svc.Start(ctx); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}

	<-ctx.Done()

	return svc.Stop()
}
