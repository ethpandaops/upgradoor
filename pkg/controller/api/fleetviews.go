package api

import (
	"context"
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
	// hostLeases and hostHistory bound the host detail.
	hostLeases  = 20
	hostHistory = 50

	maxPackageQuery = 128

	// The package and audit limits are the store's.
	defaultRolloutLimit = 20
	maxRolloutLimit     = 200

	maxAuditFilter = 256
)

// Names that several encodings share: a log field, a key of audit details, a
// wire field and, for keyAfter, the paging parameter of the package list.
const (
	keyHost  = "host"
	keyAfter = "after"
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
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request, _ *user) {
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
	out := Hosts{Hosts: make([]HostListing, 0, len(hosts))}

	for _, host := range hosts {
		sum := hostSummary(host, plan, now, settings.HostStaleAfter.Std())
		out.Hosts = append(out.Hosts, hostListing(&sum))
	}

	s.writeJSON(w, http.StatusOK, out)
}

// handleHost serves GET /api/v1/hosts/{name}.
func (s *Server) handleHost(w http.ResponseWriter, r *http.Request, _ *user) {
	detail, err := s.hostDetail(r.Context(), r.PathValue("name"))

	switch {
	case err == nil:
		s.writeJSON(w, http.StatusOK, detail)
	case errors.Is(err, store.ErrNotFound):
		s.writeError(w, http.StatusNotFound, "unknown host")
	default:
		s.internalError(w, r, err)
	}
}

// hostDetail builds the detail view of one host. An unknown host is
// store.ErrNotFound.
func (s *Server) hostDetail(ctx context.Context, name string) (*HostDetail, error) {
	host, err := s.opts.Store.GetHost(ctx, name)
	if err != nil {
		return nil, err
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		return nil, err
	}

	detail := &HostDetail{
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

	if host.ReportedValidators() != nil {
		all, listErr := s.opts.Store.ListHosts(ctx)
		if listErr != nil {
			return nil, listErr
		}

		detail.ValidatorOverlaps = host.ReportedOverlaps(all)
	}

	inv, err := s.opts.Store.GetInventory(ctx, name)

	switch {
	case err == nil:
		detail.Inventory = inv
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	detail.Leases, err = s.opts.Store.ListLeases(ctx, store.LeaseFilter{
		Host: name, Limit: hostLeases,
	})
	if err != nil {
		return nil, err
	}

	detail.History, err = s.opts.Store.ListAudit(ctx, store.AuditFilter{
		Target: name, Limit: hostHistory,
	})
	if err != nil {
		return nil, err
	}

	if detail.Leases == nil {
		detail.Leases = []*fleet.Lease{}
	}

	if detail.History == nil {
		detail.History = []fleet.AuditEntry{}
	}

	return detail, nil
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
		HoldBy:     holdBy(host),
		Offline:    offlineMark(host, now),
		Broken:     host.Broken,
		Containers: []protocol.Container{},
		Verbs:      []protocol.Verb{},
		Needs:      []protocol.Need{},
		Plan:       scheduler.HostPlan{Slot: scheduler.SlotIdle, Weight: host.Weight()},
	}

	if status := host.Status; status != nil {
		sum.ValidatorReport = status.Validators
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

// hostListing is the row of a host in the host list.
func hostListing(sum *HostSummary) HostListing {
	out := HostListing{
		Name: sum.Name, Roles: sum.Roles, Validators: sum.Validators, LastSeen: sum.LastSeen,
		Stale: sum.Stale, AgentVersion: sum.AgentVersion,
		Containers: make([]ContainerListing, 0, len(sum.Containers)),
		OS:         sum.OS, Pending: sum.Pending, UptimeSeconds: sum.UptimeSeconds, Hold: sum.Hold,
		HoldWanted: sum.HoldWanted, HoldReason: sum.HoldReason, HoldBy: sum.HoldBy, Offline: sum.Offline, Verbs: sum.Verbs,
		Needs: make([]NeedListing, 0, len(sum.Needs)), Plan: sum.Plan,
	}

	for i := range sum.Containers {
		c := &sum.Containers[i]
		out.Containers = append(out.Containers, ContainerListing{
			Name: ContainerName(c.Name), Image: c.Image, Digest: c.Digest, Revision: c.Revision,
			State: c.State, Managed: c.Managed, Kind: c.Kind, Impact: c.Impact, Owners: c.Owners,
		})
	}

	for i := range sum.Needs {
		out.Needs = append(out.Needs, NeedListing{Kind: sum.Needs[i].Kind})
	}

	return out
}

// offlineMark is the host's known-offline mark while it is in force.
func offlineMark(host *fleet.Host, now time.Time) *fleet.OfflineMark {
	if !host.KnownOffline(now) {
		return nil
	}

	return host.Offline
}

// holdBy says for the host views who set the controller hold: "team" for a
// hold set through a team feature, "staff" for every other hold, nothing for
// a host that is not held.
func holdBy(host *fleet.Host) string {
	switch {
	case !host.HoldWanted:
		return ""
	case host.HoldBy == fleet.HoldByTeam:
		return fleet.HoldByTeam
	default:
		return holdByStaff
	}
}

// nonNil returns an empty slice for nil, so JSON carries [] and not null.
func nonNil[T any](in []T) []T {
	if in == nil {
		return []T{}
	}

	return in
}

// handlePackages serves GET /api/v1/packages?q=<part of a name>&limit=&after=:
// one page of the packages installed across the fleet, one entry per name
// with its versions and the hosts on each. Without q it lists every package
// in alphabetical order; with q the names that contain it, the best match
// first. "after" is the "next" of the page before.
func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request, _ *user) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	after := r.URL.Query().Get(keyAfter)

	if len(query) > maxPackageQuery || len(after) > maxPackageQuery+1 {
		s.writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"q must be at most %d characters", maxPackageQuery))

		return
	}

	limit, err := queryLimit(r, store.DefaultPackageSearchLimit, store.MaxPackageSearchLimit)
	if err != nil {
		s.writeBadRequest(w, err)

		return
	}

	page, err := s.opts.Store.ListPackages(r.Context(), store.PackageFilter{
		Query: query, After: after, Limit: limit,
	})

	switch {
	case errors.Is(err, store.ErrBadCursor):
		s.writeError(w, http.StatusBadRequest, "after is not the next of a page")

		return
	case err != nil:
		s.internalError(w, r, err)

		return
	}

	out := Packages{Query: query, Matches: nonNil(page.Matches), Next: page.Next}
	if page.Total >= 0 {
		out.Total = &page.Total
	}

	s.writeJSON(w, http.StatusOK, out)
}

