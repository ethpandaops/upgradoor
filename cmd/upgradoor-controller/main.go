// Command upgradoor-controller is the per-devnet upgradoor controller and its key tool.
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

func main() {
	log := logrus.New()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := newRootCmd(log).ExecuteContext(ctx); err != nil {
		log.WithError(err).Error("upgradoor-controller failed")
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
		Use:           "upgradoor-controller",
		Short:         "The per-devnet upgradoor controller and its key tool",
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

	root.AddCommand(newRunCmd(log))
	root.AddCommand(newKeygenCmd())

	return root
}
