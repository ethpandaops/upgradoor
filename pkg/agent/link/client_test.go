package link_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// fakeController authenticates requests like the real controller, answers
// a hello with its keys and a sealed request with resp, signed by signer
// unless forge or a nil signer says otherwise, and sealed.
type fakeController struct {
	t        *testing.T
	srv      *httptest.Server
	verifier *identity.Verifier
	clock    *clock

	mu      sync.Mutex
	keyring *identity.Keyring
	signer  *identity.AnswerSigner
	forge   forgeFunc
	status  int
	raw     []byte // sent as is, unsigned, when set
	// helloStatus, when set, is the status of every hello, without body.
	helloStatus int
	// location is sent as the Location header when set.
	location string
	resp     any
	syncs    []protocol.SyncRequest
	// paths are the paths of the requests, unsealed is set by one with an
	// unsealed body to another path than the hello.
	paths    []string
	unsealed bool
	rekeys   int
	// used are the keys the sealed requests were sealed to.
	used []identity.KeyID
	// plain sends a signed answer unsealed; tamper alters a sealed one.
	plain  bool
	tamper bool
	// announce, when set, is the keyring whose keys are announced.
	announce *identity.Keyring
}

func newFakeController(t *testing.T, clk *clock) *fakeController {
	t.Helper()

	signerKey, err := identity.DeriveSignerKey([]byte(testSeed))
	require.NoError(t, err)

	f := &fakeController{
		t:     t,
		clock: clk,
		verifier: identity.NewVerifier(identity.PublicKey(signerKey), testNetwork,
			identity.WithClock(clk.now)),
		keyring: identity.NewKeyring(identity.WithKeyringClock(clk.now)),
		signer:  answerSigner(t, testControllerSeed),
		status:  http.StatusOK,
		resp:    &protocol.SyncResponse{},
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

// encryptionKeys are the keyring's announced keys in their wire form.
func (f *fakeController) encryptionKeys() []protocol.EncryptionKey {
	keyring := f.keyring
	if f.announce != nil {
		keyring = f.announce
	}

	keys, err := keyring.Announced()
	require.NoError(f.t, err)

	out := make([]protocol.EncryptionKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, protocol.EncryptionKey{
			ID: k.ID.String(), PublicKey: base64.RawURLEncoding.EncodeToString(k.Bytes()),
			ActiveFrom: k.ActiveFrom,
		})
	}

	return out
}

// signed is v encoded and signed by the controller key for the request.
func (f *fakeController) signed(proofID string, v any) []byte {
	out, err := json.Marshal(v)
	require.NoError(f.t, err)

	if f.signer == nil {
		return out
	}

	return sign(f.t, f.signer, testHost, proofID, out)
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

	f.mu.Lock()
	defer f.mu.Unlock()

	f.paths = append(f.paths, r.URL.Path)

	if r.URL.Path == protocol.PathHello {
		if f.helloStatus != 0 {
			w.WriteHeader(f.helloStatus)

			return
		}

		_, _ = w.Write(f.signed(proofID, &protocol.HelloResponse{Keys: f.encryptionKeys()}))

		return
	}

	if r.Header.Get("Content-Type") != protocol.ContentTypeSealed {
		f.unsealed = true

		w.WriteHeader(http.StatusBadRequest)

		return
	}

	sealed, err := identity.ParseSealedRequest(body)
	require.NoError(f.t, err)

	plain, seal, err := f.keyring.Open(sealed, &identity.SealBinding{
		Network: testNetwork, Subject: testHost, RequestID: proofID, Path: r.URL.Path,
	})
	if errors.Is(err, identity.ErrUnknownSealKey) {
		f.rekeys++
		_, _ = w.Write(f.signed(proofID, &protocol.RekeyResponse{Rekey: true, Keys: f.encryptionKeys()}))

		return
	}

	require.NoError(f.t, err)

	f.used = append(f.used, sealed.KeyID)

	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if !assert.NoError(f.t, err) {
		return
	}

	raw, err := io.ReadAll(zr)
	if !assert.NoError(f.t, err) {
		return
	}

	if r.URL.Path == protocol.PathSync {
		var req protocol.SyncRequest
		assert.NoError(f.t, json.Unmarshal(raw, &req))
		f.syncs = append(f.syncs, req)
	}

	resp := f.resp
	if sync, ok := resp.(*protocol.SyncResponse); ok {
		// As the controller: the keys go to a sender that lacks one.
		upToDate, uerr := f.keyring.UpToDate(sealed.KeyID, sealed.Known)
		require.NoError(f.t, uerr)

		if !upToDate {
			withKeys := *sync
			withKeys.Keys = f.encryptionKeys()
			resp = &withKeys
		}
	}

	out := f.raw
	if out == nil {
		out, err = json.Marshal(resp)
		require.NoError(f.t, err)

		switch {
		case f.forge != nil:
			out = f.forge(out, testHost, proofID)
		case f.signer != nil:
			out = sign(f.t, f.signer, testHost, proofID, out)
		}

		if !f.plain {
			w.Header().Set("Content-Type", protocol.ContentTypeSealed)

			out = seal.Seal(out)
		}

		if f.tamper {
			out[len(out)-1] ^= 1
		}
	}

	if f.location != "" {
		w.Header().Set("Location", f.location)
	}

	w.WriteHeader(f.status)
	_, _ = w.Write(out)
}

func answerSigner(t *testing.T, controllerSeed string) *identity.AnswerSigner {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(controllerSeed))
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, testNetwork)
	require.NoError(t, err)

	return signer
}

