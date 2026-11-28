package views

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestArgVotesCommon(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		commands [][]string
		want     []string
	}{
		{
			name: "an argument on a strict majority is common",
			commands: [][]string{
				{"--http", "--nat=extip:1.1.1.1", "--port", "30303"},
				{"--http", "--nat=extip:2.2.2.2", "--port", "30303"},
				{"--http", "--nat=extip:3.3.3.3", "--port", "9000"},
			},
			want: []string{"--http", "--port", "30303"},
		},
		{
			name: "a value every host has its own of is not common",
			commands: [][]string{
				{"--nat=extip:1.1.1.1"},
				{"--nat=extip:2.2.2.2"},
			},
			want: nil,
		},
		{
			name: "half is no majority",
			commands: [][]string{
				{"--http", "--debug"},
				{"--http"},
			},
			want: []string{"--http"},
		},
		{
			name:     "a repeated argument counts once per host",
			commands: [][]string{{"--port", "--port"}, {"--http"}, {"--http"}},
			want:     []string{"--http"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := argVotes{count: make(map[string]int, 8)}
			for _, c := range tt.commands {
				v.add(c)
			}

			assert.Equal(t, tt.want, v.common())
		})
	}
}

// A client's row has its own policy, if any, and the default of each of its
// kinds.
func TestClientIndexPolicies(t *testing.T) {
	settings := fleet.DefaultSettings()
	settings.PolicyClients = fleet.ClientPolicies{"el": {"geth": fleet.PolicyManual}}
	settings.PolicyKinds = fleet.KindPolicies{"cl": fleet.PolicyManual}

	sum := HostSummary{Name: "a", Containers: []protocol.Container{
		{Name: "geth", Managed: true, Kind: protocol.KindEL, Team: "geth", Image: "geth:1"},
		{Name: "teku", Managed: true, Kind: protocol.KindCL, Team: "teku", Image: "teku:1"},
		{Name: "teku-vc", Managed: true, Kind: protocol.KindVC, Team: "teku", Image: "teku:1"},
	}}

	idx := NewClientIndex([]HostSummary{sum}, &settings)
	require.Len(t, idx.Views, 2)

	teku, geth := idx.Views[0], idx.Views[1]
	assert.Equal(t, "teku", teku.Name)
	assert.Empty(t, teku.Policies)
	assert.Equal(t, map[protocol.ContainerKind]fleet.Policy{
		protocol.KindCL: fleet.PolicyManual, protocol.KindVC: fleet.PolicyAuto,
	}, teku.DefaultPolicies)

	assert.Equal(t, map[protocol.ContainerKind]fleet.Policy{protocol.KindEL: fleet.PolicyManual}, geth.Policies)
	assert.Equal(t, map[protocol.ContainerKind]fleet.Policy{protocol.KindEL: fleet.PolicyAuto}, geth.DefaultPolicies)
}

// The kinds of a fleet are those of its managed containers, el, cl and vc
// first.
func TestFleetKinds(t *testing.T) {
	host := func(containers ...protocol.Container) *fleet.Host {
		return &fleet.Host{Status: &protocol.SyncRequest{Containers: containers}}
	}

	kinds := FleetKinds([]*fleet.Host{
		host(
			protocol.Container{Managed: true, Kind: "snooper-el"},
			protocol.Container{Managed: true, Kind: protocol.KindCL},
			protocol.Container{Managed: true},
		),
		host(
			protocol.Container{Managed: true, Kind: protocol.KindEL},
			protocol.Container{Managed: false, Kind: "ignored"},
			protocol.Container{Managed: true, Kind: "blobber"},
		),
		{},
	})

	assert.Equal(t, []protocol.ContainerKind{protocol.KindEL, protocol.KindCL, "blobber", "snooper-el"}, kinds)
}
