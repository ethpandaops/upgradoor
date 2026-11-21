package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const hostColumns = `name, cert_hash, generation, validators_start, validators_end, roles,
	first_seen, last_seen, remote_addr, status, inventory_hash, hold_wanted, hold_reason, hold_by,
	offline, broken, sync_interval, status_hash`

// RecordSync upserts the host named by the certificate subject. first_seen
// is kept for a known host. With a nil Request only the identity, last_seen
// and remote address are updated; the stored status and validator
// range stay.
//
// The validator range of the row is what the controller counts for the host
// (fleet.Host.Validators). It follows the range the request reports: see
// countedValidators.
//
// The generation only ever goes up: it is the floor below which the API
// rejects certificates, and a sync that slipped past that check must not
// lower it.
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
		var (
			prev             storedSync
			valStart, valEnd sql.NullInt64
		)

		txErr := tx.QueryRowContext(ctx, `
			SELECT cert_hash, validators_start, validators_end, roles, remote_addr,
				status_hash, last_seen
			FROM hosts WHERE name = ?`,
			cert.Subject).Scan(&prev.certHash, &valStart, &valEnd, &prev.roles,
			&prev.remoteAddr, &prev.statusHash, &prev.lastSeen)
		if txErr == nil {
			result.PreviousSeen = fromMillis(prev.lastSeen)

			if valStart.Valid && valEnd.Valid {
				result.ValidatorsBefore = &identity.ValidatorRange{
					Start: uint64(valStart.Int64), End: uint64(valEnd.Int64),
				}
			}
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

		if !result.Created && prev.confirms(in, rolesJSON, statusHash, result.ValidatorsBefore) {
			result.Host, txErr = s.confirmSync(ctx, tx, in, at)

			return txErr
		}

		result.Changed = true

		holdReason := ""
		if result.HoldAdopted {
			holdReason = HoldReasonAdopted
		}

		counted, overlaps, txErr := countedValidators(ctx, tx, in, result.ValidatorsBefore, at)
		if txErr != nil {
			return txErr
		}

		result.ValidatorOverlaps = overlaps
		valStart, valEnd = sql.NullInt64{}, sql.NullInt64{}

		if counted != nil {
			valStart = sql.NullInt64{Int64: int64(counted.Start), Valid: true}
			valEnd = sql.NullInt64{Int64: int64(counted.End), Valid: true}
		}

		// hold_wanted and hold_reason are only written for a new row.
		_, txErr = tx.ExecContext(ctx, `
			INSERT INTO hosts (name, cert_hash, generation, validators_start, validators_end,
				roles, first_seen, last_seen, remote_addr, status, hold_wanted, hold_reason,
				status_hash, sync_interval)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET
				cert_hash = excluded.cert_hash,
				generation = max(hosts.generation, excluded.generation),
				validators_start = excluded.validators_start,
				validators_end = excluded.validators_end,
				roles = excluded.roles,
				last_seen = excluded.last_seen,
				remote_addr = excluded.remote_addr,
				status = COALESCE(excluded.status, hosts.status),
				status_hash = CASE WHEN excluded.status IS NULL
					THEN hosts.status_hash ELSE excluded.status_hash END,
				sync_interval = excluded.sync_interval`,
			cert.Subject, cert.Hash, int64(cert.Generation), valStart, valEnd,
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
// certificate, roles and address, a status with the same hash, and a
// validator range that is counted as reported (or not known, which keeps the
// counted one). A range of which part is counted for another host goes the
// full way, so it picks up validators that host has given up since.
func (p *storedSync) confirms(
	in *HostSync, roles, statusHash string, counted *identity.ValidatorRange,
) bool {
	if in.Request == nil || statusHash == "" || p.statusHash != statusHash ||
		p.certHash != in.Cert.Hash || p.roles != roles || p.remoteAddr != in.RemoteAddr {
		return false
	}

	reported := in.Request.Validators

	switch {
	case reported == nil || (reported.Known() && reported.Count() == 0):
		return counted == nil
	case !reported.Known() || reported.Validate() != nil:
		return true
	default:
		return counted != nil && counted.Start == reported.Start && counted.End == reported.End
	}
}

// confirmSync writes what a confirming sync changes: the time, the uptime
// and the asked sync interval. The certificate generation is not written:
// confirms compared the certificate hash, which covers it.
func (s *sqliteStore) confirmSync(ctx context.Context, tx *sql.Tx, in *HostSync, at time.Time) (*fleet.Host, error) {
	_, err := tx.ExecContext(ctx, `
		UPDATE hosts SET last_seen = ?, sync_interval = ?,
			status = json_set(status, '$.sentAt', ?, '$.uptimeSeconds', ?)
		WHERE name = ?`,
		millis(at), in.SyncInterval.Milliseconds(),
		in.Request.SentAt.Format(time.RFC3339Nano), in.Request.UptimeSeconds, in.Cert.Subject)
	if err != nil {
		return nil, err
	}

	return s.getHost(ctx, tx, in.Cert.Subject)
}

// countedValidators decides the validator range counted for the host of a
// sync, and names the other hosts for which validators of the reported
// range are counted.
//
//   - No request (an identity-only update), or a host that does not know its
//     range (marker missing or invalid): the range counted before stays. A
//     host whose keys are being reshuffled keeps its weight for those
//     minutes, and its stopped validators count as unavailable, which is
//     what they are.
//   - A request without a range, or with an empty one: none.
//   - A known range: the range, without the validators counted for other
//     hosts (fleet.UnclaimedRange). A host that has not synced for
//     ValidatorClaimTTL loses the validators another host reports.
//
// The store only bounds the range (protocol.Validators.Validate); a range
// that fails is treated as not known. The API checks it before.
func countedValidators(
	ctx context.Context, tx *sql.Tx, in *HostSync, before *identity.ValidatorRange, at time.Time,
) (counted *identity.ValidatorRange, overlaps []string, err error) {
	if in.Request == nil {
		return before, nil, nil
	}

	reported := in.Request.Validators
	countable := reported.Known() && reported.Validate() == nil

	switch {
	case reported == nil:
		return nil, nil, nil
	case !countable:
		return before, nil, nil
	case reported.Count() == 0:
		return nil, nil, nil
	}

	want := identity.ValidatorRange{Start: reported.Start, End: reported.End}

	type claim struct {
		host     string
		lastSeen int64
		held     identity.ValidatorRange
	}

	claims := make([]claim, 0, 2)

	err = queryRows(ctx, tx, `
		SELECT name, last_seen, validators_start, validators_end FROM hosts
		WHERE name != ? AND validators_start < ? AND validators_end > ?
		ORDER BY validators_start, name`,
		[]any{in.Cert.Subject, int64(want.End), int64(want.Start)},
		func(rows rowScanner) error {
			var (
				c          claim
				start, end int64
			)

			if scanErr := rows.Scan(&c.host, &c.lastSeen, &start, &end); scanErr != nil {
				return scanErr
			}

			c.held = identity.ValidatorRange{Start: uint64(start), End: uint64(end)}
			claims = append(claims, c)

			return nil
		})
	if err != nil {
		return nil, nil, fmt.Errorf("validator ranges of other hosts: %w", err)
	}

	taken := make([]identity.ValidatorRange, 0, len(claims))
	overlaps = make([]string, 0, len(claims))

	for _, c := range claims {
		overlaps = append(overlaps, c.host)

		if at.Sub(fromMillis(c.lastSeen)) <= ValidatorClaimTTL {
			taken = append(taken, c.held)

			continue
		}

		// The silent host gives its validators up; it claims what is left
		// when it syncs again.
		_, err = tx.ExecContext(ctx, `UPDATE hosts
			SET validators_start = NULL, validators_end = NULL WHERE name = ?`, c.host)
		if err != nil {
			return nil, nil, fmt.Errorf("release validators of %s: %w", c.host, err)
		}
	}

	slices.Sort(overlaps)

	return fleet.UnclaimedRange(want, taken), overlaps, nil
}

// encodeSync returns the roles and status columns of a sync. The status is
// NULL without a request.
func encodeSync(in *HostSync) (roles string, status sql.NullString, hash string, err error) {
	list := in.Cert.Roles
	if list == nil {
		list = []string{}
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

	return host, err
}

// ListHosts returns all hosts ordered by name.
func (s *sqliteStore) ListHosts(ctx context.Context) ([]*fleet.Host, error) {
	hosts := make([]*fleet.Host, 0, 32)

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `SELECT `+hostColumns+` FROM hosts ORDER BY name`, nil,
			func(row rowScanner) error {
				host, err := s.scanHost(row)
				if err != nil {
					return err
				}

				hosts = append(hosts, host)

				return nil
			})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list hosts: %w", err)
	}

	return hosts, nil
}

func (s *sqliteStore) scanHost(row rowScanner) (*fleet.Host, error) {
	var (
		host                fleet.Host
		generation          int64
		valStart, valEnd    sql.NullInt64
		roles               string
		firstSeen, lastSeen int64
		status              sql.NullString
		broken, offline     string
		syncInterval        int64
	)

	err := row.Scan(&host.Name, &host.CertHash, &generation, &valStart, &valEnd, &roles,
		&firstSeen, &lastSeen, &host.RemoteAddr, &status, &host.InventoryHash,
		&host.HoldWanted, &host.HoldReason, &host.HoldBy, &offline, &broken, &syncInterval,
		&host.StatusHash)
	if err != nil {
		return nil, err
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

	host.Generation = uint64(generation)
	host.SyncInterval = time.Duration(syncInterval) * time.Millisecond
	host.FirstSeen = fromMillis(firstSeen)
	host.LastSeen = fromMillis(lastSeen)

	if valStart.Valid && valEnd.Valid {
		host.Validators = &identity.ValidatorRange{
			Start: uint64(valStart.Int64),
			End:   uint64(valEnd.Int64),
		}
	}

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
	ctx context.Context, tx *sql.Tx, name string, wanted bool, reason, by string,
) error {
	if !wanted {
		by = ""
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE hosts SET hold_wanted = ?, hold_reason = ?, hold_by = ? WHERE name = ?`,
		wanted, reason, by, name)
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
