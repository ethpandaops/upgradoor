package logins

import (
	"errors"
	"strings"
	"time"
)

// Defaults of the sshLogins section.
const (
	DefaultBacklog = 7 * 24 * time.Hour
	DefaultKeyMap  = "/etc/upgradoor/ssh-keys.json"
)

// Config is the sshLogins section of agent.yaml.
type Config struct {
	// Enabled reads the SSH logins from the journal and reports them. Off,
	// nothing is read or sent.
	Enabled bool `yaml:"enabled"`
	// Backlog is how far back the journal is read when the history starts
	// (default 168h).
	Backlog time.Duration `yaml:"backlog"`
	// KeyMap is the file "upgradoor ssh-keymap" writes.
	KeyMap string `yaml:"keyMap"`
}

// ApplyDefaults fills the unset fields.
func (c *Config) ApplyDefaults() {
	if c.Backlog <= 0 {
		c.Backlog = DefaultBacklog
	}

	if c.KeyMap == "" {
		c.KeyMap = DefaultKeyMap
	}
}

// Validate checks the key map path.
func (c *Config) Validate() error {
	if c.KeyMap != "" && !strings.HasPrefix(c.KeyMap, "/") {
		return errors.New("sshLogins.keyMap must be an absolute path")
	}

	return nil
}
