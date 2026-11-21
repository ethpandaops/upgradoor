package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

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
