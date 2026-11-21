package api

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func validatorAgent(t *testing.T) *testAgent {
	t.Helper()

	return newAgent(t, identity.CertificateSpec{
		Hostname: "lighthouse-geth-1",
		Roles:    []string{"ethereum_node"},
	})
}

// validatorAgentGeneration is validatorAgent with the host key and
// certificate of the given generation.
func validatorAgentGeneration(t *testing.T, generation uint64) *testAgent {
	t.Helper()

	return newAgent(t, identity.CertificateSpec{
		Hostname:   "lighthouse-geth-1",
		Generation: generation,
		Roles:      []string{"ethereum_node"},
	})
}

// requireAgentError checks a protocol.ErrorResponse answer.
func requireAgentError(t *testing.T, rec *httptest.ResponseRecorder, status int, contains string) {
	t.Helper()

	require.Equal(t, status, rec.Code, rec.Body.String())

	body := decodeJSON[protocol.ErrorResponse](t, rec)
	assert.Contains(t, body.Error, contains)
	assert.True(t, body.ServerTime.Equal(testEpoch), "error answers carry the controller time")
}

func TestSyncPlainAndGzip(t *testing.T) {
	for _, gz := range []bool{false, true} {
		name := "plain"
		if gz {
			name = "gzip"
		}

		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			agent := validatorAgent(t)

			rec := h.agentPost(agent, protocol.PathSync, validatorSyncRequest(),
				agentCallOpts{Gzip: gz})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			resp := decodeJSON[protocol.SyncResponse](t, rec)
			assert.True(t, resp.ServerTime.Equal(testEpoch))
			assert.False(t, resp.WantInventory, "no inventory on either side")
			assert.InDelta(t, idleMinInterval.Milliseconds(), resp.NextSyncMillis,
				float64(idleMinInterval.Milliseconds())*pacingJitter, "an idle host of a small fleet")
			assert.Nil(t, resp.Lease)
			assert.Empty(t, resp.Directives)
			assert.Nil(t, resp.Queue)

			host := h.store.host(agent.name)
			require.NotNil(t, host)
			assert.Equal(t, "agent-1.0.0", host.Status.AgentVersion)
			assert.Equal(t, uint64(100), host.Weight())
			assert.Equal(t, 1, h.sched.nudgeCount())
		})
	}
}

func TestAgentAuthentication(t *testing.T) {
	body, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	otherSeedCert, err := identity.IssueCertificate(
		[]byte("another seed that is at least 32 bytes long ......"),
		identity.CertificateSpec{Network: testNetwork, Hostname: "lighthouse-geth-1"})
	require.NoError(t, err)

	// A certificate for another network, signed by this network's signer:
	// the signature is good, the network claim is not.
	signer, err := identity.DeriveSignerKey([]byte(testSeed), testNetwork)
	require.NoError(t, err)

	hostKey, err := identity.DeriveHostKey([]byte(testSeed), testNetwork, "lighthouse-geth-1", 0)
	require.NoError(t, err)

	wrongNetCert, err := identity.SignCertificate(signer, identity.PublicKey(hostKey),
		identity.CertificateSpec{Network: "other-devnet-1", Hostname: "lighthouse-geth-1"})
	require.NoError(t, err)

	// The same host name, issued under another network's seed-derived signer.
	foreign := newAgent(t, identity.CertificateSpec{
		Network: "other-devnet-1", Hostname: "lighthouse-geth-1",
	})

	tests := []struct {
		name     string
		opts     func(*Options)
		agent    *testAgent
		call     agentCallOpts
		contains string
	}{
		{
			name: "missing headers",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Del(identity.HeaderCert)
				r.Header.Del(identity.HeaderProof)
			}},
			contains: "missing",
		},
		{
			name: "missing proof",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Del(identity.HeaderProof)
			}},
			contains: "missing",
		},
		{
			name: "garbage certificate",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, "not.a.certificate")
			}},
			contains: "bad certificate",
		},
		{
			name: "certificate from another signer",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, otherSeedCert)
			}},
			contains: "bad certificate",
		},
		{
			name: "certificate for another network, good signature",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, wrongNetCert)
			}},
			contains: "another network",
		},
		{
			name:     "identity of another network",
			agent:    foreign,
			contains: "bad certificate",
		},
		{
			name: "proof signed by another host",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				other := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})
				r.Header.Set(identity.HeaderCert, other.creds.Certificate())
			}},
			contains: "bad proof",
		},
		{
			name:     "proof for another URL",
			call:     agentCallOpts{ProofURL: "https://upgradoor.evil.example" + protocol.PathSync},
			contains: "does not match",
		},
		{
			name:     "proof for the inventory endpoint",
			call:     agentCallOpts{ProofURL: testBaseURL + protocol.PathInventory},
			contains: "does not match",
		},
		{
			name:     "proof for plain http is refused by default",
			call:     agentCallOpts{ProofURL: "http://" + testHost + protocol.PathSync},
			contains: "does not match",
		},
		{
			name:     "expired proof",
			call:     agentCallOpts{ProofTime: testEpoch.Add(-10 * time.Minute)},
			contains: "time window",
		},
		{
			name:     "proof from the future",
			call:     agentCallOpts{ProofTime: testEpoch.Add(10 * time.Minute)},
			contains: "time window",
		},
		{
			name: "host-derived URL is refused when public URLs are configured",
			opts: func(o *Options) {
				o.PublicURLs = []string{"https://upgradoor.public.example/"}
			},
			contains: "does not match",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.opts)

			agent := tt.agent
			if agent == nil {
				agent = validatorAgent(t)
			}

			rec := h.serve(h.agentRequest(agent, protocol.PathSync, body, tt.call))
			requireAgentError(t, rec, http.StatusUnauthorized, tt.contains)

			assert.Nil(t, h.store.host("lighthouse-geth-1"), "nothing may be stored")
			assert.Zero(t, h.sched.nudgeCount())
		})
	}
}

func TestAgentBodyTamper(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	body, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	signed := h.agentRequest(agent, protocol.PathSync, body, agentCallOpts{})

	tampered := bytes.Replace(body, []byte("agent-1.0.0"), []byte("agent-6.6.6"), 1)
	req := httptest.NewRequest(http.MethodPost, testBaseURL+protocol.PathSync,
		bytes.NewReader(tampered))
	req.Header = signed.Header.Clone()

	requireAgentError(t, h.serve(req), http.StatusUnauthorized, "body hash")
	assert.Nil(t, h.store.host(agent.name))
}

