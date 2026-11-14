package probe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func testLog() logrus.FieldLogger {
	l := logrus.New()
	l.SetOutput(io.Discard)

	return l
}

// elServer answers JSON-RPC calls from a method -> raw result table. A
// missing method answers with a JSON-RPC error.
func elServer(t *testing.T, results map[string]string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			Method string `json:"method"`
		}

		if !assert.Equal(t, http.MethodPost, r.Method) ||
			!assert.NoError(t, json.NewDecoder(r.Body).Decode(&call)) {
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		w.Header().Set("Content-Type", "application/json")

		result, ok := results[call.Method]
		if !ok {
			_, _ = io.WriteString(w,
				`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)

			return
		}

		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":`+result+`}`)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// clServer answers beacon API GETs from a path -> body table.
func clServer(t *testing.T, bodies map[string]string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestProbeEL(t *testing.T) {
	tests := []struct {
		name    string
		results map[string]string
		want    protocol.ELHealth
		wantErr string
	}{
		{
			name: "synced",
			results: map[string]string{
				"eth_syncing": "false", "net_peerCount": `"0x19"`, "eth_blockNumber": `"0x1b4"`,
			},
			want: protocol.ELHealth{Reachable: true, Peers: 25, Head: 436},
		},
		{
			name: "syncing",
			results: map[string]string{
				"eth_syncing":     `{"startingBlock":"0x0","currentBlock":"0x10","highestBlock":"0x99"}`,
				"net_peerCount":   `"0x3"`,
				"eth_blockNumber": `"0x10"`,
			},
			want: protocol.ELHealth{Reachable: true, Syncing: true, Peers: 3, Head: 16},
		},
		{
			name:    "peer count unavailable",
			results: map[string]string{"eth_syncing": "false", "eth_blockNumber": `"0x2"`},
			want:    protocol.ELHealth{Reachable: true, Head: 2},
			wantErr: "net_peerCount: rpc error -32601: method not found",
		},
		{
			name: "garbage quantity",
			results: map[string]string{
				"eth_syncing": "false", "net_peerCount": `"12"`, "eth_blockNumber": `17`,
			},
			want:    protocol.ELHealth{Reachable: true},
			wantErr: "net_peerCount: not a hex quantity",
		},
		{
			name:    "eth_syncing fails",
			results: map[string]string{"net_peerCount": `"0x1"`},
			want:    protocol.ELHealth{},
			wantErr: "eth_syncing: rpc error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := elServer(t, tt.results)

			got := New(testLog(), srv.URL, "").Probe(context.Background())
			require.NotNil(t, got.EL)
			assert.Nil(t, got.CL, "an empty URL disables the probe")

			if tt.wantErr == "" {
				assert.Empty(t, got.EL.Error)
			} else {
				assert.Contains(t, got.EL.Error, tt.wantErr)
			}

			got.EL.Error = ""
			assert.Equal(t, tt.want, *got.EL)
		})
	}
}

func TestProbeCL(t *testing.T) {
	const (
		synced = `{"data":{"head_slot":"1200","sync_distance":"0","is_syncing":false,` +
			`"is_optimistic":false,"el_offline":false}}`
		peers = `{"data":{"disconnected":"3","connecting":"0","connected":"42","disconnecting":"1"}}`
	)

	tests := []struct {
		name    string
		bodies  map[string]string
		want    protocol.CLHealth
		wantErr string
	}{
		{
			name:   "synced",
			bodies: map[string]string{"/eth/v1/node/syncing": synced, "/eth/v1/node/peer_count": peers},
			want:   protocol.CLHealth{Reachable: true, Peers: 42, HeadSlot: 1200},
		},
		{
			name: "syncing, optimistic, el offline",
			bodies: map[string]string{
				"/eth/v1/node/syncing": `{"data":{"head_slot":"10","sync_distance":"990",` +
					`"is_syncing":true,"is_optimistic":true,"el_offline":true}}`,
				"/eth/v1/node/peer_count": `{"data":{"connected":7}}`,
			},
			want: protocol.CLHealth{
				Reachable: true, Syncing: true, Optimistic: true, ELOffline: true,
				Peers: 7, HeadSlot: 10, SyncDistance: 990,
			},
		},
		{
			name:    "peer count missing",
			bodies:  map[string]string{"/eth/v1/node/syncing": synced},
			want:    protocol.CLHealth{Reachable: true, HeadSlot: 1200},
			wantErr: "/eth/v1/node/peer_count: status 404",
		},
		{
			name:    "syncing endpoint missing",
			bodies:  map[string]string{"/eth/v1/node/peer_count": peers},
			want:    protocol.CLHealth{},
			wantErr: "/eth/v1/node/syncing: status 404",
		},
		{
			name:    "not json",
			bodies:  map[string]string{"/eth/v1/node/syncing": "<html>", "/eth/v1/node/peer_count": peers},
			want:    protocol.CLHealth{},
			wantErr: "decode answer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := clServer(t, tt.bodies)

			// A trailing slash on the configured URL must not matter.
			got := New(testLog(), "", srv.URL+"/").Probe(context.Background())
			require.NotNil(t, got.CL)
			assert.Nil(t, got.EL, "an empty URL disables the probe")

			if tt.wantErr == "" {
				assert.Empty(t, got.CL.Error)
			} else {
				assert.Contains(t, got.CL.Error, tt.wantErr)
			}

			got.CL.Error = ""
			assert.Equal(t, tt.want, *got.CL)
		})
	}
}

func TestProbeBoth(t *testing.T) {
	el := elServer(t, map[string]string{
		"eth_syncing": "false", "net_peerCount": `"0x1"`, "eth_blockNumber": `"0x5"`,
	})
	cl := clServer(t, map[string]string{
		"/eth/v1/node/syncing": `{"data":{"head_slot":"5","sync_distance":"0",` +
			`"is_syncing":false,"is_optimistic":false,"el_offline":false}}`,
		"/eth/v1/node/peer_count": `{"data":{"connected":"2"}}`,
	})

	got := New(testLog(), el.URL, cl.URL).Probe(context.Background())
	require.NotNil(t, got.EL)
	require.NotNil(t, got.CL)
	assert.True(t, got.Synced())
}

func TestProbeNothingConfigured(t *testing.T) {
	got := New(testLog(), "", "").Probe(context.Background())
	assert.Nil(t, got.EL)
	assert.Nil(t, got.CL)
	assert.True(t, got.Synced(), "no probe configured counts as synced")
}

func TestProbeUnreachable(t *testing.T) {
	// A server that is closed again: nothing listens on its address.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	got := New(testLog(), url, url).Probe(context.Background())
	require.NotNil(t, got.EL)
	require.NotNil(t, got.CL)
	assert.False(t, got.EL.Reachable)
	assert.False(t, got.CL.Reachable)
	assert.NotEmpty(t, got.EL.Error)
	assert.NotEmpty(t, got.CL.Error)
	assert.False(t, got.Synced())
}

func TestProbeTimeout(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	p := New(testLog(), srv.URL, srv.URL)
	p.timeout = 50 * time.Millisecond

	began := time.Now()
	got := p.Probe(context.Background())

	assert.Less(t, time.Since(began), 2*time.Second, "a hanging node must not hold the agent")
	require.NotNil(t, got.EL)
	require.NotNil(t, got.CL)
	assert.False(t, got.EL.Reachable)
	assert.False(t, got.CL.Reachable)
	assert.Contains(t, got.EL.Error, "deadline exceeded")
}

func TestProbeCancelledContext(t *testing.T) {
	srv := elServer(t, map[string]string{"eth_syncing": "false"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := New(testLog(), srv.URL, "").Probe(ctx)
	require.NotNil(t, got.EL)
	assert.False(t, got.EL.Reachable)
}
