package local_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
)

func TestClientWithoutAgent(t *testing.T) {
	c := local.NewClient(filepath.Join(t.TempDir(), "none.sock"))

	_, err := c.Status(t.Context())
	require.ErrorContains(t, err, "the agent's socket does not answer")
}