func TestAgentGzipTamper(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	body, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	// The proof covers the plain bytes; the sender then claims they are
	// gzip. The proof still matches the wire, so this is a decoding error
	// after authentication, not an identity error.
	req := h.agentRequest(agent, protocol.PathSync, body, agentCallOpts{
		Mutate: func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
	})
	requireAgentError(t, h.serve(req), http.StatusBadRequest, "invalid gzip")

	// A proof over the plain body does not cover a compressed wire body.
	signed := h.agentRequest(agent, protocol.PathSync, body, agentCallOpts{})
	req = httptest.NewRequest(http.MethodPost, testBaseURL+protocol.PathSync,
		bytes.NewReader(gzipBytes(t, body)))
	req.Header = signed.Header.Clone()
	req.Header.Set("Content-Encoding", "gzip")
	requireAgentError(t, h.serve(req), http.StatusUnauthorized, "body hash")
}

func TestAgentReplay(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	body, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	first := h.agentRequest(agent, protocol.PathSync, body, agentCallOpts{})
	headers := first.Header.Clone()

	require.Equal(t, http.StatusOK, h.serve(first).Code)

	replay := httptest.NewRequest(http.MethodPost, testBaseURL+protocol.PathSync,
		bytes.NewReader(body))
	replay.Header = headers

	requireAgentError(t, h.serve(replay), http.StatusUnauthorized, "replayed")
	assert.Equal(t, 1, h.sched.nudgeCount())
}

func TestAgentProofURLs(t *testing.T) {
	body, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	tests := []struct {
		name     string
		opts     func(*Options)
		proofURL string
		host     string
		status   int
	}{
		{
			name:     "derived https",
			proofURL: testBaseURL + protocol.PathSync,
			status:   http.StatusOK,
		},
		{
			name:     "derived http when allowed",
			opts:     func(o *Options) { o.AllowInsecureAgentHTTP = true },
			proofURL: "http://" + testHost + protocol.PathSync,
			status:   http.StatusOK,
		},
		{
			name:     "derived https still accepted when http is allowed",
			opts:     func(o *Options) { o.AllowInsecureAgentHTTP = true },
			proofURL: testBaseURL + protocol.PathSync,
			status:   http.StatusOK,
		},
		{
			name: "configured public URL, whatever the Host header says",
			opts: func(o *Options) {
				o.PublicURLs = []string{"https://upgradoor.public.example/"}
			},
			proofURL: "https://upgradoor.public.example" + protocol.PathSync,
			host:     "upgradoor.upgradoor.svc.cluster.local:8080",
			status:   http.StatusOK,
		},
		{
			name: "second configured public URL with a path prefix",
			opts: func(o *Options) {
				o.PublicURLs = []string{
					"https://upgradoor.public.example", "https://alt.example/prefix",
				}
			},
			proofURL: "https://alt.example/prefix" + protocol.PathSync,
			status:   http.StatusOK,
		},
		{
			name:     "another host name",
			proofURL: testBaseURL + protocol.PathSync,
			host:     "upgradoor.other.example",
			status:   http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.opts)
			agent := validatorAgent(t)

			req := h.agentRequest(agent, protocol.PathSync, body, agentCallOpts{
				ProofURL: tt.proofURL,
				Mutate: func(r *http.Request) {
					if tt.host != "" {
						r.Host = tt.host
					}
				},
			})

			assert.Equal(t, tt.status, h.serve(req).Code)
		})
	}
}

func TestAgentBodyLimits(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wire     int
		gzip     bool
		chunked  bool
		contains string
	}{
		{
			name: "sync over the wire limit", path: protocol.PathSync,
			wire: protocol.MaxSyncBody + 1, contains: "larger than",
		},
		{
			name: "sync over the wire limit without content length", path: protocol.PathSync,
			wire: protocol.MaxSyncBody + 1, chunked: true, contains: "larger than",
		},
		{
			name: "sync gzip bomb", path: protocol.PathSync,
			wire: 4 * protocol.MaxSyncBody, gzip: true, contains: "after decompression",
		},
		{
			name: "inventory over the wire limit", path: protocol.PathInventory,
			wire: protocol.MaxInventoryBody + 1, contains: "larger than",
		},
		{
			name: "inventory gzip bomb", path: protocol.PathInventory,
			wire: 2 * protocol.MaxInventoryBody, gzip: true, contains: "after decompression",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			agent := validatorAgent(t)

			// Spaces are valid JSON whitespace and compress to almost nothing.
			body := bytes.Repeat([]byte(" "), tt.wire)

			req := h.agentRequest(agent, tt.path, body, agentCallOpts{
				Gzip: tt.gzip,
				Mutate: func(r *http.Request) {
					if tt.chunked {
						r.ContentLength = -1
					}
				},
			})

			if tt.gzip {
				require.Less(t, req.ContentLength, int64(protocol.MaxSyncBody/8),
					"the bomb itself is small")
			}

			requireAgentError(t, h.serve(req), http.StatusRequestEntityTooLarge, tt.contains)
			assert.Nil(t, h.store.host(agent.name))
		})
	}
}

func TestAgentBodyAtLimitIsRead(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	raw, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	// Pad a valid request with whitespace up to exactly the limit.
	body := append(raw, bytes.Repeat([]byte(" "), protocol.MaxSyncBody-len(raw))...)
	require.Len(t, body, protocol.MaxSyncBody)

	rec := h.serve(h.agentRequest(agent, protocol.PathSync, body, agentCallOpts{}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAgentRequestErrors(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		body     string
		call     agentCallOpts
		status   int
		contains string
	}{
		{
			name: "sync: not json", path: protocol.PathSync, body: "{nope",
			status: http.StatusBadRequest, contains: "invalid sync request",
		},
		{
			name: "sync: protocol version mismatch", path: protocol.PathSync,
			body:   `{"protocolVersion":2,"agentVersion":"x"}`,
			status: http.StatusBadRequest, contains: "protocol version 2 is not supported",
		},
		{
			name: "sync: missing protocol version", path: protocol.PathSync,
			body:   `{"agentVersion":"x"}`,
			status: http.StatusBadRequest, contains: "protocol version 0 is not supported",
		},
		{
			name: "sync: unsupported content encoding", path: protocol.PathSync, body: "{}",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Set("Content-Encoding", "br")
			}},
			status: http.StatusUnsupportedMediaType, contains: "unsupported content encoding",
		},
		{
			name: "inventory: not json", path: protocol.PathInventory, body: "[",
			status: http.StatusBadRequest, contains: "invalid inventory",
		},
		{
			name: "inventory: wrong hash", path: protocol.PathInventory,
			body:   `{"hash":"00","packages":[],"upgradable":[]}`,
			status: http.StatusBadRequest, contains: "hash mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			agent := validatorAgent(t)

			rec := h.serve(h.agentRequest(agent, tt.path, []byte(tt.body), tt.call))
			requireAgentError(t, rec, tt.status, tt.contains)
			assert.Nil(t, h.store.host(agent.name))
			assert.Zero(t, h.sched.nudgeCount())
		})
	}
}

