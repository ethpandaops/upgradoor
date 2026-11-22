package controllertest

import (
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// RolloutRow is a rollout as the scheduler stores one, with members in the
// given states.
func RolloutRow(id string, state fleet.RolloutState, members map[string]fleet.HostState) *fleet.Rollout {
	r := &fleet.Rollout{
		ID: id, Kind: fleet.RolloutPackages, Key: "packages/" + id, Title: "packages " + id,
		State: state, Approved: true, CreatedBy: "scheduler", CreatedAt: Epoch, UpdatedAt: Epoch,
	}

	if state == fleet.RolloutHalted {
		r.Reason = "a host failed"
	}

	if state.Terminal() {
		ended := Epoch
		r.EndedAt = &ended
	}

	for _, host := range []string{HostMixed, HostTeku, HostUnowned} {
		if st, ok := members[host]; ok {
			r.Hosts = append(r.Hosts, fleet.RolloutHost{
				Host: host, State: st, Stage: fleet.StageRest, UpdatedAt: Epoch,
			})
		}
	}

	return r
}

// The fleet of the authorization tests.
const (
	// HostMixed runs geth (team geth), lighthouse and lighthouse-validator
	// (team lighthouse) and an unmanaged container "debug".
	HostMixed = "lighthouse-geth-1"
	// HostTeku runs geth (team geth) and teku (team teku).
	HostTeku = "teku-geth-1"
	// HostUnowned runs one managed container, "bootnode", without a team.
	HostUnowned = "bootnode-1"
	// HostFresh has registered and never synced.
	HostFresh = "fresh-1"
	// HostGhost is not known to the controller.
	HostGhost = "ghost-1"

	// LeaseMixed is open on HostMixed and rolls lighthouse, LeaseUnowned on
	// HostUnowned rolls bootnode, LeaseDone has ended on HostMixed after a
	// roll of geth, LeaseReboot on HostMixed rolls geth and reboots;
	// LeaseGhost does not exist.
	LeaseMixed   = "lease-mixed"
	LeaseUnowned = "lease-unowned"
	LeaseDone    = "lease-done"
	LeaseReboot  = "lease-reboot"
	LeaseGhost   = "lease-ghost"
)

// Owned is a running labelled container of a team; none without one.
func Owned(name string, kind protocol.ContainerKind, team string) protocol.Container {
	return protocol.Container{Name: name, State: "running", Managed: true, Kind: kind, Team: team}
}

// OwnershipFixture fills the fake store with the fleet above.
func OwnershipFixture(st *Store) {
	status := func(containers ...protocol.Container) *protocol.SyncRequest {
		req := SyncRequest()
		req.Containers = containers

		return req
	}

	st.Lock()
	defer st.Unlock()

	st.Hosts[HostMixed] = &fleet.Host{Name: HostMixed, LastSeen: Epoch, Status: status(
		Owned("geth", protocol.KindEL, "geth"),
		Owned("lighthouse", protocol.KindCL, "lighthouse"),
		Owned("lighthouse-validator", protocol.KindVC, "lighthouse"),
		protocol.Container{Name: "debug", State: "exited"},
	)}
	st.Hosts[HostTeku] = &fleet.Host{Name: HostTeku, LastSeen: Epoch, Status: status(
		Owned("geth", protocol.KindEL, "geth"),
		Owned("teku", protocol.KindCL, "teku"),
	)}
	st.Hosts[HostUnowned] = &fleet.Host{Name: HostUnowned, LastSeen: Epoch, Status: status(
		Owned("bootnode", "bootnode", ""),
	)}
	st.Hosts[HostFresh] = &fleet.Host{Name: HostFresh, LastSeen: Epoch}

	// r-1 is a halted manual roll of lighthouse on HostMixed and teku on
	// HostTeku,
	// r-aborted an aborted rollout of lighthouse, r-reboot a halted reboot,
	// r-lighthouse rolls the client lighthouse; r-ghost does not exist.
	st.Rollouts = []*fleet.Rollout{
		{ID: "r-1", Kind: fleet.RolloutManual, State: fleet.RolloutHalted},
		{ID: "r-aborted", Kind: fleet.RolloutGroup, ContainerKind: protocol.KindCL, Team: "lighthouse", State: fleet.RolloutAborted},
		{ID: "r-lighthouse", Kind: fleet.RolloutGroup, ContainerKind: protocol.KindCL, Team: "lighthouse", State: fleet.RolloutActive},
		{ID: "r-reboot", Kind: fleet.RolloutReboot, State: fleet.RolloutHalted},
	}

	st.Directives = []*fleet.Directive{
		{Directive: protocol.Directive{ID: "d-1", Kind: protocol.DirectiveRoll, Container: "lighthouse"}, Host: HostMixed, RolloutID: "r-1"},
		{Directive: protocol.Directive{ID: "d-2", Kind: protocol.DirectiveRoll, Container: "teku"}, Host: HostTeku, RolloutID: "r-1"},
	}

	roll := func(container string) protocol.Need {
		return protocol.Need{
			ID: "image/" + container, Kind: protocol.NeedImage, Image: &protocol.ImageNeed{Container: container},
		}
	}
	reboot := protocol.Need{ID: protocol.RebootNeedID, Kind: protocol.NeedReboot, Reboot: &protocol.RebootNeed{}}

	st.Leases = []*fleet.Lease{
		{ID: LeaseMixed, Host: HostMixed, State: fleet.LeaseRunning, Needs: []protocol.Need{roll("lighthouse")}},
		{ID: LeaseUnowned, Host: HostUnowned, State: fleet.LeaseGranted, Needs: []protocol.Need{roll("bootnode")}},
		{ID: LeaseDone, Host: HostMixed, State: fleet.LeaseReleased, Needs: []protocol.Need{roll("geth")}},
		{ID: LeaseReboot, Host: HostMixed, State: fleet.LeaseRunning, Needs: []protocol.Need{roll("geth"), reboot}},
	}
}
