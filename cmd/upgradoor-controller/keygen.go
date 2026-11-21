package main

import (
	"bytes"
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
	// maxBatchHosts and maxBatchInput bound the host list host-certs reads.
	maxBatchHosts = 20000
	maxBatchInput = 16 << 20
)

var errSameSeed = errors.New("the devnet seed and the controller seed are the same value: " +
	"they must be two different secrets")

// newKeygenCmd returns the operator-side key tool; Ansible runs it in the
// release image to derive keys and certificates.
func newKeygenCmd() *cobra.Command {
	var (
		seedFile string
		network  string
		hostname string
		roles    []string
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

	var controllerSeedFile string

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

	controllerPub := &cobra.Command{
		Use:   "controller-pubkey",
		Short: "Print the controller public key (the agents' controller.publicKey)",
		Long: "Print the controller public key of a network, derived from the controller\n" +
			"seed: the agents' controller.publicKey.",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := controllerSeed()
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
		"file holding the controller seed (never the devnet seed)")

	hostKey := &cobra.Command{
		Use:   "host-key",
		Short: "Print a host's private key (the content of /etc/upgradoor/host.key)",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed()
			if err != nil {
				return err
			}

			key, err := hostKeyFile(s, network, hostname)
			if err != nil {
				return err
			}

			_, err = io.WriteString(c.OutOrStdout(), key)

			return err
		},
	}
	hostFlags(hostKey)

	hostCert := &cobra.Command{
		Use:   "host-cert",
		Short: "Print a host's certificate (the content of /etc/upgradoor/host.crt)",
		Long: "Print a host's certificate (the content of /etc/upgradoor/host.crt).\n\n" +
			"The certificate names the host and its roles. It does not carry the host's\n" +
			"validator range: the agent reads that on the host\n" +
			"(validators.markerFile or validators.start/end in agent.yaml) and reports it.",
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed()
			if err != nil {
				return err
			}

			cert, err := hostCertFile(s, identity.CertificateSpec{
				Network: network, Hostname: hostname, Roles: roles,
			})
			if err != nil {
				return err
			}

			_, err = io.WriteString(c.OutOrStdout(), cert)

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
			"and print its claims as JSON. Needs no seed.",
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

	hostCerts := &cobra.Command{
		Use:   "host-certs",
		Short: "Print the keys and certificates of many hosts read as JSON from standard input",
		Long: "Read a JSON array of hosts from standard input, each\n" +
			"  {\"hostname\": \"...\", \"roles\": [\"...\"]}\n" +
			"and print one JSON object keyed by hostname, each value\n" +
			"  {\"key\": \"<host-key output>\", \"cert\": \"<host-cert output>\"}\n" +
			"Every entry is validated before anything is printed.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := seed()
			if err != nil {
				return err
			}

			return issueHostCerts(c.InOrStdin(), c.OutOrStdout(), s, network)
		},
	}

	cmd.AddCommand(signer, controllerPub, hostKey, hostCert, hostCerts, verify)

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

// hostKeyFile returns the content of a host's host.key.
func hostKeyFile(seed []byte, network, hostname string) (string, error) {
	key, err := identity.DeriveHostKey(seed, network, hostname)
	if err != nil {
		return "", err
	}

	return identity.EncodePrivateKey(key) + "\n", nil
}

// hostCertFile returns the content of a host's host.crt.
func hostCertFile(seed []byte, spec identity.CertificateSpec) (string, error) {
	cert, err := identity.IssueCertificate(seed, spec)
	if err != nil {
		return "", err
	}

	return cert + "\n", nil
}

// batchHost is one host-certs input entry: the flags of host-key and host-cert.
type batchHost struct {
	Hostname string   `json:"hostname"`
	Roles    []string `json:"roles"`
}

// hostFiles is one host-certs output entry.
type hostFiles struct {
	Key  string `json:"key"`
	Cert string `json:"cert"`
}

// issueHostCerts reads a JSON array of hosts from in and writes the key and
// certificate of each, keyed by hostname, to out. Nothing is written unless
// every entry is valid.
func issueHostCerts(in io.Reader, out io.Writer, seed []byte, network string) error {
	raw, err := io.ReadAll(io.LimitReader(in, maxBatchInput+1))
	if err != nil {
		return fmt.Errorf("read hosts: %w", err)
	}

	if len(raw) > maxBatchInput {
		return fmt.Errorf("hosts input is larger than %d bytes", maxBatchInput)
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("hosts input is not a JSON array: %w", err)
	}

	if len(entries) > maxBatchHosts {
		return fmt.Errorf("%d hosts, at most %d per run", len(entries), maxBatchHosts)
	}

	hosts := make([]batchHost, 0, len(entries))
	seen := make(map[string]int, len(entries))

	for i, entry := range entries {
		var h batchHost

		dec := json.NewDecoder(bytes.NewReader(entry))
		dec.DisallowUnknownFields()

		if err := dec.Decode(&h); err != nil {
			return fmt.Errorf("host %d: %w", i, err)
		}

		if err := identity.ValidateName(h.Hostname); err != nil {
			return fmt.Errorf("host %d: hostname: %w", i, err)
		}

		if j, ok := seen[h.Hostname]; ok {
			return fmt.Errorf("host %d (%s): duplicate of host %d", i, h.Hostname, j)
		}

		seen[h.Hostname] = i

		for _, role := range h.Roles {
			if err := identity.ValidateName(role); err != nil {
				return fmt.Errorf("host %d (%s): role: %w", i, h.Hostname, err)
			}
		}

		hosts = append(hosts, h)
	}

	files := make(map[string]hostFiles, len(hosts))

	for i, h := range hosts {
		key, err := hostKeyFile(seed, network, h.Hostname)
		if err != nil {
			return fmt.Errorf("host %d (%s): %w", i, h.Hostname, err)
		}

		cert, err := hostCertFile(seed, identity.CertificateSpec{
			Network: network, Hostname: h.Hostname, Roles: h.Roles,
		})
		if err != nil {
			return fmt.Errorf("host %d (%s): %w", i, h.Hostname, err)
		}

		files[h.Hostname] = hostFiles{Key: key, Cert: cert}
	}

	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	return enc.Encode(files)
}
