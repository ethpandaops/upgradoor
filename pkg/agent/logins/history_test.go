package logins

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	boot12 = "0b1c2d3e4f5061728394a5b6c7d8e9f0"
	boot13 = "9f8e7d6c5b4a39281706f5e4d3c2b1a0"
)

var testKeys = KeyMap{
	fpGitHub:  {Identity: "github:pk910", Source: SourceGitHub, User: "devops"},
	fpComment: {Identity: "ops@laptop", Source: SourceComment, User: "devops"},
}

func at(hms string) time.Time {
	t, err := time.Parse(time.DateTime, "2026-11-27 "+hms)
	if err != nil {
		panic(err)
	}

	return t.UTC()
}

func applyFixture(t *testing.T, h *History, name string) {
	t.Helper()

	entries, err := ParseJournal(readFixture(t, name))
	require.NoError(t, err)

	h.Apply(entries, testKeys.Owner)
}

func ptr(t time.Time) *time.Time { return &t }

func TestHistoryDebian12(t *testing.T) {
	t.Parallel()

	h := NewHistory(at("09:00:00"))
	seq := h.NextSeq
	applyFixture(t, h, "debian12-sshd.json")

	assert.Contains(t, h.Cursor, "i=f;", "the cursor moves to the last entry")
	assert.Equal(t, at("10:05:00"), h.ReadTo)

	require.Len(t, h.Groups, 2, "failed and unfinished logins are no sessions")
	assert.Equal(t, protocol.SSHLoginGroup{
		Seq: seq, Identity: "github:pk910", Source: SourceGitHub, Fingerprint: fpGitHub, KeyType: "ED25519",
		User: "devops", From: "10.0.0.5", FirstLogin: at("10:00:00"), LastLogout: ptr(at("10:02:30")),
		Sessions: 2, LongestMillis: 30000, TotalMillis: 35000,
	}, h.Groups[0], "the logout of the child process pairs by address, the second session joins")
	assert.Equal(t, protocol.SSHLoginGroup{
		Seq: seq + 1, Source: SourceUnknown, KeyType: "password", User: "alice", From: "192.168.1.9",
		FirstLogin: at("10:05:00"), Sessions: 1, Open: 1,
	}, h.Groups[1], "a login without a key is named by its method")
	require.Len(t, h.Sessions, 1)
	assert.Equal(t, Session{
		Boot: boot12, PID: 1200, From: "192.168.1.9", Port: 40000, Group: seq + 1,
		Start: at("10:05:00"), LastSeen: at("10:05:00"),
	}, h.Sessions[0])

	h.CloseIdle(at("10:12:29"))
	assert.Len(t, h.Groups, 2, "within the window after the last logout")

	h.CloseIdle(at("10:12:30"))
	require.Len(t, h.Groups, 1)
	require.Len(t, h.Closed, 1)
	assert.Equal(t, seq, h.Closed[0].Seq)

	h.CloseIdle(at("23:00:00"))
	assert.Len(t, h.Groups, 1, "a group with an open session stays open")
}

func TestHistoryDebian13(t *testing.T) {
	t.Parallel()

	h := NewHistory(at("09:00:00"))
	seq := h.NextSeq
	applyFixture(t, h, "debian13-sshd-session.json")

	require.Len(t, h.Groups, 3)

	comment := h.Groups[0]
	assert.Equal(t, "ops@laptop", comment.Identity)
	assert.Equal(t, SourceComment, comment.Source)
	assert.Equal(t, 1, comment.Sessions)
	assert.Equal(t, int64(30*time.Minute/time.Millisecond), comment.TotalMillis)
	assert.Equal(t, at("11:30:00"), *comment.LastLogout, "the first of the logout lines ends it")

	unknown := h.Groups[1]
	assert.Empty(t, unknown.Identity)
	assert.Equal(t, SourceUnknown, unknown.Source)
	assert.Equal(t, fpPlain, unknown.Fingerprint)
	assert.Equal(t, "RSA", unknown.KeyType)
	assert.Equal(t, at("11:45:00"), *unknown.LastLogout, "a timeout ends it")

	later := h.Groups[2]
	assert.Equal(t, seq+2, later.Seq, "20 minutes after the logout: a new group")
	assert.Equal(t, "ops@laptop", later.Identity)
	assert.Equal(t, 1, later.Open)
}

