package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// maxCertificate bounds the certificate verify-cert reads.
const maxCertificate = 64 << 10

func newVerifyCertCmd() *cobra.Command {
	var network, signerKey string

	cmd := &cobra.Command{
		Use:   "verify-cert",
		Short: "Verify a host certificate read from standard input and print its claims",
		Long: "Verify a host certificate read from standard input against a signer public key\n" +
			"and print its claims as JSON. Certificates issued by the Ansible filter plugin\n" +
			"carry a randomized signature, so they are compared by verifying them, not byte\n" +
			"for byte.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if network == "" {
				return errors.New("--network is required")
			}

			pub, err := identity.DecodePublicKey(signerKey)
			if err != nil {
				return fmt.Errorf("--signer-public-key: %w", err)
			}

			raw, err := io.ReadAll(io.LimitReader(c.InOrStdin(), maxCertificate+1))
			if err != nil {
				return fmt.Errorf("read certificate: %w", err)
			}

			if len(raw) > maxCertificate {
				return fmt.Errorf("certificate is larger than %d bytes", maxCertificate)
			}

			cert, err := identity.VerifyCertificate(pub, network, strings.TrimSpace(string(raw)))
			if err != nil {
				return err
			}

			enc := json.NewEncoder(c.OutOrStdout())
			enc.SetEscapeHTML(false)

			return enc.Encode(cert.Claims)
		},
	}

	cmd.Flags().StringVar(&network, "network", "", "network name (required)")
	cmd.Flags().StringVar(&signerKey, "signer-public-key", "", "signer public key (required)")

	return cmd
}
