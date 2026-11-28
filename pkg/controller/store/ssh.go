package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Paging of ListSSHLogins.
const (
	DefaultSSHLoginLimit = 50
	MaxSSHLoginLimit     = 200
	// sshScanLimit bounds the groups one page of clusters reads.
	sshScanLimit = 20000
)

// SSHLoginView is what a page of SSH logins lists.
type SSHLoginView string

// SSH login views.
const (
	// SSHViewFleet lists the clusters of the groups across the fleet.
	SSHViewFleet SSHLoginView = "fleet"
	// SSHViewGroups lists the groups.
	SSHViewGroups SSHLoginView = "groups"
)

// SSHLoginFilter selects a page of SSH login groups or clusters, newest
// start first; every field that is set must match.
type SSHLoginFilter struct {
	View SSHLoginView
	Host string
	// Identity selects a known key's owner; Fingerprint a key.
	Identity    string
	Fingerprint string
	User        string
	// From and To select the groups that start in [From, To).
	From, To time.Time
	// Unknown selects the keys the key maps do not know.
	Unknown bool
	// Open selects the groups with a session logged in now.
	Open bool
	// Cursor is the Next of the page before.
	Cursor string
	// Limit bounds the groups or clusters (0 means DefaultSSHLoginLimit).
	// A page of clusters ends where no cluster continues, so it may hold
	// more.
	Limit int
}

// SSHLoginPage is one page of SSH logins.
type SSHLoginPage struct {
	Groups   []fleet.SSHLoginGroup
	Clusters []fleet.SSHLoginCluster
	// Next continues after this page; empty when it is the last one.
	Next string
}

// SSHLoginsRecorded is what RecordSSHLogins stored.
type SSHLoginsRecorded struct {
	// Ack is the highest Seq of the report's closed groups, 0 for none.
	Ack int64
	// Closed are the groups that closed now, each with an audit entry.
	Closed int
	// FirstSeen: the host had never reported SSH logins.
	FirstSeen bool
}

// sshColumns are the columns of ssh_logins a group is read from.
const sshColumns = `id, host, identity, source, fingerprint, key_type, login_user, from_ip,
	first_login, last_logout, sessions, open, closed, longest_ms, total_ms, end_unknown`

