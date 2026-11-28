package agentapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func validatorAgent(t *testing.T) *controllertest.Agent {
	t.Helper()

	return controllertest.NewAgent(t, identity.CertificateSpec{
		Hostname: "lighthouse-geth-1",
	})
}

// requireAgentError checks a protocol.ErrorResponse answer.
func requireAgentError(t *testing.T, rec *httptest.ResponseRecorder, status int, contains string) {
	t.Helper()

	require.Equal(t, status, rec.Code, rec.Body.String())

	body := controllertest.DecodeJSON[protocol.ErrorResponse](t, rec)
	assert.Contains(t, body.Error, contains)
	assert.True(t, body.ServerTime.Equal(controllertest.Epoch), "error answers carry the controller time")
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
			h.store.Ranges.Ranges = []fleet.HostRange{{
				Host: agent.Name, ValidatorRange: identity.ValidatorRange{Start: 0, End: 100},
			}}

			rec := h.agentPost(agent, protocol.PathSync, controllertest.SyncRequest(),
				controllertest.CallOpts{Gzip: gz})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			resp := controllertest.DecodeJSON[protocol.SyncResponse](t, rec)
			assert.True(t, resp.ServerTime.Equal(controllertest.Epoch))
			assert.False(t, resp.WantInventory, "no inventory on either side")
			assert.InDelta(t, idleMinInterval.Milliseconds(), resp.NextSyncMillis,
				float64(idleMinInterval.Milliseconds())*pacingJitter, "an idle host of a small fleet")
			assert.Nil(t, resp.Lease)
			assert.Empty(t, resp.Directives)
			assert.Nil(t, resp.Queue)

			host := h.store.Host(agent.Name)
			require.NotNil(t, host)
			assert.Equal(t, "agent-1.0.0", host.Status.AgentVersion)
			assert.Equal(t, uint64(100), host.Weight())
			assert.Equal(t, 1, h.sched.NudgeCount())
		})
	}
}

func TestAgentAuthentication(t *testing.T) {
	body, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	hostKey, err := identity.GenerateHostKey()
	require.NoError(t, err)

	otherSeedCert, err := identity.IssueCertificate(
		[]byte("another seed that is at least 32 bytes long ......"), identity.PublicKey(hostKey),
		identity.CertificateSpec{Network: controllertest.Network, Hostname: "lighthouse-geth-1"})
	require.NoError(t, err)

	// A certificate for another network: the signature is good, the network
	// claim is not.
	signer, err := identity.DeriveSignerKey([]byte(controllertest.Seed))
	require.NoError(t, err)

	wrongNetCert, err := identity.SignCertificate(signer, identity.PublicKey(hostKey),
		identity.CertificateSpec{Network: "other-devnet-1", Hostname: "lighthouse-geth-1"})
	require.NoError(t, err)

	// The same host name, an agent of another network.
	foreign := controllertest.NewAgent(t, identity.CertificateSpec{
		Network: "other-devnet-1", Hostname: "lighthouse-geth-1",
	})

	tests := []struct {
		name     string
		opts     func(*Options)
		agent    *controllertest.Agent
		call     controllertest.CallOpts
		contains string
	}{
		{
			name: "missing headers",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Del(identity.HeaderCert)
				r.Header.Del(identity.HeaderProof)
			}},
			contains: "missing",
		},
		{
			name: "missing proof",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Del(identity.HeaderProof)
			}},
			contains: "missing",
		},
		{
			name: "garbage certificate",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, "not.a.certificate")
			}},
			contains: "bad certificate",
		},
		{
			name: "certificate from another signer",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, otherSeedCert)
			}},
			contains: "bad certificate",
		},
		{
			name: "certificate for another network, good signature",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, wrongNetCert)
			}},
			contains: "another network",
		},
		{
			name:     "identity of another network",
			agent:    foreign,
			contains: "another network",
		},
		{
			name: "proof signed by another host",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				other := controllertest.NewAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})
				r.Header.Set(identity.HeaderCert, other.Creds.Certificate())
			}},
			contains: "bad proof",
		},
		{
			name:     "proof for another URL",
			call:     controllertest.CallOpts{ProofURL: "https://upgradoor.evil.example" + protocol.PathSync},
			contains: "does not match",
		},
		{
			name:     "proof for the inventory endpoint",
			call:     controllertest.CallOpts{ProofURL: controllertest.BaseURL + protocol.PathInventory},
			contains: "does not match",
		},
		{
			name:     "proof for plain http is refused by default",
			call:     controllertest.CallOpts{ProofURL: "http://" + controllertest.ControllerHost + protocol.PathSync},
			contains: "does not match",
		},
		{
			name:     "expired proof",
			call:     controllertest.CallOpts{ProofTime: controllertest.Epoch.Add(-10 * time.Minute)},
			contains: "time window",
		},
		{
			name:     "proof from the future",
			call:     controllertest.CallOpts{ProofTime: controllertest.Epoch.Add(10 * time.Minute)},
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

			assert.Nil(t, h.store.Host("lighthouse-geth-1"), "nothing may be stored")
			assert.Zero(t, h.sched.NudgeCount())
		})
	}
}

func TestAgentBodyTamper(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	body, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	signed := h.agentRequest(agent, protocol.PathSync, body, controllertest.CallOpts{})

	tampered := bytes.Replace(body, []byte("agent-1.0.0"), []byte("agent-6.6.6"), 1)
	req := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+protocol.PathSync,
		bytes.NewReader(tampered))
	req.Header = signed.Header.Clone()

	requireAgentError(t, h.serve(req), http.StatusUnauthorized, "body hash")
	assert.Nil(t, h.store.Host(agent.Name))
}

