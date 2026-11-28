package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/agent/logins"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	sshTestBoot = "0b1c2d3e4f5061728394a5b6c7d8e9f0"
	sshTestKey  = "SHA256:PIVpPQL34ZOi+Stv5JaVVcp1lrNzt/o/mfpzDa/OUig"
)

// journalLine is one sshd entry as journalctl --output=json writes it.
func journalLine(t *testing.T, n int, at time.Time, pid int, msg string) []byte {
	t.Helper()

	raw, err := json.Marshal(map[string]string{
		"__CURSOR":             fmt.Sprintf("s=abc;i=%x", n),
		"__REALTIME_TIMESTAMP": fmt.Sprint(at.UnixMicro()),
		"_BOOT_ID":             sshTestBoot,
		"_PID":                 fmt.Sprint(pid),
		"MESSAGE":              msg,
	})
	require.NoError(t, err)

	return raw
}

// sshSession is the journal lines of one key login and its logout.
func sshSession(t *testing.T, n, pid int, start, end time.Time) [][]byte {
	t.Helper()

	return [][]byte{
		journalLine(t, n, start, pid, fmt.Sprintf(
			"Accepted publickey for devops from 10.0.0.5 port %d ssh2: ED25519 %s", 40000+n, sshTestKey)),
		journalLine(t, n+1, end, pid, fmt.Sprintf("Disconnected from user devops 10.0.0.5 port %d", 40000+n)),
	}
}

