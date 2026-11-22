package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// requestTimeout bounds one beacon API request.
	requestTimeout = 15 * time.Second
	// maxResponseBytes bounds a decoded response body.
	maxResponseBytes = 64 << 20
	// maxErrorBytes bounds how much of an error body is read.
	maxErrorBytes = 4 << 10
	// maxErrorMessage bounds the error text kept from a node.
	maxErrorMessage = 200
)

// errNotServed marks a request that no endpoint asked serves: every one
// answered that the route does not exist or is switched off.
var errNotServed = errors.New("not served by the beacon node")

// statusError is a non-2xx answer of a beacon node.
type statusError struct {
	Status  int
	Message string
}

// Error implements error.
func (e *statusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}

	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
}

// notServed reports whether the answer means the node does not offer the
// route at all.
func (e *statusError) notServed() bool {
	switch e.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

// rejected reports whether the node understood the route and refused this
// particular request (for example an epoch it no longer serves). Another
// endpoint would say the same, so there is no failover.
func (e *statusError) rejected() bool {
	return e.Status == http.StatusBadRequest
}

// endpoint is one beacon API base URL.
type endpoint struct {
	base string
	// label names the endpoint in logs and errors without credentials.
	label string
}

// EndpointState is how an endpoint stands against the others in the last
// poll.
type EndpointState string

// The endpoint states. Only ok endpoints serve blocks and committees.
const (
	// EndpointOK is in sync, on the followed chain and up to date.
	EndpointOK EndpointState = "ok"
	// EndpointBehind is on the followed chain more than behindSlots slots
	// below its head.
	EndpointBehind EndpointState = "behind"
	// EndpointSyncing says it is syncing or that its execution client is
	// offline.
	EndpointSyncing EndpointState = "syncing"
	// EndpointForked has a head that is not on the followed chain.
	EndpointForked EndpointState = "forked"
	// EndpointDown did not answer.
	EndpointDown EndpointState = "down"
)

// EndpointStatus is what the service saw of one beacon endpoint.
type EndpointStatus struct {
	// Label is the endpoint's scheme and host, without credentials or path.
	Label string        `json:"label"`
	State EndpointState `json:"state"`
	// HeadSlot, HeadRoot and FinalizedEpoch are what the endpoint last
	// reported; absent before.
	HeadSlot       *uint64 `json:"headSlot,omitempty"`
	HeadRoot       string  `json:"headRoot,omitempty"`
	FinalizedEpoch *uint64 `json:"finalizedEpoch,omitempty"`
	// SuccessAt is the last answer of the endpoint.
	SuccessAt time.Time `json:"successAt,omitzero"`
	// Error is why the last failed request failed, at ErrorAt.
	Error   string    `json:"error,omitempty"`
	ErrorAt time.Time `json:"errorAt,omitzero"`
}

// probe is what one endpoint said about its chain in one poll.
type probe struct {
	head      headRef
	finalized uint64
	syncing   bool
	err       error
}

// nodeClient talks to the beacon API. The poll goroutine makes the requests;
// Status may be called from any goroutine.
type nodeClient struct {
	log       logrus.FieldLogger
	http      *http.Client
	now       func() time.Time
	endpoints []endpoint
	// all is every endpoint in the configured order.
	all []int

	// mu guards status, one per endpoint.
	mu     sync.Mutex
	status []EndpointStatus
}

// newNodeClient validates the endpoint URLs.
func newNodeClient(
	log logrus.FieldLogger, client *http.Client, now func() time.Time, urls []string,
) (*nodeClient, error) {
	if len(urls) == 0 {
		return nil, errors.New("no beacon endpoints configured")
	}

	endpoints := make([]endpoint, 0, len(urls))

	for i, raw := range urls {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			// The parse error quotes the URL, which may carry credentials.
			return nil, fmt.Errorf("beacon endpoint %d is not a valid URL", i+1)
		}

		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, fmt.Errorf("beacon endpoint %d must be an http(s) URL", i+1)
		}

		endpoints = append(endpoints, endpoint{
			base:  strings.TrimRight(parsed.String(), "/"),
			label: parsed.Scheme + "://" + parsed.Host,
		})
	}

	if client == nil {
		client = &http.Client{Timeout: 2 * requestTimeout}
	}

	status := make([]EndpointStatus, len(endpoints))
	all := make([]int, len(endpoints))

	for i := range endpoints {
		status[i] = EndpointStatus{Label: endpoints[i].label, State: EndpointDown}
		all[i] = i
	}

	return &nodeClient{
		log: log, http: client, now: now, endpoints: endpoints, all: all, status: status,
	}, nil
}

// Status returns what was seen of every endpoint, in the configured order.
func (c *nodeClient) Status() []EndpointStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]EndpointStatus, len(c.status))
	for i := range c.status {
		out[i] = c.status[i]

		if slot := c.status[i].HeadSlot; slot != nil {
			out[i].HeadSlot = new(*slot)
		}

		if epoch := c.status[i].FinalizedEpoch; epoch != nil {
			out[i].FinalizedEpoch = new(*epoch)
		}
	}

	return out
}

