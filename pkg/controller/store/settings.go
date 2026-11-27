package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// LoadSettings returns defaults with every stored override applied. Every
// row was written by SaveSettings: one that is not a field of fleet.Settings
// or does not decode into its field is an error.
//
//nolint:gocritic // hugeParam: the by-value signature is fixed by the Store interface.
func (s *sqliteStore) LoadSettings(
	ctx context.Context, defaults fleet.Settings,
) (fleet.Settings, error) {
	overrides := make(map[string]json.RawMessage, 16)

	err := s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `SELECT key, value FROM settings`, nil,
			func(row rowScanner) error {
				var key, value string
				if scanErr := row.Scan(&key, &value); scanErr != nil {
					return scanErr
				}

				overrides[key] = json.RawMessage(value)

				return nil
			})
	})
	if err != nil {
		return defaults, fmt.Errorf("store: load settings: %w", err)
	}

	stored, err := json.Marshal(overrides)
	if err != nil {
		return defaults, fmt.Errorf("store: load settings: %w", err)
	}

	settings := defaults

	decoder := json.NewDecoder(bytes.NewReader(stored))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&settings); err != nil {
		return defaults, fmt.Errorf("store: load settings: %w", err)
	}

	return settings, nil
}

// SaveSettings stores one row per field of settings that differs from
// defaults and removes every other row.
//
//nolint:gocritic // hugeParam: the by-value signature is fixed by the Store interface.
func (s *sqliteStore) SaveSettings(ctx context.Context, defaults, settings fleet.Settings) error {
	base, err := settingsFields(&defaults)
	if err != nil {
		return fmt.Errorf("store: save settings: %w", err)
	}

	wanted, err := settingsFields(&settings)
	if err != nil {
		return fmt.Errorf("store: save settings: %w", err)
	}

	keys := make([]string, 0, len(wanted))

	for key, value := range wanted {
		if !bytes.Equal(value, base[key]) {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	err = s.write(ctx, func(tx *sql.Tx) error {
		_, txErr := tx.ExecContext(ctx, `DELETE FROM settings`)
		if txErr != nil {
			return txErr
		}

		for _, key := range keys {
			_, txErr = tx.ExecContext(ctx,
				`INSERT INTO settings (key, value) VALUES (?, ?)`, key, string(wanted[key]))
			if txErr != nil {
				return fmt.Errorf("setting %s: %w", key, txErr)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("store: save settings: %w", err)
	}

	return nil
}

// settingsFields returns the JSON encoding of every field of settings, keyed
// by its JSON name.
func settingsFields(settings *fleet.Settings) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}

	fields := make(map[string]json.RawMessage, 16)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}

	return fields, nil
}

// DeployWindow returns the stored deployment window; the zero value when the
// store holds none.
func (s *sqliteStore) DeployWindow(ctx context.Context) (fleet.DeployWindowState, error) {
	var w fleet.DeployWindowState

	err := s.read(ctx, func(tx *sql.Tx) error {
		var err error

		w, err = readDeployWindow(ctx, tx)

		return err
	})
	if err != nil {
		return w, fmt.Errorf("store: deploy window: %w", err)
	}

	return w, nil
}

// StartDeployWindow opens the deployment window at at unless the store holds
// one, and returns the stored window.
func (s *sqliteStore) StartDeployWindow(ctx context.Context, at time.Time) (fleet.DeployWindowState, error) {
	var w fleet.DeployWindowState

	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO deploy_window (id, started_at) VALUES (1, ?) ON CONFLICT (id) DO NOTHING`, millis(at))
		if err != nil {
			return err
		}

		w, err = readDeployWindow(ctx, tx)

		return err
	})
	if err != nil {
		return w, fmt.Errorf("store: start deploy window: %w", err)
	}

	return w, nil
}

// ArmDeployWindow arms a stored window that is not armed at at and appends
// entry, in one transaction. armed is false when there was nothing to arm;
// then nothing is written.
func (s *sqliteStore) ArmDeployWindow(
	ctx context.Context, at time.Time, entry *fleet.AuditEntry,
) (armed bool, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		res, txErr := tx.ExecContext(ctx,
			`UPDATE deploy_window SET armed_at = ? WHERE id = 1 AND armed_at IS NULL`, millis(at))
		if txErr != nil {
			return txErr
		}

		n, txErr := res.RowsAffected()
		if txErr != nil || n == 0 {
			return txErr
		}

		armed = true

		_, txErr = insertAudit(ctx, tx, []fleet.AuditEntry{*entry})

		return txErr
	})
	if err != nil {
		return false, fmt.Errorf("store: arm deploy window: %w", err)
	}

	return armed, nil
}

func readDeployWindow(ctx context.Context, tx *sql.Tx) (fleet.DeployWindowState, error) {
	var (
		w       fleet.DeployWindowState
		started int64
		armed   sql.NullInt64
	)

	err := tx.QueryRowContext(ctx, `SELECT started_at, armed_at FROM deploy_window WHERE id = 1`).
		Scan(&started, &armed)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return w, nil
	case err != nil:
		return w, err
	}

	w.StartedAt = fromMillis(started)

	if armed.Valid {
		t := fromMillis(armed.Int64)
		w.ArmedAt = &t
	}

	return w, nil
}