// writeKeyMap writes a key map naming sshTestKey's owner.
func writeKeyMap(t *testing.T, path, identity string) {
	t.Helper()

	raw, err := json.Marshal(logins.KeyMap{sshTestKey: {Identity: identity, Source: logins.SourceGitHub, User: "devops"}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}

func newTestSSHLogins(t *testing.T, h *agenttest.Host, now func() time.Time) *sshLogins {
	t.Helper()

	dir := t.TempDir()
	cfg := &logins.Config{Enabled: true, KeyMap: filepath.Join(dir, "ssh-keys.json")}
	cfg.ApplyDefaults()

	log := logrus.New()
	log.SetOutput(io.Discard)

	s := newSSHLogins(log, cfg, h, dir, now)
	s.interval = 0

	return s
}

func TestSSHLoginsOff(t *testing.T) {
	t.Parallel()

	h, err := agenttest.NewHost(agenttest.BaseScenario())
	require.NoError(t, err)

	now := time.Now()
	h.SetSSHJournal(sshSession(t, 1, 100, now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))...)

	s := newTestSSHLogins(t, h, time.Now)
	s.cfg.Enabled = false
	s.accepted = true

	s.collect(t.Context(), sshTestBoot)

	features, report := s.report()
	assert.Nil(t, features)
	assert.Nil(t, report)
	assert.NoFileExists(t, s.path, "nothing is read or kept")
}

// The history goes out only after a verified answer accepted it, and its
// closed groups stay queued until a verified answer acknowledges them.
func TestSSHLoginsQueueAndAck(t *testing.T) {
	t.Parallel()

	h, err := agenttest.NewHost(agenttest.BaseScenario())
	require.NoError(t, err)

	now := time.Now().UTC()
	h.SetSSHJournal(sshSession(t, 1, 100, now.Add(-3*time.Hour), now.Add(-3*time.Hour+time.Minute))...)
	h.SetSSHJournal(sshSession(t, 3, 101, now.Add(-2*time.Hour), now.Add(-2*time.Hour+time.Minute))...)

	s := newTestSSHLogins(t, h, func() time.Time { return now })
	writeKeyMap(t, s.cfg.KeyMap, "github:pk910")

	s.collect(t.Context(), sshTestBoot)
	require.Len(t, s.history.Closed, 2, "both groups closed long ago")
	assert.Equal(t, "github:pk910", s.history.Closed[0].Identity, "stamped from the key map")

	features, report := s.report()
	assert.Nil(t, report, "nothing before the controller accepts it")
	assert.Nil(t, features)

	s.answered(&protocol.SyncResponse{Accepts: []string{protocol.FeatureSSHLogins}})

	features, report = s.report()
	assert.Equal(t, []string{protocol.FeatureSSHLogins}, features)
	require.NotNil(t, report)
	require.Len(t, report.Closed, 2)

	// A sync without a verified answer acknowledges nothing.
	s.failed()

	ack := report.Closed[1].Seq
	s.answered(&protocol.SyncResponse{Accepts: []string{protocol.FeatureSSHLogins}, SSHAck: &ack})
	assert.Len(t, s.history.Closed, 2, "an answer to another request acknowledges nothing")

	// An answer without an acknowledgement keeps them.
	_, _ = s.report()
	s.answered(&protocol.SyncResponse{Accepts: []string{protocol.FeatureSSHLogins}})
	assert.Len(t, s.history.Closed, 2)

	_, report = s.report()
	first := report.Closed[0].Seq
	s.answered(&protocol.SyncResponse{Accepts: []string{protocol.FeatureSSHLogins}, SSHAck: &first})
	require.Len(t, s.history.Closed, 1, "up to the acknowledged Seq")

	// After a restart the queue and the journal position are where they were.
	again := newSSHLogins(s.log, s.cfg, h, filepath.Dir(s.path), func() time.Time { return now.Add(time.Minute) })
	again.interval = 0
	again.collect(t.Context(), sshTestBoot)
	require.Len(t, again.history.Closed, 1, "no session is read twice")
	assert.Equal(t, s.history.Closed[0].Seq, again.history.Closed[0].Seq)

	// A controller that stops accepting gets nothing.
	again.answered(&protocol.SyncResponse{})

	_, report = again.report()
	assert.Nil(t, report)
}

// The key map is read again when it changes; a login keeps the owner its
// key had at login time.
func TestSSHLoginsKeyMap(t *testing.T) {
	t.Parallel()

	h, err := agenttest.NewHost(agenttest.BaseScenario())
	require.NoError(t, err)

	now := time.Now().UTC()
	clock := now
	s := newTestSSHLogins(t, h, func() time.Time { return clock })

	h.SetSSHJournal(sshSession(t, 1, 100, now.Add(-3*time.Hour), now.Add(-3*time.Hour+time.Minute))...)
	s.collect(t.Context(), sshTestBoot)
	require.Len(t, s.history.Closed, 1)
	assert.Empty(t, s.history.Closed[0].Identity, "no key map: unknown")
	assert.Equal(t, logins.SourceUnknown, s.history.Closed[0].Source)
	assert.Equal(t, sshTestKey, s.history.Closed[0].Fingerprint)

	writeKeyMap(t, s.cfg.KeyMap, "github:pk910")

	clock = clock.Add(time.Minute)
	h.SetSSHJournal(sshSession(t, 3, 101, clock.Add(-time.Second), clock)...)
	s.collect(t.Context(), sshTestBoot)
	require.Len(t, s.history.Groups, 1)
	assert.Equal(t, "github:pk910", s.history.Groups[0].Identity)
	assert.Empty(t, s.history.Closed[0].Identity, "an earlier login keeps its owner")
}

// A sync carries the history only once a verified answer of the controller
// accepted it.
func TestSSHLoginsReachTheController(t *testing.T) {
	t.Parallel()

	e := newEnv(t, agenttest.BaseScenario())
	e.cfg.SSHLogins.Enabled = true
	e.cfg.SSHLogins.KeyMap = filepath.Join(t.TempDir(), "ssh-keys.json")

	now := time.Now()
	e.host.SetSSHJournal(sshSession(t, 1, 100, now.Add(-2*time.Hour), now.Add(-2*time.Hour+time.Minute))...)
	e.start()

	e.ctrl.AwaitSyncs(3)

	for _, req := range e.ctrl.Syncs() {
		assert.Nil(t, req.SSH, "a controller that does not accept the history gets none")
		assert.Empty(t, req.Features)
	}

	e.ctrl.Set(func(c *agenttest.Controller) { c.Accepts = []string{protocol.FeatureSSHLogins} })

	req := e.ctrl.Await("the SSH logins", func(req *protocol.SyncRequest) bool { return req.SSH != nil })
	assert.Equal(t, []string{protocol.FeatureSSHLogins}, req.Features)
	require.Len(t, req.SSH.Closed, 1)
	assert.Equal(t, 1, req.SSH.Closed[0].Sessions)

	e.ctrl.Await("the acknowledged group gone", func(req *protocol.SyncRequest) bool {
		return req.SSH != nil && len(req.SSH.Closed) == 0
	})

	var hellos int

	e.ctrl.Set(func(c *agenttest.Controller) { hellos = c.Hellos })
	assert.Equal(t, 1, hellos, "every sync is sealed to the keys of the one hello")
}
