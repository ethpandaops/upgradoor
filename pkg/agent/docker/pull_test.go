package docker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
)

func TestPull(t *testing.T) {
	t.Run("returns the digest the tag was pulled at and the image's revision", func(t *testing.T) {
		f := newFakeEngine()
		f.images[fakeRef].RepoDigests = []string{"ethpandaops/geth@" + digestC, "ethpandaops/geth@" + digestB}
		f.pullMsgs = []jsonstream.Message{
			{Status: "Pulling from ethpandaops/geth"},
			{Status: "Digest: " + digestB},
			{Status: "Status: Downloaded newer image"},
		}
		r := newTestRuntime(t, f)

		got, revision, err := r.Pull(t.Context(), fakeRef)
		require.NoError(t, err)
		assert.Equal(t, digestB, got)
		assert.Equal(t, "newrev", revision)

		local, revision, err := r.LocalImage(t.Context(), fakeRef)
		require.NoError(t, err)
		assert.Equal(t, digestB, local, "LocalImage agrees with the last pull")
		assert.Equal(t, "newrev", revision)

		require.Len(t, f.pulls, 1)
		assert.Empty(t, f.pulls[0].RegistryAuth, "no login, anonymous pull")
	})

	t.Run("surfaces errors from the progress stream", func(t *testing.T) {
		f := newFakeEngine()
		f.pullMsgs = []jsonstream.Message{
			{Status: "Pulling from ethpandaops/geth"},
			{Error: &jsonstream.Error{Message: "manifest unknown"}},
		}

		_, _, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef)
		require.ErrorContains(t, err, "manifest unknown")
	})

	t.Run("request error", func(t *testing.T) {
		f := newFakeEngine()
		f.fail["pull "+fakeRef] = errors.New("daemon down")

		_, _, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef)
		require.ErrorContains(t, err, "daemon down")
	})

	t.Run("image absent after the pull", func(t *testing.T) {
		f := newFakeEngine()
		delete(f.images, fakeRef)

		_, _, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef)
		require.ErrorContains(t, err, "absent after the pull")
	})

	t.Run("sends the login of the image's registry", func(t *testing.T) {
		f := newFakeEngine()
		r := newTestRuntime(t, f)

		var asked []string

		r.login = func(host string) (username, password string, ok bool) {
			asked = append(asked, host)

			return "user", "secret", host == agent.DockerHubHost
		}

		_, _, err := r.Pull(t.Context(), "registry-1.docker.io/"+fakeRef)
		require.ErrorContains(t, err, "absent after the pull", "the fake has no image under that spelling")

		_, _, err = r.Pull(t.Context(), "ghcr.io/"+fakeRef)
		require.ErrorContains(t, err, "absent after the pull")

		assert.Equal(t, []string{agent.DockerHubHost, "ghcr.io"}, asked,
			"asked by the host as the registry lookups spell it")
		require.Len(t, f.pulls, 2)

		raw, err := base64.URLEncoding.DecodeString(f.pulls[0].RegistryAuth)
		require.NoError(t, err)

		var sent registry.AuthConfig
		require.NoError(t, json.Unmarshal(raw, &sent))
		assert.Equal(t, "user", sent.Username)
		assert.Equal(t, "secret", sent.Password)

		assert.Empty(t, f.pulls[1].RegistryAuth, "no login for that registry: anonymous")
	})
}