// unreadBody fails the test when the handler reads the request body.
type unreadBody struct {
	t *testing.T
}

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("the body was read before the sender was verified")

	return 0, io.EOF
}

func (unreadBody) Close() error { return nil }

// An unverified sender gets its 401 without a slot and without its body
// being read, so it can neither hold slots nor memory.
func TestAgentIsVerifiedBeforeSlotAndBody(t *testing.T) {
	body, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	otherSeedCert, err := identity.IssueCertificate(
		[]byte("another seed that is at least 32 bytes long ......"),
		identity.CertificateSpec{Network: testNetwork, Hostname: "lighthouse-geth-1"})
	require.NoError(t, err)

	tests := []struct {
		name     string
		call     agentCallOpts
		contains string
	}{
		{
			name: "garbage headers",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, "x")
				r.Header.Set(identity.HeaderProof, "x")
			}},
			contains: "bad certificate",
		},
		{
			name: "certificate from another signer",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, otherSeedCert)
			}},
			contains: "bad certificate",
		},
		{
			// A certificate is not secret enough to hold a slot: it travels
			// in a header of every request.
			name: "valid certificate with a garbage proof",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderProof, "x")
			}},
			contains: "bad proof",
		},
		{
			name: "valid certificate with another host's proof",
			call: agentCallOpts{Mutate: func(r *http.Request) {
				other := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})
				r.Header.Set(identity.HeaderCert, other.creds.Certificate())
			}},
			contains: "bad proof",
		},
		{
			name:     "valid certificate with an expired proof",
			call:     agentCallOpts{ProofTime: testEpoch.Add(-10 * time.Minute)},
			contains: "time window",
		},
	}

	for _, path := range []string{protocol.PathSync, protocol.PathInventory} {
		for _, tt := range tests {
			t.Run(path+"/"+tt.name, func(t *testing.T) {
				h := newHarness(t, nil)

				// Every slot is taken.
				for range maxConcurrentSyncs {
					h.srv.syncSlots <- struct{}{}
				}

				for range maxConcurrentInventories {
					h.srv.inventorySlots <- struct{}{}
				}

				req := h.agentRequest(validatorAgent(t), path, body, tt.call)
				req.Body = unreadBody{t: t}

				requireAgentError(t, h.serve(req), http.StatusUnauthorized, tt.contains)
			})
		}
	}
}

// The check before the slot must not use up the proof: the same request is
// then verified with its body.
func TestAgentPreCheckDoesNotConsumeProof(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	// An empty body is the one case where a body-less check could pass.
	rec := h.serve(h.agentRequest(agent, protocol.PathSync, nil, agentCallOpts{}))
	requireAgentError(t, rec, http.StatusBadRequest, "invalid sync request")

	rec = h.agentPost(agent, protocol.PathSync, syncRequest(), agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A verified host that trickles its body loses its slot after the read
// timeout, whatever timeouts the http.Server has.
func TestAgentSlowBodyReleasesSlot(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.inventoryReadTimeout = 200 * time.Millisecond

	// No server timeouts at all: the handler has to bound the read itself.
	srv := httptest.NewServer(h.srv.Handler())
	t.Cleanup(srv.Close)

	agent := validatorAgent(t)

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	body, err := json.Marshal(inventoryFor(t))
	require.NoError(t, err)

	type result struct {
		status int
		body   string
		err    error
	}

	results := make(chan result, maxConcurrentInventories)
	writers := make([]*io.PipeWriter, 0, maxConcurrentInventories)

	t.Cleanup(func() {
		for _, pw := range writers {
			_ = pw.Close()
		}
	})

	// One slow upload per slot: half of the body, then silence.
	for range maxConcurrentInventories {
		signed := h.agentRequest(agent, protocol.PathInventory, body, agentCallOpts{})

		pr, pw := io.Pipe()
		writers = append(writers, pw)

		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srv.URL+protocol.PathInventory, pr)
		require.NoError(t, reqErr)

		req.Host = testHost
		req.ContentLength = int64(len(body))
		req.Header = signed.Header.Clone()

		go func() {
			_, _ = pw.Write(body[:len(body)/2])
		}()

		go func() {
			res, doErr := srv.Client().Do(req)
			if doErr != nil {
				results <- result{err: doErr}

				return
			}

			defer func() { _ = res.Body.Close() }()

			raw, readErr := io.ReadAll(res.Body)
			results <- result{status: res.StatusCode, body: string(raw), err: readErr}
		}()
	}

	for range maxConcurrentInventories {
		select {
		case res := <-results:
			require.NoError(t, res.err)
			assert.Equal(t, http.StatusRequestTimeout, res.status, res.body)
			assert.Contains(t, res.body, "not received within")
			assert.Contains(t, res.body, "serverTime")
		case <-time.After(10 * time.Second):
			t.Fatal("a slow upload kept its slot")
		}
	}

	assert.Empty(t, h.srv.inventorySlots, "every slot is free again")

	// A complete upload over the same server works.
	signed := h.agentRequest(agent, protocol.PathInventory, body, agentCallOpts{})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+protocol.PathInventory, bytes.NewReader(body))
	require.NoError(t, err)

	req.Host = testHost
	req.Header = signed.Header.Clone()

	res, err := srv.Client().Do(req)
	require.NoError(t, err)

	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode, string(raw))
}

