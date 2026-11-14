package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

const seedEnv = "UPGRADOOR_SEED"

// newKeygenCmd returns the operator-side key tool. Ansible derives keys with
// the filter plugin; this command is the reference implementation, used for
// local development, tests and for checking the plugin's output.
func newKeygenCmd() *cobra.Command {
	var (
		seedFile   string
		network    string
		hostname   string
		generation uint64
		valStart   uint64
		valEnd     uint64
		roles      []string
	)

	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Derive signer and host keys and issue host certificates from the devnet seed",
		Long: "Derive signer and host keys and issue host certificates from the devnet seed.\n\n" +
			"The seed is read from --seed-file or the " + seedEnv + " environment variable,\n" +
			"never from an argument. It is used as raw bytes, exactly as stored in sops\n" +
			"(a single trailing newline of a seed file is dropped).",
	}

	cmd.PersistentFlags().StringVar(&seedFile, "seed-file", "", "file holding the seed")
	cmd.PersistentFlags().StringVar(&network, "network", "", "network name (required)")

	seed := func() ([]byte, error) {
		if network == "" {
			return nil, errors.New("--network is required")
		}

		if seedFile != "" {
			raw, err := os.ReadFile(seedFile)
			if err != nil {
				return nil, fmt.Errorf("read seed: %w", err)
			}

			return []byte(strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")), nil
		}

		if v := os.Getenv(seedEnv); v != "" {
			return []byte(v), nil
		}

		return nil, errors.New("no seed: set --seed-file or " + seedEnv)
	}

	hostFlags := func(c *cobra.Command) {
		c.Flags().StringVar(&hostname, "hostname", "", "inventory hostname (required)")
		c.Flags().Uint64Var(&generation, "generation", 0, "host key generation")
	}

	signer := &cobra.Command{
		Use:   "signer-pubkey",
		Short: "Print the signer public key (the controller's signerPublicKey)",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed()
			if err != nil {
				return err
			}

			key, err := identity.DeriveSignerKey(s, network)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), identity.EncodePublicKey(identity.PublicKey(key)))

			return err
		},
	}

	hostKey := &cobra.Command{
		Use:   "host-key",
		Short: "Print a host's private key (the content of /etc/upgradoor/host.key)",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed()
			if err != nil {
				return err
			}

			key, err := identity.DeriveHostKey(s, network, hostname, generation)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), identity.EncodePrivateKey(key))

			return err
		},
	}
	hostFlags(hostKey)

	hostCert := &cobra.Command{
		Use:   "host-cert",
		Short: "Print a host's certificate (the content of /etc/upgradoor/host.crt)",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed()
			if err != nil {
				return err
			}

			spec := identity.CertificateSpec{
				Network: network, Hostname: hostname, Generation: generation, Roles: roles,
			}

			if c.Flags().Changed("validator-start") || c.Flags().Changed("validator-end") {
				if valEnd <= valStart {
					return errors.New("--validator-start and --validator-end must form a non-empty range")
				}

				spec.Validators = &identity.ValidatorRange{Start: valStart, End: valEnd}
			}

			cert, err := identity.IssueCertificate(s, spec)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), cert)

			return err
		},
	}
	hostFlags(hostCert)
	hostCert.Flags().Uint64Var(&valStart, "validator-start", 0, "first validator index")
	hostCert.Flags().Uint64Var(&valEnd, "validator-end", 0, "validator index after the last")
	hostCert.Flags().StringSliceVar(&roles, "role", nil, "role (repeatable)")

	vectors := &cobra.Command{
		Use:   "vectors",
		Short: "Print the fixed identity test vectors as JSON (no seed needed)",
		RunE: func(c *cobra.Command, _ []string) error {
			vs := identity.VectorInputs()
			for i := range vs {
				if err := vs[i].Compute(); err != nil {
					return err
				}

				if vs[i].Roles == nil {
					vs[i].Roles = []string{}
				}
			}

			enc := json.NewEncoder(c.OutOrStdout())
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")

			return enc.Encode(vs)
		},
	}

	var signerKey string

	verify := &cobra.Command{
		Use:   "verify-cert",
		Short: "Verify a host certificate read from standard input and print its claims (no seed needed)",
		Long: "Verify a host certificate read from standard input against a signer public key\n" +
			"and print its claims as JSON. Needs no seed. Certificates issued by the Ansible\n" +
			"filter plugin carry a randomized signature, so they are compared by verifying\n" +
			"them, not byte for byte.",
		RunE: func(c *cobra.Command, _ []string) error {
			if network == "" {
				return errors.New("--network is required")
			}

			pub, err := identity.DecodePublicKey(signerKey)
			if err != nil {
				return fmt.Errorf("--signer-public-key: %w", err)
			}

			raw, err := io.ReadAll(io.LimitReader(c.InOrStdin(), 64<<10))
			if err != nil {
				return fmt.Errorf("read certificate: %w", err)
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
	verify.Flags().StringVar(&signerKey, "signer-public-key", "", "signer public key (required)")

	cmd.AddCommand(signer, hostKey, hostCert, verify, vectors)

	return cmd
}