func TestHistoryEndsUnknown(t *testing.T) {
	t.Parallel()

	alive := map[int]bool{1200: true}
	isAlive := func(pid int) bool { return alive[pid] }

	h := NewHistory(at("09:00:00"))
	applyFixture(t, h, "debian12-sshd.json")

	h.Reap(boot12, isAlive, at("10:20:00"))
	require.Len(t, h.Sessions, 1)
	assert.Equal(t, at("10:20:00"), h.Sessions[0].LastSeen, "seen running")

	// The agent restarted: same boot, the process is gone.
	delete(alive, 1200)
	h.Reap(boot12, isAlive, at("10:20:30"))
	require.Len(t, h.Sessions, 1, "its logout may still be on the way")

	h.Reap(boot12, isAlive, at("10:21:00"))
	assert.Empty(t, h.Sessions)

	g := h.Groups[len(h.Groups)-1]
	assert.Equal(t, "alice", g.User)
	assert.True(t, g.EndUnknown)
	assert.Equal(t, at("10:20:00"), *g.LastLogout, "ended at its last sighting")
	assert.Equal(t, int64(15*time.Minute/time.Millisecond), g.TotalMillis)

	// A reboot ends every session of the old boot at once.
	h2 := NewHistory(at("09:00:00"))
	applyFixture(t, h2, "debian12-sshd.json")
	h2.Reap("another-boot", func(int) bool { return true }, at("12:00:00"))
	assert.Empty(t, h2.Sessions)
	assert.True(t, h2.Groups[len(h2.Groups)-1].EndUnknown)
	assert.Equal(t, at("10:05:00"), *h2.Groups[len(h2.Groups)-1].LastLogout)

	// A late logout of a session already ended changes nothing.
	h2.Apply([]Entry{{Cursor: "late", Time: at("12:01:00"), Boot: boot12, PID: 1200,
		Message: "pam_unix(sshd:session): session closed for user alice"}}, nil)
	assert.Equal(t, at("10:05:00"), *h2.Groups[len(h2.Groups)-1].LastLogout)
}

// session is a journal login and its logout.
func session(pid, port int, start, end time.Time, user, from, fp string) []Entry {
	return []Entry{
		{
			Cursor: fmt.Sprintf("c%d", pid), Time: start, Boot: boot13, PID: pid,
			Message: fmt.Sprintf("Accepted publickey for %s from %s port %d ssh2: ED25519 %s", user, from, port, fp),
		},
		{
			Cursor: fmt.Sprintf("e%d", pid), Time: end, Boot: boot13, PID: pid,
			Message: fmt.Sprintf("Disconnected from user %s %s port %d", user, from, port),
		},
	}
}

