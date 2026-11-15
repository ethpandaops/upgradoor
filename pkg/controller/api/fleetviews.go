package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// hostLivenessEpochs, hostLeases and hostHistory bound the host detail.
	hostLivenessEpochs = 32
	hostLeases         = 20
	hostHistory        = 50

	minPackageQuery     = 2
	maxPackageQuery     = 128
	defaultPackageLimit = 50
	maxPackageLimit     = 500

	defaultRolloutLimit = 20
	maxRolloutLimit     = 200

	defaultAuditLimit = 100
	maxAuditLimit     = 500
	maxAuditFilter    = 256
)

// errBadRequest marks request validation errors, answered 400.
var errBadRequest = errors.New("bad request")

func badRequestf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// writeBadRequest answers a validation error without the errBadRequest
// prefix.
func (s *Server) writeBadRequest(w http.ResponseWriter, err error) {
	s.writeError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), errBadRequest.Error()+": "))
}

// queryLimit parses the "limit" query parameter.
func queryLimit(r *http.Request, def, maximum int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, badRequestf("limit must be a positive integer")
	}

	return min(limit, maximum), nil
}

// decodeBody reads a JSON request body into v. The body must be a single
// JSON object without unknown fields, sent as application/json. Requiring
// the content type keeps cross-site form posts out when the API runs open.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get(headerContentType))
	if err != nil || mediaType != "application/json" {
		return errUnsupportedMedia
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxUserBody))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}

		return badRequestf("invalid JSON body: %s", err.Error())
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return badRequestf("invalid JSON body: trailing data")
	}

	return nil
}

var (
	errUnsupportedMedia = errors.New("the body must be application/json")
	errBodyTooLarge     = errors.New("the body is too large")
)

// writeBodyError answers an error of decodeBody.
func (s *Server) writeBodyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUnsupportedMedia):
		s.writeError(w, http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, errBodyTooLarge):
		s.writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	default:
		s.writeBadRequest(w, err)
	}
}

// handleHosts serves GET /api/v1/hosts.
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()

	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	plan := s.opts.Scheduler.Plan()
	now := s.now()
	out := Hosts{Hosts: make([]HostSummary, 0, len(hosts))}

	for _, host := range hosts {
		out.Hosts = append(out.Hosts, hostSummary(host, plan, now, settings.HostStaleAfter.Std()))
	}

	s.writeJSON(w, http.StatusOK, out)
}

// handleHost serves GET /api/v1/hosts/{name}.
func (s *Server) handleHost(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()
	name := r.PathValue("name")

	host, err := s.opts.Store.GetHost(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, http.StatusNotFound, "unknown host")

			return
		}

		s.internalError(w, r, err)

		return
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	detail := HostDetail{
		HostSummary: hostSummary(host, s.opts.Scheduler.Plan(), s.now(),
			settings.HostStaleAfter.Std()),
		CertHash:   host.CertHash,
		Generation: host.Generation,
		FirstSeen:  host.FirstSeen,
		RemoteAddr: host.RemoteAddr,
	}

	if host.Validators != nil {
		start, end := host.Validators.Start, host.Validators.End
		detail.ValidatorStart, detail.ValidatorEnd = &start, &end
	}

	inv, err := s.opts.Store.GetInventory(ctx, name)

	switch {
	case err == nil:
		detail.Inventory = inv
	case !errors.Is(err, store.ErrNotFound):
		s.internalError(w, r, err)

		return
	}

	liveness, err := s.opts.Store.ListLiveness(ctx, hostLivenessEpochs)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	detail.Liveness = make([]fleet.Liveness, 0, hostLivenessEpochs)

	for _, row := range liveness {
		if row.Host == name {
			detail.Liveness = append(detail.Liveness, row)
		}
	}

	detail.Leases, err = s.opts.Store.ListLeases(ctx, store.LeaseFilter{
		Host: name, Limit: hostLeases,
	})
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	detail.History, err = s.opts.Store.ListAudit(ctx, store.AuditFilter{
		Target: name, Limit: hostHistory,
	})
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	if detail.Leases == nil {
		detail.Leases = []*fleet.Lease{}
	}

	if detail.History == nil {
		detail.History = []fleet.AuditEntry{}
	}

	s.writeJSON(w, http.StatusOK, detail)
}

// hostSummary builds one row of the authenticated host table.
func hostSummary(
	host *fleet.Host, plan *scheduler.Plan, now time.Time, staleAfter time.Duration,
) HostSummary {
	sum := HostSummary{
		Name:       host.Name,
		Roles:      nonNil(host.Roles),
		Validators: host.Weight(),
		LastSeen:   host.LastSeen,
		Stale:      host.Stale(now, staleAfter),
		HoldWanted: host.HoldWanted,
		HoldReason: host.HoldReason,
		Containers: []protocol.Container{},
		Verbs:      []protocol.Verb{},
		Needs:      []protocol.Need{},
		Plan:       scheduler.HostPlan{Slot: scheduler.SlotIdle, Weight: host.Weight()},
	}

	if status := host.Status; status != nil {
		sum.AgentVersion = status.AgentVersion
		sum.Containers = nonNil(status.Containers)
		sum.Node = status.Node
		sum.OS = status.OS
		sum.Pending = status.Pending
		sum.UptimeSeconds = status.UptimeSeconds
		sum.Hold = status.Hold
		sum.Verbs = nonNil(status.Verbs)
		sum.Needs = nonNil(status.Needs)
	}

	if plan != nil {
		if hp, ok := plan.Hosts[host.Name]; ok {
			sum.Plan = hp
		}
	}

	return sum
}

