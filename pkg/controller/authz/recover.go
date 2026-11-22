package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// RecoverLeaseTarget is the target of end-lease.
func RecoverLeaseTarget(lease string) *Targets {
	return &Targets{lease: lease, recover: true}
}

// RecoverMembersTarget is the target of skip and retry with hosts: the
// hosts as members of the rollout.
func RecoverMembersTarget(rollout string, hosts []string) *Targets {
	return &Targets{rollout: rollout, hosts: hosts, recover: true}
}

// uncoveredRecover applies the rule of a recovery through team_manage: the
// lease, or each named member of the rollout, rolls only containers the
// teams own. A member's lease is judged as well, so work that rides along on
// it (a reboot, packages, a host action, another team's roll) is not
// covered.
func (s *authorizer) uncoveredRecover(ctx context.Context, teams []string, t *Targets) (string, error) {
	if t.lease == "" && (t.rollout == "" || len(t.hosts) == 0) {
		return "needs a target that one of your teams owns, and the request names none", nil
	}

	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return "", fmt.Errorf("authorize: list hosts: %w", err)
	}

	known := OwnershipOf(hosts)

	if t.lease != "" {
		return s.uncoveredLeaseID(ctx, teams, t.lease, known)
	}

	r, err := s.store.GetRollout(ctx, t.rollout)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Sprintf("does not cover rollout %s (owners: none)", Echo(t.rollout)), nil
	case err != nil:
		return "", fmt.Errorf("authorize: get rollout %s: %w", Echo(t.rollout), err)
	}

	var directives []*fleet.Directive

	switch r.Kind {
	case fleet.RolloutGroup:
		if !ownedBy(teams, []string{r.Team}) {
			return fmt.Sprintf("does not cover rollout %s (owners: %s)", Echo(r.ID), ownerList(teamList(r.Team))), nil
		}
	case fleet.RolloutManual:
		directives, err = s.store.ListDirectives(ctx, store.DirectiveFilter{Rollouts: []string{r.ID}})
		if err != nil {
			return "", fmt.Errorf("authorize: list directives of rollout %s: %w", Echo(r.ID), err)
		}
	case fleet.RolloutLocal:
	default:
		return fmt.Sprintf("does not cover rollout %s: it rolls no containers (%s)", Echo(r.ID), r.Kind), nil
	}

	byName := make(map[string]*fleet.Host, len(hosts))
	for _, h := range hosts {
		byName[h.Name] = h
	}

	for _, name := range t.hosts {
		own := known[name]
		if own == nil {
			own = &hostOwnership{}
		}

		if reason := uncoveredMember(teams, r, byName[name], name, own, directives); reason != "" {
			return reason, nil
		}

		if m := r.Host(name); m != nil && m.LeaseID != "" {
			reason, err := s.uncoveredLeaseID(ctx, teams, m.LeaseID, known)
			if err != nil || reason != "" {
				return reason, err
			}
		}
	}

	return "", nil
}

// uncoveredMember says what the rollout rolls on one host that the teams do
// not own: for a manual rollout its directives there, for a local one the
// host's own needs. A group rollout is the team's as a whole.
func uncoveredMember(
	teams []string, r *fleet.Rollout, h *fleet.Host, name string, own *hostOwnership, directives []*fleet.Directive,
) string {
	nothing := fmt.Sprintf("does not cover rollout %s on host %s: it rolls no container there", Echo(r.ID), Echo(name))

	switch r.Kind {
	case fleet.RolloutManual:
		found := false

		for _, d := range directives {
			if d.Host != name {
				continue
			}

			if d.Kind != protocol.DirectiveRoll {
				return fmt.Sprintf("does not cover rollout %s: it is a %s, not a roll", Echo(r.ID), d.Kind)
			}

			found = true

			if reason := uncoveredOwned(teams, d.Container, name, own); reason != "" {
				return reason
			}
		}

		if !found {
			return nothing
		}
	case fleet.RolloutLocal:
		if h == nil || h.Status == nil {
			return nothing
		}

		found := false

		for i := range h.Status.Needs {
			n := &h.Status.Needs[i]
			if n.DirectiveID != "" || (!n.Local && n.Kind != protocol.NeedOverride) {
				continue
			}

			found = true

			if n.Kind.Verb() != protocol.VerbRoll {
				return fmt.Sprintf("does not cover rollout %s: host %s asks for a %s",
					Echo(r.ID), Echo(name), Echo(string(n.Kind)))
			}

			if reason := uncoveredOwned(teams, n.Container(), name, own); reason != "" {
				return reason
			}
		}

		if !found {
			return nothing
		}
	default:
	}

	return ""
}

// uncoveredLeaseID applies the rule to the lease with that ID.
func (s *authorizer) uncoveredLeaseID(
	ctx context.Context, teams []string, id string, known map[string]*hostOwnership,
) (string, error) {
	lease, err := s.store.GetLease(ctx, id)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Sprintf("does not cover lease %s (owners: none)", Echo(id)), nil
	case err != nil:
		return "", fmt.Errorf("authorize: get lease %s: %w", Echo(id), err)
	}

	if len(lease.Needs) == 0 {
		return fmt.Sprintf("does not cover lease %s: it carries no need", Echo(id)), nil
	}

	own := known[lease.Host]
	if own == nil {
		own = &hostOwnership{}
	}

	for i := range lease.Needs {
		n := &lease.Needs[i]
		if n.Kind.Verb() != protocol.VerbRoll {
			return fmt.Sprintf("does not cover lease %s: it carries a %s", Echo(id), Echo(string(n.Kind))), nil
		}

		if reason := uncoveredOwned(teams, n.Container(), lease.Host, own); reason != "" {
			return reason, nil
		}
	}

	return "", nil
}

// uncoveredOwned applies the rule of a roll to one container of a host.
func uncoveredOwned(teams []string, container, host string, own *hostOwnership) string {
	for _, c := range own.containers {
		if c.name == container {
			if ownedBy(teams, c.owners) {
				return ""
			}

			return uncoveredContainer(container, host, c.owners)
		}
	}

	return uncoveredContainer(container, host, nil)
}

// teamList is the owners of something of one team; none for no team.
func teamList(team string) []string {
	if team == "" {
		return nil
	}

	return []string{team}
}
