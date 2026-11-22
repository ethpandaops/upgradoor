package local

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestApplyDefaults(t *testing.T) {
	var c Config
	c.EntryUser = "nobody-here"
	c.ApplyDefaults("/var/lib/upgradoor")

	assert.Equal(t, Config{
		EntryUser: "nobody-here", ManagedFile: DefaultManagedFile,
		Socket:   Socket{Path: DefaultSocket},
		AuditLog: AuditConfig{File: "/var/lib/upgradoor/audit.log", MaxSizeBytes: 1 << 20, MaxFiles: 5},
	}, c)

	c = Config{}
	c.ApplyDefaults("/s")
	assert.Equal(t, DefaultEntryUser, c.EntryUser)
	assert.Empty(t, c.OverridesFile, "overrides are opt-in")
}
