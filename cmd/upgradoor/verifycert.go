package main

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func newVerifyCertCmd() *cobra.Command {
	var network, signerKey string

	cmd := &cobra.Command{
		Use:   "verify-cert",
		Short: "Verify a host certificate read from standard input and print what it certifies",
		Long: "Verify a host certificate read from standard input against a signer public key\n" +
			"and print its hostname, network and public key as JSON.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			info, err := identity.ReadCertificate(c.InOrStdin(), signerKey, network)
			if err != nil {
				return err
			}

			return json.NewEncoder(c.OutOrStdout()).Encode(info)
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network name (required)")
	cmd.Flags().StringVar(&signerKey, "signer-public-key", "", "signer public key (required)")

	return cmd
}