func TestAgentGzipTamper(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	body, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	// The proof covers the plain bytes; the sender then claims they are
	// gzip. The proof still matches the wire, so this is a decoding error
	// after authentication, not an identity error.
	req := h.agentRequest(agent, protocol.PathSync, body, controllertest.CallOpts{
		Mutate: func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
	})
	requireAgentError(t, h.serve(req), http.StatusBadRequest, "invalid gzip")

	// A proof over the plain body does not cover a compressed wire body.
	signed := h.agentRequest(agent, protocol.PathSync, body, controllertest.CallOpts{})
	req = httptest.NewRequest(http.MethodPost, controllertest.BaseURL+protocol.PathSync,
		bytes.NewReader(controllertest.Gzip(t, body)))
	req.Header = signed.Header.Clone()
	req.Header.Set("Content-Encoding", "gzip")
	requireAgentError(t, h.serve(req), http.StatusUnauthorized, "body hash")
}

func TestAgentReplay(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	body, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	first := h.agentRequest(agent, protocol.PathSync, body, controllertest.CallOpts{})
	headers := first.Header.Clone()

	require.Equal(t, http.StatusOK, h.serve(first).Code)

	replay := httptest.NewRequest(http.MethodPost, controllertest.BaseURL+protocol.PathSync,
		bytes.NewReader(body))
	replay.Header = headers

	requireAgentError(t, h.serve(replay), http.StatusUnauthorized, "replayed")
	assert.Equal(t, 1, h.sched.NudgeCount())
}

func TestAgentProofURLs(t *testing.T) {
	body, err := json.Marshal(controllertest.SyncRequest())
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
			proofURL: controllertest.BaseURL + protocol.PathSync,
			status:   http.StatusOK,
		},
		{
			name:     "derived http when allowed",
			opts:     func(o *Options) { o.AllowInsecureAgentHTTP = true },
			proofURL: "http://" + controllertest.ControllerHost + protocol.PathSync,
			status:   http.StatusOK,
		},
		{
			name:     "derived https still accepted when http is allowed",
			opts:     func(o *Options) { o.AllowInsecureAgentHTTP = true },
			proofURL: controllertest.BaseURL + protocol.PathSync,
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
			proofURL: controllertest.BaseURL + protocol.PathSync,
			host:     "upgradoor.other.example",
			status:   http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.opts)
			agent := validatorAgent(t)

			req := h.agentRequest(agent, protocol.PathSync, body, controllertest.CallOpts{
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

			req := h.agentRequest(agent, tt.path, body, controllertest.CallOpts{
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
			assert.Nil(t, h.store.Host(agent.Name))
		})
	}
}

