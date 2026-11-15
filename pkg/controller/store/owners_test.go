package store

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContainerOwnersAreStored: the owners an agent reports come back with
// the host, also after the store was read again from its row; a row written
// before the field existed reads as a host whose containers have no owners.
func TestContainerOwnersAreStored(t *testing.T) {
	ctx := context.Background()

	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			req := testRequest()
			req.Containers[0].Owners = []string{"geth", "panda"}

			res, err := s.RecordSync(ctx, &HostSync{
				Cert: testCert("node-1", nil), Request: req, At: at(0),
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

			// The row as a build before the field wrote it.
			var status string

			require.NoError(t, rawDB(t, s).QueryRow(
				`SELECT status FROM hosts WHERE name = ?`, "node-1").Scan(&status))
			require.Contains(t, status, `"owners":["geth","panda"]`)

			old := strings.Replace(status, `,"owners":["geth","panda"]`, "", 1)
			require.NotContains(t, old, "owners")

			_, err = rawDB(t, s).Exec(`UPDATE hosts SET status = ? WHERE name = ?`, old, "node-1")
			require.NoError(t, err)

			host, err = s.GetHost(ctx, "node-1")
			require.NoError(t, err)
			require.Len(t, host.Status.Containers, 1)
			assert.Equal(t, "geth", host.Status.Containers[0].Name)
			assert.Nil(t, host.Status.Containers[0].Owners)

			// An agent of an earlier build reports none: the owners are gone
			// with its next sync.
			res, err = s.RecordSync(ctx, &HostSync{
				Cert: testCert("node-1", nil), Request: testRequest(), At: at(30),
			})
			require.NoError(t, err)
			assert.Nil(t, res.Host.Status.Containers[0].Owners)
		})
	}
}
