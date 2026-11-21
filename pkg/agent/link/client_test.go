package link_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/link"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	testSeed           = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testControllerSeed = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testNetwork        = "testnet"
	testHost           = "lighthouse-geth-1"
)

// forgeFunc returns what is sent instead of the controller's answer body.
type forgeFunc func(answer []byte, host, proofID string) []byte

// fakeController authenticates requests like the real controller and answers
// with resp, signed by signer unless forge or a nil signer says otherwise.
type fakeController struct {
	t        *testing.T
	srv      *httptest.Server
	verifier *identity.Verifier

	mu     sync.Mutex
	signer *identity.AnswerSigner
	forge  forgeFunc
	status int
	raw    []byte // sent as is, unsigned, when set
	// location is sent as the Location header when set.
	location string
	resp     any
	syncs    []protocol.SyncRequest
}

func newFakeController(t *testing.T, clk *clock) *fakeController {
	t.Helper()

	signerKey, err := identity.DeriveSignerKey([]byte(testSeed), testNetwork)
	require.NoError(t, err)

	f := &fakeController{
		t: t,
		verifier: identity.NewVerifier(identity.PublicKey(signerKey), testNetwork,
			identity.WithClock(clk.now)),
		signer: answerSigner(t, 0),
		status: http.StatusOK,
		resp:   &protocol.SyncResponse{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fakeController) set(fn func(f *fakeController)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

func (f *fakeController) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if !assert.NoError(f.t, err) {
		return
	}

	_, proofID, err := f.verifier.Verify(r.Header.Get(identity.HeaderCert),
		r.Header.Get(identity.HeaderProof), identity.Request{
			Method: r.Method, URLs: []string{f.srv.URL + r.URL.Path}, Body: body,
		})
	if !assert.NoError(f.t, err, "the request is signed") {
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	assert.Equal(f.t, "gzip", r.Header.Get("Content-Encoding"))

	zr, err := gzip.NewReader(bytes.NewReader(body))
	if !assert.NoError(f.t, err) {
		return
	}

	raw, err := io.ReadAll(zr)
	if !assert.NoError(f.t, err) {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == protocol.PathSync {
		var req protocol.SyncRequest
		assert.NoError(f.t, json.Unmarshal(raw, &req))
		f.syncs = append(f.syncs, req)
	}

	out := f.raw
	if out == nil {
		out, err = json.Marshal(f.resp)
		require.NoError(f.t, err)

		switch {
		case f.forge != nil:
			out = f.forge(out, testHost, proofID)
		case f.signer != nil:
			out = sign(f.t, f.signer, testHost, proofID, out)
		}
	}

	if f.location != "" {
		w.Header().Set("Location", f.location)
	}

	w.WriteHeader(f.status)
	_, _ = w.Write(out)
}

func answerSigner(t *testing.T, generation uint64) *identity.AnswerSigner {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(testControllerSeed), testNetwork, generation)
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, testNetwork)
	require.NoError(t, err)

	return signer
}

func controllerPublicKey(t *testing.T, generation uint64) string {
	t.Helper()

	return identity.EncodePublicKey(answerSigner(t, generation).PublicKey())
}

func sign(t *testing.T, s *identity.AnswerSigner, host, id string, body []byte) []byte {
	t.Helper()

	out, err := s.Sign(host, id, body)
	require.NoError(t, err)

	return out
}

func credentials(t *testing.T) *identity.Credentials {
	t.Helper()

	key, err := identity.DeriveHostKey([]byte(testSeed), testNetwork, testHost, 0)
	require.NoError(t, err)

	cert, err := identity.IssueCertificate([]byte(testSeed), identity.CertificateSpec{
		Network: testNetwork, Hostname: testHost,
	})
	require.NoError(t, err)

	creds, err := identity.NewCredentials(key, cert)
	require.NoError(t, err)

	return creds
}

// clock is a settable time source shared by client and fake controller.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = t
}

// env is a client against a fake controller, with its log.
type env struct {
	ctrl   *fakeController
	client link.Client
	hook   *logtest.Hook
	clock  *clock
}

func newEnv(t *testing.T, cfg func(c *link.Config)) *env {
	t.Helper()

	clk := &clock{t: time.Now()}
	ctrl := newFakeController(t, clk)
	c := &link.Config{
		URL: ctrl.srv.URL + "/", AllowInsecure: true,
		PublicKeys: []string{controllerPublicKey(t, 0)},
	}

	if cfg != nil {
		cfg(c)
	}

	c.ApplyDefaults()
	require.NoError(t, c.Validate())

	log, hook := logtest.NewNullLogger()
	log.SetLevel(logrus.DebugLevel)

	client, err := link.New(log, c, credentials(t), clk.now)
	require.NoError(t, err)

	return &env{ctrl: ctrl, client: client, hook: hook, clock: clk}
}

func (e *env) sync(t *testing.T) (*protocol.SyncResponse, error) {
	t.Helper()

	return e.client.Sync(t.Context(), &protocol.SyncRequest{})
}

// messages returns the log messages at level.
func (e *env) messages(level logrus.Level) []string {
	out := make([]string, 0, 4)

	for _, entry := range e.hook.AllEntries() {
		if entry.Level == level {
			out = append(out, entry.Message)
		}
	}

	return out
}

// An answer not signed by a configured key for this host and request is never returned.
func TestAnswersNotOfTheControllerAreNotActedOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		forge func(t *testing.T) forgeFunc
		// reason is what the error says.
		reason string
	}{
		{
			name: "unsigned answer",
			forge: func(*testing.T) forgeFunc {
				return func(answer []byte, _, _ string) []byte { return answer }
			},
			reason: "it is not signed",
		},
		{
			name: "answer to an earlier request, replayed",
			forge: func(t *testing.T) forgeFunc {
				t.Helper()

				signer := answerSigner(t, 0)
				earlier := "the-request-before-the-first"

				return func(answer []byte, host, proofID string) []byte {
					// Validly signed, but for the previous request.
					out := sign(t, signer, host, earlier, answer)
					earlier = proofID

					return out
				}
			},
			reason: "it answers another request",
		},
		{
			name: "answer for another host",
			forge: func(t *testing.T) forgeFunc {
				t.Helper()

				signer := answerSigner(t, 0)

				return func(answer []byte, _, proofID string) []byte {
					return sign(t, signer, "prysm-besu-1", proofID, answer)
				}
			},
			reason: "it is for another host",
		},
		{
			name: "body altered in transit",
			forge: func(t *testing.T) forgeFunc {
				t.Helper()

				signer := answerSigner(t, 0)

				return func(answer []byte, host, proofID string) []byte {
					// A lease injected into a signed answer without one.
					harmless, err := json.Marshal(protocol.SyncResponse{})
					require.NoError(t, err)

					var signed identity.SignedAnswer
					require.NoError(t, json.Unmarshal(sign(t, signer, host, proofID, harmless), &signed))

					signed.Payload = base64.RawURLEncoding.EncodeToString(answer)

					out, err := json.Marshal(signed)
					require.NoError(t, err)

					return out
				}
			},
			reason: "the body does not match its signature",
		},
		{
			name: "signed with a controller key the host does not accept",
			forge: func(t *testing.T) forgeFunc {
				t.Helper()

				// The next generation: a rotated key this host was not given.
				signer := answerSigner(t, 1)

				return func(answer []byte, host, proofID string) []byte {
					return sign(t, signer, host, proofID, answer)
				}
			},
			reason: "not signed by an accepted controller key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, nil)
			e.ctrl.set(func(f *fakeController) {
				f.forge = tt.forge(t)
				f.resp = &protocol.SyncResponse{
					Lease:         &protocol.Lease{ID: "lease-1"},
					Directives:    []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}},
					WantInventory: true,
				}
			})

			for range 4 {
				resp, err := e.sync(t)
				require.ErrorIs(t, err, identity.ErrBadAnswer)
				require.ErrorContains(t, err, tt.reason)
				require.Nil(t, resp, "nothing of the answer is returned")
			}

			err := e.client.Inventory(t.Context(), &protocol.Inventory{})
			require.ErrorIs(t, err, identity.ErrBadAnswer, "an inventory answer is verified too")

			// Said once for the whole episode, with the reason.
			refusals := make([]string, 0, 1)

			for _, entry := range e.hook.AllEntries() {
				if entry.Level == logrus.ErrorLevel &&
					strings.Contains(entry.Message, "refused the controller's answer") {
					logged, ok := entry.Data[logrus.ErrorKey].(error)
					require.True(t, ok)

					refusals = append(refusals, logged.Error())
				}
			}

			require.Len(t, refusals, 1)
			assert.Contains(t, refusals[0], tt.reason)

			// The controller itself answers again: its answer is returned.
			e.ctrl.set(func(f *fakeController) { f.forge = nil })

			resp, err := e.sync(t)
			require.NoError(t, err)
			require.NotNil(t, resp.Lease)
			assert.Equal(t, "lease-1", resp.Lease.ID)
			assert.Contains(t, e.messages(logrus.InfoLevel), "sync works again")
		})
	}
}

