package agent

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

// LocalClient talks to the running agent over its local socket: what the
// 'upgradoor host' commands use.
type LocalClient struct {
	http *http.Client
}

// NewLocalClient returns a client for the agent's socket file.
func NewLocalClient(socket string) *LocalClient {
	return &LocalClient{http: &http.Client{
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
func (c *LocalClient) Status(ctx context.Context) (*LocalStatus, error) {
	var out LocalStatus
	if err := c.call(ctx, http.MethodGet, LocalPathStatus, nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Override sets or clears the override of one container.
func (c *LocalClient) Override(ctx context.Context, req *OverrideRequest) (*LocalAnswer, error) {
	return c.post(ctx, LocalPathOverrides, req)
}

// Hold sets or releases the local hold.
func (c *LocalClient) Hold(ctx context.Context, req *HoldRequest) (*LocalAnswer, error) {
	return c.post(ctx, LocalPathHold, req)
}

// Roll requests a roll of one container.
func (c *LocalClient) Roll(ctx context.Context, container string) (*LocalAnswer, error) {
	return c.post(ctx, LocalPathRolls, &RollRequest{Container: container})
}

// InitOverrides writes the generated block of the overrides file.
func (c *LocalClient) InitOverrides(ctx context.Context) (*LocalAnswer, error) {
	return c.post(ctx, LocalPathInit, struct{}{})
}

func (c *LocalClient) post(ctx context.Context, path string, body any) (*LocalAnswer, error) {
	var out LocalAnswer
	if err := c.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

func (c *LocalClient) call(ctx context.Context, method, path string, body, out any) error {
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
		var failed LocalAnswer
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
