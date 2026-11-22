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
	assert.False(t, p.FallbackEnabled())
	assert.Equal(t, Fallback{After: DefaultFallbackAfter, Interval: DefaultFallbackInterval}, p.Fallback)

	p, err = loadPolicy(t, "kindPolicies: {el: {roll: true}}\nfallback: {after: 2h, interval: 10m}\n")
	require.NoError(t, err)
	assert.True(t, p.FallbackEnabled())
	assert.Equal(t, 2*time.Hour, p.Fallback.After)
	assert.Equal(t, 10*time.Minute, p.Fallback.Interval)

	p, err = loadPolicy(t, "defaultPolicy: roll\n")
	require.NoError(t, err)
	assert.True(t, p.FallbackEnabled(), "unlabelled containers by defaultPolicy")

	for raw, want := range map[string]string{
		"kindPolicies: {el: {roll: true}}\nfallback: {after: 1m}":     "fallback.after is below 5m0s",
		"kindPolicies: {el: {roll: true}}\nfallback: {interval: 10s}": "fallback.interval is below 1m0s",
	} {
		_, err = loadPolicy(t, raw)
		require.ErrorContains(t, err, want, raw)
	}

	// The times are not checked while the fallback is off.
	_, err = loadPolicy(t, "kindPolicies: {el: {roll: false}}\nfallback: {after: 1m, interval: 1s}")
	require.NoError(t, err)
}

// The fallback rolls a labelled container only by its kind's policy, an
// unlabelled one by defaultPolicy.
func TestFallbackTagsAndNeeds(t *testing.T) {
	p := needsPolicy(protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot)
	p.DefaultPolicy = DefaultRoll
	v := observed(p)
	needs := Needs(p, v, nil)

	p.DefaultPolicy = DefaultIgnore
	assert.Empty(t, FallbackTags(p, v), "no kind policy")
	assert.Empty(t, FallbackNeeds(p, v, needs))

	p.DefaultPolicy = DefaultRoll
	p.KindPolicies = map[protocol.ContainerKind]KindPolicy{protocol.KindEL: {Roll: true}, protocol.KindCL: {}}

	assert.Equal(t, map[string]string{
		"docker.io/ethpandaops/geth:master": "ethpandaops/geth:master",
		"docker.io/ethpandaops/dora:latest": "ethpandaops/dora:latest",
	}, FallbackTags(p, v), "not teku: cl does not roll")
	assert.ElementsMatch(t, []string{
		protocol.ImageNeedID("geth", "sha256:b"), protocol.ImageNeedID("dora", "sha256:e"),
	}, ids(FallbackNeeds(p, v, needs)), "no packages, no reboot, no other container")

	// An image whose pull has just failed waits.
	pullFailed(v)
	assert.Empty(t, FallbackNeeds(p, v, Needs(p, v, nil)))

	// An unlabelled container that is ignored is not pulled for either.
	p.DefaultPolicy = DefaultIgnore
	v = observed(p)
	assert.NotContains(t, FallbackTags(p, v), "docker.io/ethpandaops/dora:latest")

	p.Verbs = []protocol.Verb{protocol.VerbReboot}
	assert.Empty(t, FallbackTags(p, observed(p)), "without the roll verb")
}
