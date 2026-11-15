package protocol

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidOwner(t *testing.T) {
	for _, name := range []string{
		"geth", "lighthouse", "panda", "0x", "a", "team-1", "a.b_c-d", strings.Repeat("a", 63),
	} {
		assert.True(t, ValidOwner(name), name)
	}

	for _, name := range []string{
		"", "Geth", " geth", "geth ", "-geth", ".geth", "_geth", "ge th", "geth,panda",
		"geth:all", "geth*", "gëth", strings.Repeat("a", 64),
	} {
		assert.False(t, ValidOwner(name), "%q", name)
	}
}

func TestCleanOwners(t *testing.T) {
	assert.Nil(t, CleanOwners(nil))
	assert.Nil(t, CleanOwners([]string{}))
	assert.Nil(t, CleanOwners([]string{"Not A Team", ""}))
	assert.Equal(t, []string{"geth", "panda"},
		CleanOwners([]string{"panda", "geth", "panda", "PANDA", "geth "}))

	many := []string{"i", "h", "g", "f", "e", "d", "c", "b", "a", "a"}
	assert.Equal(t, []string{"a", "b", "c", "d", "e", "f", "g", "h"}, CleanOwners(many))
	assert.Len(t, CleanOwners(many), MaxOwners)

	// The input is not changed.
	assert.Equal(t, "i", many[0])
}

// TestOwnersOnTheWire: the field is additive. It is left out when a container
// has no owners, so the sync of a build with the field and of one without it
// are the same bytes for such a host; and a body without the field, as an
// earlier agent sends it, decodes to no owners.
func TestOwnersOnTheWire(t *testing.T) {
	raw, err := json.Marshal(Container{Name: "geth", Managed: true})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "owners")

	raw, err = json.Marshal(Container{Name: "geth", Managed: true, Owners: []string{"geth"}})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"owners":["geth"]`)

	var c Container

	require.NoError(t, json.Unmarshal([]byte(`{"name":"geth","managed":true}`), &c))
	assert.Nil(t, c.Owners)

	// What an earlier controller does with the field: its Container type
	// does not have it, and its decoder skips what it does not know.
	var earlier struct {
		Name    string `json:"name"`
		Managed bool   `json:"managed"`
	}

	require.NoError(t, json.Unmarshal(raw, &earlier))
	assert.Equal(t, "geth", earlier.Name)
	assert.True(t, earlier.Managed)
}
