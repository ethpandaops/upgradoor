package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/registry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
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

		got, revision, err := r.Pull(t.Context(), fakeRef, nil)
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

		_, _, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef, nil)
		require.ErrorContains(t, err, "manifest unknown")
	})

	t.Run("request error", func(t *testing.T) {
		f := newFakeEngine()
		f.fail["pull "+fakeRef] = errors.New("daemon down")

		_, _, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef, nil)
		require.ErrorContains(t, err, "daemon down")
	})

	t.Run("image absent after the pull", func(t *testing.T) {
		f := newFakeEngine()
		delete(f.images, fakeRef)

		_, _, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef, nil)
		require.ErrorContains(t, err, "absent after the pull")
	})

	t.Run("sends the login of the image's registry", func(t *testing.T) {
		f := newFakeEngine()
		r := newTestRuntime(t, f)

		var asked []string

		r.login = func(host string) (username, password string, ok bool) {
			asked = append(asked, host)

			return "user", "secret", host == imageref.DockerHubHost
		}

		_, _, err := r.Pull(t.Context(), "registry-1.docker.io/"+fakeRef, nil)
		require.ErrorContains(t, err, "absent after the pull", "the fake has no image under that spelling")

		_, _, err = r.Pull(t.Context(), "ghcr.io/"+fakeRef, nil)
		require.ErrorContains(t, err, "absent after the pull")

		assert.Equal(t, []string{imageref.DockerHubHost, "ghcr.io"}, asked,
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

func progressMsg(id, status string, current, total int64) jsonstream.Message {
	msg := jsonstream.Message{ID: id, Status: status}
	if total > 0 {
		msg.Progress = &jsonstream.Progress{Current: current, Total: total}
	}

	return msg
}

func TestPullProgress(t *testing.T) {
	f := newFakeEngine()
	f.pullMsgs = []jsonstream.Message{
		{ID: "latest", Status: "Pulling from ethpandaops/geth"},
		progressMsg("a", "Already exists", 0, 0),
		progressMsg("b", "Pulling fs layer", 0, 0),
		progressMsg("c", "Waiting", 0, 0),
		progressMsg("b", "Downloading", 100, 1000),
		progressMsg("b", "Downloading", 100, 1000),
		progressMsg("b", "Retrying in 5 seconds", 0, 0),
		progressMsg("c", "Downloading", 50, 500),
		progressMsg("b", "Verifying Checksum", 0, 0),
		progressMsg("b", "Download complete", 0, 0),
		progressMsg("b", "Extracting", 400, 1000),
		progressMsg("b", "Pull complete", 0, 0),
		{Status: "Digest: " + digestB},
		{Status: "Status: Downloaded newer image"},
	}

	var seen []protocol.PullProgress

	_, _, err := newTestRuntime(t, f).Pull(t.Context(), fakeRef, func(p *protocol.PullProgress) {
		seen = append(seen, *p)
	})
	require.NoError(t, err)

	assert.Equal(t, []protocol.PullProgress{
		{LayersTotal: 1, LayersDone: 1},
		{LayersTotal: 2, LayersDone: 1},
		{LayersTotal: 3, LayersDone: 1},
		{LayersTotal: 3, LayersDone: 1, BytesDone: 100, BytesTotal: 1000},
		{LayersTotal: 3, LayersDone: 1, BytesDone: 150, BytesTotal: 1500},
		{LayersTotal: 3, LayersDone: 1, BytesDone: 1050, BytesTotal: 1500},
		{LayersTotal: 3, LayersDone: 1, BytesDone: 1050, BytesTotal: 1500},
		{LayersTotal: 3, LayersDone: 2, BytesDone: 1050, BytesTotal: 1500},
	}, seen, "repeats, the retry countdown and the tag's own line are no progress")
}

func TestPullStall(t *testing.T) {
	const stall = 50 * time.Millisecond

	t.Run("cancelled after the stall bound without progress", func(t *testing.T) {
		f := newFakeEngine()
		f.pullMsgs = []jsonstream.Message{progressMsg("b", "Downloading", 1, 10)}
		f.pullHang = true
		r := newTestRuntime(t, f)
		r.stall = stall

		_, _, err := r.Pull(t.Context(), fakeRef, nil)
		require.ErrorIs(t, err, ErrPullStalled)
	})

	t.Run("progress keeps a pull longer than the bound alive", func(t *testing.T) {
		f := newFakeEngine()
		f.pullPause = stall / 3

		for i := range int64(9) {
			f.pullMsgs = append(f.pullMsgs, progressMsg("b", "Downloading", i+1, 10))
		}

		f.pullMsgs = append(f.pullMsgs, jsonstream.Message{Status: "Digest: " + digestB})
		r := newTestRuntime(t, f)
		r.stall = stall

		got, _, err := r.Pull(t.Context(), fakeRef, nil)
		require.NoError(t, err)
		assert.Equal(t, digestB, got)
	})

	t.Run("messages without progress do not keep it alive", func(t *testing.T) {
		f := newFakeEngine()
		f.pullPause = stall / 5

		for range 20 {
			f.pullMsgs = append(f.pullMsgs, progressMsg("b", "Retrying in 1 second", 0, 0))
		}

		r := newTestRuntime(t, f)
		r.stall = stall

		_, _, err := r.Pull(t.Context(), fakeRef, nil)
		require.ErrorIs(t, err, ErrPullStalled)
	})

	t.Run("the caller's cancel is not a stall", func(t *testing.T) {
		f := newFakeEngine()
		f.pullHang = true
		r := newTestRuntime(t, f)

		ctx, cancel := context.WithTimeout(t.Context(), stall)
		defer cancel()

		_, _, err := r.Pull(ctx, fakeRef, nil)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, ErrPullStalled)
	})
}
