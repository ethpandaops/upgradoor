package link

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"slices"
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
	// maxKeys bounds the encryption keys an answer may carry.
	maxKeys = 4
)

// ErrControllerTooOld is returned when the controller does not serve
// protocol.PathHello. The agent then sends nothing: it never falls back to
// unsealed requests.
var ErrControllerTooOld = errors.New("the controller does not serve " + protocol.PathHello +
	": it predates encrypted agent traffic and must be updated; this agent sends no " +
	"host state unencrypted")

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
	// bounded; else syncInterval, or leaseSyncInterval while busy. After
	// failed syncs it backs off: a random wait up to that interval doubled
	// per further failure, at most maxSyncInterval.
	Interval(busy bool) time.Duration
}

type client struct {
	log     logrus.FieldLogger
	cfg     *Config
	baseURL string
	creds   *identity.Credentials
	// answers verifies every answer; nil only with allowUnsignedAnswers.
	answers *identity.AnswerVerifier
	// network and subject are the certificate's, to which sealed bodies are
	// bound.
	network string
	subject string
	// keys are the controller's encryption keys from the last verified
	// answer that carried them, by ActiveFrom.
	keys []*identity.SealKey
	http *http.Client
	now  func() time.Time

	// asked is the interval the last answer asked for; zero after a failed
	// sync, so a vanished controller is not waited for at its own pace.
	asked time.Duration
	// failures counts the syncs that failed since the last answer.
	failures int
	// jitter returns a random number in [0, n).
	jitter func(n int64) int64
	// failing, refused and tooOld describe the current failure episode,
	// logged once.
	failing bool
	refused bool
	tooOld  bool
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
		network: claims.Network,
		subject: claims.Subject,
		http: &http.Client{
			Timeout: requestTimeout,
			// Never follow a redirect: the certificate, proof and body go to
			// the configured controller and nowhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:    now,
		jitter: rand.Int64N,
	}, nil
}

// newAnswerVerifier returns the answer verifier for the configured key;
// nil, with a loud warning, under allowUnsignedAnswers.
func newAnswerVerifier(
	log logrus.FieldLogger, cfg *Config, claims *identity.Claims,
) (*identity.AnswerVerifier, error) {
	key, err := cfg.Key()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}

	if key == nil {
		log.Warn("INSECURE: controller.allowUnsignedAnswers is set and no controller " +
			"public key is configured. The controller's answers are NOT verified: this " +
			"host acts on whatever answers under controller.url. Development only.")

		return nil, nil //nolint:nilnil // no verifier and no error is the opt-out
	}

	answers, err := identity.NewAnswerVerifier(key, claims.Network, claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("controller key: %w", err)
	}

	log.WithField("controller_key_sha256", identity.KeyFingerprint(key)).
		Info("acting only on answers signed by the configured controller key")

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
			c.failures++
			c.failed(err)
		}

		return nil, err
	}

	if c.failing {
		c.log.Info("sync works again")

		c.failing = false
		c.refused = false
		c.tooOld = false
	}

	c.asked = time.Duration(resp.NextSyncMillis) * time.Millisecond
	c.failures = 0

	return &resp, nil
}

// failed logs a failed sync once per episode; a refused answer is logged
// louder than an outage, as it means a wrong key or an impostor.
func (c *client) failed(err error) {
	refused := errors.Is(err, identity.ErrBadAnswer)
	tooOld := errors.Is(err, ErrControllerTooOld)

	switch {
	case refused && !c.refused:
		c.log.WithError(err).Error("refused the controller's answer: it does not carry " +
			"a valid signature of a configured controller key. Acting on nothing of " +
			"it; retrying on every tick")
	case tooOld && !c.tooOld:
		c.log.WithError(err).Error("cannot sync: the controller must be updated first; " +
			"retrying with backoff")
	case !refused && !tooOld && !c.failing:
		c.log.WithError(err).Warn("sync failed; retrying on every tick")
	default:
		c.log.WithError(err).Debug("sync failed")
	}

	c.failing = true
	c.refused = refused
	c.tooOld = tooOld
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

	base, limit := c.cfg.SyncInterval, c.cfg.MaxSyncInterval
	if busy {
		base = c.cfg.LeaseSyncInterval
	}

	if c.failures == 0 {
		return base
	}

	// Full jitter keeps a fleet that lost its controller from retrying in
	// step; the first retry waits at most one interval.
	ceiling := min(base, limit)
	for i := 1; i < c.failures && ceiling < limit; i++ {
		ceiling = min(2*ceiling, limit)
	}

	return max(time.Duration(c.jitter(int64(ceiling))+1), c.cfg.MinSyncInterval)
}