// handleRollouts serves GET /api/v1/rollouts.
func (s *Server) handleRollouts(w http.ResponseWriter, r *http.Request, _ *user) {
	limit, err := queryLimit(r, defaultRolloutLimit, maxRolloutLimit)
	if err != nil {
		s.writeBadRequest(w, err)

		return
	}

	out, err := s.rollouts(r.Context(), limit)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, out)
}

// rollouts builds the rollout view: everything open, and the newest limit
// closed rollouts and leases.
func (s *Server) rollouts(ctx context.Context, limit int) (*Rollouts, error) {
	open, err := s.opts.Store.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
	if err != nil {
		return nil, err
	}

	// The store filters open or all. The newest limit+len(open) rollouts
	// hold at least the newest `limit` closed ones.
	newest, err := s.opts.Store.ListRollouts(ctx, store.RolloutFilter{Limit: limit + len(open)})
	if err != nil {
		return nil, err
	}

	openLeases, err := s.opts.Store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return nil, err
	}

	newestLeases, err := s.opts.Store.ListLeases(ctx, store.LeaseFilter{
		Limit: limit + len(openLeases),
	})
	if err != nil {
		return nil, err
	}

	out := &Rollouts{
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

	return out, nil
}

// handlePlan serves GET /api/v1/plan.
func (s *Server) handlePlan(w http.ResponseWriter, _ *http.Request, _ *user) {
	s.writeJSON(w, http.StatusOK, s.plan())
}

// plan returns the last plan in the form the API serves it, or an empty one
// around the current gate before the first reconcile.
func (s *Server) plan() scheduler.Plan {
	plan := s.opts.Scheduler.Plan()
	if plan == nil {
		return scheduler.Plan{
			Gate:  s.gate(),
			Queue: []scheduler.QueueEntry{},
			Hosts: map[string]scheduler.HostPlan{},
		}
	}

	out := *plan
	out.Gate.Reasons = nonNil(out.Gate.Reasons)
	out.Queue = nonNil(out.Queue)

	if out.Hosts == nil {
		out.Hosts = map[string]scheduler.HostPlan{}
	}

	return out
}

// handleGetSettings serves GET /api/v1/settings.
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, _ *user) {
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

// handleAudit serves GET /api/v1/audit, newest first, paged by "before".
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, _ *user) {
	q := r.URL.Query()

	limit, err := queryLimit(r, store.DefaultAuditLimit, store.MaxAuditLimit)
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
