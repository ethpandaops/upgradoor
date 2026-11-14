package web_test

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/web"
)

// TestDistServesIndex holds for both builds: the placeholder (no tag) and the
// embedded UI (webui tag, which needs web/dist to be built).
func TestDistServesIndex(t *testing.T) {
	dist, err := web.Dist()
	require.NoError(t, err)

	index, err := fs.ReadFile(dist, "index.html")
	require.NoError(t, err)

	assert.Contains(t, string(index), "<title>upgradoor</title>")

	// The file system is rooted at the directory that holds index.html.
	_, err = fs.Stat(dist, "dist")
	require.ErrorIs(t, err, fs.ErrNotExist)

	_, err = fs.Stat(dist, "placeholder")
	require.ErrorIs(t, err, fs.ErrNotExist)
}
