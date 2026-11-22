package views

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

var fullAccess = &Access{Full: true}

func imageHosts() []*fleet.Host {
	host := func(name string, containers ...protocol.Container) *fleet.Host {
		return &fleet.Host{Name: name, Status: &protocol.SyncRequest{Containers: containers}}
	}

	geth := func(digest string) protocol.Container {
		return protocol.Container{Name: "geth", Image: "ethpandaops/geth:master", Digest: digest, Revision: "r-" + digest}
	}

	return []*fleet.Host{
		host("c", geth("sha256:1")),
		host("a", geth("sha256:1"),
			// A second container on the same image, and one nobody manages.
			protocol.Container{Name: "geth-2", Image: "ethpandaops/geth:master", Digest: "sha256:1"},
			protocol.Container{Name: "node-exporter", Image: "prom/node-exporter:latest"}),
		host("b", geth("sha256:2"), protocol.Container{Name: "teku", Image: "consensys/teku:develop", Digest: "sha256:t"}),
		{Name: "never-synced"},
	}
}

func imageNames(p Images) []string {
	out := make([]string, 0, len(p.Matches))
	for _, m := range p.Matches {
		out = append(out, m.Name)
	}

	return out
}

func TestNewImages(t *testing.T) {
	page, err := NewImages(imageHosts(), "", "", 50, fullAccess)
	require.NoError(t, err)

	assert.Equal(t, []string{"consensys/teku:develop", "ethpandaops/geth:master", "prom/node-exporter:latest"},
		imageNames(page))
	require.NotNil(t, page.Total)
	assert.Equal(t, 3, *page.Total)
	assert.Empty(t, page.Next)

	assert.Equal(t, []ImageVersion{
		{Digest: "sha256:1", Revision: "r-sha256:1", Hosts: []string{"a", "c"}},
		{Digest: "sha256:2", Revision: "r-sha256:2", Hosts: []string{"b"}},
	}, page.Matches[1].Versions, "the digest most hosts run first, a host once")
	assert.Equal(t, []ImageVersion{{Hosts: []string{"a"}}}, page.Matches[2].Versions, "no registry digest")
	assert.Nil(t, page.Matches[1].Registry, "no host reports a lookup")
}

// The newest lookup a host reports is the image's.
func TestNewImagesReduced(t *testing.T) {
	hosts := imageHosts()
	hosts[0].Status.Containers[0].Managed = true

	page, err := NewImages(hosts, "", "", 50, &Access{})
	require.NoError(t, err)

	require.Equal(t, []string{"ethpandaops/geth:master"}, imageNames(page), "managed containers only")
	assert.Equal(t, []ImageVersion{{Digest: "sha256:1", Revision: "r-sha256:1", Hosts: []string{"c"}}},
		page.Matches[0].Versions)
}

func TestNewImagesRegistryLookup(t *testing.T) {
	hosts := imageHosts()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

	hosts[0].Status.Containers[0].Registry = &protocol.RegistryCheck{Digest: "sha256:1", CheckedAt: at}
	hosts[2].Status.Containers[0].Registry = &protocol.RegistryCheck{
		Digest: "sha256:2", CheckedAt: at.Add(time.Minute), Error: "registry: timeout",
	}

	page, err := NewImages(hosts, "geth", "", 50, fullAccess)
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, &ImageLookup{Digest: "sha256:2", CheckedAt: at.Add(time.Minute), Error: "registry: timeout"},
		page.Matches[0].Registry)
}

func TestNewImagesFiltersAndPages(t *testing.T) {
	hosts := imageHosts()

	page, err := NewImages(hosts, "E", "", 50, fullAccess)
	require.NoError(t, err)
	assert.Equal(t, []string{"ethpandaops/geth:master", "consensys/teku:develop", "prom/node-exporter:latest"},
		imageNames(page), "a reference that starts with the query first")

	first, err := NewImages(hosts, "", "", 2, fullAccess)
	require.NoError(t, err)
	assert.Len(t, first.Matches, 2)
	require.NotEmpty(t, first.Next)

	second, err := NewImages(hosts, "", first.Next, 2, fullAccess)
	require.NoError(t, err)
	assert.Equal(t, []string{"prom/node-exporter:latest"}, imageNames(second))
	assert.Nil(t, second.Total, "counted for the first page only")
	assert.Empty(t, second.Next)

	none, err := NewImages(hosts, "nothing", "", 2, fullAccess)
	require.NoError(t, err)
	assert.Empty(t, none.Matches)
	assert.NotNil(t, none.Matches)

	for _, bad := range []string{"x", "9geth", "0"} {
		_, err = NewImages(hosts, "", bad, 2, fullAccess)
		require.ErrorIs(t, err, ErrBadImageCursor, bad)
	}
}
