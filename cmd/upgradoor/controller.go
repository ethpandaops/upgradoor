package main

import (
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/controller"
	"github.com/ethpandaops/upgradoor/pkg/controller/api"
	"github.com/ethpandaops/upgradoor/web"
)

func newControllerCmd(log *logrus.Logger) *cobra.Command {
	cfg := controller.Config{Version: version}

	var pprofListen string

	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the per-devnet controller",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pprofListen != "" {
				if err := startPprof(cmd.Context(), log, pprofListen); err != nil {
					return err
				}
			}

			dist, err := web.Dist()
			if err != nil {
				return fmt.Errorf("web ui: %w", err)
			}

			cfg.WebFS = dist

			svc, err := controller.New(log, &cfg)
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
	f.StringVar(&cfg.Listen, "listen", ":8080", "HTTP listen address (API, UI, /metrics, /healthz)")
	f.StringVar(&cfg.DataDir, "data-dir", "/data", "directory for the SQLite database")
	f.StringVar(&cfg.Network, "network", "", "network name; certificates must be issued for it")
	f.StringVar(&cfg.SignerPublicKey, "signer-public-key", "",
		"signer public key (output of the upgradoor_signer_pubkey filter or 'keygen signer-pubkey')")
	f.StringVar(&cfg.ControllerSeedFile, "controller-seed-file", "",
		"file holding the controller seed (never the devnet seed): "+
			"the key that signs answers to agents is derived from it for --network")
	f.Uint64Var(&cfg.ControllerGeneration, "controller-generation", 0,
		"generation of the controller key to derive from the controller seed")
	f.BoolVar(&cfg.AllowUnsignedAgentAnswers, "allow-unsigned-agent-answers", false,
		"run without a controller seed: answers to agents are not signed (development only)")
	f.StringArrayVar(&cfg.PublicURLs, "public-url", nil,
		"base URL agents reach the controller under, e.g. https://upgradoor.<network> (repeatable)")
	f.BoolVar(&cfg.AllowInsecureAgentHTTP, "allow-insecure-agent-http", false,
		"accept agent requests over plain http when no --public-url is set (development only)")
	f.BoolVar(&cfg.TrustForwardedHeaders, "trust-forwarded-headers", false,
		"take the client address from X-Forwarded-For (behind an ingress)")
	f.StringVar(&cfg.AuthProviderURL, "auth-provider-url", "",
		"authenticatoor URL, e.g. https://auth.<network>; empty leaves the API open")
	f.StringVar(&cfg.AuthService, "auth-service", api.DefaultAuthService,
		"name of this service in the token's services claim; "+
			"its features (roll, team_roll, ...) are what allows actions")
	f.StringArrayVar(&cfg.BeaconEndpoints, "beacon-endpoint", nil,
		"beacon API base URL (repeatable; tried in order)")
	f.StringArrayVar(&cfg.DefaultSettings, "default-setting", nil,
		"default for a setting as key=value, e.g. maxUnavailablePercent=10 (repeatable)")
	f.StringVar(&pprofListen, "pprof-listen", "",
		"serve net/http/pprof on this loopback address, e.g. 127.0.0.1:6060 (off when empty)")

	return cmd
}
