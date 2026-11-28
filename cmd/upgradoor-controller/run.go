package main

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/controller"
	"github.com/ethpandaops/upgradoor/web"
)

func newRunCmd(log *logrus.Logger) *cobra.Command {
	var (
		configFile string
		sets       []string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the per-devnet controller",
		Long: "Run the per-devnet controller with the configuration of --config, a YAML file.\n" +
			"Without --config, or for keys the file leaves out, the defaults apply; unknown keys\n" +
			"are errors. --set overrides one key by its dotted path, e.g.\n" +
			"--set auth.providerUrl=https://auth.example --set settingDefaults.leaseTtl=5m;\n" +
			"a list is comma separated.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := controller.LoadConfig(configFile, sets)
			if err != nil {
				return err
			}

			cfg.Version = version

			dist, err := web.Dist()
			if err != nil {
				return fmt.Errorf("web ui: %w", err)
			}

			cfg.WebFS = dist

			svc, err := controller.New(log, cfg)
			if err != nil {
				return err
			}

			if err := svc.Start(cmd.Context()); err != nil {
				return fmt.Errorf("start controller: %w", err)
			}

			<-cmd.Context().Done()

			return svc.Stop()
		},
	}

	f := cmd.Flags()
	f.StringVar(&configFile, "config", "", "YAML configuration file (defaults when empty)")
	f.StringArrayVar(&sets, "set", nil, "override a configuration key as path=value (repeatable)")

	return cmd
}
