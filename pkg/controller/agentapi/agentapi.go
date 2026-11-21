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
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

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

	// validatorAuditInterval is the least time between two audit entries
	// about the validator range of one host. A range is something a host
	// says about itself, as often as it likes; every change is logged.
	validatorAuditInterval = time.Minute

	// generationAuditInterval is the least time between two audit entries
	// for rejected certificates of one host: an agent that keeps trying with
	// an old key must not flood the audit log.
	generationAuditInterval = time.Hour

	// agentActorPrefix prefixes the host name in audit entries written on
	// behalf of an agent.
	agentActorPrefix = "agent:"

	// Audit detail keys.
	detailCertHash   = "certHash"
	detailGeneration = "generation"
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
// bounded decompression, the generation floor, the call, and the answer with
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

	// Before anything of the request is parsed or recorded.
	if err = s.checkGeneration(r, cert); err != nil {
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

// checkGeneration rejects a certificate of a generation lower than the
// highest one this controller has accepted for the host. That is what makes
// rotating a host key (issuing the next generation) revoke the old key: once
// the rotated host has synced, the old certificate is refused although the
// signer's signature on it is as valid as ever.
//
// It is a floor, not a pin: any certificate of the current or a higher
// generation is accepted. The floor is the host row, so a controller that
// lost its database knows it again only after the rotated host has synced.
func (s *Handler) checkGeneration(r *http.Request, cert *identity.Certificate) error {
	host, err := s.opts.Store.GetHost(r.Context(), cert.Subject)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("get host %s: %w", cert.Subject, err)
	case cert.Generation >= host.Generation:
		return nil
	}

	remote := s.remoteAddr(r)

	s.log.WithFields(logrus.Fields{
		keyHost:           cert.Subject,
		"remote":          remote,
		"generation":      cert.Generation,
		"seen_generation": host.Generation,
	}).Warn("Rejected agent request: certificate of an older generation")

	if s.generationAuditDue(cert.Subject) {
		s.auditIdentity(r.Context(), []fleet.AuditEntry{{
			Time:   s.now().UTC(),
			Actor:  agentActorPrefix + cert.Subject,
			Action: "host.certificate-rejected",
			Target: cert.Subject,
			Detail: map[string]any{
				detailCertHash:   cert.Hash,
				detailGeneration: cert.Generation,
				"seenGeneration": host.Generation,
				detailRemoteAddr: remote,
			},
		}})
	}

	return agentErrorf(http.StatusUnauthorized,
		"certificate generation %d is older than generation %d already seen for this host",
		cert.Generation, host.Generation)
}

