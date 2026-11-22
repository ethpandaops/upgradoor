package local

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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

// fakeAgent records what the socket asked and answers with err.
type fakeAgent struct {
	mu    sync.Mutex
	err   error
	calls []string
}

func (f *fakeAgent) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, call)

	return f.err
}

func (f *fakeAgent) Status() Status {
	return Status{Host: "host-1", Rolls: []Roll{{ID: "r1", State: RollQueued}}}
}

func (f *fakeAgent) Override(user string, req *OverrideRequest) (string, error) {
	return "image " + req.Image, f.record(fmt.Sprintf("override %s %s clear=%t", user, req.Container, req.Clear))
}

func (f *fakeAgent) Hold(user string, req *HoldRequest) error {
	return f.record(fmt.Sprintf("hold %s %q release=%t", user, req.Reason, req.Release))
}

func (f *fakeAgent) Roll(user, container string) (*Roll, error) {
	if err := f.record("roll " + user + " " + container); err != nil {
		return nil, err
	}

	return &Roll{ID: "roll-1", Container: container}, nil
}

func (f *fakeAgent) InitOverrides(user string) (string, error) {
	return "/home/devops/overrides.yaml", f.record("init " + user)
}

func (f *fakeAgent) Deploy(_ context.Context, user string, req *DeployRequest) (*DeployResult, error) {
	if err := f.record(fmt.Sprintf("deploy %s %s reset=%t", user, req.Container, req.Reset)); err != nil {
		return nil, err
	}

	return &DeployResult{Container: req.Container, Roll: DeployRollNone, Ignore: []string{}, Pull: true}, nil
}

// maxSocketPath is the longest path a unix socket address holds.
const maxSocketPath = 107

// socketPath is a socket path in a short directory of its own.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp(os.TempDir(), "s")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "agent.sock")
	require.LessOrEqual(t, len(path), maxSocketPath,
		"the socket path %s is too long for a unix socket: set TMPDIR to a shorter directory", path)

	return path
}

// serve starts a server on a socket in a temporary directory.
func serve(t *testing.T, cfg *Config, agent Agent) (string, Client) {
	t.Helper()

	cfg.Socket.Path = socketPath(t)
	srv := NewServer(quiet(), cfg, agent)
	require.NoError(t, srv.Start(t.Context()))

	t.Cleanup(func() {
		require.NoError(t, srv.Stop())
		require.NoError(t, srv.Stop(), "a second Stop is fine")

		_, err := os.Stat(cfg.Socket.Path)
		assert.ErrorIs(t, err, os.ErrNotExist, "the socket file is removed")
	})

	return cfg.Socket.Path, NewClient(cfg.Socket.Path)
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

// The caller's name comes from the kernel.
func TestServer(t *testing.T) {
	agent := &fakeAgent{}
	_, c := serve(t, &Config{EntryUser: "nobody-here"}, agent)

	me, err := user.Current()
	require.NoError(t, err)

	who := "local:" + me.Username

	st, err := c.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "host-1", st.Host)
	assert.Equal(t, "r1", st.Rolls[0].ID)

	answer, err := c.Override(t.Context(), &OverrideRequest{Container: "geth", Image: "a/b:c"})
	require.NoError(t, err)
	assert.Equal(t, "override of geth set (image a/b:c); the container follows under the next lease",
		answer.Message)

	answer, err = c.Override(t.Context(), &OverrideRequest{Container: "geth", Clear: true})
	require.NoError(t, err)
	assert.Equal(t, "override of geth cleared; the container is put back under the next lease",
		answer.Message)

	answer, err = c.Hold(t.Context(), &HoldRequest{Reason: "disk"})
	require.NoError(t, err)
	assert.Equal(t, "host held: the agent reports no needs and refuses all work until it is released",
		answer.Message)

	answer, err = c.Hold(t.Context(), &HoldRequest{Release: true})
	require.NoError(t, err)
	assert.Equal(t, "local hold released", answer.Message)

	answer, err = c.Roll(t.Context(), "geth")
	require.NoError(t, err)
	assert.Equal(t, Answer{Roll: "roll-1", Message: "roll of geth requested"}, *answer)

	answer, err = c.InitOverrides(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "overrides file laid out: /home/devops/overrides.yaml", answer.Message)

	assert.Equal(t, []string{
		"override " + who + " geth clear=false",
		"override " + who + " geth clear=true",
		"hold " + who + ` "disk" release=false`,
		"hold " + who + ` "" release=true`,
		"roll " + who + " geth",
		"init " + who,
	}, agent.calls)
}

