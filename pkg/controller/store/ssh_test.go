package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// sshGroup is a group of seq starting at start that lasted a minute.
func sshGroup(seq int64, identity string, start time.Time) protocol.SSHLoginGroup {
	end := start.Add(time.Minute)
	source := fleet.SSHSourceGitHub

	if identity == "" {
		source = fleet.SSHSourceUnknown
	}

	return protocol.SSHLoginGroup{
		Seq: seq, Identity: identity, Source: source, Fingerprint: "SHA256:" + identity + "key",
		KeyType: "ED25519", User: "devops", From: "10.0.0.5", FirstLogin: start, LastLogout: &end,
		Sessions: 3, LongestMillis: 30000, TotalMillis: 45000,
	}
}

func sshIDs(groups []fleet.SSHLoginGroup) []string {
	out := make([]string, 0, len(groups))
	for i := range groups {
		out = append(out, fmt.Sprintf("%s/%d", groups[i].Host, groups[i].ID))
	}

	return out
}

func TestRecordSSHLogins(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)

			seen, err := s.SSHLoginsSeen(ctx)
			require.NoError(t, err)
			assert.False(t, seen)

			open1, open2 := sshGroup(1, "github:pk910", at(0)), sshGroup(2, "", at(10))
			open1.Open, open1.LastLogout = 1, nil

			rec, err := s.RecordSSHLogins(ctx, "node-1", at(20), &protocol.SSHReport{
				Open: []protocol.SSHLoginGroup{open1, open2},
			})
			require.NoError(t, err)
			assert.Equal(t, &SSHLoginsRecorded{FirstSeen: true}, rec)

			seen, err = s.SSHLoginsSeen(ctx)
			require.NoError(t, err)
			assert.True(t, seen)

			page, err := s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewGroups})
			require.NoError(t, err)
			require.Len(t, page.Groups, 2)
			assert.Equal(t, at(0), page.Groups[1].LastLogout, "an open group without a logout ends at its start")
			assert.Equal(t, 1, page.Groups[1].Open)
			assert.False(t, page.Groups[1].Closed)
			assert.Equal(t, fleet.SSHSourceUnknown, page.Groups[0].Source)

			// The next report closes 1 and no longer has 2: the open set is
			// replaced.
			closed1 := sshGroup(1, "github:pk910", at(0))
			closed1.EndUnknown = true

			rec, err = s.RecordSSHLogins(ctx, "node-1", at(30), &protocol.SSHReport{
				Closed: []protocol.SSHLoginGroup{closed1}, Dropped: 4, Error: "journal: permission denied",
			})
			require.NoError(t, err)
			assert.Equal(t, &SSHLoginsRecorded{Ack: 1, Closed: 1}, rec)

			page, err = s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewGroups})
			require.NoError(t, err)
			require.Len(t, page.Groups, 1)
			assert.True(t, page.Groups[0].Closed)
			assert.Zero(t, page.Groups[0].Open)
			assert.True(t, page.Groups[0].EndUnknown)

			entries, err := s.ListAudit(ctx, AuditFilter{ActorKind: fleet.AuditSSH})
			require.NoError(t, err)
			require.Len(t, entries, 1, "one entry per closed group")
			assert.Equal(t, "ssh:github:pk910", entries[0].Actor)
			assert.Equal(t, fleet.AuditActionSSHSession, entries[0].Action)
			assert.Equal(t, "node-1", entries[0].Target)
			assert.Equal(t, at(30), entries[0].Time, "stamped when it arrived")
			assert.Equal(t, "10.0.0.5", entries[0].Detail["from"])
			assert.Equal(t, "devops", entries[0].Detail["user"])
			assert.Equal(t, true, entries[0].Detail["endUnknown"])
			assert.InDelta(t, 45000, entries[0].Detail["totalMs"], 0)

			people, err := s.ListAudit(ctx, AuditFilter{ActorKind: fleet.AuditPeople})
			require.NoError(t, err)
			assert.Empty(t, people, "an SSH login is no person's entry")

			// Delivered again (the acknowledgement was lost): stored once.
			closed1.Sessions = 99
			open3 := sshGroup(3, "github:pk910", at(40))
			open3.Open = 1

			rec, err = s.RecordSSHLogins(ctx, "node-1", at(50), &protocol.SSHReport{
				Open: []protocol.SSHLoginGroup{open3}, Closed: []protocol.SSHLoginGroup{closed1},
			})
			require.NoError(t, err)
			assert.Equal(t, &SSHLoginsRecorded{Ack: 1}, rec)

			entries, err = s.ListAudit(ctx, AuditFilter{ActorKind: fleet.AuditSSH})
			require.NoError(t, err)
			assert.Len(t, entries, 1)

			page, err = s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewGroups})
			require.NoError(t, err)
			require.Len(t, page.Groups, 2)
			assert.Equal(t, 3, page.Groups[1].Sessions, "a closed group does not change")

			// A reported open group that is closed here stays closed.
			_, err = s.RecordSSHLogins(ctx, "node-1", at(60), &protocol.SSHReport{
				Open: []protocol.SSHLoginGroup{open1},
			})
			require.NoError(t, err)

			page, err = s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewGroups, Host: "node-1"})
			require.NoError(t, err)
			require.Len(t, page.Groups, 1)
			assert.True(t, page.Groups[0].Closed)

			var dropped int

			var lastErr string

			require.NoError(t, rawDB(t, s).QueryRow(`SELECT dropped, error FROM ssh_hosts WHERE host = 'node-1'`).
				Scan(&dropped, &lastErr))
			assert.Equal(t, 4, dropped)
			assert.Empty(t, lastErr, "the last report's error")
		})
	}
}