func controllerPublicKey(t *testing.T) string {
	t.Helper()

	return identity.EncodePublicKey(answerSigner(t, testControllerSeed).PublicKey())
}

func sign(t *testing.T, s *identity.AnswerSigner, host, id string, body []byte) []byte {
	t.Helper()

	out, err := s.Sign(host, id, body)
	require.NoError(t, err)

	return out
}

func credentials(t *testing.T) *identity.Credentials {
	t.Helper()

	key, err := identity.GenerateHostKey()
	require.NoError(t, err)

	cert, err := identity.IssueCertificate([]byte(testSeed), identity.PublicKey(key), identity.CertificateSpec{
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
		PublicKey: controllerPublicKey(t),
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

				signer := answerSigner(t, testControllerSeed)
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

				signer := answerSigner(t, testControllerSeed)

				return func(answer []byte, _, proofID string) []byte {
					return sign(t, signer, "prysm-besu-1", proofID, answer)
				}
			},
			reason: "it is for another host",
		},
		{
			name: "answer of another network's controller",
			forge: func(t *testing.T) forgeFunc {
				t.Helper()

				// The controller key does not depend on the network.
				key, err := identity.DeriveControllerKey([]byte(testControllerSeed))
				require.NoError(t, err)

				signer, err := identity.NewAnswerSigner(key, "other-devnet-1")
				require.NoError(t, err)

				return func(answer []byte, host, proofID string) []byte {
					return sign(t, signer, host, proofID, answer)
				}
			},
			reason: "it is for another network",
		},
		{
			name: "body altered in transit",
			forge: func(t *testing.T) forgeFunc {
				t.Helper()

				signer := answerSigner(t, testControllerSeed)

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

				signer := answerSigner(t, "another controller seed, at least 32 bytes long")

				return func(answer []byte, host, proofID string) []byte {
					return sign(t, signer, host, proofID, answer)
				}
			},
			reason: "not signed by the controller key",
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
					Directives:    []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold, Holder: protocol.HolderStaff}},
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

// With allowUnsignedAnswers unsigned answers are taken, with a warning at start.
func TestUnsignedAnswersNeedTheOptOut(t *testing.T) {
	t.Parallel()

	e := newEnv(t, func(c *link.Config) {
		c.PublicKey = ""
		c.AllowUnsignedAnswers = true
	})
	e.ctrl.set(func(f *fakeController) {
		f.signer = nil
		f.resp = &protocol.SyncResponse{
			Directives: []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold, Holder: protocol.HolderStaff}},
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
		c.PublicKey = ""
		c.AllowUnsignedAnswers = true
	})
	e.ctrl.set(func(f *fakeController) {
		f.resp = &protocol.SyncResponse{
			Directives: []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold, Holder: protocol.HolderStaff}},
		}
	})

	resp, err := e.sync(t)
	require.ErrorContains(t, err, "configure controller.publicKey")
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

	// A failed sync forgets what the controller asked for; the retry waits
	// at most the configured interval.
	e.ctrl.set(func(f *fakeController) { f.status = http.StatusBadGateway })

	_, err := e.sync(t)
	require.Error(t, err)
	assertBetween(t, e.client.Interval(true), time.Second, 5*time.Second)
}

