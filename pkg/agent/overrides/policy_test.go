package overrides

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPolicyPermits(t *testing.T) {
	image := &Entry{Image: "ethpandaops/geth:fix"}
	flags := &Entry{Add: []string{"--verbosity=4"}}

	for _, tc := range []struct {
		name   string
		policy Policy
		entry  *Entry
		ok     bool
	}{
		{"tooling refuses an image", Policy{}, image, false},
		{"tooling refuses flags", Policy{}, flags, false},
		{"tooling takes an empty entry", Policy{}, &Entry{}, true},
		{"image only", Policy{Image: true}, image, true},
		{"image only refuses flags", Policy{Image: true}, flags, false},
		{"flags only refuses an image", Policy{Flags: true}, image, false},
		{"both", Policy{Image: true, Flags: true}, &Entry{Image: image.Image, Add: flags.Add}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Permits("geth", "execution", tc.entry)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestPolicyAllows(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := Policy{Image: true, Flags: true}
	at := func(d time.Duration) *time.Time {
		u := now.Add(d)

		return &u
	}

	require.NoError(t, p.Allows("geth", "execution", &Entry{Image: "a/b:c", Until: at(time.Hour)}, now))
	require.Error(t, p.Allows("geth", "execution", &Entry{Image: "a/b:c"}, now), "no end")
	require.Error(t, p.Allows("geth", "execution", &Entry{Image: "a/b:c", Until: at(-time.Hour)}, now))
	require.Error(t, p.Allows("geth", "execution", &Entry{Image: "a/b:c", Until: at(8 * 24 * time.Hour)}, now))
	require.Error(t, (&Policy{}).Allows("geth", "execution", &Entry{Image: "a/b:c", Until: at(time.Hour)}, now))
}
