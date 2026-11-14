package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNodeHealthSynced(t *testing.T) {
	el := func(mod func(*ELHealth)) *ELHealth {
		h := &ELHealth{Reachable: true, Peers: 5}
		if mod != nil {
			mod(h)
		}

		return h
	}
	cl := func(mod func(*CLHealth)) *CLHealth {
		h := &CLHealth{Reachable: true, Peers: 5}
		if mod != nil {
			mod(h)
		}

		return h
	}

	for _, tt := range []struct {
		name     string
		node     NodeHealth
		minPeers int
		missing  string
	}{
		{name: "no probe configured", minPeers: 3},
		{name: "both at head with peers", node: NodeHealth{EL: el(nil), CL: cl(nil)}, minPeers: 5},
		{name: "only the CL is probed", node: NodeHealth{CL: cl(nil)}, minPeers: 3},
		{
			name: "EL unreachable", minPeers: 1, missing: "EL is not reachable",
			node: NodeHealth{EL: &ELHealth{}, CL: cl(nil)},
		},
		{
			name: "CL unreachable", minPeers: 1, missing: "CL is not reachable",
			node: NodeHealth{EL: el(nil), CL: &CLHealth{}},
		},
		{
			name: "EL syncing", minPeers: 1, missing: "node is syncing",
			node: NodeHealth{EL: el(func(h *ELHealth) { h.Syncing = true }), CL: cl(nil)},
		},
		{
			name: "CL syncing", minPeers: 1, missing: "node is syncing",
			node: NodeHealth{EL: el(nil), CL: cl(func(h *CLHealth) { h.Syncing = true })},
		},
		{
			name: "CL without its EL", minPeers: 1, missing: "CL reports its EL offline",
			node: NodeHealth{EL: el(nil), CL: cl(func(h *CLHealth) { h.ELOffline = true })},
		},
		{
			name: "EL short of peers", minPeers: 3, missing: "waiting for peers: EL has 2 of 3",
			node: NodeHealth{EL: el(func(h *ELHealth) { h.Peers = 2 }), CL: cl(nil)},
		},
		{
			name: "CL short of peers", minPeers: 3, missing: "waiting for peers: CL has 1 of 3",
			node: NodeHealth{EL: el(nil), CL: cl(func(h *CLHealth) { h.Peers = 1 })},
		},
		{
			name: "no peers at all", minPeers: 1, missing: "waiting for peers: EL has 0 of 1",
			node: NodeHealth{EL: el(func(h *ELHealth) { h.Peers = 0 }), CL: cl(nil)},
		},
		{
			name: "syncing is named before peers", minPeers: 3, missing: "node is syncing",
			node: NodeHealth{EL: el(nil), CL: cl(func(h *CLHealth) { h.Syncing, h.Peers = true, 0 })},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.missing, tt.node.Missing(tt.minPeers))
			assert.Equal(t, tt.missing == "", tt.node.Synced(tt.minPeers))
		})
	}
}