// Failed syncs back off with full jitter up to maxSyncInterval; a refused
// answer counts, one good answer resets it.
func TestIntervalBacksOff(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)

	// ceilings are the bounds after 1, 2, ... failures of an idle agent.
	ceilings := []time.Duration{
		30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		10 * time.Minute, 10 * time.Minute,
	}

	signer := answerSigner(t, testControllerSeed)

	for i, ceiling := range ceilings {
		e.ctrl.set(func(f *fakeController) {
			f.status, f.signer = http.StatusBadGateway, signer
			if i == 2 {
				// A refused answer is a failure too.
				f.status, f.signer = http.StatusOK, nil
			}
		})

		_, err := e.sync(t)
		require.Error(t, err)

		longest := time.Duration(0)

		for range 200 {
			got := e.client.Interval(false)
			assertBetween(t, got, time.Second, ceiling)
			longest = max(longest, got)
		}

		assert.Greater(t, longest, ceiling/2, "spread over the whole range")
	}

	// While busy: from leaseSyncInterval, up to the same bound.
	longest := time.Duration(0)

	for range 200 {
		got := e.client.Interval(true)
		assertBetween(t, got, time.Second, 10*time.Minute)
		longest = max(longest, got)
	}

	assert.Greater(t, longest, time.Minute, "backs off past the lease interval")

	e.ctrl.set(func(f *fakeController) { f.status, f.signer = http.StatusOK, signer })

	_, err := e.sync(t)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, e.client.Interval(false), "one answer resets it")
}

func assertBetween(t *testing.T, got, low, high time.Duration) {
	t.Helper()

	assert.GreaterOrEqual(t, got, low)
	assert.LessOrEqual(t, got, high)
}

// Host state goes out only sealed, after the keys came with a verified hello.
func TestNothingIsSentUnsealed(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)

	_, err := e.sync(t)
	require.NoError(t, err)
	require.NoError(t, e.client.Inventory(t.Context(), &protocol.Inventory{}))

	_, err = e.sync(t)
	require.NoError(t, err)

	e.ctrl.mu.Lock()
	defer e.ctrl.mu.Unlock()

	assert.Equal(t, []string{protocol.PathHello, protocol.PathSync, protocol.PathInventory,
		protocol.PathSync}, e.ctrl.paths, "one hello, then sealed requests")
	assert.False(t, e.ctrl.unsealed)
	assert.Contains(t, e.messages(logrus.InfoLevel), "received the controller's encryption keys")
}

// Keys that do not come with a verified answer are not taken, and nothing is sent.
func TestHelloAnswerIsVerified(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)
	e.ctrl.set(func(f *fakeController) { f.signer = answerSigner(t, "another controller seed, 32 bytes long") })

	for range 2 {
		_, err := e.sync(t)
		require.ErrorIs(t, err, identity.ErrBadAnswer)
	}

	e.ctrl.mu.Lock()
	defer e.ctrl.mu.Unlock()

	assert.Equal(t, []string{protocol.PathHello, protocol.PathHello}, e.ctrl.paths)
}

// A controller without the hello endpoint gets nothing: no unsealed
// fallback, an error said once, retries with backoff.
func TestOldControllerGetsNothing(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)
	e.ctrl.set(func(f *fakeController) { f.helloStatus = http.StatusNotFound })

	for range 3 {
		resp, err := e.sync(t)
		require.ErrorIs(t, err, link.ErrControllerTooOld)
		assert.Nil(t, resp)
	}

	require.ErrorIs(t, e.client.Inventory(t.Context(), &protocol.Inventory{}), link.ErrControllerTooOld)
	assertBetween(t, e.client.Interval(false), time.Second, 2*time.Minute)

	errs := e.messages(logrus.ErrorLevel)
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0], "the controller must be updated")

	e.ctrl.mu.Lock()
	assert.Equal(t, []string{protocol.PathHello, protocol.PathHello, protocol.PathHello,
		protocol.PathHello}, e.ctrl.paths)
	assert.False(t, e.ctrl.unsealed)
	e.ctrl.mu.Unlock()

	// Updated: syncs work.
	e.ctrl.set(func(f *fakeController) { f.helloStatus = 0 })

	_, err := e.sync(t)
	require.NoError(t, err)
	assert.Contains(t, e.messages(logrus.InfoLevel), "sync works again")
}

