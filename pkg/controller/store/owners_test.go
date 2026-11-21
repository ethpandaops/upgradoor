package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContainerOwnersAreStored: the owners an agent reports come back with
// the host, also after the store was read again from its row, and are gone
// when a later sync reports none.
func TestContainerOwnersAreStored(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			req := testRequest()
			req.Containers[0].Owners = []string{"geth", "panda"}

			res, err := s.RecordSync(ctx, &HostSync{
				Cert: testCert("node-1"), Request: req, At: at(0),
			})
			require.NoError(t, err)
			assert.Equal(t, []string{"geth", "panda"}, res.Host.Status.Containers[0].Owners)

			host, err := s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			assert.Equal(t, []string{"geth", "panda"}, host.Status.Containers[0].Owners)

			hosts, err := s.ListHosts(ctx)
			require.NoError(t, err)
			require.Len(t, hosts, 1)
			assert.Equal(t, []string{"geth", "panda"}, hosts[0].Status.Containers[0].Owners)

			// The next sync reports none: the owners are gone.
			res, err = s.RecordSync(ctx, &HostSync{
				Cert: testCert("node-1"), Request: testRequest(), At: at(30),
			})
			require.NoError(t, err)
			assert.Nil(t, res.Host.Status.Containers[0].Owners)
		})
	}
}