// sshFixture: an Ansible run over 30 hosts at 14:00, an interactive session
// on node-1 at 12:00, an unknown key on node-2 at 13:00 and an open session
// of root on node-3 at 15:00.
func sshFixture(t *testing.T, s Store) {
	t.Helper()

	ctx := context.Background()
	run := base.Add(2 * time.Hour)

	for i := range 30 {
		g := sshGroup(10, "github:pk910", run.Add(time.Duration(i)*20*time.Second))
		g.Sessions = 40

		_, err := s.RecordSSHLogins(ctx, fmt.Sprintf("node-%d", i+1), run, &protocol.SSHReport{
			Closed: []protocol.SSHLoginGroup{g},
		})
		require.NoError(t, err)
	}

	interactive := sshGroup(5, "github:barnabasbusa", base)
	interactive.From = "192.0.2.7"
	unknown := sshGroup(6, "", base.Add(time.Hour))
	open := sshGroup(11, "github:pk910", base.Add(3*time.Hour))
	open.User, open.Open = "root", 1

	for host, report := range map[string]*protocol.SSHReport{
		"node-1": {Closed: []protocol.SSHLoginGroup{interactive}},
		"node-2": {Closed: []protocol.SSHLoginGroup{unknown}},
		"node-3": {Open: []protocol.SSHLoginGroup{open}},
	} {
		_, err := s.RecordSSHLogins(ctx, host, run, report)
		require.NoError(t, err)
	}
}

func TestListSSHLogins(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newFileStore(t)
	sshFixture(t, s)

	count := func(f SSHLoginFilter) int {
		f.View, f.Limit = SSHViewGroups, MaxSSHLoginLimit

		page, err := s.ListSSHLogins(ctx, &f)
		require.NoError(t, err)

		return len(page.Groups)
	}

	assert.Equal(t, 33, count(SSHLoginFilter{}))
	assert.Equal(t, 2, count(SSHLoginFilter{Host: "node-1"}))
	assert.Equal(t, 31, count(SSHLoginFilter{Identity: "github:pk910"}))
	assert.Equal(t, 1, count(SSHLoginFilter{Fingerprint: "SHA256:key"}))
	assert.Equal(t, 1, count(SSHLoginFilter{Unknown: true}))
	assert.Equal(t, 1, count(SSHLoginFilter{Open: true}))
	assert.Equal(t, 1, count(SSHLoginFilter{User: "root"}))
	assert.Equal(t, 30, count(SSHLoginFilter{From: base.Add(2 * time.Hour), To: base.Add(3 * time.Hour)}))
	assert.Equal(t, 1, count(SSHLoginFilter{Host: "node-1", To: base.Add(time.Minute)}))

	// Pages continue where the one before ended.
	var (
		all    []string
		cursor string
	)

	for {
		page, err := s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewGroups, Limit: 7, Cursor: cursor})
		require.NoError(t, err)

		all = append(all, sshIDs(page.Groups)...)

		if page.Next == "" {
			break
		}

		cursor = page.Next
	}

	assert.Len(t, all, 33)
	assert.True(t, strings.HasPrefix(all[0], "node-3/"), "newest start first")

	_, err := s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewGroups, Cursor: "nope"})
	require.ErrorIs(t, err, ErrBadCursor)
}

