package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

const (
	// DefaultAuditLimit is the number of entries ListAudit returns when the
	// filter has no limit.
	DefaultAuditLimit = 100
	// MaxAuditLimit caps the number of entries ListAudit returns.
	MaxAuditLimit = 500
)

// AppendAudit appends entries in one transaction; IDs are assigned in
// argument order and a zero Time is set to now.
func (s *sqliteStore) AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error {
	if len(entries) == 0 {
		return nil
	}

	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := insertAudit(ctx, tx, entries)

		return err
	})
	if err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}

	return nil
}

// insertAudit inserts entries and returns them as stored: with their ID and
// with a zero Time replaced by now. The input is not modified.
func insertAudit(
	ctx context.Context, tx *sql.Tx, entries []fleet.AuditEntry,
) ([]fleet.AuditEntry, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO audit (time, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, err
	}

	defer func() { _ = stmt.Close() }()

	now := time.Now()
	stored := make([]fleet.AuditEntry, 0, len(entries))

	for i := range entries {
		entry := entries[i]
		if entry.Time.IsZero() {
			entry.Time = now
		}

		var detail sql.NullString

		if entry.Detail != nil {
			raw, err := json.Marshal(entry.Detail)
			if err != nil {
				return nil, fmt.Errorf("audit entry %d (%s): encode detail: %w", i, entry.Action, err)
			}

			detail = sql.NullString{String: string(raw), Valid: true}
		}

		res, err := stmt.ExecContext(ctx,
			millis(entry.Time), entry.Actor, entry.Action, entry.Target, detail)
		if err != nil {
			return nil, fmt.Errorf("audit entry %d (%s): %w", i, entry.Action, err)
		}

		entry.ID, err = res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("audit entry %d (%s): %w", i, entry.Action, err)
		}

		stored = append(stored, entry)
	}

	return stored, nil
}

// auditClockSkew bounds how far the time of an entry can run behind that of
// an entry with a smaller ID: a scheduler pass stamps its entries with the
// time it started.
const auditClockSkew = time.Minute

// Conditions on the actor that match the partial indexes audit_people and
// audit_agents word for word, which the planner needs to use them.
const (
	auditPeopleTerm = `actor != 'scheduler' AND substr(actor, 1, 6) != 'agent:'`
	auditAgentsTerm = `substr(actor, 1, 6) = 'agent:'`
)

// auditDriver is the index a filtered audit query walks.
type auditDriver int

const (
	// auditByID walks the table backwards by ID: filters that select much of
	// the log find a page quickly that way.
	auditByID auditDriver = iota
	auditByTarget
	auditByPeople
	auditByAgents
	auditByAction
)