// nonNil returns an empty slice for nil, so JSON carries [] and not null.
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}

	return in
}

// handlePackages serves GET /api/v1/packages.
func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request, _ string) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) < minPackageQuery || len(query) > maxPackageQuery {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"q must be between %d and %d characters", minPackageQuery, maxPackageQuery))

		return
	}

	limit, err := queryLimit(r, defaultPackageLimit, maxPackageLimit)
	if err != nil {
		s.writeBadRequest(w, err)

		return
	}

	matches, err := s.opts.Store.SearchPackages(r.Context(), query, limit)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, Packages{Query: query, Matches: nonNil(matches)})
}

// handleRollouts serves GET /api/v1/rollouts.
func (s *Server) handleRollouts(w http.ResponseWriter, r *http.Request, _ string) {
	ctx := r.Context()

	limit, err := queryLimit(r, defaultRolloutLimit, maxRolloutLimit)
	if err != nil {
		s.writeBadRequest(w, err)

		return
	}

	open, err := s.opts.Store.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	// The store filters open or all. The newest limit+len(open) rollouts
	// hold at least the newest `limit` closed ones.
	newest, err := s.opts.Store.ListRollouts(ctx, store.RolloutFilter{Limit: limit + len(open)})
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	openLeases, err := s.opts.Store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	newestLeases, err := s.opts.Store.ListLeases(ctx, store.LeaseFilter{
		Limit: limit + len(openLeases),
	})
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	out := Rollouts{
		Open:         nonNil(open),
		Recent:       make([]*fleet.Rollout, 0, limit),
		Leases:       nonNil(openLeases),
		RecentLeases: make([]*fleet.Lease, 0, limit),
	}

	for _, rollout := range newest {
		if rollout.State.Terminal() && len(out.Recent) < limit {
			out.Recent = append(out.Recent, rollout)
		}
	}

	for _, lease := range newestLeases {
		if lease.State.Terminal() && len(out.RecentLeases) < limit {
			out.RecentLeases = append(out.RecentLeases, lease)
		}
	}

	s.writeJSON(w, http.StatusOK, out)
}

// handlePlan serves GET /api/v1/plan.
func (s *Server) handlePlan(w http.ResponseWriter, _ *http.Request, _ string) {
	plan := s.opts.Scheduler.Plan()
	if plan == nil {
		s.writeJSON(w, http.StatusOK, scheduler.Plan{
			Gate:  s.gate(),
			Queue: []scheduler.QueueEntry{},
			Hosts: map[string]scheduler.HostPlan{},
		})

		return
	}

	out := *plan
	out.Gate.Reasons = nonNil(out.Gate.Reasons)
	out.Queue = nonNil(out.Queue)

	if out.Hosts == nil {
		out.Hosts = map[string]scheduler.HostPlan{}
	}

	s.writeJSON(w, http.StatusOK, out)
}

// handleGetSettings serves GET /api/v1/settings.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, _ string) {
	settings, err := s.opts.Scheduler.Settings(r.Context())
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, SettingsBody{
		Settings: settings,
		Defaults: s.opts.DefaultSettings,
	})
}

// handlePutSettings serves PUT /api/v1/settings. The body replaces all
// settings; the scheduler stores and audits the change.
func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request, actor string) {
	ctx := r.Context()

	var settings fleet.Settings
	if err := decodeBody(w, r, &settings); err != nil {
		s.writeBodyError(w, err)

		return
	}

	if err := settings.Validate(); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())

		return
	}

	if err := s.opts.Scheduler.UpdateSettings(ctx, actor, settings); err != nil {
		s.writeSchedulerError(w, r, err)

		return
	}

	stored, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, SettingsBody{
		Settings: stored,
		Defaults: s.opts.DefaultSettings,
	})
}

// handleAudit serves GET /api/v1/audit, newest first, paged by "before".
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()

	limit, err := queryLimit(r, defaultAuditLimit, maxAuditLimit)
	if err != nil {
		s.writeBadRequest(w, err)

		return
	}

	filter := store.AuditFilter{
		Target:       q.Get("target"),
		ActionPrefix: q.Get("action"),
		// One more than asked tells whether another page exists.
		Limit: limit + 1,
	}

	if len(filter.Target) > maxAuditFilter || len(filter.ActionPrefix) > maxAuditFilter {
		s.writeError(w, http.StatusBadRequest, "target or action filter too long")

		return
	}

	if raw := q.Get("before"); raw != "" {
		filter.BeforeID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || filter.BeforeID < 0 {
			s.writeError(w, http.StatusBadRequest, "before must be a non-negative integer")

			return
		}
	}

	entries, err := s.opts.Store.ListAudit(r.Context(), filter)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	out := Audit{Entries: nonNil(entries)}

	if len(out.Entries) > limit {
		out.Entries = out.Entries[:limit]
		out.NextBefore = out.Entries[limit-1].ID
	}

	s.writeJSON(w, http.StatusOK, out)
}
