package beacon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// errNotServed marks a request that no configured endpoint serves: every
// endpoint answered that the route does not exist or is switched off.
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
// route at all: unknown route, or liveness tracking switched off (Teku
// without --beacon-liveness-tracking-enabled says so in the message).
func (e *statusError) notServed() bool {
	switch e.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}

	msg := strings.ToLower(e.Message)
	if !strings.Contains(msg, "liveness") {
		return false
	}

	return strings.Contains(msg, "not enabled") || strings.Contains(msg, "disabled")
}

// rejected reports whether the node understood the route and refused this
// particular request (for example an epoch it no longer serves). Another
// endpoint would say the same, so there is no failover.
func (e *statusError) rejected() bool {
	return e.Status == http.StatusBadRequest && !e.notServed()
}

// endpoint is one beacon API base URL.
type endpoint struct {
	base string
	// label names the endpoint in logs and errors without credentials.
	label string
}

// nodeClient talks to the beacon API. Endpoints are tried in order starting
// with the one that worked last. It is used by one goroutine at a time.
type nodeClient struct {
	log       logrus.FieldLogger
	http      *http.Client
	endpoints []endpoint
	active    int
}

// newNodeClient validates the endpoint URLs.
func newNodeClient(log logrus.FieldLogger, client *http.Client, urls []string) (*nodeClient, error) {
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

	return &nodeClient{log: log, http: client, endpoints: endpoints}, nil
}

// get performs a GET and decodes the JSON answer into out.
func (c *nodeClient) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// post performs a POST with a JSON body and decodes the answer into out.
func (c *nodeClient) post(ctx context.Context, path string, body []byte, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// do tries the endpoints in order, starting with the active one, and makes
// the first that answers the active one.
func (c *nodeClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	var (
		errs      = make([]error, 0, len(c.endpoints))
		notServed = 0
	)

	for i := range c.endpoints {
		idx := (c.active + i) % len(c.endpoints)
		target := c.endpoints[idx]

		err := c.request(ctx, target, method, path, body, out)
		if err == nil {
			if idx != c.active {
				c.log.WithField("endpoint", target.label).Info("Switched beacon endpoint")

				c.active = idx
			}

			return nil
		}

		if ctx.Err() != nil {
			return fmt.Errorf("%s %s: %w", method, path, ctx.Err())
		}

		var status *statusError
		if errors.As(err, &status) {
			if status.rejected() {
				return fmt.Errorf("%s %s: %s: %w", method, path, target.label, err)
			}

			if status.notServed() {
				notServed++
			}
		}

		errs = append(errs, fmt.Errorf("%s: %w", target.label, err))
	}

	joined := errors.Join(errs...)
	if notServed == len(c.endpoints) {
		return fmt.Errorf("%s %s: %w: %w", method, path, errNotServed, joined)
	}

	return fmt.Errorf("%s %s: %w", method, path, joined)
}

// request performs one request against one endpoint.
func (c *nodeClient) request(
	ctx context.Context, target endpoint, method, path string, body []byte, out any,
) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, target.base+path, reader)
	if err != nil {
		return errors.New("cannot build request")
	}

	req.Header.Set("Accept", "application/json")

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

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
