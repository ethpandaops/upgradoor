package local

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func TestContainersOf(t *testing.T) {
	view := &plan.View{
		Containers: []protocol.Container{
			{
				Name: "geth", Managed: true, Image: "geth:fix", Command: []string{"--a", "--b"},
				Base: &protocol.OverrideBase{Image: "geth:v1", Command: []string{"--a"}, HasCommand: true},
			},
			{Name: "teku", Managed: true, Image: "teku:v1", Kind: protocol.KindCL},
			{Name: "dora", Managed: true, Image: "dora:v1"},
			{Name: "debug", Image: "x:y"},
		},
		Targets: map[string]plan.Target{
			"geth": {Image: "geth:fix", InSync: true},
			"teku": {Image: "teku:v2"},
		},
	}

	got := ContainersOf(view)

	assert.Len(t, got, 3, "managed only")
	assert.Equal(t, []string{"--a"}, got[0].BaseCommand)
	assert.Empty(t, got[0].Target, "in sync")
	assert.Equal(t, "teku:v2", got[1].Target)
	assert.Nil(t, got[1].BaseCommand)
	assert.Empty(t, got[2].Target, "no target without labels")

	got[0].BaseCommand[0] = "changed"
	assert.Equal(t, []string{"--a"}, view.Containers[0].Base.Command, "a copy")
}
