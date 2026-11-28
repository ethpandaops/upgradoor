package logins

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return raw
}

func TestParseJournal(t *testing.T) {
	t.Parallel()

	entries, err := ParseJournal(readFixture(t, "debian13-sshd-session.json"))
	require.NoError(t, err)
	require.Len(t, entries, 10)

	first := entries[0]
	assert.Equal(t, time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC), first.Time)
	assert.Equal(t, "9f8e7d6c5b4a39281706f5e4d3c2b1a0", first.Boot)
	assert.Equal(t, 2000, first.PID)
	assert.Contains(t, first.Message, "Accepted publickey for devops")
	assert.Contains(t, first.Cursor, "i=1;")
}

func TestParseJournalMessages(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"__CURSOR":"c1","__REALTIME_TIMESTAMP":"1000000","SYSLOG_PID":"7","MESSAGE":[68,105,115,99,255]}
{"__CURSOR":"c2","__REALTIME_TIMESTAMP":"2000000","_PID":"8","MESSAGE":null}
{"__REALTIME_TIMESTAMP":"3000000","_PID":"9","MESSAGE":"no cursor"}

{"__CURSOR":"c4","__REALTIME_TIMESTAMP":"x","_PID":"9","MESSAGE":"no time"}
`)

	entries, err := ParseJournal(raw)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "Disc\xff", entries[0].Message, "a message that is not UTF-8 comes as bytes")
	assert.Equal(t, 7, entries[0].PID, "SYSLOG_PID without _PID")
	assert.Empty(t, entries[1].Message)

	_, err = ParseJournal([]byte("{not json\n"))
	require.Error(t, err)
}