// setStates records the probes and the states the poll gave the endpoints.
// A change of state is logged.
func (c *nodeClient) setStates(probes []probe, states []EndpointState) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range c.status {
		st := &c.status[i]

		if p := &probes[i]; p.err == nil {
			st.HeadSlot = new(p.head.slot)
			st.HeadRoot = p.head.root
			st.FinalizedEpoch = new(p.finalized)
		}

		if st.State == states[i] {
			continue
		}

		fields := logrus.Fields{"endpoint": st.Label, "state": states[i], "was": st.State}
		if states[i] == EndpointOK {
			c.log.WithFields(fields).Info("Beacon endpoint is usable")
		} else {
			c.log.WithFields(fields).Warn("Beacon endpoint is not usable")
		}

		st.State = states[i]
	}
}

// noteResult records the outcome of a request to endpoint idx.
func (c *nodeClient) noteResult(idx int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := &c.status[idx]
	if err == nil {
		st.SuccessAt = c.now()

		return
	}

	st.Error = err.Error()
	st.ErrorAt = c.now()
}

// probeAll asks every endpoint at once for its head, whether it is in sync
// and its finality.
func (c *nodeClient) probeAll(ctx context.Context) []probe {
	probes := make([]probe, len(c.endpoints))

	var wg sync.WaitGroup

	for i := range c.endpoints {
		wg.Go(func() { probes[i] = c.probe(ctx, i) })
	}

	wg.Wait()

	return probes
}

// probe asks one endpoint for its head, sync state and finality.
func (c *nodeClient) probe(ctx context.Context, idx int) probe {
	target := c.endpoints[idx]

	fetch := func(path, what string, out any) error {
		if err := c.request(ctx, target, path, out); err != nil {
			c.noteResult(idx, fmt.Errorf("%s %s: %w", http.MethodGet, path, err))

			return fmt.Errorf("reading %s: %w", what, err)
		}

		return nil
	}

	var (
		head     headResponse
		syncing  syncingResponse
		finality finalityResponse
	)

	if err := fetch("/eth/v1/beacon/headers/head", "head", &head); err != nil {
		return probe{err: err}
	}

	if err := fetch("/eth/v1/node/syncing", "sync state", &syncing); err != nil {
		return probe{err: err}
	}

	err := fetch("/eth/v1/beacon/states/head/finality_checkpoints", "finality", &finality)
	if err != nil {
		return probe{err: err}
	}

	var invalid string

	switch {
	case head.Data.Header.Message.Slot == nil:
		invalid = "reading head: answer has no slot"
	case !plausibleRoot(head.Data.Root):
		invalid = "reading head: answer has no valid root"
	case syncing.Data.IsSyncing == nil:
		invalid = "reading sync state: answer has no is_syncing"
	case finality.Data.Finalized.Epoch == nil:
		invalid = "reading finality: answer has no finalized epoch"
	}

	if invalid != "" {
		err := errors.New(invalid)
		c.noteResult(idx, err)

		return probe{err: err}
	}

	c.noteResult(idx, nil)

	return probe{
		head:      headRef{root: head.Data.Root, slot: uint64(*head.Data.Header.Message.Slot)},
		finalized: uint64(*finality.Data.Finalized.Epoch),
		syncing:   *syncing.Data.IsSyncing || syncing.Data.ELOffline,
	}
}

// get performs a GET and decodes the JSON answer into out. It tries the
// endpoints of order one after the other until one answers.
func (c *nodeClient) get(ctx context.Context, order []int, path string, out any) error {
	const method = http.MethodGet

	if len(order) == 0 {
		return fmt.Errorf("%s %s: no usable beacon endpoint", method, path)
	}

	var (
		errs      = make([]error, 0, len(order))
		notServed = 0
	)

	for _, idx := range order {
		target := c.endpoints[idx]

		err := c.request(ctx, target, path, out)
		if err == nil {
			c.noteResult(idx, nil)

			return nil
		}

		if ctx.Err() != nil {
			return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
		}

		var status *statusError
		if errors.As(err, &status) {
			if status.rejected() {
				// The endpoint answered; the request was wrong, not the endpoint.
				c.noteResult(idx, nil)

				return fmt.Errorf("%s %s: %s: %w", method, path, target.label, err)
			}

			if status.notServed() {
				notServed++
			}
		}

		c.noteResult(idx, fmt.Errorf("%s %s: %w", method, path, err))
		errs = append(errs, fmt.Errorf("%s: %w", target.label, err))
	}

	joined := errors.Join(errs...)
	if notServed == len(order) {
		return fmt.Errorf("%s %s: %w: %w", method, path, errNotServed, joined)
	}

	return fmt.Errorf("%s %s: %w", method, path, joined)
}

// request performs one request against one endpoint.
func (c *nodeClient) request(ctx context.Context, target endpoint, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.base+path, http.NoBody)
	if err != nil {
		return errors.New("cannot build request")
	}

	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(err)
	}

	defer func() {
		// Drain what is left so the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{Status: resp.StatusCode, Message: errorMessage(resp.Body)}
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(out); err != nil {
		return fmt.Errorf("invalid JSON answer: %w", err)
	}

	return nil
}

// transportError strips the request URL (it may carry credentials) from a
// client error.
func transportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}

// errorMessage extracts the "message" of a beacon API error body, or the
// body text when it is not the standard shape.
func errorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxErrorBytes))
	if err != nil {
		return ""
	}

	var parsed struct {
		Message string `json:"message"`
	}

	text := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
		text = parsed.Message
	}

	text = strings.Join(strings.Fields(text), " ")
	if len(text) > maxErrorMessage {
		text = text[:maxErrorMessage] + "..."
	}

	return text
}
