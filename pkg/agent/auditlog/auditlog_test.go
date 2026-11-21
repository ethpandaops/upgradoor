package auditlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLine(t *testing.T) {
	assert.Equal(t, "first line", Line("  first line \nsecond"))
	assert.Equal(t, "a b", Line("a\tb"))
	assert.Equal(t, "bad ? byte", Line("bad \xff byte"))
	assert.Len(t, Line(strings.Repeat("x", 5000)), MaxLine)
	assert.Equal(t, "refused", ErrRefused.Error())
}

func TestAuditLogRotates(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)

	cfg := Config{
		File: filepath.Join(dir, "sub", "audit.log"), MaxSizeBytes: 400, MaxFiles: 2,
	}
	a := New(logrus.New(), cfg, func() time.Time { return now })

	for i := range 40 {
		a.Record(Record{
			Action: "override.set", Source: SourceController, User: "sam@example.org",
			Teams: []string{"geth"}, Feature: "team_image_override", Container: "geth",
			Before: "nothing", After: "image ethpandaops/geth:fix", Lease: "", Result: Accepted,
			Reason: string(rune('a' + i%26)),
		})
	}

	entries, err := os.ReadDir(filepath.Dir(cfg.File))
	require.NoError(t, err)

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		names = append(names, e.Name())

		info, infoErr := e.Info()
		require.NoError(t, infoErr)
		assert.LessOrEqual(t, info.Size(), cfg.MaxSizeBytes, e.Name())
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	assert.Equal(t, []string{"audit.log", "audit.log.1", "audit.log.2"}, names,
		"the file and the two rotated ones, nothing older")

	f, err := os.Open(cfg.File)
	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	var last Record

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		require.NoError(t, json.Unmarshal(sc.Bytes(), &last), "every line is one record")
	}

	assert.Equal(t, "n", last.Reason, "the newest record is in the file itself")
	assert.Equal(t, now, last.Time)
	assert.Equal(t, "sam@example.org", last.User)
}