// RecordSSHLogins implements Store.
func (s *sqliteStore) RecordSSHLogins(
	ctx context.Context, host string, at time.Time, report *protocol.SSHReport,
) (*SSHLoginsRecorded, error) {
	out := &SSHLoginsRecorded{}

	err := s.write(ctx, func(tx *sql.Tx) error {
		first, markErr := markSSHHost(ctx, tx, host, at, report)
		if markErr != nil {
			return markErr
		}

		if openErr := replaceOpenSSH(ctx, tx, host, at, report); openErr != nil {
			return openErr
		}

		entries, ack, err := closeSSH(ctx, tx, host, at, report.Closed)
		if err != nil {
			return err
		}

		out.Ack, out.Closed, out.FirstSeen = ack, len(entries), first

		_, err = insertAudit(ctx, tx, entries)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: record ssh logins of %s: %w", host, err)
	}

	return out, nil
}

// markSSHHost marks the host as reporting SSH logins, adds the report's
// dropped count and keeps its error; first is true for a host new to it.
func markSSHHost(
	ctx context.Context, tx *sql.Tx, host string, at time.Time, report *protocol.SSHReport,
) (first bool, err error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO ssh_hosts (host, first_seen, last_seen, dropped, error)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (host) DO NOTHING`,
		host, millis(at), millis(at), report.Dropped, report.Error)
	if err != nil {
		return false, err
	}

	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}

	_, err = tx.ExecContext(ctx, `UPDATE ssh_hosts SET last_seen = ?, dropped = dropped + ?, error = ?
		WHERE host = ?`, millis(at), report.Dropped, report.Error, host)

	return false, err
}

// replaceOpenSSH makes the report's open groups the host's open ones: those
// it no longer reports (and does not close) are deleted.
func replaceOpenSSH(ctx context.Context, tx *sql.Tx, host string, at time.Time, report *protocol.SSHReport) error {
	keep := make([]any, 0, len(report.Open)+len(report.Closed)+1)
	keep = append(keep, host)

	for i := range report.Open {
		keep = append(keep, report.Open[i].Seq)
	}

	for i := range report.Closed {
		keep = append(keep, report.Closed[i].Seq)
	}

	// The seqs are bound; only their placeholders are written.
	query := `DELETE FROM ssh_logins WHERE host = ? AND closed = 0 AND seq NOT IN (` + //nolint:gosec // placeholders
		placeholders(len(keep)-1) + `)`

	if _, err := tx.ExecContext(ctx, query, keep...); err != nil {
		return err
	}

	for i := range report.Open {
		if _, err := upsertSSH(ctx, tx, host, at, &report.Open[i], false); err != nil {
			return err
		}
	}

	return nil
}

// closeSSH stores the closed groups and returns an audit entry for each one
// that closed now, and the highest Seq.
func closeSSH(
	ctx context.Context, tx *sql.Tx, host string, at time.Time, closed []protocol.SSHLoginGroup,
) ([]fleet.AuditEntry, int64, error) {
	entries := make([]fleet.AuditEntry, 0, len(closed))

	var ack int64

	for i := range closed {
		g := &closed[i]
		ack = max(ack, g.Seq)

		changed, err := upsertSSH(ctx, tx, host, at, g, true)
		if err != nil {
			return nil, 0, err
		}

		if changed {
			entries = append(entries, sshAuditEntry(host, at, g))
		}
	}

	return entries, ack, nil
}

// upsertSSH inserts a group or updates it while it is not closed, and
// reports whether it did.
func upsertSSH(
	ctx context.Context, tx *sql.Tx, host string, at time.Time, g *protocol.SSHLoginGroup, closed bool,
) (bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO ssh_logins (host, seq, identity, source, fingerprint,
			key_type, login_user, from_ip, first_login, last_logout, sessions, open, closed, longest_ms,
			total_ms, end_unknown, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (host, seq) DO UPDATE SET identity = excluded.identity, source = excluded.source,
			fingerprint = excluded.fingerprint, key_type = excluded.key_type,
			login_user = excluded.login_user, from_ip = excluded.from_ip,
			first_login = excluded.first_login, last_logout = excluded.last_logout,
			sessions = excluded.sessions, open = excluded.open, closed = excluded.closed,
			longest_ms = excluded.longest_ms, total_ms = excluded.total_ms,
			end_unknown = excluded.end_unknown, updated = excluded.updated
		WHERE ssh_logins.closed = 0`,
		host, g.Seq, g.Identity, g.Source, g.Fingerprint, g.KeyType, g.User, g.From,
		millis(g.FirstLogin), millis(groupEnd(g)), g.Sessions, openOf(g, closed), closed,
		g.LongestMillis, g.TotalMillis, g.EndUnknown, millis(at))
	if err != nil {
		return false, fmt.Errorf("ssh login group %d: %w", g.Seq, err)
	}

	n, err := res.RowsAffected()

	return n == 1, err
}

// groupEnd is a group's newest logout, or its first login.
func groupEnd(g *protocol.SSHLoginGroup) time.Time {
	if g.LastLogout != nil && g.LastLogout.After(g.FirstLogin) {
		return *g.LastLogout
	}

	return g.FirstLogin
}

// openOf is the open sessions of a group; a closed one has none.
func openOf(g *protocol.SSHLoginGroup, closed bool) int {
	if closed {
		return 0
	}

	return g.Open
}

