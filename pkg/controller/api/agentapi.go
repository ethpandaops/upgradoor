package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	endpointSync      = "sync"
	endpointInventory = "inventory"
	// endpointUnknown labels requests below /api/agent/ that match no
	// endpoint.
	endpointUnknown = "unknown"

	// Bodies are held in memory, twice when compressed. These bound how many
	// are in flight. A slot is only taken by a request whose certificate and
	// proof signature are already verified, so only a host of this network
	// can hold one.
	maxConcurrentSyncs       = 64
	maxConcurrentInventories = 4

	// A request holds its slot while its body is read. These bound that
	// time whatever timeouts the http.Server has, so a host that trickles
	// its body cannot keep a slot.
	syncReadTimeout      = 20 * time.Second
	inventoryReadTimeout = 60 * time.Second

	// auditWriteTimeout bounds the write of identity audit entries, which
	// does not end with the request.
	auditWriteTimeout = 5 * time.Second
	// maxAuditBacklog bounds the identity audit entries kept for another
	// attempt after a failed write.
	maxAuditBacklog = 256

	// agentActorPrefix prefixes the host name in audit entries written on
	// behalf of an agent.
	agentActorPrefix = "agent:"

	// detailCertHash is the audit detail key of a certificate hash.
	detailCertHash = "certHash"
)

// agentError is an error answer of the agent API.
type agentError struct {
	status int
	msg    string
}

func (e *agentError) Error() string { return e.msg }

func agentErrorf(status int, format string, args ...any) *agentError {
	return &agentError{status: status, msg: fmt.Sprintf(format, args...)}
}

// handleSync serves POST /api/agent/v1/sync.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	s.serveAgent(w, r, &agentEndpoint{
		name:        endpointSync,
		slots:       s.syncSlots,
		limit:       protocol.MaxSyncBody,
		readTimeout: s.syncReadTimeout,
		call:        s.sync,
	})
}

// handleInventory serves POST /api/agent/v1/inventory.
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	s.serveAgent(w, r, &agentEndpoint{
		name:        endpointInventory,
		slots:       s.inventorySlots,
		limit:       protocol.MaxInventoryBody,
		readTimeout: s.inventoryReadTimeout,
		call:        s.inventory,
	})
}

// agentCall handles one authenticated agent request: cert is the verified
// certificate, body the decompressed request body.
type agentCall func(r *http.Request, cert *identity.Certificate, body []byte) (any, error)

// agentEndpoint is one agent route with its limits.
type agentEndpoint struct {
	name string
	// slots bounds the requests in flight.
	slots chan struct{}
	// limit bounds the body, on the wire and decompressed.
	limit int64
	// readTimeout bounds the time to read the body.
	readTimeout time.Duration
	call        agentCall
}

// serveAgent is the frame around both agent endpoints. In this order:
// checks that need no body (headers, certificate, proof signature and time
// window), a slot, the bounded read, authentication over the wire bytes,
// bounded decompression, the call, and the answer with its metric.
//
// Nothing is read and no slot is taken for a sender that cannot show a
// certificate of this network and a fresh proof signed by its host key.
func (s *Server) serveAgent(w http.ResponseWriter, r *http.Request, ep *agentEndpoint) {
	pre, err := s.preAuthenticateAgent(r, ep.limit)
	if err != nil {
		s.agentFailure(w, r, ep.name, err)

		return
	}

	select {
	case ep.slots <- struct{}{}:
		defer func() { <-ep.slots }()
	default:
		w.Header().Set("Retry-After", "5")
		s.agentFailure(w, r, ep.name, agentErrorf(http.StatusServiceUnavailable,
			"too many %s requests in flight", ep.name))

		return
	}

	cert, body, err := s.authenticateAgent(w, r, pre, ep)
	if err != nil {
		s.agentFailure(w, r, ep.name, err)

		return
	}

	answer, err := ep.call(r, cert, body)
	if err != nil {
		s.agentFailure(w, r, ep.name, err)

		return
	}

	s.metrics.agentRequest(ep.name, http.StatusOK)
	s.writeJSON(w, http.StatusOK, answer)
}

