package agent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditLogRotates(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)

	cfg := &Config{AuditLog: AuditLogConfig{
		File: filepath.Join(dir, "sub", "audit.log"), MaxSizeBytes: 400, MaxFiles: 2,
	}}
	a := newAuditLog(logrus.New(), cfg, func() time.Time { return now })

	for i := range 40 {
		a.record(AuditRecord{
			Action: "override.set", Source: auditController, User: "sam@example.org",
			Teams: []string{"geth"}, Feature: "team_image_override", Container: "geth",
			Before: "nothing", After: "image ethpandaops/geth:fix", Lease: "", Result: auditAccepted,
			Reason: string(rune('a' + i%26)),
		})
	}

	entries, err := os.ReadDir(filepath.Dir(cfg.AuditLog.File))
	require.NoError(t, err)

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		names = append(names, e.Name())

		info, infoErr := e.Info()
		require.NoError(t, infoErr)
		assert.LessOrEqual(t, info.Size(), cfg.AuditLog.MaxSizeBytes, e.Name())
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	assert.Equal(t, []string{"audit.log", "audit.log.1", "audit.log.2"}, names,
		"the file and the two rotated ones, nothing older")

	f, err := os.Open(cfg.AuditLog.File)
	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	var last AuditRecord

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		require.NoError(t, json.Unmarshal(sc.Bytes(), &last), "every line is one record")
	}

	assert.Equal(t, "n", last.Reason, "the newest record is in the file itself")
	assert.Equal(t, now, last.Time)
	assert.Equal(t, "sam@example.org", last.User)
}
