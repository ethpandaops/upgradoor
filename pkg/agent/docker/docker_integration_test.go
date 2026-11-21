//go:build docker_integration

package docker

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// TestIntegrationLabels: the runtime reads the roll group and the kind off
// the labels Ansible sets, and the stop timeout a container was created
// with; a container without the group label is nobody's.
func TestIntegrationLabels(t *testing.T) {
	env.cleanup(t, "labelled")
	env.cleanup(t, "plain")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("labels")
	env.publish(t, ref, variantEntryOne)

	_, _, err := r.Pull(ctx, ref)
	require.NoError(t, err)

	timeout := 42

	for name, labels := range map[string]map[string]string{
		env.prefix + "labelled": {protocol.LabelGroup: "proxy", protocol.LabelKind: "whatever"},
		env.prefix + "plain":    {"own": "yes"},
	} {
		_, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref, Labels: labels, StopTimeout: &timeout},
		})
		require.NoError(t, err)
	}

	labelled := observed(t, r, env.prefix+"labelled")
	assert.Equal(t, "proxy", labelled.Group)
	assert.Equal(t, protocol.KindOther, labelled.Kind, "an unknown kind is other")
	assert.Equal(t, 42*time.Second, labelled.StopTimeout)

	plain := observed(t, r, env.prefix+"plain")
	assert.Empty(t, plain.Group)
	assert.Empty(t, plain.Kind)
}