func TestAgentBodyAtLimitIsRead(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	raw, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	// Pad a valid request with whitespace up to exactly the limit.
	body := append(raw, bytes.Repeat([]byte(" "), protocol.MaxSyncBody-len(raw))...)
	require.Len(t, body, protocol.MaxSyncBody)

	rec := h.serve(h.agentRequest(agent, protocol.PathSync, body, controllertest.CallOpts{}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAgentRequestErrors(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		body     string
		call     controllertest.CallOpts
		status   int
		contains string
	}{
		{
			name: "sync: not json", path: protocol.PathSync, body: "{nope",
			status: http.StatusBadRequest, contains: "invalid sync request",
		},
		{
			name: "sync: unsupported content encoding", path: protocol.PathSync, body: "{}",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
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
			assert.Nil(t, h.store.Host(agent.Name))
			assert.Zero(t, h.sched.NudgeCount())
		})
	}
}

// An agent outside MinVersion..Version is recorded, so the host shows with
// its versions, and is answered an error instead of an assignment.
func TestSyncProtocolRange(t *testing.T) {
	tests := []struct {
		name    string
		version int
		served  bool
	}{
		{name: "oldest", version: protocol.MinVersion, served: true},
		{name: "newest", version: protocol.Version, served: true},
		{name: "older", version: protocol.MinVersion - 1},
		{name: "newer", version: protocol.Version + 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			agent := validatorAgent(t)

			h.sched.Set(func(f *controllertest.Scheduler) {
				f.Assignments[agent.Name] = &scheduler.Assignment{
					Directives: []protocol.Directive{{ID: "d-1", Kind: protocol.DirectiveReboot}},
				}
			})

			req := controllertest.SyncRequest()
			req.ProtocolVersion = tt.version
			req.AgentVersion = "v9"

			rec := h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})

			detail := h.detail(agent.Name)
			assert.Equal(t, "v9", detail.AgentVersion)
			assert.Equal(t, tt.version, detail.AgentProtocol)
			assert.Equal(t, controllertest.Epoch, detail.LastSeen)
			assert.Equal(t, 1, h.sched.NudgeCount())

			if tt.served {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Len(t, controllertest.DecodeJSON[protocol.SyncResponse](t, rec).Directives, 1)
				assert.Empty(t, detail.Incompatible)

				return
			}

			want := fmt.Sprintf("agent protocol %d not supported (controller supports %d..%d)",
				tt.version, protocol.MinVersion, protocol.Version)
			requireAgentError(t, rec, http.StatusBadRequest, want)
			assert.Equal(t, want, detail.Incompatible)
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
	body, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	hostKey, err := identity.GenerateHostKey()
	require.NoError(t, err)

	otherSeedCert, err := identity.IssueCertificate(
		[]byte("another seed that is at least 32 bytes long ......"), identity.PublicKey(hostKey),
		identity.CertificateSpec{Network: controllertest.Network, Hostname: "lighthouse-geth-1"})
	require.NoError(t, err)

	tests := []struct {
		name     string
		call     controllertest.CallOpts
		contains string
	}{
		{
			name: "garbage headers",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, "x")
				r.Header.Set(identity.HeaderProof, "x")
			}},
			contains: "bad certificate",
		},
		{
			name: "certificate from another signer",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderCert, otherSeedCert)
			}},
			contains: "bad certificate",
		},
		{
			// A certificate is not secret enough to hold a slot: it travels
			// in a header of every request.
			name: "valid certificate with a garbage proof",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				r.Header.Set(identity.HeaderProof, "x")
			}},
			contains: "bad proof",
		},
		{
			name: "valid certificate with another host's proof",
			call: controllertest.CallOpts{Mutate: func(r *http.Request) {
				other := controllertest.NewAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})
				r.Header.Set(identity.HeaderCert, other.Creds.Certificate())
			}},
			contains: "bad proof",
		},
		{
			name:     "valid certificate with an expired proof",
			call:     controllertest.CallOpts{ProofTime: controllertest.Epoch.Add(-10 * time.Minute)},
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
	rec := h.serve(h.agentRequest(agent, protocol.PathSync, nil, controllertest.CallOpts{}))
	requireAgentError(t, rec, http.StatusBadRequest, "invalid sync request")

	rec = h.agentPost(agent, protocol.PathSync, controllertest.SyncRequest(), controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A verified host that trickles its body loses its slot after the read
// timeout, whatever timeouts the http.Server has.
func TestAgentSlowBodyReleasesSlot(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.inventoryReadTimeout = 200 * time.Millisecond

	// No server timeouts at all: the handler has to bound the read itself.
	srv := httptest.NewServer(h.handler)
	t.Cleanup(srv.Close)

	agent := validatorAgent(t)

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, controllertest.SyncRequest(), controllertest.CallOpts{}).Code)

	body, err := json.Marshal(controllertest.InventoryFor(t))
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
		signed := h.agentRequest(agent, protocol.PathInventory, body, controllertest.CallOpts{})

		pr, pw := io.Pipe()
		writers = append(writers, pw)

		req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodPost,
			srv.URL+protocol.PathInventory, pr)
		require.NoError(t, reqErr)

		req.Host = controllertest.ControllerHost
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
	signed := h.agentRequest(agent, protocol.PathInventory, body, controllertest.CallOpts{})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+protocol.PathInventory, bytes.NewReader(body))
	require.NoError(t, err)

	req.Host = controllertest.ControllerHost
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
		GrantedAt: controllertest.Epoch.Add(-5 * time.Minute),
		ExpiresAt: controllertest.Epoch.Add(10 * time.Minute),
		// A stale value: the response must carry the time left now.
		TTLSeconds: 1800,
		Needs:      []protocol.Need{need},
	}

	h.sched.Set(func(f *controllertest.Scheduler) {
		f.Assignments[agent.Name] = &scheduler.Assignment{
			Lease: lease,
			Directives: []protocol.Directive{
				{ID: "d-1", Kind: protocol.DirectiveReboot},
				{ID: "d-2", Kind: protocol.DirectiveRoll, Container: "geth", Reason: "manual"},
			},
			Queue: &protocol.Queue{Position: 3, Reason: "validators down: 8% now, 12% with this host, limit 10%"},
		}
	})

	req := controllertest.SyncRequest()
	req.InventoryHash = "hash-a"
	req.Needs = []protocol.Need{need}

	rec := h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := controllertest.DecodeJSON[protocol.SyncResponse](t, rec)
	assert.True(t, resp.WantInventory, "the controller holds no inventory")
	require.NotNil(t, resp.Lease)
	assert.Equal(t, "lease-1", resp.Lease.ID)
	assert.Equal(t, int64(600), resp.Lease.TTLSeconds)
	assert.Equal(t, []protocol.Need{need}, resp.Lease.Needs)
	assert.Equal(t, int64(1800), lease.TTLSeconds, "the scheduler's lease is not modified")
	require.Len(t, resp.Directives, 2)
	assert.Equal(t, protocol.DirectiveRoll, resp.Directives[1].Kind)
	assert.Equal(t, &protocol.Queue{Position: 3, Reason: "validators down: 8% now, 12% with this host, limit 10%"}, resp.Queue)

	// An expired lease is still restated, with no time left.
	h.clock.Advance(20 * time.Minute)

	rec = h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp = controllertest.DecodeJSON[protocol.SyncResponse](t, rec)
	require.NotNil(t, resp.Lease)
	assert.Zero(t, resp.Lease.TTLSeconds)
	assert.Equal(t, 2, h.sched.NudgeCount())
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

			body, err := json.Marshal(controllertest.SyncRequest())
			require.NoError(t, err)

			req := h.agentRequest(agent, protocol.PathSync, body, controllertest.CallOpts{
				Mutate: func(r *http.Request) {
					for _, v := range tt.headers {
						r.Header.Add("X-Forwarded-For", v)
					}
				},
			})

			require.Equal(t, http.StatusOK, h.serve(req).Code)
			assert.Equal(t, tt.want, h.store.Host(agent.Name).RemoteAddr)
		})
	}
}

func TestSyncAuditsIdentity(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	req := controllertest.SyncRequest()
	req.Roles = []string{"ethereum_node"}

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{}).Code)

	audit := h.store.AuditEntries()
	require.Len(t, audit, 1)
	assert.Equal(t, "host.registered", audit[0].Action)
	assert.Equal(t, []string{"ethereum_node"}, audit[0].Detail["roles"])
	assert.Equal(t, "agent:lighthouse-geth-1", audit[0].Actor)
	assert.Equal(t, "lighthouse-geth-1", audit[0].Target)
	assert.True(t, audit[0].Time.Equal(controllertest.Epoch))

	firstHash := identity.CertificateHash(agent.Creds.Certificate())
	assert.Equal(t, firstHash, audit[0].Detail["certHash"])

	// The same certificate again: nothing new.
	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{}).Code)
	require.Len(t, h.store.AuditEntries(), 1)

	// Another certificate of the host: accepted, and audited with both hashes.
	// The reported roles are stored sorted, without duplicates and invalid names.
	reissued := controllertest.NewAgent(t, identity.CertificateSpec{Hostname: "lighthouse-geth-1"})
	req.Roles = []string{"mev_relay", "ethereum_node", "mev_relay", "not a role"}

	require.Equal(t, http.StatusOK,
		h.agentPost(reissued, protocol.PathSync, req, controllertest.CallOpts{}).Code)

	audit = h.store.AuditEntries()
	require.Len(t, audit, 2)

	changed := audit[1]
	secondHash := identity.CertificateHash(reissued.Creds.Certificate())

	assert.Equal(t, "host.certificate-changed", changed.Action)
	assert.Equal(t, "agent:lighthouse-geth-1", changed.Actor)
	assert.Equal(t, "lighthouse-geth-1", changed.Target)
	assert.Equal(t, map[string]any{"certHash": firstHash}, changed.Detail["before"])
	assert.Equal(t, map[string]any{"certHash": secondHash}, changed.Detail["after"])
	assert.NotEqual(t, firstHash, secondHash)
	assert.Equal(t, secondHash, h.store.Host("lighthouse-geth-1").CertHash)
	assert.Equal(t, []string{"ethereum_node", "mev_relay"}, h.store.Host("lighthouse-geth-1").Roles)
}

