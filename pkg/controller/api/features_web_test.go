package api

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webPermissions is the page's copy of the rule, relative to this package.
const webPermissions = "../../../web/src/lib/permissions.ts"

// tsBlock returns the text between a line that starts a declaration and the
// line that closes it.
func tsBlock(t *testing.T, src, start, end string) string {
	t.Helper()

	_, rest, ok := strings.Cut(src, start)
	require.True(t, ok, "%s has no %q", webPermissions, start)

	block, _, ok := strings.Cut(rest, end)
	require.True(t, ok, "%q is not closed by %q", start, end)

	return block
}

// TestWebFeaturesAreTheServers: the features the page asks the auth client
// for are the features this package checks, name for name, and the page
// gives a team twin to the same ones. A feature added, renamed or removed on
// one side only fails here.
func TestWebFeaturesAreTheServers(t *testing.T) {
	raw, err := os.ReadFile(webPermissions)
	require.NoError(t, err)

	src := string(raw)

	// export const FEATURE = { roll: 'roll', ... } as const;
	entry := regexp.MustCompile(`(?m)^\s*(\w+):\s*'([^']+)',\s*$`)
	web := make([]string, 0, 8)

	for _, m := range entry.FindAllStringSubmatch(tsBlock(t, src, "export const FEATURE = {", "} as const;"), -1) {
		assert.Equal(t, m[1], m[2], "the page names a feature by its own name")
		web = append(web, m[2])
	}

	server := []string{
		FeatureRoll, FeatureReboot, FeatureHold, FeatureRecover, FeatureSupervise, FeatureAdmin,
	}

	slices.Sort(web)
	slices.Sort(server)
	assert.Equal(t, server, web, "features of %s and of authz.go", webPermissions)

	// export const TWINNED: readonly Feature[] = [FEATURE.roll, ...];
	twinned := make([]string, 0, 4)
	for _, m := range regexp.MustCompile(`FEATURE\.(\w+)`).FindAllStringSubmatch(
		tsBlock(t, src, "export const TWINNED: readonly Feature[] = [", "];"), -1) {
		twinned = append(twinned, m[1])
	}

	slices.Sort(twinned)
	assert.Equal(t, []string{FeatureHold, FeatureReboot, FeatureRecover, FeatureRoll}, twinned,
		"the features with a team twin")

	assert.Contains(t, src, "export const TEAM_PREFIX = '"+teamFeaturePrefix+"';")

	// The twins the server gives are the ones whose requirement names
	// targets: one request per action with a twin.
	h := newHarness(t, nil)
	ownershipFixture(h)

	withTwin := make([]string, 0, 4)

	for _, s := range actionSamples(t) {
		if s.twin && !slices.Contains(withTwin, s.feature) {
			withTwin = append(withTwin, s.feature)
		}
	}

	slices.Sort(withTwin)
	assert.Equal(t, twinned, withTwin)
}
