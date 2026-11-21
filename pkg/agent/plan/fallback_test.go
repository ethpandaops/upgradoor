package plan

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestFallbackPolicy(t *testing.T) {
	p, err := loadPolicy(t, "verbs: [roll]\n")
	require.NoError(t, err)
	assert.False(t, p.Fallback.Enabled())
	assert.Equal(t, Fallback{After: DefaultFallbackAfter, Interval: DefaultFallbackInterval}, p.Fallback)

	p, err = loadPolicy(t, "fallback: {containers: [geth, 'teku-*'], after: 2h, interval: 10m}\n")
	require.NoError(t, err)
	assert.True(t, p.Fallback.Enabled())
	assert.Equal(t, 2*time.Hour, p.Fallback.After)
	assert.Equal(t, 10*time.Minute, p.Fallback.Interval)
	assert.True(t, p.Fallback.covers("geth"))
	assert.True(t, p.Fallback.covers("teku-1"))
	assert.False(t, p.Fallback.covers("dora"))

	for raw, want := range map[string]string{
		"fallback: {containers: ['[']}":                 `fallback.containers: "[" is not a name or pattern`,
		"fallback: {containers: ['']}":                  "is not a name or pattern",
		"fallback: {containers: [geth], after: 1m}":     "fallback.after is below 5m0s",
		"fallback: {containers: [geth], interval: 10s}": "fallback.interval is below 1m0s",
	} {
		_, err = loadPolicy(t, raw)
		require.ErrorContains(t, err, want, raw)
	}

	// The times are not checked while the fallback is off.
	_, err = loadPolicy(t, "fallback: {after: 1m, interval: 1s}")
	require.NoError(t, err)
}

func TestFallbackTagsAndNeeds(t *testing.T) {
	p := needsPolicy(protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot)
	p.DefaultGroup.Roll = true
	v := observed(p)
	needs := Needs(p, v, nil)

	assert.Empty(t, FallbackTags(p, v), "nothing is listed")
	assert.Empty(t, FallbackNeeds(p, needs))

	p.Fallback.Containers = []string{"geth", "do*"}

	assert.Equal(t, map[string]string{
		"docker.io/ethpandaops/geth:master": "ethpandaops/geth:master",
		"docker.io/ethpandaops/dora:latest": "ethpandaops/dora:latest",
	}, FallbackTags(p, v))
	assert.ElementsMatch(t, []string{
		protocol.ImageNeedID("geth", "sha256:b"), protocol.ImageNeedID("dora", "sha256:e"),
	}, ids(FallbackNeeds(p, needs)), "no packages, no reboot, no other container")

	// An image whose pull has just failed waits.
	pullFailed(v)
	assert.Empty(t, FallbackNeeds(p, Needs(p, v, nil)))

	// A container that is never rolled is not pulled for either.
	p.DefaultGroup.Roll = false
	v = observed(p)
	assert.NotContains(t, FallbackTags(p, v), "docker.io/ethpandaops/dora:latest")

	p.Verbs = []protocol.Verb{protocol.VerbReboot}
	assert.Empty(t, FallbackTags(p, observed(p)), "without the roll verb")
}
