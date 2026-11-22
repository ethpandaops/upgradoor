package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// The two seeds and where each is read from. They are different secrets and
// are never read from the same place: the devnet seed yields the signer key
// of one devnet, the controller seed (one per group of devnets) the
// controller key of each devnet of the group.
const (
	seedEnv            = "UPGRADOOR_SEED"
	seedFlag           = "seed-file"
	controllerSeedEnv  = "UPGRADOOR_CONTROLLER_SEED"
	controllerSeedFlag = "controller-seed-file"
)

var errSameSeed = errors.New("the devnet seed and the controller seed are the same value: " +
	"they must be two different secrets")

// newKeygenCmd returns the operator-side key tool; Ansible runs it in the
// release image to issue host certificates.
func newKeygenCmd() *cobra.Command {
	var (
		seedFile  string
		network   string
		hostname  string
		publicKey string
	)

	cmd := &cobra.Command{
		Use: "keygen",
		Short: "Derive the signer key and issue host certificates from the devnet seed, " +
			"and controller keys from the controller seed",
		Long: "Derive keys and issue host certificates. There are two seeds, and they are\n" +
			"different secrets:\n\n" +
			"  the devnet seed      one per devnet: signer-pubkey, host-cert\n" +
			"                       --" + seedFlag + " or " + seedEnv + "\n" +
			"  the controller seed  one per devnet group: controller-pubkey\n" +
			"                       --" + controllerSeedFlag + " or " + controllerSeedEnv + "\n\n" +
			"A command reads only its own seed and refuses the other one's flag. Neither\n" +
			"seed is ever taken from an argument. Both are used as raw bytes, exactly as\n" +
			"stored in sops (a single trailing newline of a seed file is dropped), and\n" +
			"must be at least 32 bytes long. A seed file of - is standard input:\n\n" +
			"  sops -d --extract '[\"secret_upgradoor_seed\"]' all.sops.yaml |\n" +
			"    docker run --rm -i ghcr.io/ethpandaops/upgradoor keygen signer-pubkey \\\n" +
			"    --network <network> --" + seedFlag + " -",
	}

	cmd.PersistentFlags().StringVar(&seedFile, seedFlag, "",
		"file holding the devnet seed (signer-pubkey, host-cert); - reads standard input")
	cmd.PersistentFlags().StringVar(&network, "network", "", "network name (required)")

	// seed reads the devnet seed.
	seed := func(stdin io.Reader) ([]byte, error) {
		if network == "" {
			return nil, errors.New("--network is required")
		}

		s, err := readSeed(stdin, seedFile, seedEnv, seedFlag, "devnet seed")
		if err != nil {
			return nil, err
		}

		if string(s) == os.Getenv(controllerSeedEnv) {
			return nil, errSameSeed
		}

		return s, nil
	}

	var controllerSeedFile string

	// controllerSeed reads the controller seed, and never the devnet seed.
	controllerSeed := func(stdin io.Reader) ([]byte, error) {
		if network == "" {
			return nil, errors.New("--network is required")
		}

		if seedFile != "" {
			return nil, errors.New("--" + seedFlag + " is the devnet seed: the controller key " +
				"is derived from the controller seed (--" + controllerSeedFlag + " or " +
				controllerSeedEnv + ")")
		}

		s, err := readSeed(stdin, controllerSeedFile, controllerSeedEnv, controllerSeedFlag,
			"controller seed")
		if err != nil {
			return nil, err
		}

		if string(s) == os.Getenv(seedEnv) {
			return nil, errSameSeed
		}

		return s, nil
	}

	signer := &cobra.Command{
		Use:   "signer-pubkey",
		Short: "Print the signer public key (the controller's signerPublicKey)",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed(c.InOrStdin())
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

	controllerPub := &cobra.Command{
		Use:   "controller-pubkey",
		Short: "Print the controller public key (the agents' controller.publicKey)",
		Long: "Print the controller public key of a network, derived from the controller\n" +
			"seed: the agents' controller.publicKey.",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := controllerSeed(c.InOrStdin())
			if err != nil {
				return err
			}

			key, err := identity.DeriveControllerKey(s, network)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), identity.EncodePublicKey(identity.PublicKey(key)))

			return err
		},
	}
	controllerPub.Flags().StringVar(&controllerSeedFile, controllerSeedFlag, "",
		"file holding the controller seed (never the devnet seed); - reads standard input")

	hostCert := &cobra.Command{
		Use:   "host-cert",
		Short: "Print a host's certificate (the content of /etc/upgradoor/host.crt)",
		Long: "Print the certificate of a host's public key (the output of upgradoor key on\n" +
			"the host), the content of /etc/upgradoor/host.crt.\n\n" +
			"The certificate names the host, its network and its public key. The\n" +
			"controller takes the host's validators from the validator ranges inventory\n" +
			"by this name.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed(c.InOrStdin())
			if err != nil {
				return err
			}

			pub, err := identity.DecodePublicKey(publicKey)
			if err != nil {
				return fmt.Errorf("--public-key: %w", err)
			}

			cert, err := identity.IssueCertificate(s, pub, identity.CertificateSpec{
				Network: network, Hostname: hostname,
			})
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), cert)

			return err
		},
	}
	hostCert.Flags().StringVar(&hostname, "hostname", "", "inventory hostname (required)")
	hostCert.Flags().StringVar(&publicKey, "public-key", "", "the host's public key (required)")

	var signerKey string

	verify := &cobra.Command{
		Use:   "verify-cert",
		Short: "Verify a host certificate read from standard input and print its claims (no seed needed)",
		Long: "Verify a host certificate read from standard input against a signer public key\n" +
			"and print its hostname, network and public key as JSON. Needs no seed.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			info, err := identity.ReadCertificate(c.InOrStdin(), signerKey, network)
			if err != nil {
				return err
			}

			return json.NewEncoder(c.OutOrStdout()).Encode(info)
		},
	}
	verify.Flags().StringVar(&signerKey, "signer-public-key", "", "signer public key (required)")

	cmd.AddCommand(signer, controllerPub, hostCert, verify)

	return cmd
}

// readSeed reads one seed from its file flag ("-" is standard input) or its
// environment variable.
func readSeed(stdin io.Reader, file, env, flag, what string) ([]byte, error) {
	if file != "" {
		read := func() ([]byte, error) { return identity.ReadSeedFile(file) }
		if file == "-" {
			read = func() ([]byte, error) { return identity.ReadSeed(stdin) }
		}

		s, err := read()
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