// agentFailure answers a failed agent request with a protocol.ErrorResponse.
func (s *Server) agentFailure(w http.ResponseWriter, r *http.Request, endpoint string, err error) {
	var ae *agentError
	if !errors.As(err, &ae) {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			ae = agentErrorf(http.StatusServiceUnavailable, "request cancelled")
		} else {
			s.log.WithError(err).WithField("endpoint", endpoint).Error("Agent request failed")

			ae = agentErrorf(http.StatusInternalServerError, "internal error")
		}
	}

	s.metrics.agentRequest(endpoint, ae.status)
	s.writeJSON(w, ae.status, protocol.ErrorResponse{Error: ae.msg, ServerTime: s.now().UTC()})
}

// agentPreAuth is what preAuthenticateAgent established about a request.
type agentPreAuth struct {
	cert     string
	proof    string
	encoding string
	urls     []string
}

// preAuthenticateAgent runs every check that needs no body: the headers, the
// declared size, and the certificate plus the proof's signature and time
// window.
func (s *Server) preAuthenticateAgent(r *http.Request, limit int64) (*agentPreAuth, error) {
	pre := &agentPreAuth{
		cert:  r.Header.Get(identity.HeaderCert),
		proof: r.Header.Get(identity.HeaderProof),
	}

	if pre.cert == "" || pre.proof == "" {
		return nil, agentErrorf(http.StatusUnauthorized,
			"missing %s or %s header", identity.HeaderCert, identity.HeaderProof)
	}

	pre.encoding = strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if pre.encoding != "" && pre.encoding != "identity" && pre.encoding != "gzip" {
		return nil, agentErrorf(http.StatusUnsupportedMediaType,
			"unsupported content encoding %q", pre.encoding)
	}

	urls, err := s.proofURLs(r)
	if err != nil {
		return nil, err
	}

	pre.urls = urls

	// Verify checks the certificate (signer, network), the proof's signature
	// under the certificate's host key and its time window before it compares
	// the proof with the request. Without an acceptable URL it can never
	// succeed, so this call never records the proof's jti: the call with the
	// body below is the one that counts. Everything but the expected
	// mismatch is a failed identity check that needed no body.
	_, err = s.opts.Verifier.Verify(pre.cert, pre.proof, identity.Request{Method: r.Method})

	switch {
	case err == nil:
		return nil, errors.New("identity verifier accepted a request without URL")
	case !errors.Is(err, identity.ErrProofMismatch):
		return nil, s.identityFailure(r, err)
	}

	if r.ContentLength > limit {
		return nil, agentErrorf(http.StatusRequestEntityTooLarge,
			"body larger than %d bytes", limit)
	}

	return pre, nil
}

// identityFailure logs a failed identity check and returns its 401.
func (s *Server) identityFailure(r *http.Request, err error) error {
	s.log.WithError(err).WithField("remote", s.remoteAddr(r)).Warn("Rejected agent request")

	// Identity errors carry no secrets and tell an operator what to fix
	// (wrong network, clock, URL).
	return agentErrorf(http.StatusUnauthorized, "%s", err.Error())
}

// authenticateAgent reads the body under the endpoint's size and time limit,
// verifies the certificate and the proof over the bytes as received, and only
// then decompresses, again under the limit. Nothing of the body is parsed
// before the sender is verified.
func (s *Server) authenticateAgent(
	w http.ResponseWriter, r *http.Request, pre *agentPreAuth, ep *agentEndpoint,
) (*identity.Certificate, []byte, error) {
	wire, err := s.readAgentBody(w, r, ep)
	if err != nil {
		return nil, nil, err
	}

	cert, err := s.opts.Verifier.Verify(pre.cert, pre.proof, identity.Request{
		Method: r.Method,
		URLs:   pre.urls,
		Body:   wire,
	})
	if err != nil {
		return nil, nil, s.identityFailure(r, err)
	}

	if pre.encoding != "gzip" {
		return cert, wire, nil
	}

	body, err := gunzip(wire, ep.limit)
	if err != nil {
		return nil, nil, err
	}

	return cert, body, nil
}

