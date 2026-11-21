//go:build docker_integration

package docker

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pull moves the tag without touching the running container.
func TestIntegrationDigests(t *testing.T) {
	env.cleanup(t, "digests")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("digests")
	name := env.prefix + "digests"

	digest1 := env.publish(t, ref, variantOne)

	local, _, err := r.LocalImage(ctx, ref)
	require.NoError(t, err)
	assert.Empty(t, local, "nothing pulled yet")

	pulled, revision, err := r.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest1, pulled)
	assert.Equal(t, "rev1", revision, "the revision label of the pulled image")

	local, revision, err = r.LocalImage(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest1, local)
	assert.Equal(t, "rev1", revision)

	created, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: ref},
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx, name))

	before := observed(t, r, name)
	assert.Equal(t, created.ID, before.ID)
	assert.Equal(t, ref, before.Image)
	assert.NotEmpty(t, before.ImageID)
	assert.Equal(t, digest1, before.Digest)
	assert.Equal(t, "rev1", before.Revision)
	assert.Equal(t, "1.0-rev1", before.Version)
	assert.Equal(t, "running", before.State)
	assert.Contains(t, []string{"starting", "healthy"}, before.Health)
	assert.Zero(t, before.RestartCount)
	assert.WithinDuration(t, time.Now(), before.StartedAt, time.Minute)
	assert.False(t, before.Managed)

	// Every container is observed, the test registry included.
	reg := observed(t, r, env.prefix+"registry")
	assert.Equal(t, registryRef, reg.Image)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, reg.Digest)

	// A new image appears under the tag.
	digest2 := env.publish(t, ref, variantTwo)
	require.NotEqual(t, digest1, digest2)

	local, _, err = r.LocalImage(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest1, local, "the registry moved, the local tag did not")

	pulled, revision, err = r.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest2, pulled)
	assert.Equal(t, "rev2", revision, "known before any container runs the image")

	local, revision, err = r.LocalImage(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest2, local)
	assert.Equal(t, "rev2", revision)

	after := observed(t, r, name)
	assert.Equal(t, before.ID, after.ID, "the pull does not touch the container")
	assert.Equal(t, before.StartedAt, after.StartedAt)
	assert.Equal(t, "running", after.State)
	assert.Equal(t, before.ImageID, after.ImageID)
	assert.Equal(t, digest1, after.Digest, "the container still runs the old image")
	assert.Equal(t, "rev1", after.Revision)

	// Pulling again changes nothing.
	pulled, _, err = r.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest2, pulled)

	// Errors of the pull are reported.
	_, _, err = r.Pull(ctx, env.repoRef("digests-absent"))
	require.Error(t, err)

	local, _, err = r.LocalImage(ctx, env.repoRef("digests-absent"))
	require.NoError(t, err)
	assert.Empty(t, local)

	// Stop and Start are idempotent.
	require.NoError(t, r.Stop(ctx, name, time.Second))
	require.NoError(t, r.Stop(ctx, name, time.Second))
	assert.Equal(t, "exited", observed(t, r, name).State)
	require.NoError(t, r.Start(ctx, name))
	require.NoError(t, r.Start(ctx, name))
	assert.Equal(t, "running", observed(t, r, name).State)
	require.Error(t, r.Start(ctx, env.prefix+"digests-absent"))
	require.Error(t, r.Stop(ctx, env.prefix+"digests-absent", time.Second))
}
