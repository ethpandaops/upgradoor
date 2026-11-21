package local_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
)

// fakeService records what the socket asked and answers with err.
type fakeService struct {
	err   error
	calls []string
}

func (f *fakeService) Status() local.Status {
	return local.Status{Host: "host-1", Rolls: []local.Roll{{ID: "r1", State: local.RollQueued}}}
}

func (f *fakeService) Override(user string, req *local.OverrideRequest) (string, error) {
	f.calls = append(f.calls, fmt.Sprintf("override %s %s clear=%t", user, req.Container, req.Clear))

	return "image " + req.Image, f.err
}

func (f *fakeService) Hold(user string, req *local.HoldRequest) error {
	f.calls = append(f.calls, fmt.Sprintf("hold %s %q release=%t", user, req.Reason, req.Release))

	return f.err
}

func (f *fakeService) Roll(user, container string) (*local.Roll, error) {
	f.calls = append(f.calls, "roll "+user+" "+container)

	if f.err != nil {
		return nil, f.err
	}

	return &local.Roll{ID: "roll-1", Container: container}, nil
}

func (f *fakeService) InitOverrides(user string) (string, error) {
	f.calls = append(f.calls, "init "+user)

	return "/home/devops/overrides.yaml", f.err
}

// serve starts a server on a socket in a temporary directory.
func serve(t *testing.T, svc local.Service) (string, *local.Client) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "agent.sock")
	srv := local.NewServer(logrus.New(), local.Socket{Path: path, EntryUser: "nobody-here"}, svc)
	require.NoError(t, srv.Start(t.Context()))

	t.Cleanup(func() {
		require.NoError(t, srv.Stop())

		_, err := os.Stat(path)
		assert.ErrorIs(t, err, os.ErrNotExist, "the socket file is removed")
	})

	return path, local.NewClient(path)
}

// post sends a raw request to the socket and returns the status code.
func post(t *testing.T, path, route, body string) int {
	t.Helper()

	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, "unix", path)
		},
	}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://upgradoor"+route,
		strings.NewReader(body))
	require.NoError(t, err)

	resp, err := c.Do(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	return resp.StatusCode
}

func TestServer(t *testing.T) {
	svc := &fakeService{}
	path, c := serve(t, svc)

	me, err := user.Current()
	require.NoError(t, err)

	who := "local:" + me.Username

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSocket, info.Mode()&os.ModeSocket)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "no group: root only")

	st, err := c.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "host-1", st.Host)
	assert.Equal(t, "r1", st.Rolls[0].ID)

	answer, err := c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Image: "a/b:c"})
	require.NoError(t, err)
	assert.Equal(t, "override of geth set (image a/b:c); the container follows under the next lease",
		answer.Message)

	answer, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth", Clear: true})
	require.NoError(t, err)
	assert.Equal(t, "override of geth cleared; the container is put back under the next lease",
		answer.Message)

	answer, err = c.Hold(t.Context(), &local.HoldRequest{Reason: "disk"})
	require.NoError(t, err)
	assert.Equal(t, "host held: the agent reports no needs and refuses all work until it is released",
		answer.Message)

	answer, err = c.Hold(t.Context(), &local.HoldRequest{Release: true})
	require.NoError(t, err)
	assert.Equal(t, "local hold released", answer.Message)

	answer, err = c.Roll(t.Context(), "geth")
	require.NoError(t, err)
	assert.Equal(t, local.Answer{Roll: "roll-1", Message: "roll of geth requested"}, *answer)

	answer, err = c.InitOverrides(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "generated block written to /home/devops/overrides.yaml", answer.Message)

	assert.Equal(t, []string{
		"override " + who + " geth clear=false",
		"override " + who + " geth clear=true",
		"hold " + who + ` "disk" release=false`,
		"hold " + who + ` "" release=true`,
		"roll " + who + " geth",
		"init " + who,
	}, svc.calls)
}

func TestServerFailures(t *testing.T) {
	svc := &fakeService{}
	path, c := serve(t, svc)

	// A request the server cannot read reaches nobody.
	assert.Equal(t, http.StatusBadRequest, post(t, path, local.PathRolls, `{"container":"geth","x":1}`))
	assert.Equal(t, http.StatusBadRequest, post(t, path, local.PathHold, `{`))
	assert.Empty(t, svc.calls)

	_, err := c.Roll(t.Context(), "geth")
	require.NoError(t, err)

	// A refusal of the host's rules is a conflict, its message without the
	// prefix.
	svc.err = fmt.Errorf("%w: verb roll is not allowed on this host", auditlog.ErrRefused)
	assert.Equal(t, http.StatusConflict, post(t, path, local.PathRolls, `{"container":"geth"}`))

	_, err = c.Roll(t.Context(), "geth")
	require.EqualError(t, err, "verb roll is not allowed on this host")

	// Anything else is the agent's own failure.
	svc.err = errors.New("disk full")
	assert.Equal(t, http.StatusInternalServerError, post(t, path, local.PathHold, `{"reason":"x"}`))

	_, err = c.InitOverrides(t.Context())
	require.EqualError(t, err, "disk full")

	_, err = c.Override(t.Context(), &local.OverrideRequest{Container: "geth"})
	require.EqualError(t, err, "disk full")
}
