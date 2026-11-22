package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// assign adds ranges to the stored validator ranges inventory.
func assign(t *testing.T, s Store, ranges ...fleet.HostRange) {
	t.Helper()

	ctx := context.Background()

	stored, err := s.ValidatorRanges(ctx)
	require.NoError(t, err)

	stored.Ranges = append(stored.Ranges, ranges...)
	slices.SortFunc(stored.Ranges, func(a, b fleet.HostRange) int {
		return int(a.Start) - int(b.Start)
	})

	require.NoError(t, s.SetValidatorRanges(ctx, stored))
}

func TestValidatorRanges(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgradoor.db")

	s := New(quietLog(), path)
	require.NoError(t, s.Start(ctx))

	empty, err := s.ValidatorRanges(ctx)
	require.NoError(t, err)
	assert.Equal(t, &fleet.ValidatorRanges{}, empty)

	addHost(t, s, "node-a", nil)
	addHost(t, s, "node-b", nil)

	want := &fleet.ValidatorRanges{
		URL: "https://config.example/ranges", ETag: `"v1"`, LastModified: "Mon, 02 Jan 2006",
		FetchedAt: at(10),
		Ranges: []fleet.HostRange{
			{Host: "node-a", ValidatorRange: identity.ValidatorRange{Start: 0, End: 100}},
			{Host: "node-b", ValidatorRange: identity.ValidatorRange{Start: 100, End: 200}},
			{Host: "node-a", ValidatorRange: identity.ValidatorRange{Start: 300, End: 310}},
			{Host: "elsewhere", ValidatorRange: identity.ValidatorRange{Start: 400, End: 500}},
		},
	}
	require.NoError(t, s.SetValidatorRanges(ctx, want))

	a, err := s.GetHost(ctx, "node-a")
	require.NoError(t, err)
	assert.Equal(t, []identity.ValidatorRange{{Start: 0, End: 100}, {Start: 300, End: 310}}, a.Validators)
	assert.Equal(t, uint64(110), a.Weight())

	hosts, err := s.ListHosts(ctx)
	require.NoError(t, err)
	require.Len(t, hosts, 2)
	assert.Equal(t, a.Validators, hosts[0].Validators)
	assert.Equal(t, []identity.ValidatorRange{{Start: 100, End: 200}}, hosts[1].Validators)

	// A restart keeps the last version.
	require.NoError(t, s.Stop())

	s = New(quietLog(), path)
	require.NoError(t, s.Start(ctx))

	t.Cleanup(func() { require.NoError(t, s.Stop()) })

	got, err := s.ValidatorRanges(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// A new version replaces the old one in full.
	require.NoError(t, s.SetValidatorRanges(ctx, &fleet.ValidatorRanges{URL: want.URL, FetchedAt: at(20)}))

	b, err := s.GetHost(ctx, "node-b")
	require.NoError(t, err)
	assert.Nil(t, b.Validators)
}
