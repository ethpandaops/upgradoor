package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const hostColumns = `name, cert_hash, roles,
	first_seen, last_seen, remote_addr, status, inventory_hash, hold_wanted, hold_reason, hold_by,
	offline, broken, sync_interval, status_hash, hold_actor, unproven`

// RecordSync upserts the host named by the certificate subject. first_seen
// is kept for a known host. With a nil Request only the identity, last_seen
// and remote address are updated; the stored status stays.
//
// A host seen for the first time that reports a controller hold is created
// with hold_wanted set. The hold was put there by a controller whose database
// is gone (or that was told to forget the host); without this the scheduler
// would find the hold unwanted and lift it.
func (s *sqliteStore) RecordSync(ctx context.Context, in *HostSync) (*SyncResult, error) {
	if in == nil || in.Cert == nil {
		return nil, errors.New("store: record sync: certificate is required")
	}

	cert := in.Cert
	if cert.Subject == "" {
		return nil, errors.New("store: record sync: certificate has no subject")
	}

	at := in.At
	if at.IsZero() {
		at = time.Now()
	}

	rolesJSON, status, statusHash, err := encodeSync(in)
	if err != nil {
		return nil, fmt.Errorf("store: record sync of %s: %w", cert.Subject, err)
	}

	result := &SyncResult{}

	err = s.write(ctx, func(tx *sql.Tx) error {
		var prev storedSync

		txErr := tx.QueryRowContext(ctx, `
			SELECT cert_hash, roles, remote_addr, status_hash, last_seen
			FROM hosts WHERE name = ?`,
			cert.Subject).Scan(&prev.certHash, &prev.roles,
			&prev.remoteAddr, &prev.statusHash, &prev.lastSeen)
		if txErr == nil {
			result.PreviousSeen = fromMillis(prev.lastSeen)
		}

		switch {
		case errors.Is(txErr, sql.ErrNoRows):
			result.Created = true
			result.HoldAdopted = in.Request != nil && in.Request.Hold.Controller
		case txErr != nil:
			return txErr
		case prev.certHash != cert.Hash:
			result.PreviousCertHash = prev.certHash
		}

		if !result.Created && prev.confirms(in, rolesJSON, statusHash) {
			result.Host, txErr = s.confirmSync(ctx, tx, in, at, status)

			return txErr
		}

		result.Changed = true

		holdReason := ""
		if result.HoldAdopted {
			holdReason = HoldReasonAdopted
		}

		// hold_wanted and hold_reason are only written for a new row.
		_, txErr = tx.ExecContext(ctx, `
			INSERT INTO hosts (name, cert_hash,
				roles, first_seen, last_seen, remote_addr, status, hold_wanted, hold_reason,
				status_hash, sync_interval)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				cert_hash = excluded.cert_hash,
				roles = CASE WHEN excluded.status IS NULL
					THEN hosts.roles ELSE excluded.roles END,
				last_seen = excluded.last_seen,
				remote_addr = excluded.remote_addr,
				status = COALESCE(excluded.status, hosts.status),
				status_hash = CASE WHEN excluded.status IS NULL
					THEN hosts.status_hash ELSE excluded.status_hash END,
				sync_interval = excluded.sync_interval`,
			cert.Subject, cert.Hash,
			rolesJSON, millis(at), millis(at), in.RemoteAddr, status,
			result.HoldAdopted, holdReason, statusHash, in.SyncInterval.Milliseconds())
		if txErr != nil {
			return txErr
		}

		result.Host, txErr = s.getHost(ctx, tx, cert.Subject)

		return txErr
	})
	if err != nil {
		return nil, fmt.Errorf("store: record sync of %s: %w", cert.Subject, err)
	}

	return result, nil
}

// storedSync is what RecordSync compares a sync against.
type storedSync struct {
	certHash, roles, remoteAddr, statusHash string
	lastSeen                                int64
}

// confirms reports whether the sync only restates the stored row: same
// certificate, roles and address, and a status with the same hash.
func (p *storedSync) confirms(in *HostSync, roles, statusHash string) bool {
	return in.Request != nil && statusHash != "" && p.statusHash == statusHash &&
		p.certHash == in.Cert.Hash && p.roles == roles && p.remoteAddr == in.RemoteAddr
}

// confirmSync writes what a confirming sync changes: the time, the asked
// sync interval and the status, which differs from the stored one only in
// what the hash leaves out (the clock, the uptime, the probes' heads and
// peer counts).
func (s *sqliteStore) confirmSync(
	ctx context.Context, tx *sql.Tx, in *HostSync, at time.Time, status sql.NullString,
) (*fleet.Host, error) {
	_, err := tx.ExecContext(ctx, `
		UPDATE hosts SET last_seen = ?, sync_interval = ?, status = ? WHERE name = ?`,
		millis(at), in.SyncInterval.Milliseconds(), status, in.Cert.Subject)
	if err != nil {
		return nil, err
	}

	return s.getHost(ctx, tx, in.Cert.Subject)
}

