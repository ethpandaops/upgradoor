package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// storedSettings reads the settings table: key -> JSON value.
func storedSettings(t *testing.T, s Store) map[string]string {
	t.Helper()

	rows, err := rawDB(t, s).Query(`SELECT key, value FROM settings`)
	require.NoError(t, err)

	defer func() { require.NoError(t, rows.Close()) }()

	stored := make(map[string]string, 8)

	for rows.Next() {
		var key, value string

		require.NoError(t, rows.Scan(&key, &value))

		stored[key] = value
	}

	require.NoError(t, rows.Err())

	return stored
}

func TestSettings(t *testing.T) {
	ctx := context.Background()
	defaults := fleet.DefaultSettings()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			got, err := s.LoadSettings(ctx, defaults)
			require.NoError(t, err)
			assert.Equal(t, defaults, got, "nothing stored: the defaults")

			changed := defaults
			changed.MaxUnavailablePercent = 12.5
			changed.SoakEpochs = 3
			changed.LeaseTTL = fleet.Duration(45 * time.Minute)
			changed.PolicyReboot = fleet.PolicyAuto
			changed.Paused = true

			require.NoError(t, s.SaveSettings(ctx, defaults, changed))
			assert.Equal(t, map[string]string{
				"maxUnavailablePercent": "12.5",
				"soakEpochs":            "3",
				"leaseTtl":              `"45m0s"`,
				"policyReboot":          `"auto"`,
				"paused":                "true",
			}, storedSettings(t, s), "one row per field that differs, keyed by its JSON name")

			got, err = s.LoadSettings(ctx, defaults)
			require.NoError(t, err)
			assert.Equal(t, changed, got)

			// Overrides apply on top of whatever the defaults are now: a
			// changed flag default shows through for fields not overridden.
			other := defaults
			other.MinParticipationPercent = 80
			other.SoakEpochs = 9

			got, err = s.LoadSettings(ctx, other)
			require.NoError(t, err)
			assert.InDelta(t, 80.0, got.MinParticipationPercent, 0, "not overridden: the new default")
			assert.Equal(t, uint64(3), got.SoakEpochs, "overridden: the stored value")

			// Setting fields back to their defaults removes their rows.
			partly := changed
			partly.SoakEpochs = defaults.SoakEpochs
			partly.Paused = false
			partly.HostStaleAfter = fleet.Duration(5 * time.Minute)

			require.NoError(t, s.SaveSettings(ctx, defaults, partly))
			assert.Equal(t, map[string]string{
				"maxUnavailablePercent": "12.5",
				"leaseTtl":              `"45m0s"`,
				"policyReboot":          `"auto"`,
				"hostStaleAfter":        `"5m0s"`,
			}, storedSettings(t, s))

			got, err = s.LoadSettings(ctx, defaults)
			require.NoError(t, err)
			assert.Equal(t, partly, got)

			// A value that equals the default but is the zero value of its
			// type is still an override when the default is not zero.
			zeroed := defaults
			zeroed.MaxUnavailablePercent = 0
			zeroed.PolicyImage = ""

			require.NoError(t, s.SaveSettings(ctx, defaults, zeroed))
			assert.Equal(t, map[string]string{
				"maxUnavailablePercent": "0",
				"policyImage":           `""`,
			}, storedSettings(t, s))

			got, err = s.LoadSettings(ctx, defaults)
			require.NoError(t, err)
			assert.Equal(t, zeroed, got)

			// Back to the defaults: the table is empty again.
			require.NoError(t, s.SaveSettings(ctx, defaults, defaults))
			assert.Empty(t, storedSettings(t, s))

			got, err = s.LoadSettings(ctx, defaults)
			require.NoError(t, err)
			assert.Equal(t, defaults, got)
		})
	}
}

func TestLoadSettingsIgnoresBadRows(t *testing.T) {
	ctx := context.Background()
	defaults := fleet.DefaultSettings()

	log, hook := logrustest.NewNullLogger()
	s := startStore(t, New(log, filepath.Join(t.TempDir(), "upgradoor.db")))

	_, err := rawDB(t, s).Exec(`
		INSERT INTO settings (key, value) VALUES
			('soakEpochs', '4'),
			('removedInV2', '"whatever"'),
			('leaseTtl', '"not a duration"'),
			('maxFinalityLagEpochs', '"three"'),
			('paused', 'tru'),
			('policyImage', '"manual"')`)
	require.NoError(t, err)

	got, err := s.LoadSettings(ctx, defaults)
	require.NoError(t, err)

	want := defaults
	want.SoakEpochs = 4
	want.PolicyImage = fleet.PolicyManual
	assert.Equal(t, want, got, "good rows apply; unknown and undecodable ones leave the default")

	warned := make(map[string]string, 4)

	for _, entry := range hook.AllEntries() {
		if entry.Level != logrus.WarnLevel {
			continue
		}

		key, ok := entry.Data["key"].(string)
		require.True(t, ok)

		warned[key] = entry.Message
	}

	require.Len(t, warned, 4)
	assert.Contains(t, warned["removedInV2"], "unknown key")
	assert.Contains(t, warned["leaseTtl"], "invalid value")
	assert.Contains(t, warned["maxFinalityLagEpochs"], "invalid value")
	assert.Contains(t, warned["paused"], "invalid value")

	// Saving cleans the table up: only real differences remain.
	require.NoError(t, s.SaveSettings(ctx, defaults, got))
	assert.Equal(t, map[string]string{"soakEpochs": "4", "policyImage": `"manual"`}, storedSettings(t, s))
}

func TestSettingsFieldsCoverEveryField(t *testing.T) {
	defaults := fleet.DefaultSettings()

	fields, err := settingsFields(&defaults)
	require.NoError(t, err)

	// Every field of fleet.Settings must have its own key: a field with
	// `json:"-"` or omitempty would silently never be stored.
	assert.ElementsMatch(t, []string{
		"maxUnavailablePercent", "minParticipationPercent", "maxFinalityLagEpochs", "soakEpochs",
		"leaseTtl", "hostStaleAfter", "readinessTimeout",
		"policyImage", "policyPackages", "policyReboot", "paused",
	}, keysOf(fields))
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	return keys
}
