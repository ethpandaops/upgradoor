package authz

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// recoverStore is what the rule of a recovery reads.
type recoverStore struct {
	hosts      []*fleet.Host
	rollouts   []*fleet.Rollout
	leases     []*fleet.Lease
	directives []*fleet.Directive
}

func (s *recoverStore) GetRollout(_ context.Context, id string) (*fleet.Rollout, error) {
	if i := slices.IndexFunc(s.rollouts, func(r *fleet.Rollout) bool { return r.ID == id }); i >= 0 {
		return s.rollouts[i], nil
	}

	return nil, store.ErrNotFound
}

func (s *recoverStore) GetLease(_ context.Context, id string) (*fleet.Lease, error) {
	if i := slices.IndexFunc(s.leases, func(l *fleet.Lease) bool { return l.ID == id }); i >= 0 {
		return s.leases[i], nil
	}

	return nil, store.ErrNotFound
}

func (s *recoverStore) ListHosts(context.Context) ([]*fleet.Host, error) { return s.hosts, nil }

func (s *recoverStore) ListDirectives(_ context.Context, f store.DirectiveFilter) ([]*fleet.Directive, error) {
	out := make([]*fleet.Directive, 0, len(s.directives))

	for _, d := range s.directives {
		if slices.Contains(f.Rollouts, d.RolloutID) {
			out = append(out, d)
		}
	}

	return out, nil
}

func (s *recoverStore) AppendAudit(context.Context, ...fleet.AuditEntry) error { return nil }

func rollNeed(container string) protocol.Need {
	return protocol.Need{ID: "image/" + container, Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Container: container}}
}

// TestRecoverRule: team_manage recovers a lease or rollout member that only
// rolls containers of the token's teams, never a reboot, packages or a host
// action, also not riding along on a lease.
func TestRecoverRule(t *testing.T) {
	reboot := protocol.Need{ID: protocol.RebootNeedID, Kind: protocol.NeedReboot, Reboot: &protocol.RebootNeed{}}
	local := rollNeed("lighthouse")
	local.Local = true

	st := &recoverStore{
		hosts: []*fleet.Host{{Name: "h", Status: &protocol.SyncRequest{
			Containers: []protocol.Container{
				{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth"},
				{Name: "lighthouse", Managed: true, Kind: protocol.KindCL, Team: "lighthouse"},
			},
			Needs: []protocol.Need{local},
		}}},
		rollouts: []*fleet.Rollout{
			{ID: "group", Kind: fleet.RolloutGroup, Team: "lighthouse", Hosts: []fleet.RolloutHost{{Host: "h"}}},
			{ID: "group-ride", Kind: fleet.RolloutGroup, Team: "lighthouse", Hosts: []fleet.RolloutHost{{Host: "h", LeaseID: "l-ride"}}},
			{ID: "manual-roll", Kind: fleet.RolloutManual, Hosts: []fleet.RolloutHost{{Host: "h"}}},
			{ID: "manual-reboot", Kind: fleet.RolloutManual, Hosts: []fleet.RolloutHost{{Host: "h"}}},
			{ID: "reboot", Kind: fleet.RolloutReboot, Hosts: []fleet.RolloutHost{{Host: "h"}}},
			{ID: "local", Kind: fleet.RolloutLocal, Hosts: []fleet.RolloutHost{{Host: "h"}}},
		},
		leases: []*fleet.Lease{
			{ID: "l-roll", Host: "h", Needs: []protocol.Need{rollNeed("lighthouse")}},
			{ID: "l-ride", Host: "h", Needs: []protocol.Need{rollNeed("lighthouse"), reboot}},
			{ID: "l-other", Host: "h", Needs: []protocol.Need{rollNeed("lighthouse"), rollNeed("geth")}},
			{ID: "l-empty", Host: "h"},
		},
		directives: []*fleet.Directive{
			{Directive: protocol.Directive{Kind: protocol.DirectiveRoll, Container: "lighthouse"}, Host: "h", RolloutID: "manual-roll"},
			{Directive: protocol.Directive{Kind: protocol.DirectiveReboot}, Host: "h", RolloutID: "manual-reboot"},
		},
	}
	a := &authorizer{store: st}
	teams := []string{"lighthouse"}

	for _, tt := range []struct {
		name   string
		target *Targets
		want   string
	}{
		{"a lease of the team's roll", RecoverLeaseTarget("l-roll"), ""},
		{"a lease with a reboot along", RecoverLeaseTarget("l-ride"), "does not cover lease l-ride: it carries a reboot"},
		{"a lease with another team's roll", RecoverLeaseTarget("l-other"),
			"does not cover container geth on host h (owners: geth)"},
		{"a lease without needs", RecoverLeaseTarget("l-empty"), "does not cover lease l-empty: it carries no need"},
		{"an unknown lease", RecoverLeaseTarget("l-ghost"), "does not cover lease l-ghost (owners: none)"},
		{"a group rollout of the team", RecoverMembersTarget("group", []string{"h"}), ""},
		{"a group rollout whose lease carries a reboot", RecoverMembersTarget("group-ride", []string{"h"}),
			"does not cover lease l-ride: it carries a reboot"},
		{"a manual roll of the team's container", RecoverMembersTarget("manual-roll", []string{"h"}), ""},
		{"a manual roll on a host it does not roll", RecoverMembersTarget("manual-roll", []string{"x"}),
			"does not cover rollout manual-roll on host x: it rolls no container there"},
		{"a manual reboot", RecoverMembersTarget("manual-reboot", []string{"h"}),
			"does not cover rollout manual-reboot: it is a reboot, not a roll"},
		{"a reboot rollout", RecoverMembersTarget("reboot", []string{"h"}),
			"does not cover rollout reboot: it rolls no containers (reboot)"},
		{"the host's own roll", RecoverMembersTarget("local", []string{"h"}), ""},
		{"an unknown rollout", RecoverMembersTarget("ghost", []string{"h"}), "does not cover rollout ghost (owners: none)"},
		{"no hosts", RecoverMembersTarget("group", nil),
			"needs a target that one of your teams owns, and the request names none"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.uncovered(context.Background(), teams, tt.target)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	got, err := a.uncovered(context.Background(), []string{"geth"}, RecoverMembersTarget("group", []string{"h"}))
	require.NoError(t, err)
	assert.Equal(t, "does not cover rollout group (owners: lighthouse)", got)
}
