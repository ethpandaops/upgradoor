package agentapi

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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	endpointSync      = "sync"
	endpointInventory = "inventory"

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

	// agentActorPrefix prefixes the host name in audit entries written on
	// behalf of an agent.
	agentActorPrefix = "agent:"

	// Audit detail keys.
	detailCertHash   = "certHash"
	detailRemoteAddr = "remoteAddr"
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
func (s *Handler) handleSync(w http.ResponseWriter, r *http.Request) {
	s.serveAgent(w, r, &agentEndpoint{
		name:        endpointSync,
		slots:       s.syncSlots,
		limit:       protocol.MaxSyncBody,
		readTimeout: s.syncReadTimeout,
		call:        s.sync,
	})
}

// handleInventory serves POST /api/agent/v1/inventory.
func (s *Handler) handleInventory(w http.ResponseWriter, r *http.Request) {
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
// bounded decompression, the call, and the answer with
// its metric.
//
// Nothing is read and no slot is taken for a sender that cannot show a
// certificate of this network and a fresh proof signed by its host key.
func (s *Handler) serveAgent(w http.ResponseWriter, r *http.Request, ep *agentEndpoint) {
	start := time.Now()

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

	cert, proofID, body, err := s.authenticateAgent(w, r, pre, ep)
	if err != nil {
		s.agentFailure(w, r, ep.name, err)

		return
	}

	answer, err := ep.call(r, cert, body)
	if err != nil {
		s.agentFailure(w, r, ep.name, err)

		return
	}

	// Every answer is signed. Without a controller key (the development
	// opt-out) it goes out as it is.
	if s.opts.AnswerSigner != nil {
		answer, err = s.signAnswer(cert.Subject, proofID, answer)
		if err != nil {
			s.agentFailure(w, r, ep.name, err)

			return
		}
	}

	s.metrics.AgentRequest(ep.name, http.StatusOK)
	httpjson.Write(s.log, w, http.StatusOK, answer)
	s.metrics.AgentAnswered(ep.name, time.Since(start))
}

// signAnswer wraps an answer into an identity.SignedAnswer for the host and
// the request it answers. The signature is over the answer's bytes exactly as
// they travel inside the wrapper, and names the host and the id of the
// request's proof, so the answer is of no use to another host or for another
// request.
//
// Only answers are signed, never errors: an agent acts on nothing of an
// error whoever sent it, and most errors arise before there is a verified
// request to bind a signature to.
func (s *Handler) signAnswer(host, proofID string, answer any) (json.RawMessage, error) {
	body, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("encode answer for %s: %w", host, err)
	}

	signed, err := s.opts.AnswerSigner.Sign(host, proofID, body)
	if err != nil {
		return nil, fmt.Errorf("sign answer for %s: %w", host, err)
	}

	return signed, nil
}

