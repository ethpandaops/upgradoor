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

// Client talks to the running agent over its socket for the on-host
// commands. An answer that is not 200 is an error with the agent's message.
type Client interface {
	// Status returns what the agent last saw of the host.
	Status(ctx context.Context) (*Status, error)
	// Override sets or clears the override of one container.
	Override(ctx context.Context, req *OverrideRequest) (*Answer, error)
	// Hold sets or releases the local hold.
	Hold(ctx context.Context, req *HoldRequest) (*Answer, error)
	// Roll requests a roll of one container.
	Roll(ctx context.Context, container string) (*Answer, error)
	// InitOverrides writes the generated block of the overrides file.
	InitOverrides(ctx context.Context) (*Answer, error)
}

const (
	clientTimeout = 30 * time.Second

	maxAnswer = 8 << 20
)

type client struct {
	http *http.Client
}

var _ Client = (*client)(nil)

// NewClient returns a client for the agent's socket file.
func NewClient(socket string) Client {
	return &client{http: &http.Client{
		Timeout: clientTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, "unix", socket)
			},
		},
	}}
}

// Status implements Client.
func (c *client) Status(ctx context.Context) (*Status, error) {
	var out Status
	if err := c.call(ctx, http.MethodGet, PathStatus, nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// Override implements Client.
func (c *client) Override(ctx context.Context, req *OverrideRequest) (*Answer, error) {
	return c.post(ctx, PathOverrides, req)
}

// Hold implements Client.
func (c *client) Hold(ctx context.Context, req *HoldRequest) (*Answer, error) {
	return c.post(ctx, PathHold, req)
}

// Roll implements Client.
func (c *client) Roll(ctx context.Context, container string) (*Answer, error) {
	return c.post(ctx, PathRolls, &RollRequest{Container: container})
}

// InitOverrides implements Client.
func (c *client) InitOverrides(ctx context.Context) (*Answer, error) {
	return c.post(ctx, PathInit, struct{}{})
}

func (c *client) post(ctx context.Context, path string, body any) (*Answer, error) {
	var out Answer
	if err := c.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

func (c *client) call(ctx context.Context, method, path string, body, out any) error {
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

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
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
