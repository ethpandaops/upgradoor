package agent

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

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// defaultRequestTimeout bounds one request to the controller.
	defaultRequestTimeout = 30 * time.Second
	// maxResponseBody bounds what the agent reads of a controller answer.
	maxResponseBody = 4 << 20
)

// controllerClient posts authenticated requests to the controller and hands
// out only answers the controller has authenticated itself for.
type controllerClient struct {
	baseURL string
	creds   *identity.Credentials
	// answers verifies the controller's signature on every answer. Nil only
	// with controller.allowUnsignedAnswers: answers are then taken as sent,
	// and a signed one is refused, because nothing can check it.
	answers *identity.AnswerVerifier
	http    *http.Client
	now     func() time.Time
}

func newControllerClient(
	baseURL string, creds *identity.Credentials, answers *identity.AnswerVerifier,
	now func() time.Time,
) *controllerClient {
	return &controllerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		creds:   creds,
		answers: answers,
		http:    &http.Client{Timeout: defaultRequestTimeout},
		now:     now,
	}
}

// Sync posts one sync request.
func (c *controllerClient) Sync(
	ctx context.Context, req *protocol.SyncRequest,
) (*protocol.SyncResponse, error) {
	var resp protocol.SyncResponse

	if err := c.post(ctx, protocol.PathSync, req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// Inventory uploads the package inventory.
func (c *controllerClient) Inventory(ctx context.Context, inv *protocol.Inventory) error {
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
func (c *controllerClient) post(ctx context.Context, path string, in, out any) error {
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
