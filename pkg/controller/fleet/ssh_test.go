package fleet

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSHLoginGroupActor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ssh:github:pk910", (&SSHLoginGroup{Identity: "github:pk910", Fingerprint: "SHA256:x"}).Actor())
	assert.Equal(t, "ssh:SHA256:x", (&SSHLoginGroup{Fingerprint: "SHA256:x", KeyType: "RSA"}).Actor())
	assert.Equal(t, "ssh:password", (&SSHLoginGroup{KeyType: "password"}).Actor())
	assert.Equal(t, "ssh:unknown", (&SSHLoginGroup{}).Actor())
	assert.Equal(t, AuditSSH, AuditActorKindOf((&SSHLoginGroup{}).Actor()))
}

func TestSSHClusters(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 28, 16, 0, 0, 0, time.UTC)
	run := now.Add(-2 * time.Hour)

	group := func(host, identity, user string, start time.Time, d time.Duration) *SSHLoginGroup {
		return &SSHLoginGroup{
			Host: host, Identity: identity, User: user, From: "10.0.0.5", FirstLogin: start,
			LastLogout: start.Add(d), Sessions: 10, TotalMs: d.Milliseconds(), LongestMs: d.Milliseconds(),
		}
	}

	c := NewSSHClusters(now)

	// Newest start first: an open session, then a run over 20 hosts with an
	// interleaved login of another user, then an earlier run of the same key.
	open := group("node-1", "github:pk910", "devops", now.Add(-time.Hour), 0)
	open.Open = 1
	c.Add(open)

	for i := 19; i >= 0; i-- {
		c.Add(group(fmt.Sprintf("node-%d", i+1), "github:pk910", "devops", run.Add(time.Duration(i)*time.Minute), time.Minute))

		if i == 10 {
			c.Add(group("node-5", "github:pk910", "root", run.Add(10*time.Minute), time.Minute))
		}
	}

	assert.False(t, c.Quiet(group("node-1", "x", "y", run.Add(-5*time.Minute), time.Minute)))
	assert.True(t, c.Quiet(group("node-1", "x", "y", run.Add(-20*time.Minute), time.Minute)))

	c.Add(group("node-1", "github:pk910", "devops", run.Add(-90*time.Minute), time.Minute))
	c.Add(group("node-2", "", "devops", run.Add(-91*time.Minute), time.Minute))

	got := c.Clusters()
	require.Len(t, got, 5)
	assert.Equal(t, 5, c.Len())

	// The open group started an hour after the run ended.
	assert.Equal(t, 1, got[0].Hosts)
	assert.Equal(t, 1, got[0].Open)

	ansible := got[1]
	assert.Equal(t, 20, ansible.Hosts)
	assert.Equal(t, 20, ansible.Groups)
	assert.Equal(t, 200, ansible.Sessions)
	assert.Equal(t, run, ansible.FirstLogin)
	assert.Equal(t, run.Add(19*time.Minute), ansible.LastStart)
	assert.Equal(t, run.Add(20*time.Minute), ansible.LastLogout)
	assert.Equal(t, []string{"node-20", "node-19", "node-18", "node-17", "node-16"}, ansible.HostNames)
	assert.Equal(t, []string{"10.0.0.5"}, ansible.From)
	assert.Equal(t, int64(20*60000), ansible.TotalMs)

	assert.Equal(t, "root", got[2].User, "another user is another cluster")
	assert.Equal(t, run.Add(-90*time.Minute), got[3].FirstLogin, "an hour apart: another cluster")
	assert.Empty(t, got[4].Identity, "an unknown key is its own")
}
