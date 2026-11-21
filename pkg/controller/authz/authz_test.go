package authz

import (
	"slices"
	"strings"
	"testing"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
	"github.com/stretchr/testify/assert"
)

// TestFeatureNames: the names are what a grant spells, a twin is the name
// with the prefix, and none collides with what the grammar reserves.
func TestFeatureNames(t *testing.T) {
	features := []string{
		FeatureRoll, FeatureReboot, FeatureHold, FeatureRecover, FeatureSupervise,
		FeatureClientPolicy, FeatureImageOverride, FeatureFlagOverride, FeatureAdmin,
	}

	for _, f := range features {
		assert.NotEqual(t, auth.FeatureBasic, f)
		assert.NotEqual(t, "all", f)
		assert.False(t, strings.HasPrefix(f, TeamFeaturePrefix), f)

		for _, name := range []string{f, TeamFeaturePrefix + f} {
			assert.True(t, auth.FeatureAllowed("+upgradoor:"+name, "upgradoor", name,
				auth.FeatureDefaultDeny), name)
			assert.False(t, auth.FeatureAllowed("+upgradoor", "upgradoor", name,
				auth.FeatureDefaultDeny), name)
		}
	}

	// A prefix grant covers every feature that starts with it. No name starts
	// with another, so a grant that spells a feature and adds "*" gives that
	// feature only; and no twin is reached by a prefix of a plain feature.
	all := slices.Clone(features)
	for _, f := range features {
		all = append(all, TeamFeaturePrefix+f)
	}

	for _, f := range features {
		granted := make([]string, 0, 1)

		for _, other := range all {
			if auth.FeatureAllowed("+upgradoor:"+f+"*", "upgradoor", other, auth.FeatureDefaultDeny) {
				granted = append(granted, other)
			}
		}

		assert.Equal(t, []string{f}, granted, "upgradoor:%s*", f)
	}

	// What a short prefix does reach, so that nobody writes it by accident.
	assert.True(t, auth.FeatureAllowed("+upgradoor:r*", "upgradoor", FeatureRecover, auth.FeatureDefaultDeny))
	assert.True(t, auth.FeatureAllowed("+upgradoor:r*", "upgradoor", FeatureReboot, auth.FeatureDefaultDeny))
	assert.False(t, auth.FeatureAllowed("+upgradoor:team_*", "upgradoor", FeatureRoll, auth.FeatureDefaultDeny))

	// The team names of a token and the owners of a container are the same
	// kind of name.
	for _, name := range []string{"geth", "a.b_c-d", "0x", "Geth", "", "-a", "a b", "a:b", "a*"} {
		assert.Equal(t, auth.ValidTeam(name), protocol.ValidOwner(name), "%q", name)
	}
}

// A roll of the whole host is a team's when the team owns every container
// the roll touches: one the host rolls to nothing (a default-group container
// on a host that does not roll its default group) does not count.
func TestUncoveredOnSkipsContainersNotRolled(t *testing.T) {
	host := &fleet.Host{Name: "h", Status: &protocol.SyncRequest{Containers: []protocol.Container{
		{Name: "geth", Managed: true, Owners: []string{"geth"}},
		{Name: "cadvisor", Managed: true, Group: protocol.DefaultGroup, NotRolled: "not rolled"},
	}}}
	own := OwnershipOf([]*fleet.Host{host})["h"]
	whole := ContainerTargets([]string{"h"}, false, nil)

	assert.Empty(t, UncoveredOn([]string{"geth"}, "h", own, whole))

	host.Status.Containers[1].NotRolled = ""
	own = OwnershipOf([]*fleet.Host{host})["h"]
	assert.Contains(t, UncoveredOn([]string{"geth"}, "h", own, whole), "cadvisor")
}
