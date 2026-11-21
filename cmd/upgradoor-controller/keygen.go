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

// The two seeds and where each is read from. They are different secrets and
// are never read from the same place: the devnet seed yields the signer and
// host keys of one devnet, the controller seed (one per group of devnets)
// the controller key of each devnet of the group.
const (
	seedEnv            = "UPGRADOOR_SEED"
	seedFlag           = "seed-file"
	controllerSeedEnv  = "UPGRADOOR_CONTROLLER_SEED"
	controllerSeedFlag = "controller-seed-file"
	// maxCertificate bounds the certificate verify-cert reads.
	maxCertificate = 64 << 10
)

var errSameSeed = errors.New("the devnet seed and the controller seed are the same value: " +
	"they must be two different secrets")

// newKeygenCmd returns the operator-side key tool. Ansible derives keys with
// the filter plugin; this command is the reference implementation, used for
// local development, tests and for checking the plugin's output.
func newKeygenCmd() *cobra.Command {
	var (
		seedFile   string
		network    string
		hostname   string
		generation uint64
		roles      []string
	)

	cmd := &cobra.Command{
		Use: "keygen",
		Short: "Derive signer and host keys and issue host certificates from the devnet " +
			"seed, and controller keys from the controller seed",
		Long: "Derive keys and issue host certificates. There are two seeds, and they are\n" +
			"different secrets:\n\n" +
			"  the devnet seed      one per devnet: signer-pubkey, host-key, host-cert\n" +
			"                       --" + seedFlag + " or " + seedEnv + "\n" +
			"  the controller seed  one per devnet group: controller-pubkey\n" +
			"                       --" + controllerSeedFlag + " or " + controllerSeedEnv + "\n\n" +
			"A command reads only its own seed and refuses the other one's flag. Neither\n" +
			"seed is ever taken from an argument. Both are used as raw bytes, exactly as\n" +
			"stored in sops (a single trailing newline of a seed file is dropped), and\n" +
			"must be at least 32 bytes long.",
	}

	cmd.PersistentFlags().StringVar(&seedFile, seedFlag, "",
		"file holding the devnet seed (signer-pubkey, host-key, host-cert)")
	cmd.PersistentFlags().StringVar(&network, "network", "", "network name (required)")

	// seed reads the devnet seed.
	seed := func() ([]byte, error) {
		if network == "" {
			return nil, errors.New("--network is required")
		}

		s, err := readSeed(seedFile, seedEnv, seedFlag, "devnet seed")
		if err != nil {
			return nil, err
		}

		if string(s) == os.Getenv(controllerSeedEnv) {
			return nil, errSameSeed
		}

		return s, nil
	}

	var (
		controllerSeedFile   string
		controllerGeneration uint64
	)

	// controllerSeed reads the controller seed, and never the devnet seed.
	controllerSeed := func() ([]byte, error) {
		if network == "" {
			return nil, errors.New("--network is required")
		}

		if seedFile != "" {
			return nil, errors.New("--" + seedFlag + " is the devnet seed: the controller key " +
				"is derived from the controller seed (--" + controllerSeedFlag + " or " +
				controllerSeedEnv + ")")
		}

		s, err := readSeed(controllerSeedFile, controllerSeedEnv, controllerSeedFlag,
			"controller seed")
		if err != nil {
			return nil, err
		}

		if string(s) == os.Getenv(seedEnv) {
			return nil, errSameSeed
		}

		return s, nil
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

	controllerFlags := func(c *cobra.Command) {
		c.Flags().StringVar(&controllerSeedFile, controllerSeedFlag, "",
			"file holding the controller seed (never the devnet seed)")
		c.Flags().Uint64Var(&controllerGeneration, "controller-generation", 0,
			"controller key generation (the controller's --controller-generation)")
	}

	controllerPub := &cobra.Command{
		Use:   "controller-pubkey",
		Short: "Print the controller public key (an entry of the agents' controller.publicKeys)",
		Long: "Print the controller public key of a network, derived from the controller\n" +
			"seed: what the agents of that network list under controller.publicKeys.",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := controllerSeed()
			if err != nil {
				return err
			}

			key, err := identity.DeriveControllerKey(s, network, controllerGeneration)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), identity.EncodePublicKey(identity.PublicKey(key)))

			return err
		},
	}
	controllerFlags(controllerPub)

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
		Long: "Print a host's certificate (the content of /etc/upgradoor/host.crt).\n\n" +
			"The certificate names the host, its key generation and its roles. It does not\n" +
			"carry the host's validator range: the agent reads that on the host\n" +
			"(validators.markerFile or validators.start/end in agent.yaml) and reports it.",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed()
			if err != nil {
				return err
			}

			cert, err := identity.IssueCertificate(s, identity.CertificateSpec{
				Network: network, Hostname: hostname, Generation: generation, Roles: roles,
			})
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), cert)

			return err
		},
	}
	hostFlags(hostCert)
	hostCert.Flags().StringSliceVar(&roles, "role", nil, "role (repeatable)")

	var signerKey string

	verify := &cobra.Command{
		Use:   "verify-cert",
		Short: "Verify a host certificate read from standard input and print its claims (no seed needed)",
		Long: "Verify a host certificate read from standard input against a signer public key\n" +
			"and print its claims as JSON. Needs no seed. Certificates issued by the Ansible\n" +
			"filter plugin carry a randomized signature, so they are compared by verifying\n" +
			"them, not byte for byte.",
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
	verify.Flags().StringVar(&signerKey, "signer-public-key", "", "signer public key (required)")

	cmd.AddCommand(signer, controllerPub, hostKey, hostCert, verify)

	return cmd
}

// readSeed reads one seed from its file flag or its environment variable.
func readSeed(file, env, flag, what string) ([]byte, error) {
	if file != "" {
		s, err := identity.ReadSeedFile(file)
		if err != nil {
			return nil, fmt.Errorf("%s (--%s): %w", what, flag, err)
		}

		return s, nil
	}

	if v := os.Getenv(env); v != "" {
		return []byte(v), nil
	}

	return nil, fmt.Errorf("no %s: set --%s or %s", what, flag, env)
}