// post sends in sealed to the controller's encryption key, as gzip JSON
// with the certificate and a proof over the bytes as sent, and decodes into
// out only an answer sealed with this request's key and signed for this
// host and this request's proof id; anything else leaves out untouched.
// Without a key it asks the controller for its keys first. An answer that
// asks for new keys is no answer: in is sent again once, sealed to them.
func (c *client) post(ctx context.Context, path string, in, out any) error {
	body, err := gzipJSON(in)
	if err != nil {
		return err
	}

	for attempt := 0; ; attempt++ {
		if len(c.keys) == 0 {
			if err := c.hello(ctx); err != nil {
				return err
			}
		}

		rekeyed, err := c.postSealed(ctx, path, body, out)
		if err != nil || !rekeyed {
			return err
		}

		if attempt > 0 {
			return fmt.Errorf("answer of %s: the controller asked for new keys twice", path)
		}
	}
}

// postSealed posts one sealed request and reports whether the answer was a
// rekey answer, whose keys replace the held ones.
func (c *client) postSealed(ctx context.Context, path string, body []byte, out any) (bool, error) {
	id, err := identity.NewRequestID()
	if err != nil {
		return false, fmt.Errorf("sign request: %w", err)
	}

	key, known := c.sealKey()

	wire, seal, err := identity.SealRequest(key, known, &identity.SealBinding{
		Network: c.network, Subject: c.subject, RequestID: id, Path: path,
	}, body)
	if err != nil {
		return false, fmt.Errorf("seal request: %w", err)
	}

	answer, contentType, err := c.exchange(ctx, path, id, wire, true)
	if err != nil {
		return false, err
	}

	if !isSealed(contentType) {
		return true, c.rekey(path, id, answer)
	}

	if answer, err = seal.Open(answer); err != nil {
		return false, fmt.Errorf("answer of %s: %w: %w", path, identity.ErrBadAnswer, err)
	}

	if answer, err = c.verify(path, id, answer); err != nil {
		return false, err
	}

	if err := json.Unmarshal(answer, out); err != nil {
		return false, fmt.Errorf("decode answer of %s: %w", path, err)
	}

	var carried struct {
		Keys []protocol.EncryptionKey `json:"keys"`
	}

	if json.Unmarshal(answer, &carried) == nil && len(carried.Keys) > 0 {
		if err := c.setKeys(carried.Keys); err != nil {
			return false, fmt.Errorf("answer of %s: %w", path, err)
		}
	}

	return false, nil
}

// rekey takes the keys of an unsealed answer to a sealed request, which is
// only ever a verified rekey answer.
func (c *client) rekey(path, id string, answer []byte) error {
	answer, err := c.verify(path, id, answer)
	if err != nil {
		return err
	}

	var rekey protocol.RekeyResponse
	if err := json.Unmarshal(answer, &rekey); err != nil || !rekey.Rekey || len(rekey.Keys) == 0 {
		return fmt.Errorf("answer of %s: %w: an unsealed answer to a sealed request",
			path, identity.ErrBadAnswer)
	}

	if err := c.setKeys(rekey.Keys); err != nil {
		return fmt.Errorf("answer of %s: %w", path, err)
	}

	c.log.Info("the controller holds none of the encryption keys known here " +
		"(it restarted?); sending again with its current key")

	return nil
}

// hello asks the controller for its encryption keys. Its request carries
// no host state.
func (c *client) hello(ctx context.Context) error {
	body, err := gzipJSON(protocol.HelloRequest{})
	if err != nil {
		return err
	}

	id, err := identity.NewRequestID()
	if err != nil {
		return fmt.Errorf("sign request: %w", err)
	}

	answer, _, err := c.exchange(ctx, protocol.PathHello, id, body, false)
	if err != nil {
		return err
	}

	if answer, err = c.verify(protocol.PathHello, id, answer); err != nil {
		return err
	}

	var hello protocol.HelloResponse
	if err := json.Unmarshal(answer, &hello); err != nil {
		return fmt.Errorf("decode answer of %s: %w", protocol.PathHello, err)
	}

	if err := c.setKeys(hello.Keys); err != nil {
		return fmt.Errorf("answer of %s: %w", protocol.PathHello, err)
	}

	c.log.WithField("key", c.keys[len(c.keys)-1].ID.String()).
		Info("received the controller's encryption keys")

	return nil
}