func TestSyncResponseAssembly(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	need := protocol.Need{
		ID:   protocol.ImageNeedID("lighthouse", "sha256:new"),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: "lighthouse", Image: "sigp/lighthouse:unstable",
			FromDigest: "sha256:old", ToDigest: "sha256:new",
		},
	}

	lease := &protocol.Lease{
		ID:        "lease-1",
		GrantedAt: testEpoch.Add(-5 * time.Minute),
		ExpiresAt: testEpoch.Add(10 * time.Minute),
		// A stale value: the response must carry the time left now.
		TTLSeconds: 1800,
		Needs:      []protocol.Need{need},
	}

	h.sched.set(func(f *fakeScheduler) {
		f.assignments[agent.name] = &scheduler.Assignment{
			Lease: lease,
			Directives: []protocol.Directive{
				{ID: "d-1", Kind: protocol.DirectiveRecheck},
				{ID: "d-2", Kind: protocol.DirectiveRoll, Container: "geth", Reason: "manual"},
			},
			Queue: &protocol.Queue{Position: 3, Reason: "budget 8% of 10%"},
		}
	})

	req := syncRequest()
	req.InventoryHash = "hash-a"
	req.Needs = []protocol.Need{need}

	rec := h.agentPost(agent, protocol.PathSync, req, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := decodeJSON[protocol.SyncResponse](t, rec)
	assert.True(t, resp.WantInventory, "the controller holds no inventory")
	require.NotNil(t, resp.Lease)
	assert.Equal(t, "lease-1", resp.Lease.ID)
	assert.Equal(t, int64(600), resp.Lease.TTLSeconds)
	assert.Equal(t, []protocol.Need{need}, resp.Lease.Needs)
	assert.Equal(t, int64(1800), lease.TTLSeconds, "the scheduler's lease is not modified")
	require.Len(t, resp.Directives, 2)
	assert.Equal(t, protocol.DirectiveRoll, resp.Directives[1].Kind)
	assert.Equal(t, &protocol.Queue{Position: 3, Reason: "budget 8% of 10%"}, resp.Queue)

	// An expired lease is still restated, with no time left.
	h.clock.Advance(20 * time.Minute)

	rec = h.agentPost(agent, protocol.PathSync, req, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp = decodeJSON[protocol.SyncResponse](t, rec)
	require.NotNil(t, resp.Lease)
	assert.Zero(t, resp.Lease.TTLSeconds)
	assert.Equal(t, 2, h.sched.nudgeCount())
}

func TestSyncStoresRemoteAddr(t *testing.T) {
	tests := []struct {
		name    string
		trust   bool
		headers []string
		want    string
	}{
		{name: "socket address", want: "192.0.2.1"},
		{
			name: "forwarded header ignored when not trusted", headers: []string{"203.0.113.9"},
			want: "192.0.2.1",
		},
		{
			name: "forwarded header", trust: true, headers: []string{"203.0.113.9"},
			want: "203.0.113.9",
		},
		{
			name: "last forwarded entry wins over client-supplied ones", trust: true,
			headers: []string{"198.51.100.66, 10.9.9.9", "203.0.113.9"}, want: "203.0.113.9",
		},
		{
			name: "garbage forwarded header falls back", trust: true,
			headers: []string{"not-an-address"}, want: "192.0.2.1",
		},
		{
			name: "ipv6 forwarded header", trust: true, headers: []string{"2001:db8::1"},
			want: "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, func(o *Options) { o.TrustForwardedHeaders = tt.trust })
			agent := validatorAgent(t)

			body, err := json.Marshal(syncRequest())
			require.NoError(t, err)

			req := h.agentRequest(agent, protocol.PathSync, body, agentCallOpts{
				Mutate: func(r *http.Request) {
					for _, v := range tt.headers {
						r.Header.Add("X-Forwarded-For", v)
					}
				},
			})

			require.Equal(t, http.StatusOK, h.serve(req).Code)
			assert.Equal(t, tt.want, h.store.host(agent.name).RemoteAddr)
		})
	}
}