// encodeSync returns the roles and status columns of a sync. Without a
// request the status is NULL and the stored roles stay.
func encodeSync(in *HostSync) (roles string, status sql.NullString, hash string, err error) {
	list := []string{}
	if in.Request != nil && in.Request.Roles != nil {
		list = in.Request.Roles
	}

	raw, err := json.Marshal(list)
	if err != nil {
		return "", status, "", fmt.Errorf("encode roles: %w", err)
	}

	roles = string(raw)

	if in.Request == nil {
		return roles, status, "", nil
	}

	// The hash leaves out the fields that change on every sync.
	steady := *in.Request
	steady.SentAt, steady.UptimeSeconds = time.Time{}, 0
	steady.Node = steadyNode(in.Request.Node)

	raw, err = json.Marshal(&steady)
	if err != nil {
		return "", status, "", fmt.Errorf("encode status: %w", err)
	}

	sum := sha256.Sum256(raw)
	hash = hex.EncodeToString(sum[:16])

	raw, err = json.Marshal(in.Request)
	if err != nil {
		return "", status, "", fmt.Errorf("encode status: %w", err)
	}

	return roles, sql.NullString{String: string(raw), Valid: true}, hash, nil
}

// steadyNode is the part of the node probes the status hash covers: the
// flags (reachable, syncing, optimistic, EL offline), the errors and whether
// each client has peers at all. The heads and the sync distance move with
// every slot; the exact peer counts drift on a healthy node. Readiness
// compares those from the stored status at the scheduler's next tick
// (confirmSync stores them), so a change in them needs no early reconcile.
func steadyNode(n protocol.NodeHealth) protocol.NodeHealth {
	out := protocol.NodeHealth{}

	if n.EL != nil {
		el := *n.EL
		el.Head, el.Peers = 0, min(max(el.Peers, 0), 1)
		out.EL = &el
	}

	if n.CL != nil {
		cl := *n.CL
		cl.HeadSlot, cl.SyncDistance, cl.Peers = 0, 0, min(max(cl.Peers, 0), 1)
		out.CL = &cl
	}

	return out
}

// GetHost returns one host or ErrNotFound.
func (s *sqliteStore) GetHost(ctx context.Context, name string) (*fleet.Host, error) {
	var host *fleet.Host

	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error

		host, err = s.getHost(ctx, tx, name)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: get host %s: %w", name, err)
	}

	return host, nil
}

