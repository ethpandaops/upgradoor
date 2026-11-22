package local

import (
	"path/filepath"
)

// Default locations.
const (
	DefaultSocket    = "/run/upgradoor/agent.sock"
	DefaultEntryUser = "devops"
)

// Config is local's part of agent.yaml, embedded inline in the root's Config.
type Config struct {
	// EntryUser is the user people log in as; its group may use the socket.
	EntryUser string `yaml:"entryUser"`
	// OverridesFile enables overrides; empty disables them.
	OverridesFile string `yaml:"overridesFile"`
	// ManagedFile is managed.yaml, which upgradoor deploy writes.
	ManagedFile string `yaml:"managedFile"`
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

// ApplyDefaults fills unset fields; the audit log defaults into stateDir.
// The overrides file has no default.
func (c *Config) ApplyDefaults(stateDir string) {
	str := func(field *string, def func() string) {
		if *field == "" {
			*field = def()
		}
	}

	str(&c.EntryUser, func() string { return DefaultEntryUser })
	str(&c.ManagedFile, func() string { return DefaultManagedFile })
	str(&c.Socket.Path, func() string { return DefaultSocket })
	str(&c.AuditLog.File, func() string { return filepath.Join(stateDir, auditFileName) })

	if c.AuditLog.MaxSizeBytes <= 0 {
		c.AuditLog.MaxSizeBytes = defaultAuditMaxSize
	}

	if c.AuditLog.MaxFiles <= 0 {
		c.AuditLog.MaxFiles = defaultAuditMaxFiles
	}
}