// The holds a host reports are what it holds: the controller keeps them as
// reported, one per valid holder in order, and adds none of its own.
func TestSyncKeepsTheHoldsTheHostReports(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	req := controllertest.SyncRequest()
	req.Hold.Holds = []protocol.HostHold{
		{Holder: "geth", Reason: "sync"},
		{Holder: "Bad Team"},
		{Holder: fleet.HolderStaff, By: "alice", Reason: "fork"},
		{Holder: "geth", Reason: "again"},
		{Holder: protocol.HolderHost, By: "pk"},
	}

	require.Equal(t, http.StatusOK, h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{}).Code)

	host := h.store.Host("lighthouse-geth-1")
	assert.Equal(t, []protocol.HostHold{
		{Holder: protocol.HolderHost, By: "pk"},
		{Holder: fleet.HolderStaff, By: "alice", Reason: "fork"},
		{Holder: "geth", Reason: "sync"},
	}, host.Holds())
	assert.Equal(t, "held by the host, staff, geth", host.HeldReason())

	audit := h.store.AuditEntries()
	require.Len(t, audit, 1)
	assert.Equal(t, "host.registered", audit[0].Action)
	assert.NotContains(t, audit[0].Detail, "holdAdopted")
}

// A failed audit write does not fail the sync. The entries are logged and
// dropped: nothing is retried with a later sync.
func TestSyncSurvivesAFailedAuditWrite(t *testing.T) {
	h := newHarness(t, nil)

	h.store.Lock()
	h.store.AuditErr = errors.New("disk full")
	h.store.Unlock()

	rec := h.agentPost(validatorAgent(t), protocol.PathSync, controllertest.SyncRequest(), controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, h.store.AuditEntries())

	h.store.Lock()
	h.store.AuditErr = nil
	h.store.Unlock()

	other := controllertest.NewAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})

	rec = h.agentPost(other, protocol.PathSync, controllertest.SyncRequest(), controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	audit := h.store.AuditEntries()
	require.Len(t, audit, 1)
	assert.Equal(t, "prysm-besu-1", audit[0].Target)
}

// The agent may be gone by the time the sync is stored; the audit entry of a
// certificate change must be written all the same.
func TestSyncAuditsIdentityAfterClientWentAway(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	require.Equal(t, http.StatusOK,
		h.agentPost(agent, protocol.PathSync, controllertest.SyncRequest(), controllertest.CallOpts{}).Code)

	reissued := controllertest.NewAgent(t, identity.CertificateSpec{Hostname: "lighthouse-geth-1"})

	body, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := h.agentRequest(reissued, protocol.PathSync, body, controllertest.CallOpts{})
	h.serve(req.WithContext(ctx))

	audit := h.store.AuditEntries()
	require.Len(t, audit, 2)
	assert.Equal(t, "host.certificate-changed", audit[1].Action)
	assert.Equal(t,
		map[string]any{"certHash": identity.CertificateHash(reissued.Creds.Certificate())},
		audit[1].Detail["after"])
}

func TestSyncInternalErrors(t *testing.T) {
	tests := []struct {
		name string
		prep func(*harness)
	}{
		{name: "store fails", prep: func(h *harness) {
			h.store.SetErr(errors.New("secret dsn /var/lib/upgradoor/db.sqlite is locked"))
		}},
		{name: "scheduler fails", prep: func(h *harness) {
			h.sched.Set(func(f *controllertest.Scheduler) {
				f.AssignmentErr = errors.New("secret scheduler state")
			})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, nil)
			tt.prep(h)

			rec := h.agentPost(validatorAgent(t), protocol.PathSync, controllertest.SyncRequest(),
				controllertest.CallOpts{})
			requireAgentError(t, rec, http.StatusInternalServerError, "internal error")
			assert.NotContains(t, rec.Body.String(), "secret")
		})
	}
}