// Answers signed with any listed controller key are accepted.
func TestAnswersOfEachListedControllerKeyAreAccepted(t *testing.T) {
	t.Parallel()

	for _, generation := range []uint64{0, 1} {
		e := newEnv(t, func(c *link.Config) {
			c.PublicKeys = []string{controllerPublicKey(t, 0), controllerPublicKey(t, 1)}
		})
		e.ctrl.set(func(f *fakeController) {
			f.signer = answerSigner(t, generation)
			f.resp = &protocol.SyncResponse{Lease: &protocol.Lease{ID: "lease-1"}}
		})

		resp, err := e.sync(t)
		require.NoError(t, err)
		require.NotNil(t, resp.Lease)
	}
}

// With allowUnsignedAnswers unsigned answers are taken, with a warning at start.
func TestUnsignedAnswersNeedTheOptOut(t *testing.T) {
	t.Parallel()

	e := newEnv(t, func(c *link.Config) {
		c.PublicKeys = nil
		c.AllowUnsignedAnswers = true
	})
	e.ctrl.set(func(f *fakeController) {
		f.signer = nil
		f.resp = &protocol.SyncResponse{
			Directives: []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}},
		}
	})

	resp, err := e.sync(t)
	require.NoError(t, err)
	require.Len(t, resp.Directives, 1)

	warned := false

	for _, msg := range e.messages(logrus.WarnLevel) {
		if strings.HasPrefix(msg, "INSECURE") {
			warned = true
		}
	}

	assert.True(t, warned, "running without verification is logged loudly")
}