// readAgentBody reads the wire bytes under the endpoint's size limit and
// within its read timeout.
func (s *Server) readAgentBody(
	w http.ResponseWriter, r *http.Request, ep *agentEndpoint,
) ([]byte, error) {
	// The deadline is on the connection, so it holds whatever ReadTimeout the
	// http.Server was given. A writer without a connection (tests with a
	// recorder) does not support it and has no slow sender either.
	rc := http.NewResponseController(w)
	deadlineSet := rc.SetReadDeadline(time.Now().Add(ep.readTimeout)) == nil

	wire, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ep.limit))

	if deadlineSet && err == nil {
		// The server keeps reading the connection after the body to notice
		// a client that goes away; a deadline left in place would end that
		// read and cancel the request's context in the middle of the call.
		if derr := rc.SetReadDeadline(time.Time{}); derr != nil {
			s.log.WithError(derr).Debug("Cannot clear the read deadline")
		}
	}

	if err == nil {
		return wire, nil
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return nil, agentErrorf(http.StatusRequestEntityTooLarge,
			"body larger than %d bytes", ep.limit)
	}

	var netErr net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return nil, agentErrorf(http.StatusRequestTimeout,
			"body not received within %s", ep.readTimeout)
	}

	return nil, agentErrorf(http.StatusBadRequest, "cannot read body")
}

// gunzip decompresses wire and refuses output beyond limit, so a small
// compressed body cannot expand without bound.
func gunzip(wire []byte, limit int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		return nil, agentErrorf(http.StatusBadRequest, "invalid gzip body")
	}

	body, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, agentErrorf(http.StatusBadRequest, "invalid gzip body")
	}

	if int64(len(body)) > limit {
		return nil, agentErrorf(http.StatusRequestEntityTooLarge,
			"body larger than %d bytes after decompression", limit)
	}

	if err := zr.Close(); err != nil {
		return nil, agentErrorf(http.StatusBadRequest, "invalid gzip body")
	}

	return body, nil
}

// proofURLs lists the absolute URLs a proof for this request may name.
func (s *Server) proofURLs(r *http.Request) ([]string, error) {
	// The mux matched one of the two fixed agent paths.
	path := r.URL.Path

	if len(s.publicURLs) > 0 {
		urls := make([]string, 0, len(s.publicURLs))
		for _, base := range s.publicURLs {
			urls = append(urls, base+path)
		}

		return urls, nil
	}

	if r.Host == "" {
		return nil, agentErrorf(http.StatusBadRequest, "missing Host header")
	}

	urls := make([]string, 0, 2)
	urls = append(urls, "https://"+r.Host+path)

	if s.opts.AllowInsecureAgentHTTP {
		urls = append(urls, "http://"+r.Host+path)
	}

	return urls, nil
}

// sync records a host's state and tells it what to do. The host is the
// certificate subject; nothing in the body can name another one.
func (s *Server) sync(r *http.Request, cert *identity.Certificate, body []byte) (any, error) {
	ctx := r.Context()

	var req protocol.SyncRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, agentErrorf(http.StatusBadRequest, "invalid sync request: %s", err.Error())
	}

	if req.ProtocolVersion != protocol.Version {
		return nil, agentErrorf(http.StatusBadRequest,
			"protocol version %d is not supported, the controller speaks %d",
			req.ProtocolVersion, protocol.Version)
	}

	now := s.now().UTC()
	remote := s.remoteAddr(r)

	result, err := s.opts.Store.RecordSync(ctx, &store.HostSync{
		Cert:       cert,
		RemoteAddr: remote,
		Request:    &req,
		At:         now,
	})
	if err != nil {
		return nil, fmt.Errorf("record sync of %s: %w", cert.Subject, err)
	}

	s.auditIdentity(ctx, s.identityAudit(cert, result, remote))

	assignment, err := s.opts.Scheduler.Assignment(ctx, cert.Subject)
	if err != nil {
		return nil, fmt.Errorf("assignment of %s: %w", cert.Subject, err)
	}

	resp := &protocol.SyncResponse{ServerTime: now}

	if result != nil && result.Host != nil {
		resp.WantInventory = req.InventoryHash != result.Host.InventoryHash
	}

	if assignment != nil {
		resp.Directives = assignment.Directives
		resp.Queue = assignment.Queue

		if assignment.Lease != nil {
			// TTLSeconds is defined as the time left when the response is
			// built, which is here.
			lease := *assignment.Lease
			if !lease.ExpiresAt.IsZero() {
				lease.TTLSeconds = max(0, int64(lease.ExpiresAt.Sub(now).Seconds()))
			}

			resp.Lease = &lease
		}
	}

	s.opts.Scheduler.Nudge()

	return resp, nil
}