// sshAuditEntry is the audit entry of a closed group.
func sshAuditEntry(host string, at time.Time, g *protocol.SSHLoginGroup) fleet.AuditEntry {
	view := fleet.SSHLoginGroup{Identity: g.Identity, Fingerprint: g.Fingerprint, KeyType: g.KeyType}

	return fleet.AuditEntry{
		Time:   at,
		Actor:  view.Actor(),
		Action: fleet.AuditActionSSHSession,
		Target: host,
		Detail: map[string]any{
			"user":        g.User,
			"from":        g.From,
			"identity":    g.Identity,
			"source":      g.Source,
			"fingerprint": g.Fingerprint,
			"keyType":     g.KeyType,
			"firstLogin":  storedTime(g.FirstLogin),
			"lastLogout":  storedTime(groupEnd(g)),
			"sessions":    g.Sessions,
			"longestMs":   g.LongestMillis,
			"totalMs":     g.TotalMillis,
			"endUnknown":  g.EndUnknown,
		},
	}
}

// SSHLoginsSeen implements Store.
func (s *sqliteStore) SSHLoginsSeen(ctx context.Context) (bool, error) {
	var seen bool

	err := s.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ssh_hosts)`).Scan(&seen)
	})
	if err != nil {
		return false, fmt.Errorf("store: ssh logins seen: %w", err)
	}

	return seen, nil
}

// DeleteSSHLoginsBefore implements Store.
func (s *sqliteStore) DeleteSSHLoginsBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}

	var deleted int64

	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM ssh_logins WHERE id IN (
			SELECT id FROM ssh_logins INDEXED BY ssh_logins_end
			WHERE closed = 1 AND last_logout < ? ORDER BY last_logout LIMIT ?)`,
			millis(before), limit)
		if err != nil {
			return err
		}

		deleted, err = res.RowsAffected()

		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: delete ssh logins: %w", err)
	}

	return deleted, nil
}

// ListSSHLogins implements Store.
func (s *sqliteStore) ListSSHLogins(ctx context.Context, filter *SSHLoginFilter) (*SSHLoginPage, error) {
	limit := filter.Limit

	switch {
	case limit <= 0:
		limit = DefaultSSHLoginLimit
	case limit > MaxSSHLoginLimit:
		limit = MaxSSHLoginLimit
	}

	after, err := parseSSHCursor(filter.Cursor)
	if err != nil {
		return nil, err
	}

	page := &SSHLoginPage{}

	err = s.read(ctx, func(tx *sql.Tx) error {
		if filter.View == SSHViewGroups {
			return sshGroupPage(ctx, tx, filter, after, limit, page)
		}

		return sshClusterPage(ctx, tx, filter, after, limit, page)
	})
	if err != nil {
		return nil, fmt.Errorf("store: list ssh logins: %w", err)
	}

	return page, nil
}

// sshGroupPage reads a page of groups.
func sshGroupPage(
	ctx context.Context, tx *sql.Tx, f *SSHLoginFilter, after *sshCursor, limit int, page *SSHLoginPage,
) error {
	query, args := sshQuery(f, after, limit+1)
	page.Groups = make([]fleet.SSHLoginGroup, 0, min(limit+1, DefaultSSHLoginLimit))

	err := queryRows(ctx, tx, query, args, func(row rowScanner) error {
		g, err := scanSSH(row)
		if err == nil {
			page.Groups = append(page.Groups, g)
		}

		return err
	})
	if err != nil {
		return err
	}

	if len(page.Groups) > limit {
		page.Groups = page.Groups[:limit]
		page.Next = sshCursorOf(&page.Groups[limit-1])
	}

	return nil
}