// A restarted controller answers with its new keys; the request is sent
// again at once, sealed to them, and its answer returned.
func TestControllerRestartIsRekeyed(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)
	e.ctrl.set(func(f *fakeController) {
		f.resp = &protocol.SyncResponse{Lease: &protocol.Lease{ID: "lease-1"}}
	})

	_, err := e.sync(t)
	require.NoError(t, err)

	e.ctrl.set(func(f *fakeController) {
		f.keyring = identity.NewKeyring(identity.WithKeyringClock(e.clock.now))
	})

	resp, err := e.sync(t)
	require.NoError(t, err)
	require.NotNil(t, resp.Lease)
	assert.Equal(t, "lease-1", resp.Lease.ID)

	require.NoError(t, e.client.Inventory(t.Context(), &protocol.Inventory{}))

	e.ctrl.mu.Lock()
	defer e.ctrl.mu.Unlock()

	assert.Equal(t, 1, e.ctrl.rekeys)
	assert.Len(t, e.ctrl.syncs, 2, "the request to the lost key was not processed")
	assert.Equal(t, []string{protocol.PathHello, protocol.PathSync, protocol.PathSync,
		protocol.PathSync, protocol.PathInventory}, e.ctrl.paths, "no further hello")
}

// A controller that asks for new keys again and again gets no answer taken.
func TestRepeatedRekeyIsAnError(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)

	_, err := e.sync(t)
	require.NoError(t, err)

	other := identity.NewKeyring(identity.WithKeyringClock(e.clock.now))

	e.ctrl.set(func(f *fakeController) {
		// Announces keys of another keyring than the one it opens with.
		f.keyring = identity.NewKeyring(identity.WithKeyringClock(e.clock.now))
		f.announce = other
	})

	_, err = e.sync(t)
	require.ErrorContains(t, err, "asked for new keys twice")
}

// The agent follows the rotation: the next key comes ahead of its time,
// is used from then on, and the keys come along only when one is missing.
func TestKeyRotation(t *testing.T) {
	t.Parallel()

	e := newEnv(t, nil)
	start := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	e.clock.set(start.Add(time.Minute))

	syncKeys := func() []protocol.EncryptionKey {
		t.Helper()

		resp, err := e.sync(t)
		require.NoError(t, err)

		return resp.Keys
	}

	assert.Empty(t, syncKeys(), "the hello's keys are up to date")

	e.clock.set(start.Add(55 * time.Minute))
	assert.Len(t, syncKeys(), 2, "the next key is announced")
	assert.Empty(t, syncKeys())

	e.clock.set(start.Add(time.Hour + time.Second))
	assert.Empty(t, syncKeys(), "the next key is used once current")

	// Silent past the previous key's window: rekeyed, not failed.
	e.clock.set(start.Add(4 * time.Hour))
	assert.Empty(t, syncKeys())

	e.ctrl.mu.Lock()
	defer e.ctrl.mu.Unlock()

	assert.Equal(t, 1, e.ctrl.rekeys)
	require.Len(t, e.ctrl.used, 5)
	assert.Equal(t, e.ctrl.used[0], e.ctrl.used[2], "the current key until the next is due")
	assert.NotEqual(t, e.ctrl.used[2], e.ctrl.used[3])
	assert.Equal(t, 1, countOf(e.ctrl.paths, protocol.PathHello))
}

func countOf(list []string, s string) int {
	n := 0

	for _, v := range list {
		if v == s {
			n++
		}
	}

	return n
}

// Answers to sealed requests must be sealed with the request's key.
func TestSealedAnswersAreVerified(t *testing.T) {
	t.Parallel()

	for name, mod := range map[string]func(f *fakeController){
		"altered":  func(f *fakeController) { f.tamper = true },
		"unsealed": func(f *fakeController) { f.plain = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := newEnv(t, nil)
			e.ctrl.set(func(f *fakeController) {
				mod(f)
				f.resp = &protocol.SyncResponse{Lease: &protocol.Lease{ID: "lease-1"}}
			})

			resp, err := e.sync(t)
			require.ErrorIs(t, err, identity.ErrBadAnswer)
			assert.Nil(t, resp)

			require.ErrorIs(t, e.client.Inventory(t.Context(), &protocol.Inventory{}), identity.ErrBadAnswer)
		})
	}
}
