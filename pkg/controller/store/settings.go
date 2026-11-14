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

// renamedSettings maps the stored key of a setting that was renamed to the
// key it has now. The value means the same under both names.
var renamedSettings = map[string]string{"soakEpochs": "readyEpochs"}

// LoadSettings returns defaults with every stored override applied. A value
// stored under the former name of a setting is read under its new name,
// unless one is stored there too; the next save writes only the new name. A
// stored key that is not a field of fleet.Settings, or whose value no longer
// decodes into its field, is skipped with a warning and the default stays.
//
//nolint:gocritic // hugeParam: the by-value signature is fixed by the Store interface.
func (s *sqliteStore) LoadSettings(
	ctx context.Context, defaults fleet.Settings,
) (fleet.Settings, error) {
	known, err := settingsFields(&defaults)
	if err != nil {
		return defaults, fmt.Errorf("store: load settings: %w", err)
	}

	overrides := make(map[string]string, len(known))

	err = s.read(ctx, func(tx *sql.Tx) error {
		return queryRows(ctx, tx, `SELECT key, value FROM settings ORDER BY key`, nil,
			func(row rowScanner) error {
				var key, value string
				if scanErr := row.Scan(&key, &value); scanErr != nil {
					return scanErr
				}

				overrides[key] = value

				return nil
			})
	})
	if err != nil {
		return defaults, fmt.Errorf("store: load settings: %w", err)
	}

	for old, key := range renamedSettings {
		value, ok := overrides[old]
		if !ok {
			continue
		}

		if _, stored := overrides[key]; !stored {
			overrides[key] = value
		}

		delete(overrides, old)
	}

	settings := defaults

	for key, value := range overrides {
		if _, ok := known[key]; !ok {
			s.log.WithField("key", key).Warn("ignoring stored setting: unknown key")

			continue
		}

		field, err := json.Marshal(map[string]json.RawMessage{key: json.RawMessage(value)})
		if err == nil {
			// Decode into a copy: a failed decode must not leave a
			// half-applied value behind.
			next := settings
			if err = json.Unmarshal(field, &next); err == nil {
				settings = next

				continue
			}
		}

		s.log.WithField("key", key).WithError(err).
			Warn("ignoring stored setting: invalid value, using the default")
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