func TestServerFailures(t *testing.T) {
	agent := &fakeAgent{}
	path, c := serve(t, &Config{EntryUser: "nobody-here"}, agent)

	assert.Equal(t, http.StatusBadRequest, post(t, path, PathHold, `{`))
	assert.Empty(t, agent.calls)

	_, err := c.Roll(t.Context(), "geth")
	require.NoError(t, err)

	// A refusal is a conflict, its message without the prefix.
	agent.err = plan.Refusal("verb roll is not allowed on this host")
	assert.Equal(t, http.StatusConflict, post(t, path, PathRolls, `{"container":"geth"}`))

	_, err = c.Roll(t.Context(), "geth")
	require.EqualError(t, err, "verb roll is not allowed on this host")

	agent.err = errors.New("disk full")
	assert.Equal(t, http.StatusInternalServerError, post(t, path, PathHold, `{"reason":"x"}`))

	_, err = c.InitOverrides(t.Context())
	require.EqualError(t, err, "disk full")

	_, err = c.Override(t.Context(), &OverrideRequest{Container: "geth"})
	require.EqualError(t, err, "disk full")
}

// Socket requests are bounded to 64 KiB, and unknown fields are errors.
func TestServerRefusesUnknownAndLargeBodies(t *testing.T) {
	agent := &fakeAgent{}
	path, _ := serve(t, &Config{EntryUser: "nobody-here"}, agent)

	assert.Equal(t, http.StatusBadRequest, post(t, path, PathRolls, `{"container":"geth","x":1}`))
	assert.Equal(t, http.StatusBadRequest,
		post(t, path, PathHold, `{"reason":"`+strings.Repeat("x", maxBody)+`"}`))
	assert.Empty(t, agent.calls)
}

// The socket is for root and one group.
func TestServerSocketMode(t *testing.T) {
	path, _ := serve(t, &Config{EntryUser: "nobody-here"}, &fakeAgent{})

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSocket, info.Mode()&os.ModeSocket)
	assert.Equal(t, os.FileMode(rootOnlyMode), info.Mode().Perm(), "no group: root only")

	me, err := user.Current()
	require.NoError(t, err)

	g, err := user.LookupGroupId(me.Gid)
	require.NoError(t, err)

	path, _ = serve(t, &Config{Socket: Socket{Group: g.Name}}, &fakeAgent{})

	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(socketMode), info.Mode().Perm())

	// A stale socket file is replaced.
	stale := &Config{EntryUser: "nobody-here"}
	stale.Socket.Path = socketPath(t)

	var lc net.ListenConfig

	l, err := lc.Listen(t.Context(), "unix", stale.Socket.Path)
	require.NoError(t, err)

	ul, ok := l.(*net.UnixListener)
	require.True(t, ok)
	ul.SetUnlinkOnClose(false)
	require.NoError(t, ul.Close())
	require.FileExists(t, stale.Socket.Path)

	srv := NewServer(quiet(), stale, &fakeAgent{})
	require.NoError(t, srv.Start(t.Context()))
	require.NoError(t, srv.Stop())
}

func TestServerStopAfterFailedStart(t *testing.T) {
	cfg := &Config{}
	cfg.Socket.Path = filepath.Join(t.TempDir(), "missing", "\x00", "agent.sock")

	srv := NewServer(quiet(), cfg, &fakeAgent{})
	require.Error(t, srv.Start(t.Context()))
	require.NoError(t, srv.Stop())
}

// Only the deployer's uid (root) may deploy; anyone else in the group is refused.
func TestServerDeploy(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deployer int
		status   int
	}{
		{"another user", os.Getuid() + 1, http.StatusForbidden},
		{"the deployer", os.Getuid(), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{}
			cfg := &Config{EntryUser: "nobody-here"}
			cfg.Socket.Path = socketPath(t)

			srv, ok := NewServer(quiet(), cfg, agent).(*server)
			require.True(t, ok)

			srv.deployer = tc.deployer
			require.NoError(t, srv.Start(t.Context()))
			t.Cleanup(func() { _ = srv.Stop() })

			body := `{"container":"geth","reset":true}`
			assert.Equal(t, tc.status, post(t, cfg.Socket.Path, PathDeploy, body))

			res, err := NewClient(cfg.Socket.Path).Deploy(t.Context(), &DeployRequest{Container: "geth"})
			if tc.status != http.StatusOK {
				require.EqualError(t, err, "deploy is for root only")
				assert.Empty(t, agent.calls)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, DeployResult{Container: "geth", Roll: DeployRollNone, Ignore: []string{}, Pull: true}, *res)

			me, err := user.Current()
			require.NoError(t, err)

			assert.Equal(t, []string{
				"deploy local:" + me.Username + " geth reset=true",
				"deploy local:" + me.Username + " geth reset=false",
			}, agent.calls)
		})
	}
}