func TestHistoryGroups(t *testing.T) {
	t.Parallel()

	h := NewHistory(at("09:00:00"))
	start := at("14:00:00")

	// An Ansible run: many short sessions, one after another.
	for i := range 300 {
		s := start.Add(time.Duration(i) * 3 * time.Second)
		h.Apply(session(3000+i, 40000+i, s, s.Add(2*time.Second), "devops", "10.0.0.9", fpGitHub), testKeys.Owner)
	}

	require.Len(t, h.Groups, 1, "one group")
	assert.Equal(t, 300, h.Groups[0].Sessions)
	assert.Equal(t, int64(600000), h.Groups[0].TotalMillis)
	assert.Equal(t, int64(2000), h.Groups[0].LongestMillis)

	last := start.Add(299*3*time.Second + 2*time.Second)

	// Another user, another address and another key are other groups.
	h.Apply(session(5000, 1, last, last.Add(time.Second), "root", "10.0.0.9", fpGitHub), testKeys.Owner)
	h.Apply(session(5001, 2, last, last.Add(time.Second), "devops", "10.0.0.10", fpGitHub), testKeys.Owner)
	h.Apply(session(5002, 3, last, last.Add(time.Second), "devops", "10.0.0.9", fpComment), testKeys.Owner)
	h.Apply(session(5003, 4, last, last.Add(time.Second), "devops", "10.0.0.9", fpPlain), testKeys.Owner)
	assert.Len(t, h.Groups, 5)

	// Within the window after the last logout a session joins.
	join := last.Add(Window)
	h.Apply(session(6000, 5, join, join.Add(time.Minute), "devops", "10.0.0.9", fpGitHub), testKeys.Owner)
	assert.Equal(t, 301, h.Groups[0].Sessions)

	// Past the window it starts a new group.
	late := join.Add(time.Minute + Window + time.Second)
	h.Apply(session(6001, 6, late, late.Add(time.Minute), "devops", "10.0.0.9", fpGitHub), testKeys.Owner)
	assert.Len(t, h.Groups, 6)

	// A session that starts while one is open joins, however long it lasts.
	h3 := NewHistory(at("09:00:00"))
	h3.Apply(session(1, 1, at("10:00:00"), at("13:00:00"), "devops", "10.0.0.9", fpGitHub)[:1], testKeys.Owner)
	h3.Apply(session(2, 2, at("12:00:00"), at("12:01:00"), "devops", "10.0.0.9", fpGitHub), testKeys.Owner)
	require.Len(t, h3.Groups, 1)
	assert.Equal(t, 2, h3.Groups[0].Sessions)
	assert.Equal(t, 1, h3.Groups[0].Open)

	// A login read twice is one session.
	h3.Apply(session(1, 1, at("10:00:00"), at("13:00:00"), "devops", "10.0.0.9", fpGitHub)[:1], testKeys.Owner)
	assert.Equal(t, 2, h3.Groups[0].Sessions)
}

func TestHistoryQueue(t *testing.T) {
	t.Parallel()

	h := NewHistory(at("00:00:00"))
	assert.Equal(t, at("00:00:00").UnixMilli(), h.NextSeq, "the sequence starts above any earlier one")

	start := at("01:00:00")
	for i := range MaxClosed + 150 {
		s := start.Add(time.Duration(i) * time.Hour)
		h.Apply(session(i+1, i+1, s, s.Add(time.Second), "devops", "10.0.0.9", fpGitHub), testKeys.Owner)
		h.CloseIdle(s.Add(Window + time.Second))
	}

	assert.Len(t, h.Closed, MaxClosed)
	assert.Equal(t, int64(150), h.Dropped, "the oldest are dropped and counted")
	assert.Equal(t, h.NextSeq-MaxClosed, h.Closed[0].Seq)

	sent := h.Report()
	require.Len(t, sent.Closed, protocol.MaxSSHClosed, "the oldest first")
	assert.Equal(t, h.Closed[0].Seq, sent.Closed[0].Seq)
	assert.Equal(t, int64(150), sent.Dropped)
	assert.Empty(t, sent.Open)

	// The report is a copy.
	sent.Closed[0].User = "changed"
	assert.Equal(t, "devops", h.Closed[0].User)

	// An acknowledgement covers what was sent up to its Seq.
	h.Ack(sent, sent.Closed[49].Seq)
	assert.Len(t, h.Closed, MaxClosed-50)
	assert.Equal(t, sent.Closed[50].Seq, h.Closed[0].Seq)
	assert.Zero(t, h.Dropped)

	// A group that closed after the report keeps waiting.
	next := h.Report()
	s := start.Add(time.Duration(MaxClosed+200) * time.Hour)
	h.Apply(session(9999, 1, s, s.Add(time.Second), "devops", "10.0.0.9", fpGitHub), testKeys.Owner)
	h.CloseIdle(s.Add(Window + time.Second))
	h.Ack(next, h.NextSeq)
	require.Len(t, h.Closed, MaxClosed-50-protocol.MaxSSHClosed+1)
	assert.Equal(t, h.NextSeq-1, h.Closed[len(h.Closed)-1].Seq)
}
