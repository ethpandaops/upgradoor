package authz

import (
	"slices"
	"strings"
	"testing"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
)

// TestFeatureNames: the names are what a grant spells, a twin is the name
// with the prefix, none collides with what the grammar reserves, and a host
// action may name exactly the plain ones.
func TestFeatureNames(t *testing.T) {
	features := []string{FeatureDetails, FeatureManage, FeatureManageImage, FeatureManageFlags, FeatureAdmin}

	for _, f := range features {
		assert.NotEqual(t, auth.FeatureBasic, f)
		assert.NotEqual(t, "all", f)
		assert.False(t, strings.HasPrefix(f, TeamFeaturePrefix), f)
		assert.True(t, protocol.ValidActionFeature(f), f)
		assert.False(t, protocol.ValidActionFeature(TeamFeaturePrefix+f), f)

		for _, name := range []string{f, TeamFeaturePrefix + f} {
			assert.True(t, auth.FeatureAllowed("+upgradoor:"+name, "upgradoor", name,
				auth.FeatureDefaultDeny), name)
			assert.False(t, auth.FeatureAllowed("+upgradoor", "upgradoor", name,
				auth.FeatureDefaultDeny), name)
		}
	}

	assert.True(t, protocol.ValidActionFeature(protocol.DefaultActionFeature))
	assert.Equal(t, FeatureManage, protocol.DefaultActionFeature)

	// audit gates reading, never a host action.
	assert.False(t, protocol.ValidActionFeature(FeatureAudit))
	assert.True(t, auth.FeatureAllowed("+upgradoor:audit", "upgradoor", FeatureAudit, auth.FeatureDefaultDeny))
	assert.False(t, auth.FeatureAllowed("+upgradoor:admin", "upgradoor", FeatureAudit, auth.FeatureDefaultDeny))

	// A prefix grant covers every feature that starts with it: "manage*" is
	// the three manage features, any other spelled name with "*" that feature
	// only, and no twin is reached by a prefix of a plain feature.
	all := slices.Clone(features)
	for _, f := range features {
		all = append(all, TeamFeaturePrefix+f)
	}

	for _, f := range features {
		granted := make([]string, 0, 3)

		for _, other := range all {
			if auth.FeatureAllowed("+upgradoor:"+f+"*", "upgradoor", other, auth.FeatureDefaultDeny) {
				granted = append(granted, other)
			}
		}

		want := []string{f}
		if f == FeatureManage {
			want = []string{FeatureManage, FeatureManageImage, FeatureManageFlags}
		}

		assert.Equal(t, want, granted, "upgradoor:%s*", f)
	}

	assert.False(t, auth.FeatureAllowed("+upgradoor:team_*", "upgradoor", FeatureManage, auth.FeatureDefaultDeny))
	assert.True(t, auth.FeatureAllowed("+upgradoor:team_*", "upgradoor", TeamFeaturePrefix+FeatureManageFlags,
		auth.FeatureDefaultDeny))

	// The team names of a token and the owners of a container are the same
	// kind of name.
	for _, name := range []string{"geth", "a.b_c-d", "0x", "Geth", "", "-a", "a b", "a:b", "a*"} {
		assert.Equal(t, auth.ValidTeam(name), protocol.ValidOwner(name), "%q", name)
	}
}

