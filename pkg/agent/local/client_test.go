package local

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientWithoutAgent(t *testing.T) {
	c := NewClient(filepath.Join(t.TempDir(), "none.sock"))

	_, err := c.Status(t.Context())
	require.ErrorContains(t, err, "the agent's socket does not answer")
}