func TestListSSHLoginClusters(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newFileStore(t)
	sshFixture(t, s)

	page, err := s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewFleet})
	require.NoError(t, err)
	assert.Empty(t, page.Next)
	require.Len(t, page.Clusters, 4)

	open, run, unknown, interactive := page.Clusters[0], page.Clusters[1], page.Clusters[2], page.Clusters[3]
	assert.Equal(t, "root", open.User)
	assert.Equal(t, 1, open.Open)

	assert.Equal(t, "github:pk910", run.Identity)
	assert.Equal(t, 30, run.Hosts, "the Ansible run is one row")
	assert.Equal(t, 30, run.Groups)
	assert.Equal(t, 1200, run.Sessions)
	assert.Len(t, run.HostNames, fleet.MaxSSHClusterHosts)
	assert.Equal(t, []string{"10.0.0.5"}, run.From)
	assert.Equal(t, base.Add(2*time.Hour), run.FirstLogin)
	assert.Equal(t, base.Add(2*time.Hour+29*20*time.Second), run.LastStart)

	assert.Empty(t, unknown.Identity)
	assert.Equal(t, "SHA256:key", unknown.Fingerprint)
	assert.Equal(t, "github:barnabasbusa", interactive.Identity)

	// One cluster a page: each page ends where no cluster continues.
	var (
		identities []string
		cursor     string
	)

	for range 10 {
		page, err := s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewFleet, Limit: 1, Cursor: cursor})
		require.NoError(t, err)

		for _, c := range page.Clusters {
			identities = append(identities, c.User+" "+c.Identity+" "+fmt.Sprint(c.Hosts))
		}

		if page.Next == "" {
			break
		}

		cursor = page.Next
	}

	assert.Equal(t, []string{
		"root github:pk910 1", "devops github:pk910 30", "devops  1", "devops github:barnabasbusa 1",
	}, identities)
}

func TestDeleteSSHLoginsBefore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := newFileStore(t)
	sshFixture(t, s)

	n, err := s.DeleteSSHLoginsBefore(ctx, base.Add(2*time.Hour), 0)
	require.NoError(t, err)
	assert.Zero(t, n)

	n, err = s.DeleteSSHLoginsBefore(ctx, base.Add(5*time.Hour), 1000)
	require.NoError(t, err)
	assert.Equal(t, int64(32), n, "every closed group; the open one stays")

	page, err := s.ListSSHLogins(ctx, &SSHLoginFilter{View: SSHViewGroups})
	require.NoError(t, err)
	require.Len(t, page.Groups, 1)
	assert.Equal(t, "node-3", page.Groups[0].Host)
}

// Every filter is served from its index in the newest-first order, without
// a sort.
func TestSSHLoginQueriesUseTheirIndex(t *testing.T) {
	t.Parallel()

	tests := []struct {
		filter SSHLoginFilter
		index  string
	}{
		{SSHLoginFilter{}, "ssh_logins_time"},
		{SSHLoginFilter{From: base, To: base.Add(time.Hour)}, "ssh_logins_time"},
		{SSHLoginFilter{Host: "node-1", Identity: "github:pk910"}, "ssh_logins_host"},
		{SSHLoginFilter{Identity: "github:pk910", User: "root"}, "ssh_logins_identity"},
		{SSHLoginFilter{Fingerprint: "SHA256:x"}, "ssh_logins_unknown"},
		{SSHLoginFilter{Unknown: true}, "ssh_logins_unknown"},
		{SSHLoginFilter{Open: true}, "ssh_logins_open"},
		{SSHLoginFilter{User: "root"}, "ssh_logins_user"},
	}

	for _, filled := range []bool{false, true} {
		t.Run(fmt.Sprint("filled=", filled), func(t *testing.T) {
			t.Parallel()

			s := newFileStore(t)

			if filled {
				sshFixture(t, s)

				_, err := rawDB(t, s).Exec(`ANALYZE`)
				require.NoError(t, err)
			}

			for _, tt := range tests {
				query, args := sshQuery(&tt.filter, &sshCursor{start: millis(base.Add(time.Hour)), id: 9}, 50)
				plan := strings.Join(queryPlan(t, s, query, args), "\n")

				assert.Regexp(t, `INDEX `+tt.index+`\b`, plan, "%+v:\n%s", tt.filter, plan)
				assert.NotContains(t, plan, "TEMP B-TREE", "%+v:\n%s", tt.filter, plan)
			}

			plan := strings.Join(queryPlan(t, s, `DELETE FROM ssh_logins WHERE id IN (
				SELECT id FROM ssh_logins INDEXED BY ssh_logins_end
				WHERE closed = 1 AND last_logout < ? ORDER BY last_logout LIMIT ?)`, []any{1, 10}), "\n")
			assert.NotContains(t, plan, "TEMP B-TREE", plan)

			plan = strings.Join(queryPlan(t, s,
				`DELETE FROM ssh_logins WHERE host = ? AND closed = 0 AND seq NOT IN (?)`, []any{"a", 1}), "\n")
			assert.Regexp(t, `INDEX ssh_logins_unclosed\b|sqlite_autoindex_ssh_logins_1`, plan)
		})
	}
}