func (s *sqliteStore) getHost(ctx context.Context, tx *sql.Tx, name string) (*fleet.Host, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM hosts WHERE name = ?`, name)

	host, err := s.scanHost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, err
	}

	ranges, err := hostValidators(ctx, tx, name)
	if err != nil {
		return nil, err
	}

	host.Validators = ranges[name]

	return host, nil
}

// ListHosts returns all hosts ordered by name.
func (s *sqliteStore) ListHosts(ctx context.Context) ([]*fleet.Host, error) {
	hosts := make([]*fleet.Host, 0, 32)

	err := s.read(ctx, func(tx *sql.Tx) error {
		err := queryRows(ctx, tx, `SELECT `+hostColumns+` FROM hosts ORDER BY name`, nil,
			func(row rowScanner) error {
				host, err := s.scanHost(row)
				if err != nil {
					return err
				}

				hosts = append(hosts, host)

				return nil
			})
		if err != nil {
			return err
		}

		ranges, err := hostValidators(ctx, tx, "")
		if err != nil {
			return err
		}

		for _, host := range hosts {
			host.Validators = ranges[host.Name]
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: list hosts: %w", err)
	}

	return hosts, nil
}

func (s *sqliteStore) scanHost(row rowScanner) (*fleet.Host, error) {
	var (
		host                fleet.Host
		roles               string
		firstSeen, lastSeen int64
		status              sql.NullString
		broken, offline     string
		syncInterval        int64
		unproven            sql.NullInt64
	)

	err := row.Scan(&host.Name, &host.CertHash, &roles,
		&firstSeen, &lastSeen, &host.RemoteAddr, &status, &host.InventoryHash,
		&host.HoldWanted, &host.HoldReason, &host.HoldBy, &offline, &broken, &syncInterval,
		&host.StatusHash, &host.HoldActor, &unproven)
	if err != nil {
		return nil, err
	}

	if unproven.Valid {
		at := fromMillis(unproven.Int64)
		host.Unproven = &at
	}

	if offline != "" {
		if err := json.Unmarshal([]byte(offline), &host.Offline); err != nil {
			return nil, fmt.Errorf("decode offline mark of %s: %w", host.Name, err)
		}
	}

	if broken != "{}" {
		if err := json.Unmarshal([]byte(broken), &host.Broken); err != nil {
			return nil, fmt.Errorf("decode broken marks of %s: %w", host.Name, err)
		}
	}

	host.SyncInterval = time.Duration(syncInterval) * time.Millisecond
	host.FirstSeen = fromMillis(firstSeen)
	host.LastSeen = fromMillis(lastSeen)

	if err := json.Unmarshal([]byte(roles), &host.Roles); err != nil {
		return nil, fmt.Errorf("decode roles of %s: %w", host.Name, err)
	}

	if host.Roles == nil {
		host.Roles = []string{}
	}

	if status.Valid {
		decoded, err := s.statuses.decode(host.Name, status.String)
		if err != nil {
			return nil, fmt.Errorf("decode status of %s: %w", host.Name, err)
		}

		host.Status = decoded
	}

	return &host, nil
}

// statusCache keeps the last decoded status of each host. Listing the fleet
// is what the scheduler and the event stream do all the time, and decoding
// every host's status JSON was most of that; between two listings only the
// hosts that synced have a different status.
//
// A hit is a copy of the struct whose slices and pointers are shared with the
// cache and with every other reader: a fleet.Host's Status is read-only.
type statusCache struct {
	mu     sync.Mutex
	byHost map[string]cachedStatus
}

type cachedStatus struct {
	raw    string
	status protocol.SyncRequest
}

// decode returns raw decoded, from the cache when raw is what the host's
// entry was decoded from.
func (c *statusCache) decode(host, raw string) (*protocol.SyncRequest, error) {
	c.mu.Lock()
	entry, ok := c.byHost[host]
	c.mu.Unlock()

	if ok && entry.raw == raw {
		status := entry.status

		return &status, nil
	}

	entry = cachedStatus{raw: raw}
	if err := json.Unmarshal([]byte(raw), &entry.status); err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.byHost == nil {
		c.byHost = make(map[string]cachedStatus, 1024)
	}

	c.byHost[host] = entry
	c.mu.Unlock()

	status := entry.status

	return &status, nil
}

// forget drops the entry of a host.
func (c *statusCache) forget(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.byHost, host)
}

// setHostOffline writes the offline mark of a host; nil lifts it.
func setHostOffline(ctx context.Context, tx *sql.Tx, name string, mark *fleet.OfflineMark) error {
	encoded := ""

	if mark != nil {
		raw, err := json.Marshal(mark)
		if err != nil {
			return fmt.Errorf("encode offline mark: %w", err)
		}

		encoded = string(raw)
	}

	res, err := tx.ExecContext(ctx, `UPDATE hosts SET offline = ? WHERE name = ?`, encoded, name)
	if err != nil {
		return err
	}

	return requireRow(res)
}

// setHostHold writes the hold switch of a host. by is fleet.Host.HoldBy; a
// host that is not held has none.
func setHostHold(
	ctx context.Context, tx *sql.Tx, name string, wanted bool, reason, by, actor string,
) error {
	if !wanted {
		by, actor = "", ""
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE hosts SET hold_wanted = ?, hold_reason = ?, hold_by = ?, hold_actor = ? WHERE name = ?`,
		wanted, reason, by, actor, name)
	if err != nil {
		return err
	}

	return requireRow(res)
}

// setHostBroken replaces the broken marks of a host. A host that does not
// exist has none: nothing is written and nothing fails.
func setHostBroken(ctx context.Context, tx *sql.Tx, name string, marks map[string]string) error {
	if marks == nil {
		marks = map[string]string{}
	}

	raw, err := json.Marshal(marks)
	if err != nil {
		return fmt.Errorf("encode broken marks: %w", err)
	}

	_, err = tx.ExecContext(ctx, `UPDATE hosts SET broken = ? WHERE name = ?`, string(raw), name)

	return err
}

// setHostUnproven replaces fleet.Host.Unproven of a host. A host that does
// not exist is passed over.
func setHostUnproven(ctx context.Context, tx *sql.Tx, name string, at *time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE hosts SET unproven = ? WHERE name = ?`, nullMillis(at), name)

	return err
}

// DeleteHost removes a host with its inventory, packages, upgradable and
// per-slot participation rows. Rollouts, leases, directives and audit
// entries stay. It returns ErrNotFound for an unknown host.
func (s *sqliteStore) DeleteHost(ctx context.Context, name string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		// inventories, packages and upgradable go with the host by
		// ON DELETE CASCADE; the per-slot rows have no foreign key because the
		// beacon service may store rows for a host before or after it exists.
		// The fleet-wide slot rows stay: they are the network's history.
		_, err := tx.ExecContext(ctx,
			`DELETE FROM slot_participation_hosts WHERE host = ?`, name)
		if err != nil {
			return err
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM hosts WHERE name = ?`, name)
		if err != nil {
			return err
		}

		return requireRow(res)
	})
	if err != nil {
		return fmt.Errorf("store: delete host %s: %w", name, err)
	}

	s.statuses.forget(name)

	return nil
}

// requireRow turns "no row affected" into ErrNotFound.
func requireRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if n == 0 {
		return ErrNotFound
	}

	return nil
}