// sshClusterPage reads groups until limit clusters are complete: up to a
// group that ends before every cluster so far starts, or sshScanLimit.
func sshClusterPage(
	ctx context.Context, tx *sql.Tx, f *SSHLoginFilter, after *sshCursor, limit int, page *SSHLoginPage,
) error {
	query, args := sshQuery(f, after, sshScanLimit+1)
	clusters := fleet.NewSSHClusters(time.Now().UTC())

	var (
		last    *fleet.SSHLoginGroup
		scanned int
		more    bool
	)

	err := queryRows(ctx, tx, query, args, func(row rowScanner) error {
		if more {
			return nil
		}

		g, err := scanSSH(row)
		if err != nil {
			return err
		}

		if scanned == sshScanLimit || (clusters.Len() >= limit && clusters.Quiet(&g)) {
			more = true

			return nil
		}

		clusters.Add(&g)
		last, scanned = &g, scanned+1

		return nil
	})
	if err != nil {
		return err
	}

	page.Clusters = clusters.Clusters()

	if more && last != nil {
		page.Next = sshCursorOf(last)
	}

	return nil
}

// sshQuery is the SELECT of a filtered page, newest start first, from the
// index of its most selective filter.
func sshQuery(f *SSHLoginFilter, after *sshCursor, limit int) (query string, args []any) {
	var cond conditions

	index := "ssh_logins_time"

	switch {
	case f.Host != "":
		index = "ssh_logins_host"
	case f.Identity != "":
		index = "ssh_logins_identity"
	case f.Fingerprint != "" || f.Unknown:
		index = "ssh_logins_unknown"
	case f.Open:
		index = "ssh_logins_open"
	case f.User != "":
		index = "ssh_logins_user"
	}

	if f.Host != "" {
		cond.add(`host = ?`, f.Host)
	}

	if f.Identity != "" {
		cond.add(`identity = ?`, f.Identity)
	}

	if f.Fingerprint != "" {
		cond.add(`identity = ''`)
		cond.add(`fingerprint = ?`, f.Fingerprint)
	}

	if f.Unknown {
		cond.add(`identity = ''`)
	}

	if f.Open {
		cond.add(`open > 0`)
	}

	if f.User != "" {
		cond.add(`login_user = ?`, f.User)
	}

	if !f.From.IsZero() {
		cond.add(`first_login >= ?`, millis(f.From))
	}

	if !f.To.IsZero() {
		cond.add(`first_login < ?`, millis(f.To))
	}

	if after != nil {
		cond.add(`(first_login, id) < (?, ?)`, after.start, after.id)
	}

	query = `SELECT ` + sshColumns + ` FROM ssh_logins INDEXED BY ` + index + cond.where() +
		` ORDER BY first_login DESC, id DESC LIMIT ?`

	return query, append(cond.args, limit)
}

// scanSSH reads one group.
func scanSSH(row rowScanner) (fleet.SSHLoginGroup, error) {
	var (
		g           fleet.SSHLoginGroup
		first, last int64
	)

	err := row.Scan(&g.ID, &g.Host, &g.Identity, &g.Source, &g.Fingerprint, &g.KeyType, &g.User, &g.From,
		&first, &last, &g.Sessions, &g.Open, &g.Closed, &g.LongestMs, &g.TotalMs, &g.EndUnknown)
	g.FirstLogin, g.LastLogout = fromMillis(first), fromMillis(last)

	if g.Source == "" {
		g.Source = fleet.SSHSourceUnknown
	}

	return g, err
}

// sshCursor is a position in the newest-first order.
type sshCursor struct {
	start int64
	id    int64
}

// sshCursorOf is the cursor after a group: "<first login ms>.<id>".
func sshCursorOf(g *fleet.SSHLoginGroup) string {
	return strconv.FormatInt(millis(g.FirstLogin), 10) + "." + strconv.FormatInt(g.ID, 10)
}

// parseSSHCursor reads a cursor; empty is the first page.
func parseSSHCursor(raw string) (*sshCursor, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // no cursor is the first page
	}

	start, id, ok := strings.Cut(raw, ".")

	c := &sshCursor{}

	var err1, err2 error

	c.start, err1 = strconv.ParseInt(start, 10, 64)
	c.id, err2 = strconv.ParseInt(id, 10, 64)

	if !ok || err1 != nil || err2 != nil || c.id <= 0 {
		return nil, ErrBadCursor
	}

	return c, nil
}
