// Package controller is the agent's side of the controller: requests signed
// with the host key, answers acted on only when the controller key signed
// them, and the pace of the syncs the answers ask for.
package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// defaultRequestTimeout bounds one request to the controller.
	defaultRequestTimeout = 30 * time.Second
	// maxResponseBody bounds what the agent reads of a controller answer.
	maxResponseBody = 4 << 20
)

// Client posts authenticated requests to the controller and hands out only
// answers the controller has authenticated itself for. It is used by the sync
// loop alone.
type Client interface {
	// Sync posts one sync request.
	Sync(ctx context.Context, req *protocol.SyncRequest) (*protocol.SyncResponse, error)
	// Inventory uploads the package inventory.
	Inventory(ctx context.Context, inv *protocol.Inventory) error
	// Interval is the time to the next regular sync: what the controller's
	// last answer asked for, kept within controller.minSyncInterval and
	// controller.maxSyncInterval; without such an answer syncInterval, or
	// leaseSyncInterval while busy (a lease or a roll is open).
	Interval(busy bool) time.Duration
}

// client is the Client.
type client struct {
	cfg     *config.Controller
	baseURL string
	creds   *identity.Credentials
	// answers verifies the controller's signature on every answer. Nil only
	// with controller.allowUnsignedAnswers: answers are then taken as sent,
	// and a signed one is refused, because nothing can check it.
	answers *identity.AnswerVerifier
	http    *http.Client
	now     func() time.Time

	// asked is the interval the controller's last answer asked for; zero
	// after a failed sync.
	asked time.Duration
}

var _ Client = (*client)(nil)

// New returns the client of the controller that cfg names, for the host and
// network of claims (the host certificate).
func New(
	log logrus.FieldLogger, cfg *config.Controller, creds *identity.Credentials,
	claims *identity.Claims, now func() time.Time,
) (Client, error) {
	answers, err := newAnswerVerifier(log, cfg, claims)
	if err != nil {
		return nil, err
	}

	return &client{
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.URL, "/"),
		creds:   creds,
		answers: answers,
		http:    &http.Client{Timeout: defaultRequestTimeout},
		now:     now,
	}, nil
}

// newAnswerVerifier builds the check of the controller's answers from the
// configured controller keys, for the host and network of the certificate.
// Without keys (controller.allowUnsignedAnswers) there is none, and it says
// so loudly.
func newAnswerVerifier(
	log logrus.FieldLogger, cfg *config.Controller, claims *identity.Claims,
) (*identity.AnswerVerifier, error) {
	keys, err := cfg.ControllerKeys()
	if err != nil {
		return nil, fmt.Errorf("agent: configuration: %w", err)
	}

	if len(keys) == 0 {
		log.Warn("INSECURE: controller.allowUnsignedAnswers is set and no controller " +
			"public key is configured. The controller's answers are NOT verified: this " +
			"host acts on whatever answers under controller.url. Development only.")

		return nil, nil //nolint:nilnil // no verifier and no error is the opt-out
	}

	answers, err := identity.NewAnswerVerifier(keys, claims.Network, claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("agent: controller keys: %w", err)
	}

	prints := make([]string, 0, len(keys))
	for _, key := range keys {
		prints = append(prints, identity.KeyFingerprint(key))
	}

	log.WithField("controller_keys_sha256", prints).
		Info("acting only on answers signed by one of the configured controller keys")

	return answers, nil
}

// Sync posts one sync request.
func (c *client) Sync(
	ctx context.Context, req *protocol.SyncRequest,
) (*protocol.SyncResponse, error) {
	var resp protocol.SyncResponse

	if err := c.post(ctx, protocol.PathSync, req, &resp); err != nil {
		c.asked = 0

		return nil, err
	}

	c.asked = time.Duration(resp.NextSyncMillis) * time.Millisecond

	return &resp, nil
}

// Interval is the time to the next regular sync.
func (c *client) Interval(busy bool) time.Duration {
	if c.asked > 0 {
		return min(max(c.asked, c.cfg.MinSyncInterval), c.cfg.MaxSyncInterval)
	}

	if busy {
		return c.cfg.LeaseSyncInterval
	}

	return c.cfg.SyncInterval
}

// Inventory uploads the package inventory.
func (c *client) Inventory(ctx context.Context, inv *protocol.Inventory) error {
	var resp protocol.InventoryResponse

	return c.post(ctx, protocol.PathInventory, inv, &resp)
}

// post sends in as gzip-encoded JSON with the host certificate and a fresh
// proof over the method, the full URL and the body bytes as sent, and
// decodes the answer into out.
//
// out is filled only from an answer that carries a controller signature
// naming this host and this very request (the id of its proof) over the
// answer's bytes. Anything else is an identity.ErrBadAnswer and leaves out
// untouched. An error answer (non-2xx) is never signed and never more than an
// error to log. Without controller keys (controller.allowUnsignedAnswers,
// development only) the answer is taken as sent.
func (c *client) post(ctx context.Context, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	var body bytes.Buffer

	zw := gzip.NewWriter(&body)

	if _, err = zw.Write(raw); err != nil {
		return fmt.Errorf("compress request: %w", err)
	}

	if err = zw.Close(); err != nil {
		return fmt.Errorf("compress request: %w", err)
	}

	target := c.baseURL + path

	proof, proofID, err := c.creds.Proof(http.MethodPost, target, body.Bytes(), c.now())
	if err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, target, bytes.NewReader(body.Bytes()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(identity.HeaderCert, c.creds.Certificate())
	req.Header.Set(identity.HeaderProof, proof)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return fmt.Errorf("read answer of %s: %w", path, err)
	}

	if len(answer) > maxResponseBody {
		return fmt.Errorf("answer of %s exceeds %d bytes", path, maxResponseBody)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return controllerError(path, resp.StatusCode, answer, c.now())
	}

	if c.answers != nil {
		if answer, err = c.answers.Verify(proofID, answer); err != nil {
			return fmt.Errorf("answer of %s: %w", path, err)
		}
	} else if identity.IsSignedAnswer(answer) {
		return fmt.Errorf("answer of %s: the controller signs its answers and "+
			"controller.allowUnsignedAnswers is set: configure controller.publicKeys", path)
	}

	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("decode answer of %s: %w", path, err)
	}

	return nil
}

// controllerError turns a non-2xx answer into an error. Authentication
// failures are usually clock problems, so the controller's time is shown
// next to the local one.
func controllerError(path string, status int, answer []byte, now time.Time) error {
	var er protocol.ErrorResponse

	if err := json.Unmarshal(answer, &er); err != nil || er.Error == "" {
		text := strings.TrimSpace(string(answer))
		if len(text) > 200 {
			text = text[:200]
		}

		return fmt.Errorf("post %s: status %d: %s", path, status, text)
	}

	if er.ServerTime.IsZero() {
		return fmt.Errorf("post %s: status %d: %s", path, status, er.Error)
	}

	return fmt.Errorf("post %s: status %d: %s (controller time %s, local time %s)",
		path, status, er.Error,
		er.ServerTime.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
}
