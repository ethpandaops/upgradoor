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

func TestValidKind(t *testing.T) {
	assert.True(t, ValidKind("snooper-el"))
	assert.True(t, ValidKind(KindVC))
	assert.False(t, ValidKind(""))
	assert.False(t, ValidKind("EL"))
}

// TestTeamOnTheWire: the field is left out when a container has no team.
func TestTeamOnTheWire(t *testing.T) {
	raw, err := json.Marshal(Container{Name: "geth", Managed: true})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "team")

	raw, err = json.Marshal(Container{Name: "geth", Managed: true, Kind: KindEL, Team: "geth"})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"kind":"el","team":"geth"`)
}