// Without controller keys a signed answer is refused, not read as empty.
func TestSignedAnswersAreRefusedWithoutControllerKeys(t *testing.T) {
	t.Parallel()

	e := newEnv(t, func(c *link.Config) {
		c.PublicKeys = nil
		c.AllowUnsignedAnswers = true
	})
	e.ctrl.set(func(f *fakeController) {
		f.resp = &protocol.SyncResponse{
			Directives: []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}},
		}
	})

	resp, err := e.sync(t)
	require.ErrorContains(t, err, "configure controller.publicKeys")
	require.NotErrorIs(t, err, identity.ErrBadAnswer,
		"a misconfigured opt-out, not somebody else answering")
	assert.Nil(t, resp)

	// After the INSECURE line of the start: the warning of a failed sync.
	assert.Equal(t, "sync failed; retrying on every tick", e.messages(logrus.WarnLevel)[1])
	assert.Empty(t, e.messages(logrus.ErrorLevel))
}

// A redirect is never followed.
func TestRedirectsAreNotFollowed(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex

	reached := 0
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		reached++
	}))
	t.Cleanup(other.Close)

	e := newEnv(t, nil)
	e.ctrl.set(func(f *fakeController) {
		f.status = http.StatusTemporaryRedirect
		f.location = other.URL + protocol.PathSync
		f.raw = []byte{}
	})

	resp, err := e.sync(t)
	require.EqualError(t, err, "post /api/agent/v1/sync: status 307: ")
	assert.Nil(t, resp)

	mu.Lock()
	defer mu.Unlock()

	assert.Zero(t, reached, "the other server receives nothing")
}

// A cancelled sync logs nothing above debug and forgets the controller's pace.
func TestCancelledSyncIsNoEpisode(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)
	e.ctrl.set(func(f *fakeController) {
		f.resp = &protocol.SyncResponse{NextSyncMillis: 2000}
	})

	_, err := e.sync(t)
	require.NoError(t, err)
	require.Equal(t, 2*time.Second, e.client.Interval(false))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	resp, err := e.client.Sync(ctx, &protocol.SyncRequest{})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, resp)
	assert.Equal(t, 30*time.Second, e.client.Interval(false))
	assert.Empty(t, e.messages(logrus.WarnLevel))
	assert.Empty(t, e.messages(logrus.ErrorLevel))

	// The episode did not start: the next answer is no "works again".
	_, err = e.sync(t)
	require.NoError(t, err)
	assert.NotContains(t, e.messages(logrus.InfoLevel), "sync works again")
}

