package store

import (
	"context"
	"testing"
	"time"

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
	t.Parallel()

	ctx := context.Background()
	defaults := fleet.DefaultSettings()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)

			got, err := s.LoadSettings(ctx, defaults)
			require.NoError(t, err)
			assert.Equal(t, defaults, got, "nothing stored: the defaults")

			changed := defaults
			changed.MaxUnavailablePercent = 12.5
			changed.ReadyAttestations = 3
			changed.LeaseTTL = fleet.Duration(45 * time.Minute)
			changed.PolicyReboot = fleet.PolicyAuto
			changed.Paused = true

			require.NoError(t, s.SaveSettings(ctx, defaults, changed))
			assert.Equal(t, map[string]string{
				"maxUnavailablePercent": "12.5",
				"readyAttestations":     "3",
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
			other.ReadyAttestations = 9

			got, err = s.LoadSettings(ctx, other)
			require.NoError(t, err)
			assert.InDelta(t, 80.0, got.MinParticipationPercent, 0, "not overridden: the new default")
			assert.Equal(t, uint64(3), got.ReadyAttestations, "overridden: the stored value")

			// Setting fields back to their defaults removes their rows.
			partly := changed
			partly.ReadyAttestations = defaults.ReadyAttestations
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

// Every row is one SaveSettings wrote: anything else is an error, and the
// defaults come back with it.
func TestLoadSettingsRefusesBadRows(t *testing.T) {
	ctx := context.Background()
	defaults := fleet.DefaultSettings()

	for _, row := range []string{
		`('notASetting', '"whatever"')`,
		`('leaseTtl', '"not a duration"')`,
		`('paused', 'maybe')`,
	} {
		t.Run(row, func(t *testing.T) {
			s := newFileStore(t)

			_, err := rawDB(t, s).Exec(`INSERT INTO settings (key, value) VALUES ('readyAttestations', '4'), ` + row)
			require.NoError(t, err)

			got, err := s.LoadSettings(ctx, defaults)
			require.Error(t, err)
			assert.Equal(t, defaults, got)
		})
	}
}

func TestSettingsFieldsCoverEveryField(t *testing.T) {
	t.Parallel()

	defaults := fleet.DefaultSettings()

	fields, err := settingsFields(&defaults)
	require.NoError(t, err)

	// Every field of fleet.Settings must have its own key: a field with
	// `json:"-"` or omitempty would silently never be stored.
	assert.ElementsMatch(t, []string{
		"maxUnavailablePercent", "maxLeasedPercent", "maxIndependentPercent", "minParticipationPercent", "maxFinalityLagEpochs", "readyAttestations", "readyStableFor", "minPeers",
		"leaseTtl", "hostStaleAfter", "readinessTimeout", "baselineGrace", "maxConcurrentPulls",
		"policyImage", "policyPackages", "policyReboot", "paused",
		"pauseAfterCanaryImage", "pauseAfterCanaryPackages", "pauseAfterCanaryReboot", "readyPeerShare",
		"lookupInterval", "deployWindow",
		"policyClients", "policyKinds", "actionPolicies",
	}, keysOf(fields))
}

func TestDeployWindow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()

			s := backend.open(t)

			w, err := s.DeployWindow(ctx)
			require.NoError(t, err)
			assert.Equal(t, fleet.DeployWindowState{}, w, "a fresh store holds no window")

			entry := fleet.AuditEntry{Time: start, Actor: "alice", Action: "action.arm-deploy-window", Target: "settings"}

			armed, err := s.ArmDeployWindow(ctx, start, &entry)
			require.NoError(t, err)
			assert.False(t, armed, "no window to arm")

			w, err = s.StartDeployWindow(ctx, start)
			require.NoError(t, err)
			assert.Equal(t, fleet.DeployWindowState{StartedAt: start}, w)

			w, err = s.StartDeployWindow(ctx, start.Add(time.Hour))
			require.NoError(t, err)
			assert.Equal(t, start, w.StartedAt, "a later start keeps the first")

			at := start.Add(2 * time.Hour)

			armed, err = s.ArmDeployWindow(ctx, at, &entry)
			require.NoError(t, err)
			assert.True(t, armed)

			armed, err = s.ArmDeployWindow(ctx, at.Add(time.Hour), &entry)
			require.NoError(t, err)
			assert.False(t, armed, "armed once")

			w, err = s.DeployWindow(ctx)
			require.NoError(t, err)
			assert.Equal(t, fleet.DeployWindowState{StartedAt: start, ArmedAt: &at}, w)

			entries, err := s.ListAudit(ctx, AuditFilter{ActionPrefix: "action.arm-deploy-window"})
			require.NoError(t, err)
			assert.Len(t, entries, 1, "one entry, with the arm")
		})
	}
}

func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}

	return keys
}
