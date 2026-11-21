// Command upgradoor is the upgradoor agent and its on-host commands.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// version is set at build time.
var version = "dev"

// Command groups of the root command.
const (
	groupHost   = "host"
	groupDaemon = "daemon"
)

func main() {
	log := logrus.New()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := newRootCmd(log).ExecuteContext(ctx); err != nil {
		log.WithError(err).Error("upgradoor failed")
		cancel()
		os.Exit(1) //nolint:gocritic // cancel is called explicitly above
	}
}

func newRootCmd(log *logrus.Logger) *cobra.Command {
	var (
		logLevel  string
		logFormat string
	)

	root := &cobra.Command{
		Use:   "upgradoor",
		Short: "Staged, health-gated image rolls, package upgrades and reboots for devnet hosts",
		Long: "Staged, health-gated image rolls, package upgrades and reboots for devnet hosts.\n\n" +
			"list, status, roll, override, hold, release and init-overrides talk to the\n" +
			"running agent over its local socket (root and the entry user's group). Nothing\n" +
			"they ask for happens at once: an override and a roll are served under a lease\n" +
			"of the controller, like every other change.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			level, err := logrus.ParseLevel(logLevel)
			if err != nil {
				return err
			}

			log.SetLevel(level)

			if logFormat == "json" {
				log.SetFormatter(&logrus.JSONFormatter{})
			}

			return nil
		},
	}

	root.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level")
	root.PersistentFlags().StringVar(&logFormat, "log-format", "text", "log format: text or json")

	root.AddGroup(
		&cobra.Group{ID: groupHost, Title: "On-host commands:"},
		&cobra.Group{ID: groupDaemon, Title: "Daemons:"},
	)

	onHost := []*cobra.Command{
		newListCmd(), newStatusCmd(), newRollCmd(), newOverrideCmd(),
		newHoldCmd(), newReleaseCmd(), newInitOverridesCmd(), newDeployCmd(), newVerifyCertCmd(),
	}
	for _, cmd := range onHost {
		cmd.GroupID = groupHost
	}

	daemons := []*cobra.Command{newAgentCmd(log)}
	for _, cmd := range daemons {
		cmd.GroupID = groupDaemon
	}

	root.AddCommand(onHost...)
	root.AddCommand(daemons...)

	return root
}