// generationAuditDue reports whether a rejected certificate of the host is to
// be audited now: at most once per generationAuditInterval and host.
func (s *Handler) generationAuditDue(host string) bool {
	s.generationMu.Lock()
	defer s.generationMu.Unlock()

	now := s.now()

	if last, ok := s.generationAudited[host]; ok && now.Sub(last) < generationAuditInterval {
		return false
	}

	if s.generationAudited == nil {
		s.generationAudited = make(map[string]time.Time, 4)
	}

	s.generationAudited[host] = now

	return true
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

	if req.ProtocolVersion != protocol.Version {
		return nil, agentErrorf(http.StatusBadRequest,
			"protocol version %d is not supported, the controller speaks %d",
			req.ProtocolVersion, protocol.Version)
	}

	req.Containers = cleanContainers(req.Containers)
	req.Validators = cleanValidators(req.Validators)

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

	s.auditIdentity(ctx, append(s.identityAudit(cert, result, remote),
		s.validatorAudit(cert, req.Validators, result, remote)...))

	assignment, err := s.opts.Scheduler.Assignment(ctx, cert.Subject)
	if err != nil {
		return nil, fmt.Errorf("assignment of %s: %w", cert.Subject, err)
	}

	resp := &protocol.SyncResponse{ServerTime: now}

	if result != nil && result.Host != nil {
		resp.WantInventory = req.InventoryHash != result.Host.InventoryHash
	}

	interval := fleet.DefaultSettings().LookupInterval.Std()
	if assignment != nil && assignment.LookupInterval > 0 {
		interval = assignment.LookupInterval
	}

	resp.Lookups, resp.Digests = s.lookups.Sync(cert.Subject, &req, now, interval)
	resp.LookupFallbackSeconds = int64(3 * interval / time.Second)

	if assignment != nil {
		resp.Directives = assignment.Directives
		resp.Queue = assignment.Queue
		resp.Ended = assignment.Ended
		resp.Pull = assignment.Pull

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

	// A sync that only confirmed the row changes nothing a viewer or the
	// scheduler sees, unless the host may have been stale before it: silent
	// for longer than the shortest stale bound the settings allow. Such a
	// sync waits for the scheduler's next tick; nudging it on every sync kept
	// the scheduler reconciling back to back.
	if result == nil || result.Host == nil || result.Changed ||
		now.Sub(result.PreviousSeen) > result.Host.StaleAfter(fleet.MinHostStaleAfter) {
		s.opts.Scheduler.Nudge()
		s.opts.Notify()
	}

	s.pacer.observe(time.Since(started))

	return resp, nil
}

// cleanValidators checks the validator range a host reports before it is
// stored and counted. A range that is out of bounds, or a state this build
// does not know, is kept as a report of an invalid range with the reason:
// the host then counts as one that does not know its range, never as one
// with the range it claimed.
func cleanValidators(in *protocol.Validators) *protocol.Validators {
	if in == nil {
		return nil
	}

	out := *in

	if err := out.Validate(); err != nil {
		out.State = protocol.ValidatorsInvalid
		out.Error = "refused by the controller: " + err.Error()
	}

	if !out.Known() {
		out.Start, out.End = 0, 0
	}

	if len(out.Error) > 256 {
		out.Error = out.Error[:256]
	}

	if len(out.Source) > 32 {
		out.Source = out.Source[:32]
	}

	return &out
}

// validatorAudit builds the audit entries about the validator range of a
// sync: one when the range counted for the host changed, and one when the
// host starts to report validators that are counted for other hosts (once
// per episode: until the overlap is gone or is with other hosts). A range is
// what a host says about itself, so neither is refused; both are logged and
// audited, at most one entry of each kind per host and validatorAuditInterval.
func (s *Handler) validatorAudit(
	cert *identity.Certificate, reported *protocol.Validators, result *store.SyncResult,
	remote string,
) []fleet.AuditEntry {
	if result == nil || result.Host == nil {
		return nil
	}

	entries := make([]fleet.AuditEntry, 0, 2)
	before, after := result.ValidatorsBefore, result.Host.Validators
	changed := !result.Created && (before.Count() != after.Count() ||
		(before != nil && after != nil && *before != *after))

	rangeDetail := func(r *identity.ValidatorRange) any {
		if r == nil {
			return nil
		}

		return map[string]any{"start": r.Start, "end": r.End}
	}

	detail := func() map[string]any {
		d := map[string]any{
			"counted":        rangeDetail(after),
			detailRemoteAddr: remote,
		}

		if reported != nil {
			d["reported"] = reported
		}

		return d
	}

	if changed {
		s.log.WithFields(logrus.Fields{
			keyHost: cert.Subject, "before": rangeDetail(before), keyAfter: rangeDetail(after),
		}).Info("Validator range of a host changed")

		if s.validatorAuditDue("changed:" + cert.Subject) {
			d := detail()
			d["before"] = rangeDetail(before)

			entries = append(entries, fleet.AuditEntry{
				Time:   s.now().UTC(),
				Actor:  agentActorPrefix + cert.Subject,
				Action: "host.validators-changed",
				Target: cert.Subject,
				Detail: d,
			})
		}
	}

	if s.overlapEpisodeBegins(cert.Subject, result.ValidatorOverlaps) {
		s.log.WithFields(logrus.Fields{
			keyHost: cert.Subject, "with": result.ValidatorOverlaps,
			"counted": rangeDetail(after),
		}).Warn("Host reports validators that are counted for other hosts; " +
			"no validator is counted twice")

		if s.validatorAuditDue("overlap:" + cert.Subject) {
			d := detail()
			d["with"] = result.ValidatorOverlaps

			entries = append(entries, fleet.AuditEntry{
				Time:   s.now().UTC(),
				Actor:  agentActorPrefix + cert.Subject,
				Action: "host.validators-overlap",
				Target: cert.Subject,
				Detail: d,
			})
		}
	}

	return entries
}

// overlapEpisodeBegins records the hosts a host's reported range overlaps
// with and reports whether that is a new episode: there is an overlap and it
// is not the one recorded.
func (s *Handler) overlapEpisodeBegins(host string, with []string) bool {
	s.generationMu.Lock()
	defer s.generationMu.Unlock()

	key := strings.Join(with, ",")
	if key == "" {
		delete(s.overlapAudited, host)

		return false
	}

	if s.overlapAudited[host] == key {
		return false
	}

	if s.overlapAudited == nil {
		s.overlapAudited = make(map[string]string, 4)
	}

	s.overlapAudited[host] = key

	return true
}

// validatorAuditDue reports whether an audit entry of the kind and host in
// key is to be written now: at most one per validatorAuditInterval.
func (s *Handler) validatorAuditDue(key string) bool {
	s.generationMu.Lock()
	defer s.generationMu.Unlock()

	now := s.now()

	if last, ok := s.validatorsAudited[key]; ok && now.Sub(last) < validatorAuditInterval {
		return false
	}

	if s.validatorsAudited == nil {
		s.validatorsAudited = make(map[string]time.Time, 4)
	}

	s.validatorsAudited[key] = now

	return true
}

// cleanContainers makes what a host reports about its containers unambiguous
// before it is stored, because what is stored is what authorizes. A container
// name stands for one container: Docker has no two of a name, so of several
// entries with one name the first is kept and the others are dropped. Owners
// are kept only in the form a team name has, and only for the containers the
// host manages; a client only in the form a client name has, for a managed
// execution, consensus or validator client.
func cleanContainers(in []protocol.Container) []protocol.Container {
	out := make([]protocol.Container, 0, len(in))
	seen := make(map[string]struct{}, len(in))

	for i := range in {
		c := in[i]

		if _, dup := seen[c.Name]; dup {
			continue
		}

		seen[c.Name] = struct{}{}
		c.Owners = protocol.CleanOwners(c.Owners)

		if !c.Managed {
			c.Owners = nil
		}

		// A client is a name the page lists and a setting is keyed by.
		if !c.Managed || c.Kind == protocol.KindOther || !protocol.ValidClient(c.Client) {
			c.Client = ""
		}

		out = append(out, c)
	}

	return out
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
			detailGeneration: cert.Generation,
			"roles":          cert.Roles,
			"validators":     result.Host.Weight(),
			detailRemoteAddr: remote,
		}

		// The agent reported a controller hold this controller did not know
		// of; the host row was created with the hold wanted.
		if result.HoldAdopted {
			detail["holdAdopted"] = true
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
				detailGeneration: cert.Generation,
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
