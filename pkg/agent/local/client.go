package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to the running agent over its local socket: what the
// 'upgradoor host' commands use.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the agent's socket file.
func NewClient(socket string) *Client {
	return &Client{http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

// Status returns what the agent last saw of the host.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var out Status
	if err := c.call(ctx, http.MethodGet, PathStatus, nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Override sets or clears the override of one container.
func (c *Client) Override(ctx context.Context, req *OverrideRequest) (*Answer, error) {
	return c.post(ctx, PathOverrides, req)
}

// Hold sets or releases the local hold.
func (c *Client) Hold(ctx context.Context, req *HoldRequest) (*Answer, error) {
	return c.post(ctx, PathHold, req)
}

// Roll requests a roll of one container.
func (c *Client) Roll(ctx context.Context, container string) (*Answer, error) {
	return c.post(ctx, PathRolls, &RollRequest{Container: container})
}

// InitOverrides writes the generated block of the overrides file.
func (c *Client) InitOverrides(ctx context.Context) (*Answer, error) {
	return c.post(ctx, PathInit, struct{}{})
}

func (c *Client) post(ctx context.Context, path string, body any) (*Answer, error) {
	var out Answer
	if err := c.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader

	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}

		reader = bytes.NewReader(raw)
	}

	// The host name is not used: the transport dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://upgradoor"+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("the agent's socket does not answer (is the agent running, and "+
			"are you root or in the socket's group?): %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read answer: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var failed Answer
		if json.Unmarshal(raw, &failed) == nil && failed.Error != "" {
			return errors.New(failed.Error)
		}

		return fmt.Errorf("the agent answered %s", resp.Status)
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode answer: %w", err)
	}

	return nil
}
