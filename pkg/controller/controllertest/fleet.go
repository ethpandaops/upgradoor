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
	// HostMixed runs geth (owner geth), lighthouse and lighthouse-validator
	// (owner lighthouse) and an unmanaged container "debug".
	HostMixed = "lighthouse-geth-1"
	// HostTeku runs geth (owner geth) and teku (owners teku and nimbus).
	HostTeku = "teku-geth-1"
	// HostUnowned runs one managed container, "bootnode", nobody owns.
	HostUnowned = "bootnode-1"
	// HostFresh has registered and never synced.
	HostFresh = "fresh-1"
	// HostGhost is not known to the controller.
	HostGhost = "ghost-1"

	// LeaseMixed is open on HostMixed, LeaseUnowned on HostUnowned, LeaseDone
	// has ended on HostMixed, LeaseGhost does not exist.
	LeaseMixed   = "lease-mixed"
	LeaseUnowned = "lease-unowned"
	LeaseDone    = "lease-done"
	LeaseGhost   = "lease-ghost"
)

// Owned is a running managed container with owners.
func Owned(name string, owners ...string) protocol.Container {
	return protocol.Container{Name: name, State: "running", Managed: true, Owners: owners}
}

// OfClient is a managed container of a client.
func OfClient(c protocol.Container, kind protocol.ContainerKind, client string) protocol.Container {
	c.Kind, c.Client = kind, client

	return c
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
		Owned("geth", "geth"),
		OfClient(Owned("lighthouse", "lighthouse"), protocol.KindCL, "lighthouse"),
		OfClient(Owned("lighthouse-validator", "lighthouse"), protocol.KindVC, "lighthouse"),
		protocol.Container{Name: "debug", State: "exited"},
	)}
	st.Hosts[HostTeku] = &fleet.Host{Name: HostTeku, LastSeen: Epoch, Status: status(
		OfClient(Owned("geth", "geth"), protocol.KindEL, "geth"),
		OfClient(Owned("teku", "nimbus", "teku"), protocol.KindCL, "teku"),
	)}
	st.Hosts[HostUnowned] = &fleet.Host{Name: HostUnowned, LastSeen: Epoch, Status: status(
		Owned("bootnode"),
	)}
	st.Hosts[HostFresh] = &fleet.Host{Name: HostFresh, LastSeen: Epoch}

	// r-1 is halted, r-aborted is aborted; r-ghost does not exist.
	st.Rollouts = []*fleet.Rollout{
		{ID: "r-1", State: fleet.RolloutHalted},
		{ID: "r-aborted", State: fleet.RolloutAborted},
	}

	st.Leases = []*fleet.Lease{
		{ID: LeaseMixed, Host: HostMixed, State: fleet.LeaseRunning},
		{ID: LeaseUnowned, Host: HostUnowned, State: fleet.LeaseGranted},
		{ID: LeaseDone, Host: HostMixed, State: fleet.LeaseReleased},
	}
}
