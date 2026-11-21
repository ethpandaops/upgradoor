package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// clientContainerOn is a managed container of a client on an image, running
// digest while the registry points at want.
func clientContainerOn(
	name string, kind protocol.ContainerKind, client, image, digest, want string,
) protocol.Container {
	return protocol.Container{
		Name: name, Kind: kind, Client: client, Managed: true, State: protocol.StateRunning,
		Image: image, ConfiguredImage: image, Digest: digest, Owners: []string{client},
		Registry: &protocol.RegistryCheck{Digest: want, Revision: "rev-" + want, CheckedAt: testEpoch},
	}
}

// clientFixture is a fleet of lighthouse/geth hosts in every state of the
// page, and one host without clients.
func clientFixture(h *harness) {
	const (
		geth       = "ethpandaops/geth:master"
		lighthouse = "sigp/lighthouse:latest"
	)

	host := func(name string, change func(host *fleet.Host)) {
		status := syncRequest()
		status.Containers = []protocol.Container{
			clientContainerOn("geth", protocol.KindEL, "geth", geth, "sha256:g1", "sha256:g1"),
			clientContainerOn("beacon", protocol.KindCL, "lighthouse", lighthouse, "sha256:l1", "sha256:l1"),
			clientContainerOn("validator", protocol.KindVC, "lighthouse", lighthouse, "sha256:l1", "sha256:l1"),
			{Name: "debug", State: protocol.StateExited},
		}

		status.Containers[1].Command = []string{"--network=hoodi", "--http"}

		out := &fleet.Host{Name: name, LastSeen: testEpoch, Status: status}
		if change != nil {
			change(out)
		}

		h.store.hosts[name] = out
	}

	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	host("n-current", nil)
	host("n-pending", func(host *fleet.Host) {
		host.Status.Containers[1].Registry.Digest = "sha256:l2"
		host.Status.Containers[2].Registry.Digest = "sha256:l2"
	})
	host("n-held", func(host *fleet.Host) {
		host.HoldWanted, host.HoldReason = true, "debugging"
		// A flag override: its command line is not the configured one.
		host.Status.Containers[1].Command = []string{"--network=hoodi", "--http", "--debug"}
		host.Status.Containers[1].Override = &protocol.ContainerOverride{AddFlags: []string{"--debug"}}
	})
	host("n-custom", func(host *fleet.Host) {
		// Ansible named another image; the agent rolls it under a lease.
		host.Status.Containers[0].Image = "me/geth:fix"
		host.Status.Needs = []protocol.Need{{
			ID: protocol.SpecNeedID("geth", "k1"), Kind: protocol.NeedSpec,
			Spec: &protocol.SpecNeed{Container: "geth", Key: "k1", Image: "ethpandaops/geth:master", ToDigest: "sha256:g2"},
		}}
	})
	host("n-broken", func(host *fleet.Host) {
		host.Broken = map[string]string{"image:" + geth: "lease-1"}
	})
	host("n-rolling", nil)
	host("n-plain", func(host *fleet.Host) {
		for i := range host.Status.Containers {
			host.Status.Containers[i].Client = ""
		}
	})

	h.sched.set(func(f *fakeScheduler) {
		f.plan = &scheduler.Plan{Hosts: map[string]scheduler.HostPlan{
			"n-rolling": {Slot: scheduler.SlotLeased, LeaseID: "lease-2"},
		}}
		f.settings.PolicyClients = fleet.ClientPolicies{"geth": fleet.PolicyManual}
	})
}