// identityAudit builds the audit entries for a new host and for a changed
// certificate.
func (s *Server) identityAudit(
	cert *identity.Certificate, result *store.SyncResult, remote string,
) []fleet.AuditEntry {
	if result == nil {
		return nil
	}

	entries := make([]fleet.AuditEntry, 0, 2)
	actor := agentActorPrefix + cert.Subject

	if result.Created {
		entries = append(entries, fleet.AuditEntry{
			Time:   s.now().UTC(),
			Actor:  actor,
			Action: "host.registered",
			Target: cert.Subject,
			Detail: map[string]any{
				detailCertHash: cert.Hash,
				"generation":   cert.Generation,
				"roles":        cert.Roles,
				"validators":   cert.Validators.Count(),
				"remoteAddr":   remote,
			},
		})
	}

	if result.PreviousCertHash != "" && result.PreviousCertHash != cert.Hash {
		entries = append(entries, fleet.AuditEntry{
			Time:   s.now().UTC(),
			Actor:  actor,
			Action: "host.certificate-changed",
			Target: cert.Subject,
			Detail: map[string]any{
				"before":     map[string]any{detailCertHash: result.PreviousCertHash},
				"after":      map[string]any{detailCertHash: cert.Hash},
				"generation": cert.Generation,
				"remoteAddr": remote,
			},
		})
	}

	return entries
}

// auditIdentity writes identity audit entries. The sync that caused them is
// already stored and is reported only once, so the write must not depend on
// the request: it survives a client that went away, and entries that could
// not be written are kept and written with the next sync of any host. A
// failed write is logged loudly and does not fail the sync.
func (s *Server) auditIdentity(ctx context.Context, entries []fleet.AuditEntry) {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	if len(entries) == 0 && len(s.auditBacklog) == 0 {
		return
	}

	pending := make([]fleet.AuditEntry, 0, len(s.auditBacklog)+len(entries))
	pending = append(pending, s.auditBacklog...)
	pending = append(pending, entries...)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()

	err := s.opts.Store.AppendAudit(ctx, pending...)
	if err == nil {
		s.auditBacklog = nil

		return
	}

	for i := range entries {
		// Logged in full: if the process ends before the next attempt, the
		// log is the only record.
		s.log.WithError(err).WithFields(logrus.Fields{
			"action": entries[i].Action,
			"host":   entries[i].Target,
			"detail": entries[i].Detail,
		}).Error("Cannot write identity audit entry, keeping it for the next sync")
	}

	if dropped := len(pending) - maxAuditBacklog; dropped > 0 {
		s.log.WithField("dropped", dropped).
			Error("Identity audit backlog is full, dropping the oldest entries")

		pending = pending[dropped:]
	}

	s.auditBacklog = pending
}

// inventory stores a host's inventory after checking its hash.
func (s *Server) inventory(r *http.Request, cert *identity.Certificate, body []byte) (any, error) {
	ctx := r.Context()

	var inv protocol.Inventory
	if err := json.Unmarshal(body, &inv); err != nil {
		return nil, agentErrorf(http.StatusBadRequest, "invalid inventory: %s", err.Error())
	}

	hash, err := inv.ComputeHash()
	if err != nil {
		return nil, fmt.Errorf("hash inventory of %s: %w", cert.Subject, err)
	}

	if inv.Hash != hash {
		return nil, agentErrorf(http.StatusBadRequest,
			"inventory hash mismatch: the body hashes to %s", hash)
	}

	// An inventory belongs to a host row, which the first sync creates.
	if _, err := s.opts.Store.GetHost(ctx, cert.Subject); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, agentErrorf(http.StatusConflict, "unknown host: sync first")
		}

		return nil, fmt.Errorf("get host %s: %w", cert.Subject, err)
	}

	if err := s.opts.Store.SaveInventory(ctx, cert.Subject, &inv); err != nil {
		return nil, fmt.Errorf("save inventory of %s: %w", cert.Subject, err)
	}

	s.log.WithFields(logrus.Fields{
		"host":     cert.Subject,
		"packages": strconv.Itoa(len(inv.Packages)),
	}).Debug("Stored inventory")

	return &protocol.InventoryResponse{ServerTime: s.now().UTC(), Hash: hash}, nil
}
