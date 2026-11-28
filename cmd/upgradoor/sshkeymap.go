package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/logins"
)

// keyMapMode is the mode of the key map: root writes it, the agent's group
// reads it.
const keyMapMode fs.FileMode = 0o640

func newSSHKeyMapCmd() *cobra.Command {
	var (
		users []string
		out   string
		group string
	)

	cmd := &cobra.Command{
		Use:   "ssh-keymap",
		Short: "Write the SSH key map the agent names SSH logins by",
		Long: "Read the authorized_keys of the login users and write the map of their key\n" +
			"fingerprints to who the keys belong to: the keys after a \"# github:<name>\"\n" +
			"line are github:<name>'s, any other key is its comment's. The agent stamps\n" +
			"each SSH login with the owner of its key. Run as root: the file is written\n" +
			"root:<group> 0640.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if len(users) == 0 {
				return errors.New("name at least one --user")
			}

			m := make(logins.KeyMap, 16)

			for _, name := range users {
				if err := addUserKeys(m, name); err != nil {
					return err
				}
			}

			if err := writeKeyMap(out, group, m); err != nil {
				return err
			}

			_, err := fmt.Fprintf(c.OutOrStdout(), "%d keys written to %s\n", len(m), out)

			return err
		},
	}

	cmd.Flags().StringArrayVar(&users, "user", nil, "login user whose authorized_keys to read (repeatable)")
	cmd.Flags().StringVar(&out, "out", logins.DefaultKeyMap, "key map file")
	cmd.Flags().StringVar(&group, "group", "upgradoor", "group that reads the key map")

	return cmd
}

// addUserKeys adds the keys of a login user's ~/.ssh/authorized_keys; a user
// without the file adds none.
func addUserKeys(m logins.KeyMap, name string) error {
	u, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("user %s: %w", name, err)
	}

	raw, err := os.ReadFile(filepath.Join(u.HomeDir, ".ssh", "authorized_keys"))

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("user %s: %w", name, err)
	}

	m.AddAuthorizedKeys(name, raw)

	return nil
}

// writeKeyMap writes the map to path through a renamed temporary file, mode
// keyMapMode and, when run as root, owned by root and group.
func writeKeyMap(path, group string, m logins.KeyMap) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	gid := -1

	if os.Geteuid() == 0 {
		g, lookupErr := user.LookupGroup(group)
		if lookupErr != nil {
			return fmt.Errorf("group %s: %w", group, lookupErr)
		}

		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return fmt.Errorf("group %s: %w", group, err)
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	name := tmp.Name()

	err = tmp.Chmod(keyMapMode)
	if err == nil && gid >= 0 {
		err = tmp.Chown(0, gid)
	}

	if err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}

	if err == nil {
		err = tmp.Sync()
	}

	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(name, path)
	}

	if err != nil {
		_ = os.Remove(name)

		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}