func TestClients(t *testing.T) {
	h := newHarness(t, nil)
	clientFixture(h)

	rec := h.get("/api/v1/clients")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	clients := decodeJSON[Clients](t, rec).Clients
	require.Len(t, clients, 2, "a host without clients adds none")

	lighthouse, geth := clients[0], clients[1]

	assert.Equal(t, "lighthouse", lighthouse.Name, "consensus clients come first")
	assert.Equal(t, LayerConsensus, lighthouse.Layer)
	assert.Equal(t, fleet.PolicyAuto, lighthouse.Policy)
	assert.False(t, lighthouse.PolicySet)
	assert.Equal(t, []string{"lighthouse"}, lighthouse.Owners)
	assert.Equal(t, []string{"lighthouse"}, lighthouse.AllOwned)
	assert.Equal(t, []ClientImage{
		{
			Kind: protocol.KindCL, Image: "sigp/lighthouse:latest", Digest: "sha256:l1",
			Revision: "rev-sha256:l1", Hosts: 6,
			Command: []string{"--network=hoodi", "--http"}, CommandHosts: 5,
		},
		{
			Kind: protocol.KindVC, Image: "sigp/lighthouse:latest", Digest: "sha256:l1",
			Revision: "rev-sha256:l1", Hosts: 6, CommandHosts: 6,
		},
	}, lighthouse.Images, "of two digests checked at the same time the smaller is the target")
	assert.Equal(t, 6, lighthouse.Hosts)
	assert.Equal(t, 1, lighthouse.Pending)
	assert.Equal(t, 1, lighthouse.Held)
	assert.Equal(t, 1, lighthouse.Rolling)
	assert.Equal(t, 3, lighthouse.Current, "broken and the spec need are about geth")
	assert.Equal(t, 1, lighthouse.FlagOverrides, "whatever the state of the host")
	assert.Equal(t, 0, lighthouse.ImageOverrides)

	// The hosts with a flag override, as names: what a selection of the filter acts on.
	rec = h.get("/api/v1/clients/lighthouse/hosts?override=flags&names=true")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	flagged := decodeJSON[ClientHosts](t, rec)
	assert.Equal(t, []string{"n-held"}, flagged.Names)
	assert.Empty(t, flagged.Hosts)
	assert.Equal(t, 1, flagged.Matched)

	assert.Equal(t, http.StatusBadRequest, h.get("/api/v1/clients/lighthouse/hosts?override=both").Code)

	assert.Equal(t, LayerExecution, geth.Layer)
	assert.Equal(t, fleet.PolicyManual, geth.Policy)
	assert.True(t, geth.PolicySet)
	assert.Equal(t, 1, geth.Failed)
	assert.Equal(t, 1, geth.Pending, "a container on another image than agent.yaml says is pending")
	assert.Equal(t, 1, geth.Held)
	assert.Equal(t, 1, geth.Rolling)
	assert.Equal(t, 2, geth.Current)
	assert.Equal(t, geth.Hosts,
		geth.Current+geth.Pending+geth.Rolling+geth.Overridden+geth.Held+geth.Failed)

	// The version follows the list, and nothing else.
	again := decodeJSON[Clients](t, h.get("/api/v1/clients")).Clients
	assert.Equal(t, geth.Version, again[1].Version)

	h.store.mu.Lock()
	h.store.hosts["n-current"].Status.Containers[0].Registry.CheckedAt = testEpoch.Add(time.Minute)
	h.store.mu.Unlock()

	again = decodeJSON[Clients](t, h.get("/api/v1/clients")).Clients
	assert.Equal(t, geth.Version, again[1].Version, "a registry check that found nothing new")

	h.store.mu.Lock()
	h.store.hosts["n-current"].Status.Containers[0].Digest = "sha256:g0"
	h.store.mu.Unlock()

	again = decodeJSON[Clients](t, h.get("/api/v1/clients")).Clients
	assert.NotEqual(t, geth.Version, again[1].Version)
	assert.Equal(t, lighthouse.Version, again[0].Version)
}

func TestClientHosts(t *testing.T) {
	h := newHarness(t, nil)
	clientFixture(h)

	page := func(t *testing.T, query string) ClientHosts {
		t.Helper()

		rec := h.get("/api/v1/clients/geth/hosts" + query)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		return decodeJSON[ClientHosts](t, rec)
	}

	names := func(p ClientHosts) []string {
		out := make([]string, 0, len(p.Hosts))
		for _, row := range p.Hosts {
			out = append(out, row.Host)
		}

		return out
	}

	all := page(t, "")
	assert.Equal(t, 6, all.Total)
	assert.Equal(t, 6, all.Matched)
	assert.Equal(t, []string{"n-broken", "n-current", "n-custom", "n-held", "n-pending", "n-rolling"},
		names(all))
	assert.Equal(t, decodeJSON[Clients](t, h.get("/api/v1/clients")).Clients[1].Version, all.Version)

	broken := all.Hosts[0]
	assert.Equal(t, ClientFailed, broken.State)
	assert.Equal(t, "the last lease for ethpandaops/geth:master failed", broken.Reason)
	require.Len(t, broken.Containers, 1, "the containers of this client only")
	assert.Equal(t, "geth", broken.Containers[0].Name)

	assert.Equal(t, ClientHeld, all.Hosts[3].State)
	assert.Equal(t, "debugging", all.Hosts[3].Reason)
	assert.Equal(t, ClientCurrent, all.Hosts[4].State, "pending for lighthouse is not pending for geth")

	second := page(t, "?limit=2&offset=2")
	assert.Equal(t, []string{"n-custom", "n-held"}, names(second))
	assert.Equal(t, 6, second.Matched)

	assert.Equal(t, []string{"n-custom", "n-rolling"}, names(page(t, "?state=pending,rolling")))
	assert.Equal(t, []string{"n-current", "n-custom"}, names(page(t, "?q=N-CU")))

	for query, status := range map[string]int{
		"?state=nope": http.StatusBadRequest,
		"?offset=-1":  http.StatusBadRequest,
		"?limit=0":    http.StatusBadRequest,
	} {
		assert.Equal(t, status, h.get("/api/v1/clients/geth/hosts"+query).Code, query)
	}

	assert.Equal(t, http.StatusNotFound, h.get("/api/v1/clients/besu/hosts").Code)
}

// A team feature covers a client only when a team of the login owns every
// container any host reports for it.
func TestUncoveredClient(t *testing.T) {
	h := newHarness(t, nil)
	clientFixture(h)

	hosts, err := h.store.ListHosts(t.Context())
	require.NoError(t, err)

	assert.Empty(t, uncoveredClient([]string{"geth"}, "geth", hosts))
	assert.Equal(t, "does not cover client besu (owners: none)",
		uncoveredClient([]string{"geth"}, "besu", hosts))
	assert.Contains(t, uncoveredClient([]string{"lighthouse"}, "geth", hosts),
		"does not cover client geth: container geth on host ")

	h.store.mu.Lock()
	h.store.hosts["n-held"].Status.Containers[0].Owners = []string{"somebody"}
	h.store.mu.Unlock()

	assert.Equal(t, fmt.Sprintf(
		"does not cover client geth: container geth on host %s (owners: somebody)", "n-held"),
		uncoveredClient([]string{"geth"}, "geth", hosts))
}