func TestSyncAuditsIdentity(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	audit := h.store.auditEntries()
	require.Len(t, audit, 1)
	assert.Equal(t, "host.registered", audit[0].Action)
	assert.Equal(t, "agent:lighthouse-geth-1", audit[0].Actor)
	assert.Equal(t, "lighthouse-geth-1", audit[0].Target)
	assert.True(t, audit[0].Time.Equal(testEpoch))

	firstHash := identity.CertificateHash(agent.creds.Certificate())
	assert.Equal(t, firstHash, audit[0].Detail["certHash"])

	// The same certificate again: nothing new.
	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)
	require.Len(t, h.store.auditEntries(), 1)

	// A rotated key: accepted, and audited with both hashes.
	rotated := newAgent(t, identity.CertificateSpec{
		Hostname:   "lighthouse-geth-1",
		Generation: 1,
		Roles:      []string{"ethereum_node"},
	})

	require.Equal(t, http.StatusOK,
		h.agentPost(rotated, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	audit = h.store.auditEntries()
	require.Len(t, audit, 2)

	changed := audit[1]
	secondHash := identity.CertificateHash(rotated.creds.Certificate())

	assert.Equal(t, "host.certificate-changed", changed.Action)
	assert.Equal(t, "agent:lighthouse-geth-1", changed.Actor)
	assert.Equal(t, "lighthouse-geth-1", changed.Target)
	assert.Equal(t, map[string]any{"certHash": firstHash}, changed.Detail["before"])
	assert.Equal(t, map[string]any{"certHash": secondHash}, changed.Detail["after"])
	assert.NotEqual(t, firstHash, secondHash)
	assert.Equal(t, secondHash, h.store.host("lighthouse-geth-1").CertHash)
}

// Rotating a host key means issuing the next generation. Once the controller
// has accepted it, the certificates of older generations are refused, on both
// endpoints, although the signer signed them.
func TestOlderCertificateGenerationIsRejected(t *testing.T) {
	const wantErr = "certificate generation 0 is older than generation 1 already seen for this host"

	h := newHarness(t, nil)
	leaked := validatorAgentGeneration(t, 0)
	rotated := validatorAgentGeneration(t, 1)

	inv := inventoryFor(t, protocol.Package{Name: "openssl", Version: "3.5.1-1", Arch: "amd64"})

	// Before the rotation the generation 0 key is the host's key.
	require.Equal(t, http.StatusOK,
		h.agentPost(leaked, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)
	require.Equal(t, http.StatusOK,
		h.agentPost(leaked, protocol.PathInventory, inv, agentCallOpts{}).Code)

	// The rotated host syncs: accepted and audited as a changed certificate.
	require.Equal(t, http.StatusOK,
		h.agentPost(rotated, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)
	require.Equal(t, uint64(1), h.store.host("lighthouse-geth-1").Generation)

	before := *h.store.host("lighthouse-geth-1")
	auditBefore := len(h.store.auditEntries())

	// Whoever holds the old key is refused, and nothing of the request is
	// recorded.
	stolen := syncRequest()
	stolen.AgentVersion = "evil"
	stolen.Hold.Controller = true

	h.clock.Advance(time.Minute)

	rec := h.agentPost(leaked, protocol.PathSync, stolen, agentCallOpts{})
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, wantErr, decodeJSON[protocol.ErrorResponse](t, rec).Error)
	assert.Equal(t, before, *h.store.host("lighthouse-geth-1"), "the host row is untouched")

	evil := inventoryFor(t, protocol.Package{Name: "evil", Version: "6.6.6", Arch: "amd64"})

	rec = h.agentPost(leaked, protocol.PathInventory, evil, agentCallOpts{Gzip: true})
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, wantErr, decodeJSON[protocol.ErrorResponse](t, rec).Error)

	stored, err := h.store.GetInventory(t.Context(), "lighthouse-geth-1")
	require.NoError(t, err)
	assert.Equal(t, inv.Hash, stored.Hash, "the inventory is untouched")

	// One audit entry for both rejections.
	audit := h.store.auditEntries()
	require.Len(t, audit, auditBefore+1)

	rejected := audit[len(audit)-1]
	assert.Equal(t, "host.certificate-rejected", rejected.Action)
	assert.Equal(t, "agent:lighthouse-geth-1", rejected.Actor)
	assert.Equal(t, "lighthouse-geth-1", rejected.Target)
	assert.Equal(t, uint64(0), rejected.Detail["generation"])
	assert.Equal(t, uint64(1), rejected.Detail["seenGeneration"])
	assert.Equal(t, identity.CertificateHash(leaked.creds.Certificate()), rejected.Detail["certHash"])

	// Counted like every other answer.
	body := h.get("/metrics").Body.String()
	assert.Contains(t, body, `upgradoor_agent_requests_total{code="401",endpoint="sync"} 1`+"\n")
	assert.Contains(t, body, `upgradoor_agent_requests_total{code="401",endpoint="inventory"} 1`+"\n")

	// The rotated host is not affected.
	require.Equal(t, http.StatusOK,
		h.agentPost(rotated, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)
	require.Equal(t, http.StatusOK,
		h.agentPost(rotated, protocol.PathInventory, inv, agentCallOpts{}).Code)
}

// The floor is not a pin: any certificate of the current or a higher
// generation that the signer issued is accepted.
func TestGenerationFloorIsNotAPin(t *testing.T) {
	h := newHarness(t, nil)

	require.Equal(t, http.StatusOK,
		h.agentPost(validatorAgentGeneration(t, 1), protocol.PathSync, syncRequest(),
			agentCallOpts{}).Code)

	first := h.store.host("lighthouse-geth-1").CertHash

	// The same generation, another certificate (the host got another role):
	// accepted, audited as changed.
	reissued := newAgent(t, identity.CertificateSpec{
		Hostname:   "lighthouse-geth-1",
		Generation: 1,
		Roles:      []string{"ethereum_node", "mev_relay"},
	})

	require.Equal(t, http.StatusOK,
		h.agentPost(reissued, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	host := h.store.host("lighthouse-geth-1")
	assert.NotEqual(t, first, host.CertHash)
	assert.Equal(t, uint64(1), host.Generation)
	assert.Equal(t, []string{"ethereum_node", "mev_relay"}, host.Roles)

	// A jump over a generation is accepted too, and raises the floor.
	require.Equal(t, http.StatusOK,
		h.agentPost(validatorAgentGeneration(t, 4), protocol.PathSync, syncRequest(),
			agentCallOpts{}).Code)
	assert.Equal(t, uint64(4), h.store.host("lighthouse-geth-1").Generation)

	rec := h.agentPost(reissued, protocol.PathSync, syncRequest(), agentCallOpts{})
	requireAgentError(t, rec, http.StatusUnauthorized,
		"certificate generation 1 is older than generation 4 already seen for this host")

	// Another host is not concerned by this host's generation.
	other := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})

	require.Equal(t, http.StatusOK,
		h.agentPost(other, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	actions := make([]string, 0, 5)
	for _, e := range h.store.auditEntries() {
		actions = append(actions, e.Action)
	}

	assert.Equal(t, []string{
		"host.registered", "host.certificate-changed", "host.certificate-changed",
		"host.certificate-rejected", "host.registered",
	}, actions)
}

// An agent that keeps trying with an old key must not flood the audit log:
// one entry per host and hour.
func TestRejectedGenerationAuditIsRateLimited(t *testing.T) {
	h := newHarness(t, nil)
	old := validatorAgentGeneration(t, 0)
	otherOld := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})

	require.Equal(t, http.StatusOK,
		h.agentPost(validatorAgentGeneration(t, 2), protocol.PathSync, syncRequest(),
			agentCallOpts{}).Code)
	require.Equal(t, http.StatusOK,
		h.agentPost(newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1", Generation: 1}),
			protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	rejections := func() map[string]int {
		out := make(map[string]int, 2)

		for _, e := range h.store.auditEntries() {
			if e.Action == "host.certificate-rejected" {
				out[e.Target]++
			}
		}

		return out
	}

	attempt := func(agent *testAgent) {
		t.Helper()

		h.clock.Advance(time.Second)
		require.Equal(t, http.StatusUnauthorized,
			h.agentPost(agent, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)
	}

	for range 20 {
		attempt(old)
	}

	assert.Equal(t, map[string]int{"lighthouse-geth-1": 1}, rejections())

	// The limit is per host.
	attempt(otherOld)
	attempt(otherOld)
	assert.Equal(t, map[string]int{"lighthouse-geth-1": 1, "prysm-besu-1": 1}, rejections())

	// Just short of an hour after the first entry: still quiet.
	h.clock.Advance(generationAuditInterval - 30*time.Second)
	attempt(old)
	assert.Equal(t, 1, rejections()["lighthouse-geth-1"])

	h.clock.Advance(time.Minute)
	attempt(old)
	attempt(old)
	assert.Equal(t, 2, rejections()["lighthouse-geth-1"])
}

// A host that the controller sees for the first time and that reports a
// controller hold (the database was lost) keeps the hold, and the audit trail
// says so.
func TestSyncAdoptsControllerHoldOfANewHost(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	req := syncRequest()
	req.Hold.Controller = true

	require.Equal(t, http.StatusOK, h.agentPost(agent, protocol.PathSync, req, agentCallOpts{}).Code)

	host := h.store.host("lighthouse-geth-1")
	assert.True(t, host.HoldWanted)
	assert.Equal(t, "held on the agent before this controller knew the host", host.HoldReason)

	audit := h.store.auditEntries()
	require.Len(t, audit, 1)
	assert.Equal(t, "host.registered", audit[0].Action)
	assert.Equal(t, true, audit[0].Detail["holdAdopted"])

	// A host without a controller hold: no such detail.
	other := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})
	local := syncRequest()
	local.Hold.Local = true

	require.Equal(t, http.StatusOK, h.agentPost(other, protocol.PathSync, local, agentCallOpts{}).Code)

	audit = h.store.auditEntries()
	require.Len(t, audit, 2)
	assert.Equal(t, "host.registered", audit[1].Action)
	assert.NotContains(t, audit[1].Detail, "holdAdopted")
	assert.False(t, h.store.host("prysm-besu-1").HoldWanted)
}

// A failed audit write does not fail the sync. The entries are logged and
// dropped: nothing is retried with a later sync.
func TestSyncSurvivesAFailedAuditWrite(t *testing.T) {
	h := newHarness(t, nil)

	h.store.mu.Lock()
	h.store.auditErr = errors.New("disk full")
	h.store.mu.Unlock()

	rec := h.agentPost(validatorAgent(t), protocol.PathSync, syncRequest(), agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, h.store.auditEntries())

	h.store.mu.Lock()
	h.store.auditErr = nil
	h.store.mu.Unlock()

	other := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})

	rec = h.agentPost(other, protocol.PathSync, syncRequest(), agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	audit := h.store.auditEntries()
	require.Len(t, audit, 1)
	assert.Equal(t, "prysm-besu-1", audit[0].Target)
}

// The agent may be gone by the time the sync is stored; the audit entry of a
// certificate change must be written all the same.
func TestSyncAuditsIdentityAfterClientWentAway(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	rotated := newAgent(t, identity.CertificateSpec{
		Hostname:   "lighthouse-geth-1",
		Generation: 1,
		Roles:      []string{"ethereum_node"},
	})

	body, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := h.agentRequest(rotated, protocol.PathSync, body, agentCallOpts{})
	h.serve(req.WithContext(ctx))

	audit := h.store.auditEntries()
	require.Len(t, audit, 2)
	assert.Equal(t, "host.certificate-changed", audit[1].Action)
	assert.Equal(t,
		map[string]any{"certHash": identity.CertificateHash(rotated.creds.Certificate())},
		audit[1].Detail["after"])
}

func TestSyncInternalErrors(t *testing.T) {
	tests := []struct {
		name string
		prep func(*harness)
	}{
		{name: "store fails", prep: func(h *harness) {
			h.store.setErr(errors.New("secret dsn /var/lib/upgradoor/db.sqlite is locked"))
		}},
		{name: "scheduler fails", prep: func(h *harness) {
			h.sched.set(func(f *fakeScheduler) {
				f.assignmentErr = errors.New("secret scheduler state")
			})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			tt.prep(h)

			rec := h.agentPost(validatorAgent(t), protocol.PathSync, syncRequest(),
				agentCallOpts{})
			requireAgentError(t, rec, http.StatusInternalServerError, "internal error")
			assert.NotContains(t, rec.Body.String(), "secret")
		})
	}
}

func TestAgentCanOnlyWriteItself(t *testing.T) {
	h := newHarness(t, nil)
	victim := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})
	attacker := validatorAgent(t)

	require.Equal(t, http.StatusOK,
		h.agentPost(victim, protocol.PathSync, syncRequest(), agentCallOpts{}).Code)

	victimBefore := *h.store.host("prysm-besu-1")

	// Every way a body could try to name another host.
	raw, err := json.Marshal(syncRequest())
	require.NoError(t, err)

	body := strings.Replace(string(raw), "{",
		`{"host":"prysm-besu-1","name":"prysm-besu-1","sub":"prysm-besu-1",`, 1)

	rec := h.serve(h.agentRequest(attacker, protocol.PathSync+"?host=prysm-besu-1",
		[]byte(body), agentCallOpts{ProofURL: testBaseURL + protocol.PathSync}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.NotNil(t, h.store.host("lighthouse-geth-1"), "stored under the certificate subject")
	assert.Equal(t, victimBefore, *h.store.host("prysm-besu-1"), "the other host is untouched")

	inv := inventoryFor(t, protocol.Package{Name: "evil", Version: "6.6.6", Arch: "amd64"})

	invRaw, err := json.Marshal(inv)
	require.NoError(t, err)

	invBody := strings.Replace(string(invRaw), "{", `{"host":"prysm-besu-1",`, 1)

	rec = h.serve(h.agentRequest(attacker, protocol.PathInventory, []byte(invBody),
		agentCallOpts{}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	_, err = h.store.GetInventory(t.Context(), "prysm-besu-1")
	require.Error(t, err, "no inventory was written for the other host")

	got, err := h.store.GetInventory(t.Context(), "lighthouse-geth-1")
	require.NoError(t, err)
	assert.Equal(t, inv.Hash, got.Hash)
}

func TestInventoryFlow(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	inv := inventoryFor(t,
		protocol.Package{Name: "docker-ce", Version: "5:28.3.3-1~debian.13~trixie", Arch: "amd64"},
		protocol.Package{Name: "openssl", Version: "3.5.1-1", Arch: "amd64"},
	)

	// Before the first sync the host does not exist.
	rec := h.agentPost(agent, protocol.PathInventory, inv, agentCallOpts{Gzip: true})
	requireAgentError(t, rec, http.StatusConflict, "sync first")

	// First sync: the controller asks for the inventory.
	req := syncRequest()
	req.InventoryHash = inv.Hash

	rec = h.agentPost(agent, protocol.PathSync, req, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, decodeJSON[protocol.SyncResponse](t, rec).WantInventory)

	// Upload, gzip as the agent does.
	rec = h.agentPost(agent, protocol.PathInventory, inv, agentCallOpts{Gzip: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	answer := decodeJSON[protocol.InventoryResponse](t, rec)
	assert.Equal(t, inv.Hash, answer.Hash)
	assert.True(t, answer.ServerTime.Equal(testEpoch))

	stored, err := h.store.GetInventory(t.Context(), agent.name)
	require.NoError(t, err)
	assert.Equal(t, inv.Packages, stored.Packages)

	// Next sync: nothing more to ask.
	rec = h.agentPost(agent, protocol.PathSync, req, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, decodeJSON[protocol.SyncResponse](t, rec).WantInventory)

	// The inventory changes: asked again.
	req.InventoryHash = "another-hash"

	rec = h.agentPost(agent, protocol.PathSync, req, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, decodeJSON[protocol.SyncResponse](t, rec).WantInventory)

	// A plain (uncompressed) upload works as well.
	rec = h.agentPost(agent, protocol.PathInventory, inv, agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAgentConcurrencyLimit(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	// Fill every inventory slot, as uploads in flight would.
	for range maxConcurrentInventories {
		h.srv.inventorySlots <- struct{}{}
	}

	rec := h.agentPost(agent, protocol.PathInventory, inventoryFor(t), agentCallOpts{})
	requireAgentError(t, rec, http.StatusServiceUnavailable, "in flight")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	// Syncs have their own slots.
	rec = h.agentPost(agent, protocol.PathSync, syncRequest(), agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	<-h.srv.inventorySlots

	rec = h.agentPost(agent, protocol.PathInventory, inventoryFor(t), agentCallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// signedCall serves one agent request and returns the answer with the id of
// the request's proof.
func signedCall(
	t *testing.T, h *harness, a *testAgent, path string, v any,
) (rec *httptest.ResponseRecorder, proofID string) {
	t.Helper()

	body, err := json.Marshal(v)
	require.NoError(t, err)

	req := h.agentRequest(a, path, body, agentCallOpts{Gzip: true})

	parts := strings.Split(req.Header.Get(identity.HeaderProof), ".")
	require.Len(t, parts, 3)

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)

	var claims identity.ProofClaims
	require.NoError(t, json.Unmarshal(raw, &claims))

	return h.serve(req), claims.ID
}

func TestAgentAnswersAreSigned(t *testing.T) {
	key, err := identity.DeriveControllerKey([]byte("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"), testNetwork, 0)
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, testNetwork)
	require.NoError(t, err)

	verifierFor := func(t *testing.T, host string) *identity.AnswerVerifier {
		t.Helper()

		v, verr := identity.NewAnswerVerifier(
			[]*mldsa.PublicKey{identity.PublicKey(key)}, testNetwork, host)
		require.NoError(t, verr)

		return v
	}

	h := newHarness(t, func(o *Options) { o.AnswerSigner = signer })
	agent := validatorAgent(t)
	answers := verifierFor(t, agent.name)

	t.Run("sync and inventory answers verify for the host and the request", func(t *testing.T) {
		inv := inventoryFor(t, protocol.Package{Name: "openssl", Version: "3.5.1-1", Arch: "amd64"})

		req := syncRequest()
		req.InventoryHash = inv.Hash

		rec, id := signedCall(t, h, agent, protocol.PathSync, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		body, err := answers.Verify(id, rec.Body.Bytes())
		require.NoError(t, err)

		var resp protocol.SyncResponse
		require.NoError(t, json.Unmarshal(body, &resp))
		assert.True(t, resp.ServerTime.Equal(testEpoch))
		assert.True(t, resp.WantInventory)

		// Bound to the host and to the request.
		_, err = verifierFor(t, "prysm-besu-1").Verify(id, rec.Body.Bytes())
		require.ErrorIs(t, err, identity.ErrBadAnswer)

		invRec, invID := signedCall(t, h, agent, protocol.PathInventory, inv)
		require.Equal(t, http.StatusOK, invRec.Code, invRec.Body.String())

		_, err = answers.Verify(invID, rec.Body.Bytes())
		require.ErrorIs(t, err, identity.ErrBadAnswer, "the sync answer answers no other request")

		body, err = answers.Verify(invID, invRec.Body.Bytes())
		require.NoError(t, err)

		var invResp protocol.InventoryResponse
		require.NoError(t, json.Unmarshal(body, &invResp))
		assert.Equal(t, inv.Hash, invResp.Hash)
	})

	t.Run("errors are not signed", func(t *testing.T) {
		req := syncRequest()
		req.ProtocolVersion = protocol.Version + 1

		rec, id := signedCall(t, h, agent, protocol.PathSync, req)
		requireAgentError(t, rec, http.StatusBadRequest, "protocol version")

		_, err := answers.Verify(id, rec.Body.Bytes())
		require.ErrorIs(t, err, identity.ErrBadAnswer)
	})
}

// The validators counted for a host follow what the host reports, sync by
// sync, and nothing in its certificate.
func TestSyncCountsTheReportedValidators(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	sync := func(t *testing.T, a *testAgent, v *protocol.Validators) *fleet.Host {
		t.Helper()

		req := syncRequest()
		req.Validators = v

		rec := h.agentPost(a, protocol.PathSync, req, agentCallOpts{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Past the limit on audit entries about one host's range.
		h.clock.Advance(2 * validatorAuditInterval)

		return h.store.host(a.name)
	}

	actions := func() []string {
		out := make([]string, 0, 4)
		for _, e := range h.store.auditEntries() {
			out = append(out, e.Action)
		}

		return out
	}

	host := sync(t, agent, knownValidators(0, 100))
	assert.Equal(t, &identity.ValidatorRange{Start: 0, End: 100}, host.Validators)
	assert.Equal(t, []string{"host.registered"}, actions())
	assert.Equal(t, uint64(100), h.store.auditEntries()[0].Detail["validators"])

	// The range changes while the controller runs: keys were moved.
	host = sync(t, agent, knownValidators(50, 250))
	assert.Equal(t, uint64(200), host.Weight())
	assert.Equal(t, []string{"host.registered", "host.validators-changed"}, actions())

	changed := h.store.auditEntries()[1]
	assert.Equal(t, map[string]any{"start": uint64(0), "end": uint64(100)}, changed.Detail["before"])
	assert.Equal(t, map[string]any{"start": uint64(50), "end": uint64(250)}, changed.Detail["counted"])

	// The same range again changes nothing and audits nothing.
	sync(t, agent, knownValidators(50, 250))
	assert.Len(t, actions(), 2)

	// A host that does not know its range (its keys are being reshuffled, or
	// the marker is damaged) keeps what was counted: it is not "none".
	for _, unknown := range []*protocol.Validators{
		{Source: protocol.ValidatorsFromMarker, State: protocol.ValidatorsMissing},
		{Source: protocol.ValidatorsFromMarker, State: protocol.ValidatorsInvalid, Error: "garbage"},
	} {
		host = sync(t, agent, unknown)
		assert.Equal(t, &identity.ValidatorRange{Start: 50, End: 250}, host.Validators)
		assert.Equal(t, unknown.State, host.Status.Validators.State)
	}

	// A range out of bounds is not counted: it is stored as refused, and the
	// host counts as one that does not know its range.
	for _, bad := range []*protocol.Validators{
		knownValidators(300, 200),
		knownValidators(0, protocol.MaxHostValidators+1),
		knownValidators(protocol.MaxValidatorIndex, protocol.MaxValidatorIndex+10),
		{Start: 0, End: 10, Source: protocol.ValidatorsFromMarker, State: "certain"},
	} {
		host = sync(t, agent, bad)
		assert.Equal(t, &identity.ValidatorRange{Start: 50, End: 250}, host.Validators)
		assert.Equal(t, protocol.ValidatorsInvalid, host.Status.Validators.State)
		assert.Contains(t, host.Status.Validators.Error, "refused by the controller")
		assert.Zero(t, host.Status.Validators.End, "a refused range is not shown as one")
	}

	assert.Len(t, actions(), 2, "nothing changed meanwhile")

	// An empty range, and no range at all (also an agent built before the
	// field), say that the host has no validators.
	host = sync(t, agent, knownValidators(0, 0))
	assert.Nil(t, host.Validators)

	host = sync(t, agent, knownValidators(50, 250))
	assert.Equal(t, uint64(200), host.Weight())

	host = sync(t, agent, nil)
	assert.Nil(t, host.Validators)
	assert.Zero(t, host.Weight())
}

// Two hosts that report the same validators: no validator is counted twice,
// the one who held it first keeps it, and the overlap is shown and audited
// once for as long as it lasts.
func TestOverlappingValidatorRangesAreCountedOnce(t *testing.T) {
	h := newHarness(t, nil)
	first := validatorAgent(t)
	second := newAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})

	sync := func(t *testing.T, a *testAgent, start, end uint64) {
		t.Helper()

		req := syncRequest()
		req.Validators = knownValidators(start, end)

		rec := h.agentPost(a, protocol.PathSync, req, agentCallOpts{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		h.clock.Advance(2 * validatorAuditInterval)
	}

	overlapAudits := func() []fleet.AuditEntry {
		out := make([]fleet.AuditEntry, 0, 2)

		for _, e := range h.store.auditEntries() {
			if e.Action == "host.validators-overlap" {
				out = append(out, e)
			}
		}

		return out
	}

	total := func() uint64 {
		return h.store.host(first.name).Weight() + h.store.host(second.name).Weight()
	}

	sync(t, first, 0, 100)
	sync(t, second, 50, 150)

	assert.Equal(t, &identity.ValidatorRange{Start: 0, End: 100}, h.store.host(first.name).Validators)
	assert.Equal(t, &identity.ValidatorRange{Start: 100, End: 150}, h.store.host(second.name).Validators,
		"the validators 50 to 99 stay with the host that had them")
	assert.Equal(t, uint64(150), total(), "150 validators exist, not 200")

	// Said once, however long both keep reporting it.
	for range 3 {
		sync(t, first, 0, 100)
		sync(t, second, 50, 150)
	}

	// It is audited for the host that reports validators counted for
	// another one; the host that holds them loses nothing.
	audits := overlapAudits()
	require.Len(t, audits, 1)
	assert.Equal(t, second.name, audits[0].Target)
	assert.Equal(t, []string{first.name}, audits[0].Detail["with"])
	assert.Equal(t, map[string]any{"start": uint64(100), "end": uint64(150)}, audits[0].Detail["counted"])
	assert.Equal(t, uint64(150), total())

	// Both host views show what is reported, what is counted and with whom
	// the report overlaps.
	detail := decodeJSON[HostDetail](t, h.get("/api/v1/hosts/"+second.name))
	assert.Equal(t, uint64(50), detail.Validators)
	assert.Equal(t, knownValidators(50, 150), detail.ValidatorReport)
	assert.Equal(t, uint64(100), *detail.ValidatorStart)
	assert.Equal(t, uint64(150), *detail.ValidatorEnd)
	assert.Equal(t, []string{first.name}, detail.ValidatorOverlaps)

	detail = decodeJSON[HostDetail](t, h.get("/api/v1/hosts/"+first.name))
	assert.Equal(t, []string{second.name}, detail.ValidatorOverlaps)

	// The first host gives the validators up (the reshuffle is through): the
	// second one now counts all it reports, and the episode is over.
	sync(t, first, 0, 50)
	sync(t, second, 50, 150)

	assert.Equal(t, uint64(50), h.store.host(first.name).Weight())
	assert.Equal(t, uint64(100), h.store.host(second.name).Weight())
	assert.Len(t, overlapAudits(), 1)
	assert.Empty(t, decodeJSON[HostDetail](t, h.get("/api/v1/hosts/"+second.name)).ValidatorOverlaps)

	// A host cannot take validators from another by reporting them: a range
	// over everything counts only what nobody holds.
	sync(t, second, 0, 1000)
	assert.Equal(t, uint64(50), h.store.host(first.name).Weight())
	assert.Equal(t, &identity.ValidatorRange{Start: 50, End: 1000}, h.store.host(second.name).Validators)
	assert.Len(t, overlapAudits(), 2, "a new overlap is a new episode")
}