// A roll of the whole host is a team's when the team owns every container
// the roll touches: one the host rolls to nothing (pinned to a digest) does
// not count, a container without a team does.
func TestUncoveredOnSkipsContainersNotRolled(t *testing.T) {
	host := &fleet.Host{Name: "h", Status: &protocol.SyncRequest{Containers: []protocol.Container{
		{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth"},
		{Name: "cadvisor", Managed: true, NotRolled: "pinned to a digest"},
	}}}
	own := OwnershipOf([]*fleet.Host{host})["h"]
	whole := ContainerTargets([]string{"h"}, false, nil)

	assert.Empty(t, UncoveredOn([]string{"geth"}, "h", own, whole))

	host.Status.Containers[1].NotRolled = ""
	own = OwnershipOf([]*fleet.Host{host})["h"]
	assert.Contains(t, UncoveredOn([]string{"geth"}, "h", own, whole), "cadvisor")
}

// An unhold through team_manage names only holds of the token's teams that own
// a container of the host.
func TestUncoveredOnHolders(t *testing.T) {
	host := &fleet.Host{Name: "h", Status: &protocol.SyncRequest{Containers: []protocol.Container{
		{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth"},
		{Name: "lighthouse", Managed: true, Kind: protocol.KindCL, Team: "lighthouse"},
	}}}
	own := OwnershipOf([]*fleet.Host{host})["h"]
	teams := []string{"lighthouse", "teku"}

	assert.Empty(t, UncoveredOn(teams, "h", own, HoldTargets([]string{"h"}, nil)))
	assert.Empty(t, UncoveredOn(teams, "h", own, HoldTargets([]string{"h"}, []string{"lighthouse"})))

	for _, holder := range []string{fleet.HolderStaff, "geth", "teku"} {
		assert.Equal(t, "does not lift the hold of "+holder+" on host h",
			UncoveredOn(teams, "h", own, HoldTargets([]string{"h"}, []string{holder})), holder)
	}
}

// TestImplied: admin includes every other feature, a plain feature its
// twin; nothing else.
func TestImplied(t *testing.T) {
	grants := func(features ...string) func(string) bool {
		return func(f string) bool { return slices.Contains(features, f) }
	}

	assert.True(t, Implied(FeatureAudit, grants(FeatureAdmin)))
	assert.True(t, Implied(FeatureAudit, grants(FeatureAudit)))
	assert.False(t, Implied(FeatureAudit,
		grants(FeatureDetails, FeatureManage, FeatureManageImage, FeatureManageFlags)))
	assert.False(t, Implied(FeatureAdmin, grants(FeatureAudit)))

	for _, f := range []string{FeatureDetails, FeatureManage, FeatureManageImage, FeatureManageFlags} {
		assert.True(t, Implied(f, grants(FeatureAdmin)), f)
		assert.True(t, Implied(TeamFeaturePrefix+f, grants(FeatureAdmin)), f)
		assert.True(t, Implied(TeamFeaturePrefix+f, grants(f)), f)
		assert.False(t, Implied(f, grants(TeamFeaturePrefix+f)), f)
	}

	assert.False(t, Implied(FeatureAdmin, grants(FeatureDetails, FeatureManage, FeatureManageImage, FeatureManageFlags)))

	for _, f := range []string{FeatureDetails, FeatureManageImage, FeatureManageFlags} {
		assert.False(t, Implied(f, grants(FeatureManage)), f)
		assert.False(t, Implied(TeamFeaturePrefix+f, grants(FeatureManage, TeamFeaturePrefix+FeatureManage)), f)
	}

	assert.False(t, Implied(FeatureManage, grants(FeatureDetails, FeatureManageImage, FeatureManageFlags)))
	assert.False(t, Implied("team_admin", grants(TeamFeaturePrefix+FeatureManage)))
}

// TestActionBasis: admin, the action's feature (manage when it names none,
// nobody's when it is unknown) or one of its teams.
func TestActionBasis(t *testing.T) {
	s := New(logrus.New(), "upgradoor", nil, nil)
	user := func(services string, teams ...string) *User { return &User{Services: services, Teams: teams} }

	for _, tt := range []struct {
		user    *User
		feature string
		teams   []string
		want    string
	}{
		{&User{Open: true}, "admin", nil, ActionByOpen},
		{user("+upgradoor:admin"), "", nil, FeatureAdmin},
		{user("+upgradoor:admin"), "bogus", nil, FeatureAdmin},
		{user("+upgradoor:manage"), "", nil, ActionByFeature},
		{user("+upgradoor:manage"), FeatureManage, nil, ActionByFeature},
		{user("+upgradoor:manage"), FeatureDetails, nil, ""},
		{user("+upgradoor:manage"), "bogus", nil, ""},
		{user("+upgradoor:team_manage"), "", nil, ""},
		{user("+upgradoor:details"), FeatureDetails, nil, ActionByFeature},
		{user("+upgradoor:manage_flags"), FeatureManageFlags, nil, ActionByFeature},
		{user("+upgradoor:manage"), FeatureAdmin, nil, ""},
		{user("+upgradoor", "devops"), "bogus", []string{"devops"}, ActionByTeam},
		{user("+upgradoor:team_*", "geth"), "", []string{"devops"}, ""},
	} {
		got := s.ActionBasis(tt.user, &protocol.ActionInfo{Name: "a", Feature: tt.feature, Teams: tt.teams})
		assert.Equal(t, tt.want, got, "%+v %q", tt.user, tt.feature)
	}
}