func TestAgentCanOnlyWriteItself(t *testing.T) {
	h := newHarness(t, nil)
	victim := controllertest.NewAgent(t, identity.CertificateSpec{Hostname: "prysm-besu-1"})
	attacker := validatorAgent(t)

	require.Equal(t, http.StatusOK,
		h.agentPost(victim, protocol.PathSync, controllertest.SyncRequest(), controllertest.CallOpts{}).Code)

	victimBefore := *h.store.Host("prysm-besu-1")

	// Every way a body could try to name another host.
	raw, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	body := strings.Replace(string(raw), "{",
		`{"host":"prysm-besu-1","name":"prysm-besu-1","sub":"prysm-besu-1",`, 1)

	rec := h.serve(h.agentRequest(attacker, protocol.PathSync+"?host=prysm-besu-1",
		[]byte(body), controllertest.CallOpts{ProofURL: controllertest.BaseURL + protocol.PathSync}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.NotNil(t, h.store.Host("lighthouse-geth-1"), "stored under the certificate subject")
	assert.Equal(t, victimBefore, *h.store.Host("prysm-besu-1"), "the other host is untouched")

	inv := controllertest.InventoryFor(t, protocol.Package{Name: "evil", Version: "6.6.6", Arch: "amd64"})

	invRaw, err := json.Marshal(inv)
	require.NoError(t, err)

	invBody := strings.Replace(string(invRaw), "{", `{"host":"prysm-besu-1",`, 1)

	rec = h.serve(h.agentRequest(attacker, protocol.PathInventory, []byte(invBody),
		controllertest.CallOpts{}))
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

	inv := controllertest.InventoryFor(t,
		protocol.Package{Name: "docker-ce", Version: "5:28.3.3-1~debian.13~trixie", Arch: "amd64"},
		protocol.Package{Name: "openssl", Version: "3.5.1-1", Arch: "amd64"},
	)

	// Before the first sync the host does not exist.
	rec := h.agentPost(agent, protocol.PathInventory, inv, controllertest.CallOpts{Gzip: true})
	requireAgentError(t, rec, http.StatusConflict, "sync first")

	// First sync: the controller asks for the inventory.
	req := controllertest.SyncRequest()
	req.InventoryHash = inv.Hash

	rec = h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, controllertest.DecodeJSON[protocol.SyncResponse](t, rec).WantInventory)

	// Upload, gzip as the agent does.
	rec = h.agentPost(agent, protocol.PathInventory, inv, controllertest.CallOpts{Gzip: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	answer := controllertest.DecodeJSON[protocol.InventoryResponse](t, rec)
	assert.Equal(t, inv.Hash, answer.Hash)
	assert.True(t, answer.ServerTime.Equal(controllertest.Epoch))

	stored, err := h.store.GetInventory(t.Context(), agent.Name)
	require.NoError(t, err)
	assert.Equal(t, inv.Packages, stored.Packages)

	// Next sync: nothing more to ask.
	rec = h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.False(t, controllertest.DecodeJSON[protocol.SyncResponse](t, rec).WantInventory)

	// The inventory changes: asked again.
	req.InventoryHash = "another-hash"

	rec = h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, controllertest.DecodeJSON[protocol.SyncResponse](t, rec).WantInventory)

	// A plain (uncompressed) upload works as well.
	rec = h.agentPost(agent, protocol.PathInventory, inv, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAgentConcurrencyLimit(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	// Fill every inventory slot, as uploads in flight would.
	for range maxConcurrentInventories {
		h.srv.inventorySlots <- struct{}{}
	}

	rec := h.agentPost(agent, protocol.PathInventory, controllertest.InventoryFor(t), controllertest.CallOpts{})
	requireAgentError(t, rec, http.StatusServiceUnavailable, "in flight")
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	// Syncs have their own slots.
	rec = h.agentPost(agent, protocol.PathSync, controllertest.SyncRequest(), controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	<-h.srv.inventorySlots

	rec = h.agentPost(agent, protocol.PathInventory, controllertest.InventoryFor(t), controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// signedCall serves one agent request and returns the answer with the id of
// the request's proof.
func signedCall(
	t *testing.T, h *harness, a *controllertest.Agent, path string, v any,
) (rec *httptest.ResponseRecorder, proofID string) {
	t.Helper()

	body, err := json.Marshal(v)
	require.NoError(t, err)

	req := h.agentRequest(a, path, body, controllertest.CallOpts{Gzip: true})

	parts := strings.Split(req.Header.Get(identity.HeaderProof), ".")
	require.Len(t, parts, 3)

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)

	var claims identity.ProofClaims
	require.NoError(t, json.Unmarshal(raw, &claims))

	return h.serve(req), claims.ID
}

func TestAgentAnswersAreSigned(t *testing.T) {
	key, err := identity.DeriveControllerKey([]byte("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, controllertest.Network)
	require.NoError(t, err)

	verifierFor := func(t *testing.T, host string) *identity.AnswerVerifier {
		t.Helper()

		v, verr := identity.NewAnswerVerifier(identity.PublicKey(key), controllertest.Network, host)
		require.NoError(t, verr)

		return v
	}

	h := newHarness(t, func(o *Options) { o.AnswerSigner = signer })
	agent := validatorAgent(t)
	answers := verifierFor(t, agent.Name)

	t.Run("sync and inventory answers verify for the host and the request", func(t *testing.T) {
		inv := controllertest.InventoryFor(t, protocol.Package{Name: "openssl", Version: "3.5.1-1", Arch: "amd64"})

		req := controllertest.SyncRequest()
		req.InventoryHash = inv.Hash

		rec, id := signedCall(t, h, agent, protocol.PathSync, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		body, err := answers.Verify(id, rec.Body.Bytes())
		require.NoError(t, err)

		var resp protocol.SyncResponse
		require.NoError(t, json.Unmarshal(body, &resp))
		assert.True(t, resp.ServerTime.Equal(controllertest.Epoch))
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
		req := controllertest.SyncRequest()
		req.ProtocolVersion = protocol.Version + 1

		rec, id := signedCall(t, h, agent, protocol.PathSync, req)
		requireAgentError(t, rec, http.StatusBadRequest, "not supported")

		_, err := answers.Verify(id, rec.Body.Bytes())
		require.ErrorIs(t, err, identity.ErrBadAnswer)
	})
}

// TestDuplicateContainerNames: a host that reports one name twice does not
// get to pick the owners afterwards. The first entry is the container; the
// later ones are dropped when the sync is stored.
func TestDuplicateContainerNames(t *testing.T) {
	h := newHarness(t, nil)

	req := controllertest.SyncRequest()
	req.Containers = []protocol.Container{
		controllertest.Owned("lighthouse", protocol.KindCL, "lighthouse"),
		controllertest.Owned("geth", protocol.KindEL, "geth"),
		// The same name again, for another team, and once more unmanaged.
		controllertest.Owned("lighthouse", protocol.KindCL, "teku"),
		{Name: "lighthouse", State: "exited"},
		controllertest.Owned("geth", protocol.KindEL, ""),
	}

	rec := h.agentPost(controllertest.NewAgent(t, identity.CertificateSpec{Hostname: controllertest.HostMixed}), protocol.PathSync,
		req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored := h.store.Host(controllertest.HostMixed).Status.Containers
	require.Len(t, stored, 2)
	assert.Equal(t, controllertest.Owned("lighthouse", protocol.KindCL, "lighthouse"), stored[0])
	assert.Equal(t, controllertest.Owned("geth", protocol.KindEL, "geth"), stored[1])

	// The view shows what was stored.
	detail := h.detail(controllertest.HostMixed)
	require.Len(t, detail.Containers, 2)
}

func TestSyncAnswersTheDigestsOfTheHostsTags(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	want := protocol.TagDigest{Ref: "ethpandaops/geth:master", Digest: "sha256:new", CheckedAt: controllertest.Epoch}
	h.lookups.Digests = map[string]protocol.TagDigest{want.Ref: want}

	h.sched.Set(func(f *controllertest.Scheduler) {
		f.Assignments[agent.Name] = &scheduler.Assignment{LookupInterval: 7 * time.Minute}
	})

	req := controllertest.SyncRequest()
	req.Tags = []string{want.Ref, "ethpandaops/lighthouse:unstable"}

	rec := h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := controllertest.DecodeJSON[protocol.SyncResponse](t, rec)
	require.Len(t, resp.Digests, 1)
	assert.Equal(t, want.Digest, resp.Digests[0].Digest)
	assert.Equal(t, req.Tags, h.lookups.Listed[agent.Name])
	assert.Equal(t, 7*time.Minute, h.lookups.Interval)
}

// What a host reports of its actions authorizes: it is kept only in the form
// of names, and an unknown feature counts as admin.
func TestCleanActions(t *testing.T) {
	got := cleanActions([]protocol.ActionInfo{
		{Name: "zeta", Impact: "host", Feature: "upgradoor:manage", Teams: []string{"Dev Ops", "devops"}, When: "always", Due: true},
		{Name: "alpha", Impact: protocol.ImpactIndependent, Feature: "details", When: protocol.ConditionSecurityUpdates, Due: true},
		{Name: "alpha", Impact: protocol.ImpactNode},
		{Name: "beta", Impact: protocol.ImpactNode, Feature: "team_manage"},
		{Name: "gamma", Impact: protocol.ImpactNode},
		{Name: "Bad Name", Impact: protocol.ImpactNode},
	})

	assert.Equal(t, []protocol.ActionInfo{
		{Name: "alpha", Impact: protocol.ImpactIndependent, Feature: "details", When: protocol.ConditionSecurityUpdates, Due: true},
		{Name: "beta", Impact: protocol.ImpactNode, Feature: "admin"},
		{Name: "gamma", Impact: protocol.ImpactNode},
		{Name: "zeta", Impact: protocol.ImpactNode, Feature: "admin", Teams: []string{"devops"}},
	}, got)
}

// sealing is a harness whose answers are signed and whose keyring runs on
// its clock, with the verifier of the answers to agent.
type sealing struct {
	*harness
	keyring *identity.Keyring
	agent   *controllertest.Agent
	answers *identity.AnswerVerifier
}

func newSealing(t *testing.T) *sealing {
	t.Helper()

	key, err := identity.DeriveControllerKey([]byte(
		"fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"))
	require.NoError(t, err)

	signer, err := identity.NewAnswerSigner(key, controllertest.Network)
	require.NoError(t, err)

	s := &sealing{agent: validatorAgent(t)}
	s.harness = newHarness(t, func(o *Options) {
		o.AnswerSigner = signer
		o.Keyring = identity.NewKeyring(identity.WithKeyringClock(o.Now))
		s.keyring = o.Keyring
	})

	s.answers, err = identity.NewAnswerVerifier(identity.PublicKey(key), controllertest.Network,
		s.agent.Name)
	require.NoError(t, err)

	return s
}

// keys are the announced keys.
func (s *sealing) keys() []*identity.SealKey {
	s.t.Helper()

	keys, err := s.keyring.Announced()
	require.NoError(s.t, err)

	return keys
}

// call serves v sealed as seal says and returns the answer, opened when
// sealed and verified, and whether it was sealed.
func (s *sealing) call(path string, v any, seal controllertest.Sealed) (
	rec *httptest.ResponseRecorder, answer []byte, sealed bool,
) {
	s.t.Helper()

	body, err := json.Marshal(v)
	require.NoError(s.t, err)

	req, answerSeal := s.agent.SealedRequest(s.t, s.clock.Now(), path, body, seal,
		controllertest.CallOpts{})
	id := proofID(s.t, req)

	rec = s.serve(req)
	if rec.Code != http.StatusOK {
		return rec, nil, false
	}

	answer = rec.Body.Bytes()

	sealed = rec.Header().Get("Content-Type") == protocol.ContentTypeSealed
	if sealed {
		answer, err = answerSeal.Open(answer)
		require.NoError(s.t, err)
	}

	answer, err = s.answers.Verify(id, answer)
	require.NoError(s.t, err)

	return rec, answer, sealed
}

// proofID is the id of the proof of req.
func proofID(t *testing.T, req *http.Request) string {
	t.Helper()

	parts := strings.Split(req.Header.Get(identity.HeaderProof), ".")
	require.Len(t, parts, 3)

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)

	var claims identity.ProofClaims
	require.NoError(t, json.Unmarshal(raw, &claims))

	return claims.ID
}

func decodeAs[T any](t *testing.T, raw []byte) T {
	t.Helper()

	var v T

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&v), string(raw))

	return v
}

// A hello answers the keys, signed, and stores nothing.
func TestHello(t *testing.T) {
	s := newSealing(t)

	rec, id := signedCall(t, s.harness, s.agent, protocol.PathHello, protocol.HelloRequest{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))

	raw, err := s.answers.Verify(id, rec.Body.Bytes())
	require.NoError(t, err)

	hello := decodeAs[protocol.HelloResponse](t, raw)
	require.Len(t, hello.Keys, 1, "no next key before its announcement")

	cur := s.keys()[0]
	assert.Equal(t, cur.ID.String(), hello.Keys[0].ID)
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(cur.Bytes()), hello.Keys[0].PublicKey)
	assert.True(t, hello.Keys[0].ActiveFrom.Equal(controllertest.Epoch))

	s.clock.Advance(50 * time.Minute)

	rec, id = signedCall(t, s.harness, s.agent, protocol.PathHello, protocol.HelloRequest{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	raw, err = s.answers.Verify(id, rec.Body.Bytes())
	require.NoError(t, err)
	assert.Len(t, decodeAs[protocol.HelloResponse](t, raw).Keys, 2, "the next key is announced")

	assert.Nil(t, s.store.Host(s.agent.Name), "a hello stores nothing")
	assert.Zero(t, s.sched.NudgeCount())
}

// A sealed sync and inventory are opened and answered sealed; the keys go
// along only to a sender that lacks one.
func TestSealedSyncAndInventory(t *testing.T) {
	s := newSealing(t)
	inv := controllertest.InventoryFor(t, protocol.Package{Name: "openssl", Version: "3.5.1-1", Arch: "amd64"})
	req := controllertest.SyncRequest()
	req.InventoryHash = inv.Hash

	cur := s.keys()[0]

	rec, raw, sealed := s.call(protocol.PathSync, req, controllertest.Sealed{Key: cur})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, sealed)
	assert.NotContains(t, rec.Body.String(), "serverTime", "the answer is encrypted")

	resp := decodeAs[protocol.SyncResponse](t, raw)
	assert.True(t, resp.WantInventory)
	assert.Empty(t, resp.Keys, "up to date")
	require.NotNil(t, s.store.Host(s.agent.Name))
	assert.Equal(t, "agent-1.0.0", s.store.Host(s.agent.Name).Status.AgentVersion)

	rec, raw, sealed = s.call(protocol.PathInventory, inv, controllertest.Sealed{Key: cur})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, sealed)
	assert.Equal(t, inv.Hash, decodeAs[protocol.InventoryResponse](t, raw).Hash)

	// The next key is announced: a sender without it gets both.
	s.clock.Advance(50 * time.Minute)

	_, raw, _ = s.call(protocol.PathSync, req, controllertest.Sealed{Key: cur})
	keys := decodeAs[protocol.SyncResponse](t, raw).Keys
	require.Len(t, keys, 2)

	next := s.keys()[1]
	assert.Equal(t, next.ID.String(), keys[1].ID)

	_, raw, _ = s.call(protocol.PathSync, req, controllertest.Sealed{Key: cur, Known: next.ID})
	assert.Empty(t, decodeAs[protocol.SyncResponse](t, raw).Keys)

	// Rotated: the previous key still opens, its sender gets the keys.
	s.clock.Advance(20 * time.Minute)

	_, raw, sealed = s.call(protocol.PathSync, req, controllertest.Sealed{Key: cur, Known: next.ID})
	require.True(t, sealed)
	assert.Len(t, decodeAs[protocol.SyncResponse](t, raw).Keys, 1)

	_, raw, _ = s.call(protocol.PathSync, req, controllertest.Sealed{Key: next, Known: cur.ID})
	assert.Empty(t, decodeAs[protocol.SyncResponse](t, raw).Keys)
}

// A request sealed to a key the controller does not hold is answered the
// keys, signed and unsealed, and processed in no part.
func TestSealedToAnUnknownKeyIsRekeyed(t *testing.T) {
	restarted := identity.NewKeyring(identity.WithKeyringClock(func() time.Time {
		return controllertest.Epoch
	}))
	before, err := restarted.Announced()
	require.NoError(t, err)

	tests := []struct {
		name string
		key  func(s *sealing) *identity.SealKey
	}{
		{name: "after a controller restart", key: func(*sealing) *identity.SealKey { return before[0] }},
		{name: "expired", key: func(s *sealing) *identity.SealKey {
			old := s.keys()[0]
			s.clock.Advance(2 * time.Hour)

			return old
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSealing(t)
			key := tt.key(s)

			rec, raw, sealed := s.call(protocol.PathSync, controllertest.SyncRequest(),
				controllertest.Sealed{Key: key})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.False(t, sealed)

			rekey := decodeAs[protocol.RekeyResponse](t, raw)
			assert.True(t, rekey.Rekey)
			require.Len(t, rekey.Keys, 1)
			assert.Equal(t, s.keys()[0].ID.String(), rekey.Keys[0].ID)

			assert.Nil(t, s.store.Host(s.agent.Name), "nothing of the request is processed")
			assert.Zero(t, s.sched.NudgeCount())
		})
	}
}

// A sealed body opens only for the network, host, request and path it was
// sealed for.
func TestSealedBodyIsBound(t *testing.T) {
	body, err := json.Marshal(controllertest.SyncRequest())
	require.NoError(t, err)

	tests := []struct {
		name    string
		binding identity.SealBinding
	}{
		{name: "another host", binding: identity.SealBinding{Subject: "prysm-besu-1"}},
		{name: "another network", binding: identity.SealBinding{Network: "other-devnet-1"}},
		{name: "another request", binding: identity.SealBinding{RequestID: "another-jti"}},
		{name: "another path", binding: identity.SealBinding{Path: protocol.PathInventory}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSealing(t)

			req, _ := s.agent.SealedRequest(t, s.clock.Now(), protocol.PathSync, body,
				controllertest.Sealed{Key: s.keys()[0], Binding: tt.binding}, controllertest.CallOpts{})

			requireAgentError(t, s.serve(req), http.StatusBadRequest, "does not decrypt")
			assert.Nil(t, s.store.Host(s.agent.Name))
		})
	}

	t.Run("replayed under a new proof", func(t *testing.T) {
		s := newSealing(t)

		first, _ := s.agent.SealedRequest(t, s.clock.Now(), protocol.PathSync, body,
			controllertest.Sealed{Key: s.keys()[0]}, controllertest.CallOpts{})
		wire, err := io.ReadAll(first.Body)
		require.NoError(t, err)

		first.Body = io.NopCloser(bytes.NewReader(wire))
		require.Equal(t, http.StatusOK, s.serve(first).Code)

		replay := s.agentRequest(s.agent, protocol.PathSync, wire, controllertest.CallOpts{
			Mutate: func(r *http.Request) { r.Header.Set("Content-Type", protocol.ContentTypeSealed) },
		})
		requireAgentError(t, s.serve(replay), http.StatusBadRequest, "does not decrypt")
		assert.Equal(t, 1, s.sched.NudgeCount())
	})

	t.Run("malformed", func(t *testing.T) {
		s := newSealing(t)

		req := s.agentRequest(s.agent, protocol.PathSync, []byte("not sealed"), controllertest.CallOpts{
			Mutate: func(r *http.Request) { r.Header.Set("Content-Type", protocol.ContentTypeSealed) },
		})
		requireAgentError(t, s.serve(req), http.StatusBadRequest, "malformed")
	})

	t.Run("with a content encoding", func(t *testing.T) {
		s := newSealing(t)

		req, _ := s.agent.SealedRequest(t, s.clock.Now(), protocol.PathSync, body,
			controllertest.Sealed{Key: s.keys()[0]}, controllertest.CallOpts{
				Mutate: func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") },
			})
		requireAgentError(t, s.serve(req), http.StatusUnsupportedMediaType, "no content encoding")
	})
}

// A sync answers that SSH logins are accepted; a report of a host with the
// feature on is stored and acknowledged, the notify fired for what closed.
func TestSyncStoresSSHLogins(t *testing.T) {
	h := newHarness(t, nil)
	agent := validatorAgent(t)

	start := controllertest.Epoch.Add(-time.Hour)
	end := start.Add(time.Minute)
	closed := protocol.SSHLoginGroup{
		Seq: 7, Identity: "github:pk910", Source: "github", Fingerprint: "SHA256:x", KeyType: "ED25519",
		User: "devops", From: "10.0.0.5", FirstLogin: start, LastLogout: &end, Sessions: 2,
	}
	open := protocol.SSHLoginGroup{Seq: 8, User: "devops", From: "10.0.0.5", FirstLogin: end, Sessions: 1, Open: 1}

	req := controllertest.SyncRequest()
	req.SSH = &protocol.SSHReport{Open: []protocol.SSHLoginGroup{open}, Closed: []protocol.SSHLoginGroup{closed}}

	// Without the feature named, a report is not taken.
	rec := h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := controllertest.DecodeJSON[protocol.SyncResponse](t, rec)
	assert.Equal(t, []string{protocol.FeatureSSHLogins}, resp.Accepts)
	assert.Nil(t, resp.SSHAck)
	assert.Empty(t, h.store.SSH)

	req.Features = []string{protocol.FeatureSSHLogins}
	notified := h.notified.Load()

	rec = h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp = controllertest.DecodeJSON[protocol.SyncResponse](t, rec)
	require.NotNil(t, resp.SSHAck)
	assert.Equal(t, int64(7), *resp.SSHAck)
	assert.Len(t, h.store.SSH, 2)
	assert.Greater(t, h.notified.Load(), notified)

	audit := h.store.AuditEntries()
	require.NotEmpty(t, audit)
	assert.Equal(t, "ssh:github:pk910", audit[len(audit)-1].Actor)

	// A report that cannot be stored is not acknowledged; the sync is
	// answered all the same.
	h.store.Set(func(f *controllertest.Store) { f.SSHErr = errors.New("disk full") })

	rec = h.agentPost(agent, protocol.PathSync, req, controllertest.CallOpts{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp = controllertest.DecodeJSON[protocol.SyncResponse](t, rec)
	assert.Nil(t, resp.SSHAck)
	assert.Equal(t, []string{protocol.FeatureSSHLogins}, resp.Accepts)
}

func TestCleanSSH(t *testing.T) {
	t.Parallel()

	start := controllertest.Epoch

	group := func(seq int64) protocol.SSHLoginGroup {
		return protocol.SSHLoginGroup{Seq: seq, User: "devops", From: "10.0.0.5", FirstLogin: start, Sessions: 1}
	}

	long := strings.Repeat("x", 300)
	odd := group(3)
	odd.Identity, odd.Source, odd.User, odd.Sessions, odd.TotalMillis = long, "made-up", "dev\xffops", -1, -5

	in := &protocol.SSHReport{
		Open:    []protocol.SSHLoginGroup{group(1), group(1), group(2), {Seq: 9}},
		Closed:  []protocol.SSHLoginGroup{group(2), odd, {Seq: 4, User: "x", From: "y"}},
		Dropped: -3,
		Error:   long,
	}

	out := cleanSSH(in)
	require.Len(t, out.Closed, 2)
	require.Len(t, out.Open, 1, "each Seq once, a closed one wins, an empty one dropped")
	assert.Equal(t, int64(1), out.Open[0].Seq)
	assert.Equal(t, fleet.SSHSourceUnknown, out.Open[0].Source)
	assert.Zero(t, out.Dropped)
	assert.Len(t, out.Error, maxSSHError)

	got := out.Closed[1]
	assert.Len(t, got.Identity, maxSSHIdentity)
	assert.Equal(t, fleet.SSHSourceUnknown, got.Source)
	assert.Equal(t, "devops", got.User)
	assert.Zero(t, got.Sessions)
	assert.Zero(t, got.TotalMillis)

	many := &protocol.SSHReport{}
	for i := range protocol.MaxSSHClosed + 10 {
		many.Closed = append(many.Closed, group(int64(i+1)))
	}

	assert.Len(t, cleanSSH(many).Closed, protocol.MaxSSHClosed)
}