// An answer above 4 MiB is not read to its end, and nothing of it is used.
func TestAnswersAreCapped(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)
	e.ctrl.set(func(f *fakeController) { f.raw = bytes.Repeat([]byte("x"), 4<<20+1) })

	resp, err := e.sync(t)
	require.ErrorContains(t, err, "exceeds 4194304 bytes")
	assert.Nil(t, resp)
}

// A non-2xx answer is only an error, logged once per episode.
func TestErrorAnswersAreOnlyErrors(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)
	serverTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e.clock.set(serverTime.Add(time.Hour))

	e.ctrl.set(func(f *fakeController) {
		f.status = http.StatusUnauthorized
		f.raw = []byte(`{"error":"proof expired","serverTime":"2026-01-02T03:04:05Z"}`)
	})

	for range 3 {
		resp, err := e.sync(t)
		require.EqualError(t, err, "post /api/agent/v1/sync: status 401: proof expired "+
			"(controller time 2026-01-02T03:04:05Z, local time 2026-01-02T04:04:05Z)")
		require.Nil(t, resp)
	}

	e.ctrl.set(func(f *fakeController) { f.raw = []byte("bad gateway\n") })

	_, err := e.sync(t)
	require.EqualError(t, err, "post /api/agent/v1/sync: status 401: bad gateway")

	assert.Equal(t, []string{"sync failed; retrying on every tick"}, e.messages(logrus.WarnLevel))
	assert.Len(t, e.messages(logrus.DebugLevel), 3)

	// Within an outage a refused answer is still said, at error level.
	e.ctrl.set(func(f *fakeController) {
		f.status = http.StatusOK
		f.raw = nil
		f.signer = nil
	})

	_, err = e.sync(t)
	require.ErrorIs(t, err, identity.ErrBadAnswer)
	require.Len(t, e.messages(logrus.ErrorLevel), 1)

	// An outage after a refused answer is the same episode: debug only.
	e.ctrl.set(func(f *fakeController) {
		f.status = http.StatusBadGateway
		f.raw = []byte("bad gateway")
	})

	_, err = e.sync(t)
	require.Error(t, err)
	assert.Len(t, e.messages(logrus.ErrorLevel), 1)
	assert.Len(t, e.messages(logrus.WarnLevel), 1)
	assert.Len(t, e.messages(logrus.DebugLevel), 4)
}

func TestInterval(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)

	// Before an answer: the configured intervals.
	assert.Equal(t, 30*time.Second, e.client.Interval(false))
	assert.Equal(t, 5*time.Second, e.client.Interval(true))

	for _, tt := range []struct {
		asked int64
		want  time.Duration
	}{
		{asked: 2000, want: 2 * time.Second},
		{asked: 10, want: time.Second},
		{asked: int64(time.Hour / time.Millisecond), want: 10 * time.Minute},
	} {
		e.ctrl.set(func(f *fakeController) {
			f.resp = &protocol.SyncResponse{NextSyncMillis: tt.asked}
		})

		_, err := e.sync(t)
		require.NoError(t, err)
		assert.Equal(t, tt.want, e.client.Interval(false))
		assert.Equal(t, tt.want, e.client.Interval(true), "the controller's pace wins")
	}

	// A failed sync forgets what the controller asked for.
	e.ctrl.set(func(f *fakeController) { f.status = http.StatusBadGateway })

	_, err := e.sync(t)
	require.Error(t, err)
	assert.Equal(t, 5*time.Second, e.client.Interval(true))
}

func TestSilent(t *testing.T) {
	t.Parallel()

	const def = 90 * time.Minute

	e := newEnv(t, nil)
	assert.True(t, e.client.Silent(def), "silent before the first answer")

	_, err := e.sync(t)
	require.NoError(t, err)

	e.clock.set(e.clock.now().Add(def))
	assert.False(t, e.client.Silent(def))

	e.clock.set(e.clock.now().Add(time.Second))
	assert.True(t, e.client.Silent(def), "def without a fallback of the controller")

	e.ctrl.set(func(f *fakeController) {
		f.resp = &protocol.SyncResponse{LookupFallbackSeconds: 60}
	})

	_, err = e.sync(t)
	require.NoError(t, err)

	// The answer's fallback stays when a later answer names none.
	e.ctrl.set(func(f *fakeController) { f.resp = &protocol.SyncResponse{} })

	_, err = e.sync(t)
	require.NoError(t, err)

	e.clock.set(e.clock.now().Add(61 * time.Second))
	assert.True(t, e.client.Silent(def), "the controller's fallback")
}
