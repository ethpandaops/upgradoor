package main

import (
	"crypto/mldsa"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

func newKeyCmd() *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "key",
		Short: "Create the host key if it is missing and print its public key",
		Long: "Create the host key if it is missing and print its public key, the input of\n" +
			"upgradoor-controller keygen host-cert. A new key is mode 0640, owned by the\n" +
			"caller and the group of its directory, so the agent reads it through that\n" +
			"group (/etc/upgradoor is root:upgradoor 0750). An existing key is kept as it is.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			key, err := ensureHostKey(path)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintln(c.OutOrStdout(), identity.EncodePublicKey(identity.PublicKey(key)))

			return err
		},
	}

	cmd.Flags().StringVar(&path, "path", agent.DefaultKeyFile, "host key file")

	return cmd
}

// ensureHostKey reads the host key at path, or creates it there when the
// file is missing.
func ensureHostKey(path string) (*mldsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)

	switch {
	case err == nil:
		key, decodeErr := identity.DecodePrivateKey(string(raw))
		if decodeErr != nil {
			return nil, fmt.Errorf("host key %s: %w", path, decodeErr)
		}

		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("read host key: %w", err)
	}

	key, err := identity.GenerateHostKey()
	if err != nil {
		return nil, err
	}

	if err := writeHostKey(path, identity.EncodePrivateKey(key)+"\n"); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}

	return key, nil
}

// writeHostKey writes a complete key file and links it into place, never
// over an existing one.
func writeHostKey(path, content string) error {
	dir := filepath.Dir(path)

	info, err := os.Stat(dir)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".host.key-*")
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	gid := -1
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		gid = int(st.Gid)
	}

	err = errors.Join(tmp.Chown(-1, gid), tmp.Chmod(0o640))
	if err == nil {
		_, err = tmp.WriteString(content)
	}

	if err == nil {
		err = tmp.Sync()
	}

	if closeErr := tmp.Close(); err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}

	return os.Link(tmp.Name(), path)
}