// setKeys replaces the held encryption keys with keys, which come from a
// verified answer.
func (c *client) setKeys(keys []protocol.EncryptionKey) error {
	if len(keys) == 0 || len(keys) > maxKeys {
		return fmt.Errorf("%d encryption keys", len(keys))
	}

	parsed := make([]*identity.SealKey, 0, len(keys))

	for i := range keys {
		raw, err := base64.RawURLEncoding.DecodeString(keys[i].PublicKey)
		if err != nil {
			return fmt.Errorf("encryption key %s: %w", keys[i].ID, err)
		}

		key, err := identity.NewSealKey(raw, keys[i].ActiveFrom)
		if err != nil {
			return fmt.Errorf("encryption key %s: %w", keys[i].ID, err)
		}

		if key.ID.String() != keys[i].ID {
			return fmt.Errorf("encryption key %s: its id is %s", keys[i].ID, key.ID)
		}

		parsed = append(parsed, key)
	}

	slices.SortFunc(parsed, func(a, b *identity.SealKey) int {
		return a.ActiveFrom.Compare(b.ActiveFrom)
	})

	c.keys = parsed

	return nil
}

// sealKey is the key to seal to, the newest one active by the local clock
// (else the oldest), and the newest other key held, zero for none.
func (c *client) sealKey() (*identity.SealKey, identity.KeyID) {
	now := c.now()
	use := c.keys[0]

	for _, k := range c.keys {
		if !k.ActiveFrom.After(now) {
			use = k
		}
	}

	var known identity.KeyID

	for _, k := range c.keys {
		if k != use {
			known = k.ID
		}
	}

	return use, known
}

// verify checks an answer's signature and returns what it carries; without
// a verifier (allowUnsignedAnswers) a signed answer is an error.
func (c *client) verify(path, id string, answer []byte) ([]byte, error) {
	if c.answers != nil {
		out, err := c.answers.Verify(id, answer)
		if err != nil {
			return nil, fmt.Errorf("answer of %s: %w", path, err)
		}

		return out, nil
	}

	if identity.IsSignedAnswer(answer) {
		return nil, fmt.Errorf("answer of %s: the controller signs its answers and "+
			"controller.allowUnsignedAnswers is set: configure controller.publicKey", path)
	}

	return answer, nil
}

// exchange posts wire with the certificate and a proof with the id id over
// it, and returns a 2xx answer and its content type.
func (c *client) exchange(
	ctx context.Context, path, id string, wire []byte, sealed bool,
) (answer []byte, contentType string, err error) {
	target := c.baseURL + path

	proof, err := c.creds.ProofFor(id, http.MethodPost, target, wire, c.now())
	if err != nil {
		return nil, "", fmt.Errorf("sign request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(wire))
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}

	if sealed {
		req.Header.Set("Content-Type", protocol.ContentTypeSealed)
		req.Header.Set("Accept", protocol.ContentTypeSealed+", application/json")
	} else {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "gzip")
		req.Header.Set("Accept", "application/json")
	}

	req.Header.Set(identity.HeaderCert, c.creds.Certificate())
	req.Header.Set(identity.HeaderProof, proof)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("post %s: %w", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	answer, err = io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read answer of %s: %w", path, err)
	}

	if len(answer) > maxAnswerBytes {
		return nil, "", fmt.Errorf("answer of %s exceeds %d bytes", path, maxAnswerBytes)
	}

	if resp.StatusCode == http.StatusNotFound && path == protocol.PathHello {
		return nil, "", fmt.Errorf("%w (%w)",
			ErrControllerTooOld, controllerError(path, resp.StatusCode, answer, c.now()))
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", controllerError(path, resp.StatusCode, answer, c.now())
	}

	return answer, resp.Header.Get("Content-Type"), nil
}

// isSealed reports whether contentType is protocol.ContentTypeSealed.
func isSealed(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)

	return err == nil && mt == protocol.ContentTypeSealed
}

// gzipJSON encodes v as gzip JSON.
func gzipJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var body bytes.Buffer

	zw := gzip.NewWriter(&body)

	if _, err = zw.Write(raw); err != nil {
		return nil, fmt.Errorf("compress request: %w", err)
	}

	if err = zw.Close(); err != nil {
		return nil, fmt.Errorf("compress request: %w", err)
	}

	return body.Bytes(), nil
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