// ListAudit returns entries, newest first. Limit 0 means DefaultAuditLimit;
// it is capped at MaxAuditLimit.
func (s *sqliteStore) ListAudit(
	ctx context.Context, filter AuditFilter, //nolint:gocritic // hugeParam: the signature is the Store contract.
) ([]fleet.AuditEntry, error) {
	limit := filter.Limit

	switch {
	case limit <= 0:
		limit = DefaultAuditLimit
	case limit > MaxAuditLimit:
		limit = MaxAuditLimit
	}

	entries := make([]fleet.AuditEntry, 0, min(limit, DefaultAuditLimit))

	err := s.read(ctx, func(tx *sql.Tx) error {
		after, before, err := auditTimeBounds(ctx, tx, filter.From, filter.To)
		if err != nil {
			return err
		}

		query, args := auditQuery(&filter, limit, after, before)

		return queryRows(ctx, tx, query, args, func(row rowScanner) error {
			var (
				entry  fleet.AuditEntry
				at     int64
				detail sql.NullString
			)

			err := row.Scan(&entry.ID, &at, &entry.Actor, &entry.Action, &entry.Target, &detail)
			if err != nil {
				return err
			}

			entry.Time = fromMillis(at)

			if detail.Valid {
				if err := json.Unmarshal([]byte(detail.String), &entry.Detail); err != nil {
					return fmt.Errorf("decode detail of entry %d: %w", entry.ID, err)
				}
			}

			entries = append(entries, entry)

			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}

	return entries, nil
}

// auditTimeBounds turns a time range into IDs: every entry from "from" on
// has an ID above after, every entry before "to" one below before; 0 is no
// bound. IDs grow with time but for auditClockSkew.
func auditTimeBounds(ctx context.Context, tx *sql.Tx, from, to time.Time) (after, before int64, err error) {
	bound := func(query string, at time.Time) (int64, error) {
		var id int64

		scanErr := tx.QueryRowContext(ctx, query, millis(at)).Scan(&id)
		if errors.Is(scanErr, sql.ErrNoRows) {
			return 0, nil
		}

		return id, scanErr
	}

	if !from.IsZero() {
		after, err = bound(`SELECT id FROM audit WHERE time < ? ORDER BY time DESC LIMIT 1`,
			from.Add(-auditClockSkew))
		if err != nil {
			return 0, 0, err
		}
	}

	if !to.IsZero() {
		before, err = bound(`SELECT id FROM audit WHERE time >= ? ORDER BY time LIMIT 1`,
			to.Add(auditClockSkew))
		if err != nil {
			return 0, 0, err
		}
	}

	return after, before, nil
}

// auditActions is a filter on the action: the actions it allows, or for a
// prefix that names no action of fleet.AuditActions the prefix itself.
type auditActions struct {
	actions []string
	prefix  string
	// targetSettings: the filter is the target "settings" instead.
	targetSettings bool
	// dense: the scheduler records some of the actions on its own.
	dense bool
}

// auditActionFilters are the action filters of a filter.
func auditActionFilters(f *AuditFilter) []auditActions {
	out := make([]auditActions, 0, 3)

	if f.Group != "" {
		if f.Group == fleet.AuditGroupSettings {
			out = append(out, auditActions{targetSettings: true})
		} else {
			actions, dense := fleet.AuditGroupActions(f.Group)
			out = append(out, auditActions{actions: actions, dense: dense})
		}
	}

	if f.TargetKind != "" {
		if f.TargetKind == fleet.AuditTargetSetting {
			out = append(out, auditActions{targetSettings: true})
		} else {
			actions, dense := fleet.AuditTargetActions(f.TargetKind)
			out = append(out, auditActions{actions: actions, dense: dense})
		}
	}

	if f.ActionPrefix != "" {
		out = append(out, auditActions{prefix: f.ActionPrefix, dense: prefixDense(f.ActionPrefix)})
	}

	return out
}

// prefixDense says whether entries with an action prefix can be many: the
// scheduler records one of its actions on its own, or it names none.
func prefixDense(prefix string) bool {
	known := false

	for _, a := range fleet.AuditActions {
		if strings.HasPrefix(a.Action, prefix) {
			known = true

			if a.Scheduler {
				return true
			}
		}
	}

	return !known
}

// auditDriverOf chooses the index a filter is served from: the one that
// selects fewest entries in ID order or nearly so.
func auditDriverOf(f *AuditFilter, actions []auditActions) auditDriver {
	kind := f.ActorKind
	if f.Actor != "" {
		kind = fleet.AuditActorKindOf(f.Actor)
	}

	switch {
	case f.Target != "" || slices.ContainsFunc(actions, func(a auditActions) bool { return a.targetSettings }):
		return auditByTarget
	case kind == fleet.AuditPeople:
		return auditByPeople
	case kind == fleet.AuditAgents:
		return auditByAgents
	case slices.ContainsFunc(actions, func(a auditActions) bool { return !a.dense }):
		return auditByAction
	default:
		return auditByID
	}
}

// auditIndexes are the indexes the drivers name; auditByID reads none.
var auditIndexes = map[auditDriver]string{
	auditByID:     "NOT INDEXED",
	auditByTarget: "INDEXED BY audit_target",
	auditByPeople: "INDEXED BY audit_people",
	auditByAgents: "INDEXED BY audit_agents",
	auditByAction: "INDEXED BY audit_action",
}

// auditKindTerms are the conditions of the actor kinds.
var auditKindTerms = map[fleet.AuditActorKind]string{
	fleet.AuditPeople:    auditPeopleTerm,
	fleet.AuditAgents:    auditAgentsTerm,
	fleet.AuditScheduler: `actor = 'scheduler'`,
}

// auditQuery is the SELECT of a filtered page, from the index auditDriverOf
// chooses: the planner has no statistics of how few entries are people's.
func auditQuery(f *AuditFilter, limit int, after, before int64) (query string, args []any) {
	actions := auditActionFilters(f)
	driver := auditDriverOf(f, actions)

	var cond conditions

	if f.Target != "" {
		cond.add(`target = ?`, f.Target)
	}

	if f.ActorKind != "" {
		cond.add(auditKindTerms[f.ActorKind])
	}

	if f.Actor != "" {
		cond.add(`actor = ?`, f.Actor)

		if kind := fleet.AuditActorKindOf(f.Actor); kind != f.ActorKind {
			cond.add(auditKindTerms[kind])
		}
	}

	for _, a := range actions {
		switch {
		case a.targetSettings:
			cond.add(`target = ?`, fleet.AuditTargetSettings)
		case a.prefix != "":
			// substr compares exactly (LIKE would ignore case and need
			// escaping); the range lets the index serve it.
			cond.add(`action >= ? AND action < ? AND substr(action, 1, length(?)) = ?`,
				a.prefix, a.prefix+"\U0010FFFF", a.prefix, a.prefix)
		default:
			args := make([]any, 0, len(a.actions)+1)
			for _, action := range a.actions {
				args = append(args, action)
			}

			cond.add(`action IN (`+placeholders(len(a.actions))+`) AND target != ?`,
				append(args, fleet.AuditTargetSettings)...)
		}
	}

	if !f.From.IsZero() {
		cond.add(`time >= ?`, millis(f.From))
	}

	if !f.To.IsZero() {
		cond.add(`time < ?`, millis(f.To))
	}

	if after > 0 {
		cond.add(`id > ?`, after)
	}

	if f.BeforeID > 0 && (before == 0 || f.BeforeID < before) {
		before = f.BeforeID
	}

	if before > 0 {
		cond.add(`id < ?`, before)
	}

	if f.Search != "" {
		text := `actor || ' ' || action || ' ' || target`
		if f.SearchDetail {
			text += ` || ' ' || ifnull(detail, '')`
		}

		cond.add(`instr(lower(`+text+`), ?) > 0`, strings.ToLower(f.Search))
	}

	query = `SELECT id, time, actor, action, target, detail FROM audit ` + auditIndexes[driver] +
		cond.where() + ` ORDER BY id DESC LIMIT ?`

	return query, append(cond.args, limit)
}

// AuditActors counts the entries of each person, and those of the agents.
func (s *sqliteStore) AuditActors(ctx context.Context) (people []AuditActor, agents int, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		people = make([]AuditActor, 0, 16)

		rowsErr := queryRows(ctx, tx,
			`SELECT actor, count(*) FROM audit WHERE `+auditPeopleTerm+` GROUP BY actor ORDER BY actor`, nil,
			func(row rowScanner) error {
				var a AuditActor
				if scanErr := row.Scan(&a.Actor, &a.Entries); scanErr != nil {
					return scanErr
				}

				people = append(people, a)

				return nil
			})
		if rowsErr != nil {
			return rowsErr
		}

		return tx.QueryRowContext(ctx, `SELECT count(*) FROM audit WHERE `+auditAgentsTerm).Scan(&agents)
	})
	if err != nil {
		return nil, 0, fmt.Errorf("store: audit actors: %w", err)
	}

	return people, agents, nil
}