// agentFailure answers a failed agent request with a protocol.ErrorResponse.
func (s *Handler) agentFailure(w http.ResponseWriter, r *http.Request, endpoint string, err error) {
	var ae *agentError
	if !errors.As(err, &ae) {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			ae = agentErrorf(http.StatusServiceUnavailable, "request cancelled")
		} else {
			s.log.WithError(err).WithField("endpoint", endpoint).Error("Agent request failed")

			ae = agentErrorf(http.StatusInternalServerError, "internal error")
		}
	}

	s.metrics.AgentRequest(endpoint, ae.status)
	httpjson.Write(s.log, w, ae.status, protocol.ErrorResponse{Error: ae.msg, ServerTime: s.now().UTC()})
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
func (s *Handler) preAuthenticateAgent(r *http.Request, limit int64) (*agentPreAuth, error) {
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
	_, _, err = s.opts.Verifier.Verify(pre.cert, pre.proof, identity.Request{Method: r.Method})

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
func (s *Handler) identityFailure(r *http.Request, err error) error {
	s.log.WithError(err).WithField("remote", s.remoteAddr(r)).Warn("Rejected agent request")

	// Identity errors carry no secrets and tell an operator what to fix
	// (wrong network, clock, URL).
	return agentErrorf(http.StatusUnauthorized, "%s", err.Error())
}

// authenticateAgent reads the body under the endpoint's size and time limit,
// verifies the certificate and the proof over the bytes as received, and only
// then decompresses, again under the limit. Nothing of the body is parsed
// before the sender is verified. proofID is the id of the verified proof.
func (s *Handler) authenticateAgent(
	w http.ResponseWriter, r *http.Request, pre *agentPreAuth, ep *agentEndpoint,
) (cert *identity.Certificate, proofID string, body []byte, err error) {
	wire, err := s.readAgentBody(w, r, ep)
	if err != nil {
		return nil, "", nil, err
	}

	cert, proofID, err = s.opts.Verifier.Verify(pre.cert, pre.proof, identity.Request{
		Method: r.Method,
		URLs:   pre.urls,
		Body:   wire,
	})
	if err != nil {
		return nil, "", nil, s.identityFailure(r, err)
	}

	if pre.encoding != "gzip" {
		return cert, proofID, wire, nil
	}

	body, err = gunzip(wire, ep.limit)
	if err != nil {
		return nil, "", nil, err
	}

	return cert, proofID, body, nil
}

// readAgentBody reads the wire bytes under the endpoint's size limit and
// within its read timeout.
func (s *Handler) readAgentBody(
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
func (s *Handler) proofURLs(r *http.Request) ([]string, error) {
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
func (s *Handler) sync(r *http.Request, cert *identity.Certificate, body []byte) (any, error) {
	ctx := r.Context()

	var req protocol.SyncRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, agentErrorf(http.StatusBadRequest, "invalid sync request: %s", err.Error())
	}

	req.Containers = cleanContainers(req.Containers)
	req.Roles = cleanRoles(req.Roles)
	req.Actions = cleanActions(req.Actions)
	req.Hold.Holds = cleanHolds(req.Hold.Holds)

	now := s.now().UTC()
	remote := s.remoteAddr(r)
	started := time.Now()
	pc := s.pacer.current(now, float64(len(s.syncSlots))/float64(cap(s.syncSlots)))

	result, err := s.opts.Store.RecordSync(ctx, &store.HostSync{
		Cert:         cert,
		RemoteAddr:   remote,
		Request:      &req,
		At:           now,
		SyncInterval: pc.bound(),
	})
	if err != nil {
		return nil, fmt.Errorf("record sync of %s: %w", cert.Subject, err)
	}

	s.auditIdentity(ctx, s.identityAudit(cert, result, remote))

	// An agent outside the supported range is recorded, so the host shows
	// with its versions, and gets nothing: no lease, no directive.
	if reason := fleet.Unsupported(req.ProtocolVersion); reason != "" {
		s.recorded(cert.Subject, result, now)

		return nil, agentErrorf(http.StatusBadRequest, "%s", reason)
	}

	assignment, err := s.opts.Scheduler.Assignment(ctx, cert.Subject)
	if err != nil {
		return nil, fmt.Errorf("assignment of %s: %w", cert.Subject, err)
	}

	resp := &protocol.SyncResponse{ServerTime: now}

	if result != nil && result.Host != nil {
		resp.WantInventory = req.InventoryHash != result.Host.InventoryHash
	}

	// Zero keeps the tracker's interval.
	var interval time.Duration
	if assignment != nil {
		interval = assignment.LookupInterval
	}

	resp.Digests = s.lookups.Sync(cert.Subject, req.Tags, interval)

	if assignment != nil {
		resp.Directives = assignment.Directives
		resp.Queue = assignment.Queue
		resp.Ended = assignment.Ended

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

	busy := req.Operation != nil || (assignment != nil && (assignment.Lease != nil ||
		len(assignment.Directives) > 0 ||
		(assignment.Queue != nil && assignment.Queue.Position <= queueBusy)))
	resp.NextSyncMillis = s.pacer.ask(cert.Subject, now, busy, pc).Milliseconds()

	s.recorded(cert.Subject, result, now)
	s.pacer.observe(time.Since(started))

	return resp, nil
}

// recorded tells the scheduler and the views about a recorded sync. A sync
// that only confirmed the row changes nothing the scheduler decides on,
// unless the host may have been stale before it: silent for longer than the
// shortest stale bound the settings allow. Such a sync waits for the
// scheduler's next tick; nudging it on every sync kept the scheduler
// reconciling back to back. Its fresh probe values show in the host's
// details only.
func (s *Handler) recorded(host string, result *store.SyncResult, now time.Time) {
	switch {
	case result == nil || result.Host == nil || result.Changed ||
		now.Sub(result.PreviousSeen) > result.Host.StaleAfter(fleet.MinHostStaleAfter):
		s.opts.Scheduler.Nudge()
		s.opts.Notify()
	case s.opts.NotifyHost != nil:
		s.opts.NotifyHost(host)
	}
}

// cleanRoles keeps the valid role names a host reports, sorted, without
// duplicates and at most protocol.MaxRoles.
func cleanRoles(in []string) []string {
	out := make([]string, 0, min(len(in), protocol.MaxRoles))

	for _, role := range in {
		if identity.ValidateName(role) == nil {
			out = append(out, role)
		}
	}

	slices.Sort(out)
	out = slices.Compact(out)

	return out[:min(len(out), protocol.MaxRoles)]
}

// cleanHolds keeps the holds of valid holders a host reports, one per
// holder, in protocol.HoldOrder and at most protocol.MaxHolds: a team's hold
// is what lets the team lift it.
func cleanHolds(in []protocol.HostHold) []protocol.HostHold {
	out := make([]protocol.HostHold, 0, min(len(in), protocol.MaxHolds))

	for _, h := range in {
		if (h.Holder == protocol.HolderHost || protocol.ValidHolder(h.Holder)) &&
			!slices.ContainsFunc(out, func(o protocol.HostHold) bool { return o.Holder == h.Holder }) {
			out = append(out, h)
		}
	}

	slices.SortFunc(out, protocol.HoldOrder)

	if len(out) == 0 {
		return nil
	}

	return out[:min(len(out), protocol.MaxHolds)]
}

// cleanContainers makes what a host reports about its containers unambiguous
// before it is stored, because what is stored is what authorizes. A container
// name stands for one container: Docker has no two of a name, so of several
// entries with one name the first is kept and the others are dropped. A kind
// and a team are kept only in the form of a name, and only for the
// containers the host manages; a team only next to a kind.
func cleanContainers(in []protocol.Container) []protocol.Container {
	out := make([]protocol.Container, 0, len(in))
	seen := make(map[string]struct{}, len(in))

	for i := range in {
		c := in[i]

		if _, dup := seen[c.Name]; dup {
			continue
		}

		seen[c.Name] = struct{}{}
		// A team is a name that authorizes, the page lists and a setting is
		// keyed by.
		if !c.Managed || c.Kind == protocol.KindNone || (c.Kind != "" && !protocol.ValidKind(c.Kind)) {
			c.Kind = ""
		}

		if c.Kind == "" || !protocol.ValidOwner(c.Team) {
			c.Team = ""
		}

		out = append(out, c)
	}

	return out
}

// cleanActions keeps the actions a host reports with a valid name, the
// first of each name, sorted and at most protocol.MaxActions. What is stored
// authorizes: an unknown feature counts as admin, a team that is not a name
// is dropped, an unknown impact counts as node and an unknown condition as
// none.
func cleanActions(in []protocol.ActionInfo) []protocol.ActionInfo {
	out := make([]protocol.ActionInfo, 0, min(len(in), protocol.MaxActions))
	seen := make(map[string]struct{}, len(in))

	for i := range in {
		a := in[i]

		if _, dup := seen[a.Name]; dup || !protocol.ValidActionName(a.Name) {
			continue
		}

		seen[a.Name] = struct{}{}

		if !a.Impact.Valid() {
			a.Impact = protocol.ImpactNode
		}

		if a.Feature != "" && !protocol.ValidActionFeature(a.Feature) {
			a.Feature = authz.FeatureAdmin
		}

		if !a.When.Valid() {
			a.When, a.Due = "", false
		}

		if len(a.Description) > protocol.MaxActionDescription {
			a.Description = strings.ToValidUTF8(a.Description[:protocol.MaxActionDescription], "")
		}

		a.Teams = protocol.CleanOwners(a.Teams)
		out = append(out, a)
	}

	slices.SortFunc(out, func(a, b protocol.ActionInfo) int { return strings.Compare(a.Name, b.Name) })

	return out[:min(len(out), protocol.MaxActions)]
}

// identityAudit builds the audit entries for a new host and for a changed
// certificate.
func (s *Handler) identityAudit(
	cert *identity.Certificate, result *store.SyncResult, remote string,
) []fleet.AuditEntry {
	if result == nil {
		return nil
	}

	entries := make([]fleet.AuditEntry, 0, 2)
	actor := agentActorPrefix + cert.Subject

	if result.Created {
		detail := map[string]any{
			detailCertHash:   cert.Hash,
			"roles":          result.Host.Roles,
			"validators":     result.Host.Weight(),
			detailRemoteAddr: remote,
		}

		entries = append(entries, fleet.AuditEntry{
			Time:   s.now().UTC(),
			Actor:  actor,
			Action: "host.registered",
			Target: cert.Subject,
			Detail: detail,
		})
	}

	if result.PreviousCertHash != "" && result.PreviousCertHash != cert.Hash {
		entries = append(entries, fleet.AuditEntry{
			Time:   s.now().UTC(),
			Actor:  actor,
			Action: "host.certificate-changed",
			Target: cert.Subject,
			Detail: map[string]any{
				"before":         map[string]any{detailCertHash: result.PreviousCertHash},
				"after":          map[string]any{detailCertHash: cert.Hash},
				detailRemoteAddr: remote,
			},
		})
	}

	return entries
}

// auditIdentity writes identity audit entries. The sync that caused them is
// already stored and is reported only once, so the write must not depend on
// the request: it survives a client that went away. A failed write does not
// fail the sync; the entries are logged in full, which is then their only
// record.
func (s *Handler) auditIdentity(ctx context.Context, entries []fleet.AuditEntry) {
	if len(entries) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteTimeout)
	defer cancel()

	err := s.opts.Store.AppendAudit(ctx, entries...)
	if err == nil {
		return
	}

	for i := range entries {
		s.log.WithError(err).WithFields(logrus.Fields{
			"action": entries[i].Action,
			"host":   entries[i].Target,
			"detail": entries[i].Detail,
		}).Error("Cannot write identity audit entry")
	}
}

// inventory stores a host's inventory after checking its hash.
func (s *Handler) inventory(r *http.Request, cert *identity.Certificate, body []byte) (any, error) {
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
		// The agent's fault, not the controller's: say so instead of a 500
		// the agent would retry unchanged.
		if errors.Is(err, store.ErrInventoryInvalid) {
			return nil, agentErrorf(http.StatusBadRequest, "invalid inventory: %s", err.Error())
		}

		return nil, fmt.Errorf("save inventory of %s: %w", cert.Subject, err)
	}

	s.log.WithFields(logrus.Fields{
		"host":     cert.Subject,
		"packages": strconv.Itoa(len(inv.Packages)),
	}).Debug("Stored inventory")

	s.opts.Notify()

	return &protocol.InventoryResponse{ServerTime: s.now().UTC(), Hash: hash}, nil
}
