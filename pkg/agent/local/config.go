package local

import (
	"os/user"
	"path/filepath"
)

// Default locations.
const (
	DefaultSocket    = "/run/upgradoor/agent.sock"
	DefaultEntryUser = "devops"
)

// Config is local's part of agent.yaml, embedded inline in the root's Config.
type Config struct {
	// EntryUser is the user people log in as; the overrides file lives in
	// its home and its group may use the socket.
	EntryUser string `yaml:"entryUser"`
	// OverridesFile defaults to overrides.yaml in the entry user's home.
	OverridesFile string `yaml:"overridesFile"`
	// Socket is the socket of the on-host commands.
	Socket Socket `yaml:"socket"`
	// AuditLog defaults to audit.log in stateDir.
	AuditLog AuditConfig `yaml:"auditLog"`
}

// Socket is the local Unix socket of the agent.
type Socket struct {
	Path string `yaml:"path"`
	// Group may use the socket besides root (mode 0660).
	Group string `yaml:"group"`
}

// AuditConfig is the audit log and its rotation.
type AuditConfig struct {
	File string `yaml:"file"`
	// MaxSizeBytes is the size at which the file is rotated.
	MaxSizeBytes int64 `yaml:"maxSizeBytes"`
	// MaxFiles is how many rotated files are kept.
	MaxFiles int `yaml:"maxFiles"`
}

// ApplyDefaults fills unset fields; the audit log defaults into stateDir and
// the overrides file into the entry user's home.
func (c *Config) ApplyDefaults(stateDir string) {
	str := func(field *string, def func() string) {
		if *field == "" {
			*field = def()
		}
	}

	str(&c.EntryUser, func() string { return DefaultEntryUser })
	str(&c.OverridesFile, func() string { return filepath.Join(homeOf(c.EntryUser), overridesFileName) })
	str(&c.Socket.Path, func() string { return DefaultSocket })
	str(&c.AuditLog.File, func() string { return filepath.Join(stateDir, auditFileName) })

	if c.AuditLog.MaxSizeBytes <= 0 {
		c.AuditLog.MaxSizeBytes = defaultAuditMaxSize
	}

	if c.AuditLog.MaxFiles <= 0 {
		c.AuditLog.MaxFiles = defaultAuditMaxFiles
	}
}

const overridesFileName = "overrides.yaml"

// homeOf is a user's home as the system knows it, else /home/<name>.
func homeOf(name string) string {
	if u, err := user.Lookup(name); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}

	return "/home/" + name
}
