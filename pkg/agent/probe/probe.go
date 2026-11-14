// Package probe checks the health of the local execution and consensus
// clients for the agent: sync state, peers and head.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// DefaultTimeout bounds one probe of one client, all its requests
	// together. The clients are local; a slow answer is a bad sign in itself.
	DefaultTimeout = 4 * time.Second

	maxAnswer = 1 << 20
)

// Prober probes the local EL (JSON-RPC) and CL (beacon API).
type Prober struct {
	log     logrus.FieldLogger
	elURL   string
	clURL   string
	timeout time.Duration
	client  *http.Client
}

var _ agent.Prober = (*Prober)(nil)

// New returns a prober. An empty URL disables that probe: its member of
// the result stays nil.
func New(log logrus.FieldLogger, elURL, clURL string) *Prober {
	return &Prober{
		log:     log.WithField("package", "probe"),
		elURL:   strings.TrimSpace(elURL),
		clURL:   strings.TrimRight(strings.TrimSpace(clURL), "/"),
		timeout: DefaultTimeout,
		client: &http.Client{
			// The context carries the deadline. Redirects are not followed:
			// a node API does not redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Probe asks both clients at the same time and returns what they said. It
// never fails: an unreachable client is a result.
func (p *Prober) Probe(ctx context.Context) protocol.NodeHealth {
	var (
		out protocol.NodeHealth
		wg  sync.WaitGroup
	)

	if p.elURL != "" {
		wg.Go(func() { out.EL = p.probeEL(ctx) })
	}

	if p.clURL != "" {
		wg.Go(func() { out.CL = p.probeCL(ctx) })
	}

	wg.Wait()

	return out
}

// probeEL calls eth_syncing, net_peerCount and eth_blockNumber. The client
// is reachable when eth_syncing answered.
func (p *Prober) probeEL(ctx context.Context) *protocol.ELHealth {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	h := &protocol.ELHealth{}

	raw, err := p.rpc(ctx, "eth_syncing")
	if err != nil {
		h.Error = err.Error()
		p.log.WithError(err).Debug("execution client probe failed")

		return h
	}

	h.Reachable = true
	// eth_syncing is false when in sync and an object while syncing.
	h.Syncing = strings.TrimSpace(string(raw)) != "false"

	var errs []error

	peers, err := p.rpcQuantity(ctx, "net_peerCount")
	if err != nil {
		errs = append(errs, err)
	}

	head, err := p.rpcQuantity(ctx, "eth_blockNumber")
	if err != nil {
		errs = append(errs, err)
	}

	h.Peers, h.Head = int(peers), head

	if err := errors.Join(errs...); err != nil {
		h.Error = strings.ReplaceAll(err.Error(), "\n", "; ")
	}

	return h
}

// rpcAnswer is a JSON-RPC 2.0 response.
type rpcAnswer struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc makes one JSON-RPC call without parameters.
func (p *Prober) rpc(ctx context.Context, method string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": []any{},
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.elURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	req.Header.Set("Content-Type", "application/json")

	raw, err := p.do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	var answer rpcAnswer

	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("%s: decode answer: %w", method, err)
	}

	if answer.Error != nil {
		return nil, fmt.Errorf("%s: rpc error %d: %s", method, answer.Error.Code, answer.Error.Message)
	}

	if len(answer.Result) == 0 {
		return nil, fmt.Errorf("%s: answer without result", method)
	}

	return answer.Result, nil
}

// rpcQuantity makes a call whose result is a hex quantity.
func (p *Prober) rpcQuantity(ctx context.Context, method string) (uint64, error) {
	raw, err := p.rpc(ctx, method)
	if err != nil {
		return 0, err
	}

	n, err := quantity(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", method, err)
	}

	return n, nil
}

// quantity parses a JSON-RPC hex quantity ("0x1f").
func quantity(raw json.RawMessage) (uint64, error) {
	var s string

	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("not a string: %w", err)
	}

	digits, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return 0, fmt.Errorf("not a hex quantity: %q", s)
	}

	n, err := strconv.ParseUint(digits, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("not a hex quantity: %q", s)
	}

	return n, nil
}

// decimal is a beacon API number, which is a decimal string (plain numbers
// are accepted too).
type decimal uint64

// UnmarshalJSON implements json.Unmarshaler.
func (d *decimal) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(string(raw), `"`)

	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("not a decimal number: %q", s)
	}

	*d = decimal(n)

	return nil
}

// syncingAnswer is the answer of GET /eth/v1/node/syncing.
//
//nolint:tagliatelle // the beacon API spells its fields in snake_case
type syncingAnswer struct {
	Data struct {
		HeadSlot     decimal `json:"head_slot"`
		SyncDistance decimal `json:"sync_distance"`
		IsSyncing    bool    `json:"is_syncing"`
		IsOptimistic bool    `json:"is_optimistic"`
		ELOffline    bool    `json:"el_offline"`
	} `json:"data"`
}

// peerCountAnswer is the answer of GET /eth/v1/node/peer_count.
type peerCountAnswer struct {
	Data struct {
		Connected decimal `json:"connected"`
	} `json:"data"`
}

// probeCL calls /eth/v1/node/syncing and /eth/v1/node/peer_count. The
// client is reachable when the syncing endpoint answered.
func (p *Prober) probeCL(ctx context.Context) *protocol.CLHealth {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	h := &protocol.CLHealth{}

	var syncing syncingAnswer

	if err := p.get(ctx, "/eth/v1/node/syncing", &syncing); err != nil {
		h.Error = err.Error()
		p.log.WithError(err).Debug("consensus client probe failed")

		return h
	}

	h.Reachable = true
	h.Syncing = syncing.Data.IsSyncing
	h.Optimistic = syncing.Data.IsOptimistic
	h.ELOffline = syncing.Data.ELOffline
	h.HeadSlot = uint64(syncing.Data.HeadSlot)
	h.SyncDistance = uint64(syncing.Data.SyncDistance)

	var peers peerCountAnswer

	if err := p.get(ctx, "/eth/v1/node/peer_count", &peers); err != nil {
		h.Error = err.Error()

		return h
	}

	h.Peers = int(peers.Data.Connected)

	return h
}

// get fetches one beacon API endpoint into out.
func (p *Prober) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.clURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	req.Header.Set("Accept", "application/json")

	raw, err := p.do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode answer: %w", path, err)
	}

	return nil
}

// do sends a request and returns the body of a 2xx answer.
func (p *Prober) do(req *http.Request) ([]byte, error) {
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, fmt.Errorf("read answer: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	return raw, nil
}
