package link

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// requestTimeout keeps a controller that never answers from stalling the loop.
	requestTimeout = 30 * time.Second
	// maxAnswerBytes bounds what the agent reads of a controller answer.
	maxAnswerBytes = 4 << 20
	// maxErrorText bounds the quoted text of a non-JSON error answer.
	maxErrorText = 200
)

// Client posts signed requests to the controller and returns only answers
// signed for this host and request. Not safe for concurrent use.
type Client interface {
	// Sync posts one sync request and returns the verified answer; an
	// answer that is not the controller's wraps identity.ErrBadAnswer. It
	// logs each failure episode once.
	Sync(ctx context.Context, req *protocol.SyncRequest) (*protocol.SyncResponse, error)
	// Inventory uploads the package inventory, signed and verified like Sync.
	Inventory(ctx context.Context, inv *protocol.Inventory) error
	// Interval is the time to the next sync: what the last answer asked for,
	// bounded; else syncInterval, or leaseSyncInterval while busy.
	Interval(busy bool) time.Duration
	// Silent reports whether the controller has not answered for its lookup
	// fallback (def when no answer named one); true before the first answer.
	Silent(def time.Duration) bool
}

type client struct {
	log     logrus.FieldLogger
	cfg     *Config
	baseURL string
	creds   *identity.Credentials
	// answers verifies every answer; nil only with allowUnsignedAnswers.
	answers *identity.AnswerVerifier
	http    *http.Client
	now     func() time.Time

	// asked is the interval the last answer asked for; zero after a failed
	// sync, so a vanished controller is not waited for at its own pace.
	asked time.Duration
	// lastAnswer is when the controller last answered; fallback is the last
	// lookupFallbackSeconds named.
	lastAnswer time.Time
	fallback   time.Duration
	// failing and refused describe the current failure episode, logged once.
	failing bool
	refused bool
}

var _ Client = (*client)(nil)

// New returns the client of the controller cfg names, for the host and
// network of the certificate in creds.
func New(
	log logrus.FieldLogger, cfg *Config, creds *identity.Credentials, now func() time.Time,
) (Client, error) {
	log = log.WithField("package", "link")

	claims, err := identity.UnverifiedClaims(creds.Certificate())
	if err != nil {
		return nil, fmt.Errorf("host certificate: %w", err)
	}

	answers, err := newAnswerVerifier(log, cfg, claims)
	if err != nil {
		return nil, err
	}

	return &client{
		log:     log,
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.URL, "/"),
		creds:   creds,
		answers: answers,
		http: &http.Client{
			Timeout: requestTimeout,
			// Never follow a redirect: the certificate, proof and body go to
			// the configured controller and nowhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: now,
	}, nil
}

// newAnswerVerifier returns the answer verifier for the configured keys;
// nil, with a loud warning, under allowUnsignedAnswers.
func newAnswerVerifier(
	log logrus.FieldLogger, cfg *Config, claims *identity.Claims,
) (*identity.AnswerVerifier, error) {
	keys, err := cfg.Keys()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	if len(keys) == 0 {
		log.Warn("INSECURE: controller.allowUnsignedAnswers is set and no controller " +
			"public key is configured. The controller's answers are NOT verified: this " +
			"host acts on whatever answers under controller.url. Development only.")

		return nil, nil //nolint:nilnil // no verifier and no error is the opt-out
	}

	answers, err := identity.NewAnswerVerifier(keys, claims.Network, claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("controller keys: %w", err)
	}

	prints := make([]string, 0, len(keys))
	for _, key := range keys {
		prints = append(prints, identity.KeyFingerprint(key))
	}

	log.WithField("controller_keys_sha256", prints).
		Info("acting only on answers signed by one of the configured controller keys")

	return answers, nil
}

// Sync implements Client.
func (c *client) Sync(
	ctx context.Context, req *protocol.SyncRequest,
) (*protocol.SyncResponse, error) {
	var resp protocol.SyncResponse

	if err := c.post(ctx, protocol.PathSync, req, &resp); err != nil {
		c.asked = 0

		// A sync cut short by the agent stopping is no episode.
		if ctx.Err() == nil {
			c.failed(err)
		}

		return nil, err
	}

	if c.failing {
		c.log.Info("sync works again")

		c.failing = false
		c.refused = false
	}

	c.asked = time.Duration(resp.NextSyncMillis) * time.Millisecond
	c.lastAnswer = c.now()

	if resp.LookupFallbackSeconds > 0 {
		c.fallback = time.Duration(resp.LookupFallbackSeconds) * time.Second
	}

	return &resp, nil
}

// failed logs a failed sync once per episode; a refused answer is logged
// louder than an outage, as it means a wrong key or an impostor.
func (c *client) failed(err error) {
	refused := errors.Is(err, identity.ErrBadAnswer)

	switch {
	case refused && !c.refused:
		c.log.WithError(err).Error("refused the controller's answer: it does not carry " +
			"a valid signature of a configured controller key. Acting on nothing of " +
			"it; retrying on every tick")
	case !refused && !c.failing:
		c.log.WithError(err).Warn("sync failed; retrying on every tick")
	default:
		c.log.WithError(err).Debug("sync failed")
	}

	c.failing = true
	c.refused = refused
}

// Inventory implements Client.
func (c *client) Inventory(ctx context.Context, inv *protocol.Inventory) error {
	var resp protocol.InventoryResponse

	return c.post(ctx, protocol.PathInventory, inv, &resp)
}

// Interval implements Client.
func (c *client) Interval(busy bool) time.Duration {
	if c.asked > 0 {
		return min(max(c.asked, c.cfg.MinSyncInterval), c.cfg.MaxSyncInterval)
	}

	if busy {
		return c.cfg.LeaseSyncInterval
	}

	return c.cfg.SyncInterval
}

// Silent implements Client.
func (c *client) Silent(def time.Duration) bool {
	if c.lastAnswer.IsZero() {
		return true
	}

	fallback := c.fallback
	if fallback <= 0 {
		fallback = def
	}

	return c.now().Sub(c.lastAnswer) > fallback
}

// post sends in as gzip JSON with the certificate and a proof over the bytes
// as sent, and decodes into out only an answer signed for this host and this
// request's proof id; anything else leaves out untouched.
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

	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil {
		return fmt.Errorf("read answer of %s: %w", path, err)
	}

	if len(answer) > maxAnswerBytes {
		return fmt.Errorf("answer of %s exceeds %d bytes", path, maxAnswerBytes)
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

// controllerError turns a non-2xx answer into an error, with the controller's
// and the local time, since authentication failures are usually clock skew.
func controllerError(path string, status int, answer []byte, now time.Time) error {
	var er protocol.ErrorResponse

	if err := json.Unmarshal(answer, &er); err != nil || er.Error == "" {
		text := strings.TrimSpace(string(answer))
		if len(text) > maxErrorText {
			text = text[:maxErrorText]
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
