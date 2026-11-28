package userapi

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

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

const (
	maxPackageQuery = 128
	maxImageQuery   = 256

	// The package and audit limits are the store's.
	defaultRolloutLimit = 20
	maxRolloutLimit     = 200

	maxAuditFilter = 256
)

// queryLimit parses the "limit" query parameter.
func queryLimit(r *http.Request, def, maximum int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 {
		return 0, httpjson.BadRequestf("limit must be a positive integer")
	}

	return min(limit, maximum), nil
}

// decodeBody reads a JSON request body into v. The body must be a single
// JSON object without unknown fields, sent as application/json. Requiring
// the content type keeps cross-site form posts out when the API runs open.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get(httpjson.HeaderContentType))
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

		return httpjson.BadRequestf("invalid JSON body: %s", err.Error())
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return httpjson.BadRequestf("invalid JSON body: trailing data")
	}

	return nil
}

var (
	errUnsupportedMedia = errors.New("the body must be application/json")
	errBodyTooLarge     = errors.New("the body is too large")
)

// writeBodyError answers an error of decodeBody.
func (s *Handler) writeBodyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUnsupportedMedia):
		httpjson.Error(s.log, w, http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, errBodyTooLarge):
		httpjson.Error(s.log, w, http.StatusRequestEntityTooLarge, err.Error())
	default:
		httpjson.BadRequest(s.log, w, err)
	}
}

// Access is how much of the hosts u reads: everything with the feature
// details or when the API runs open; otherwise the reduced view, with
// the hosts a team of u owns a container on in full under the twin. The
// audit log in full needs admin too.
func (s *Handler) Access(u *authz.User) views.Access {
	admin := u.Open || s.authz.HasFeature(u, authz.FeatureAdmin)

	if u.Open || s.authz.HasFeature(u, authz.FeatureDetails) {
		return views.Access{Full: true, Admin: admin}
	}

	return views.Access{
		Admin:      admin,
		Teams:      u.Teams,
		OwnedHosts: s.authz.HasFeature(u, authz.TeamFeaturePrefix+authz.FeatureDetails),
	}
}

// handleHosts serves GET /api/v1/hosts.
func (s *Handler) handleHosts(w http.ResponseWriter, r *http.Request, u *authz.User) {
	ctx := r.Context()

	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	plan := s.views.Plan()
	now := s.now()
	access := s.Access(u)
	out := views.Hosts{Hosts: make([]views.HostListing, 0, len(hosts))}

	for _, host := range hosts {
		sum := views.Summarize(host, &plan, now, settings.HostStaleAfter.Std())
		sum = access.Summary(&sum)
		out.Hosts = append(out.Hosts, views.Listing(&sum))
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}

// handleHost serves GET /api/v1/hosts/{name}.
func (s *Handler) handleHost(w http.ResponseWriter, r *http.Request, u *authz.User) {
	detail, err := s.views.HostDetail(r.Context(), r.PathValue("name"))
	access := s.Access(u)

	switch {
	case err == nil:
		httpjson.Write(s.log, w, http.StatusOK, access.Detail(detail))
	case errors.Is(err, store.ErrNotFound):
		httpjson.Error(s.log, w, http.StatusNotFound, "unknown host")
	default:
		httpjson.InternalError(s.log, w, r, err)
	}
}

// handleImages serves GET /api/v1/images?q=<part of a reference>&limit=&after=:
// one page of the images the containers of the fleet run, one entry per
// reference with its digests and the hosts on each. It pages like
// handlePackages.
func (s *Handler) handleImages(w http.ResponseWriter, r *http.Request, u *authz.User) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	after := r.URL.Query().Get("after")

	if len(query) > maxImageQuery || len(after) > maxImageQuery+1 {
		httpjson.Error(s.log, w, http.StatusBadRequest, fmt.Sprintf(
			"q must be at most %d characters", maxImageQuery))

		return
	}

	limit, err := queryLimit(r, store.DefaultPackageSearchLimit, store.MaxPackageSearchLimit)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	hosts, err := s.opts.Store.ListHosts(r.Context())
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	access := s.Access(u)

	out, err := views.NewImages(hosts, query, after, limit, &access)
	if err != nil {
		httpjson.Error(s.log, w, http.StatusBadRequest, "after is not the next of a page")

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}

// handleRefreshImages serves POST /api/v1/images/refresh: every followed tag
// is looked up at its registry now instead of at its next interval, and the
// source branch of every image is read now, which
// needs the feature manage; a tag looked up within discovery.RefreshFloor is
// left alone, and the answer says until when.
func (s *Handler) handleRefreshImages(w http.ResponseWriter, r *http.Request, u *authz.User) {
	if _, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: authz.FeatureManage}); !ok {
		return
	}

	var body struct{}

	if err := decodeBody(w, r, &body); err != nil {
		s.writeBodyError(w, err)

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, s.refreshImages())
}

// refreshImages asks for a registry lookup of every followed tag and a read
// of every source branch now.
func (s *Handler) refreshImages() views.ImagesRefresh {
	var (
		done    discovery.Refreshed
		sources int
	)

	if s.opts.Lookups != nil {
		done = s.opts.Lookups.Refresh()
	}

	if s.opts.Upstream != nil {
		sources = s.opts.Upstream.Refresh()
	}

	return views.NewImagesRefresh(done.Tags, done.Recent, sources, done.Next, discovery.RefreshFloor)
}

// handlePackages serves GET /api/v1/packages?q=<part of a name>&limit=&after=
// &show=&sort=&compact=: one page of the packages installed across the fleet,
// one entry per name with its versions, the hosts on each and the upgrades
// apt offers them. Without q it lists every package in alphabetical order;
// with q the names that contain it, the best match first. show narrows to
// the names that differ, are outdated or have a security upgrade; sort puts
// the most versions, hosts or outdated hosts first; compact leaves out the
// hosts of each name's most common version and of its upgrades. "after" is
// the "next" of the page before. It needs the plain feature details.
func (s *Handler) handlePackages(w http.ResponseWriter, r *http.Request, u *authz.User) {
	if _, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: authz.FeatureDetails}); !ok {
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	after := r.URL.Query().Get("after")

	if len(query) > maxPackageQuery || len(after) > maxPackageQuery+1 {
		httpjson.Error(s.log, w, http.StatusBadRequest, fmt.Sprintf(
			"q must be at most %d characters", maxPackageQuery))

		return
	}

	limit, err := queryLimit(r, store.DefaultPackageSearchLimit, store.MaxPackageSearchLimit)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	filter := store.PackageFilter{
		Query:   query,
		After:   after,
		Limit:   limit,
		Show:    store.PackageShow(r.URL.Query().Get("show")),
		Sort:    store.PackageSort(r.URL.Query().Get("sort")),
		Compact: r.URL.Query().Get("compact") == "1",
	}

	if !validPackageFilter(&filter) {
		httpjson.Error(s.log, w, http.StatusBadRequest,
			"show must be differing, outdated or security; sort must be versions, hosts or outdated")

		return
	}

	page, err := s.opts.Store.ListPackages(r.Context(), &filter)

	switch {
	case errors.Is(err, store.ErrBadCursor):
		httpjson.Error(s.log, w, http.StatusBadRequest, "after is not the next of a page")

		return
	case err != nil:
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	out := views.Packages{Query: query, Matches: views.NonNil(page.Matches), Counts: page.Counts, Next: page.Next}
	if page.Total >= 0 {
		out.Total = &page.Total
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}

// validPackageFilter reports whether the show and the sort are known.
func validPackageFilter(filter *store.PackageFilter) bool {
	switch filter.Show {
	case store.PackageShowAll, store.PackageShowDiffering, store.PackageShowOutdated, store.PackageShowSecurity:
	default:
		return false
	}

	switch filter.Sort {
	case store.PackageSortName, store.PackageSortVersions, store.PackageSortHosts, store.PackageSortOutdated:
		return true
	default:
		return false
	}
}

// handleRollouts serves GET /api/v1/rollouts.
func (s *Handler) handleRollouts(w http.ResponseWriter, r *http.Request, u *authz.User) {
	limit, err := queryLimit(r, defaultRolloutLimit, maxRolloutLimit)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	out, err := s.views.Rollouts(r.Context(), limit)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	access := s.Access(u)
	httpjson.Write(s.log, w, http.StatusOK, access.Rollouts(out))
}

// handlePlan serves GET /api/v1/plan.
func (s *Handler) handlePlan(w http.ResponseWriter, _ *http.Request, _ *authz.User) {
	httpjson.Write(s.log, w, http.StatusOK, s.views.Plan())
}

// handleGetSettings serves GET /api/v1/settings; it needs the feature admin.
// The other views carry what they need of the settings: the policy of each
// rollout and client, the threshold of the slot history.
func (s *Handler) handleGetSettings(w http.ResponseWriter, r *http.Request, u *authz.User) {
	ctx, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: authz.FeatureAdmin})
	if !ok {
		return
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	body, err := s.settingsBody(ctx, &settings)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, body)
}

// settingsBody is the body of GET and PUT /api/v1/settings for settings.
func (s *Handler) settingsBody(ctx context.Context, settings *fleet.Settings) (*views.SettingsBody, error) {
	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}

	return &views.SettingsBody{
		Settings: *settings,
		Defaults: s.opts.DefaultSettings,
		Kinds:    views.FleetKinds(hosts),
	}, nil
}

// handleAudit serves GET /api/v1/audit, newest first, paged by "before".
func (s *Handler) handleAudit(w http.ResponseWriter, r *http.Request, u *authz.User) {
	limit, err := queryLimit(r, store.DefaultAuditLimit, store.MaxAuditLimit)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	access := s.Access(u)

	filter, err := auditFilter(r, &access)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	// One more than asked tells whether another page exists.
	filter.Limit = limit + 1

	entries, err := s.opts.Store.ListAudit(r.Context(), filter)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	out := views.Audit{Entries: views.NonNil(access.Audit(entries))}

	if len(out.Entries) > limit {
		out.Entries = out.Entries[:limit]
		out.NextBefore = out.Entries[limit-1].ID
	}

	if r.URL.Query().Get("actors") == "1" {
		people, agents, err := s.opts.Store.AuditActors(r.Context())
		if err != nil {
			httpjson.InternalError(s.log, w, r, err)

			return
		}

		out.Actors = &views.AuditActors{People: views.NonNil(people), Agents: agents}
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}

// auditFilter reads the filter of GET /api/v1/audit. The detail is searched
// only for a login that reads every detail in full.
func auditFilter(r *http.Request, access *views.Access) (store.AuditFilter, error) {
	q := r.URL.Query()

	filter := store.AuditFilter{
		Target:       q.Get("target"),
		TargetKind:   fleet.AuditTargetKind(q.Get("kind")),
		Actor:        q.Get("actor"),
		ActorKind:    fleet.AuditActorKind(q.Get("who")),
		Group:        fleet.AuditGroup(q.Get("group")),
		ActionPrefix: q.Get("action"),
		Search:       strings.TrimSpace(q.Get("q")),
		SearchDetail: access.Full && access.Admin,
	}

	for _, v := range []string{filter.Target, filter.Actor, filter.ActionPrefix, filter.Search} {
		if len(v) > maxAuditFilter {
			return filter, errors.New("a filter is too long")
		}
	}

	if !validAuditFilter(&filter) {
		return filter, errors.New("unknown actor kind, target kind or group")
	}

	var err error

	if raw := q.Get("before"); raw != "" {
		filter.BeforeID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || filter.BeforeID < 0 {
			return filter, errors.New("before must be a non-negative integer")
		}
	}

	for _, t := range []struct {
		name string
		into *time.Time
	}{{"from", &filter.From}, {"to", &filter.To}} {
		raw := q.Get(t.name)
		if raw == "" {
			continue
		}

		if *t.into, err = time.Parse(time.RFC3339, raw); err != nil {
			return filter, fmt.Errorf("%s must be an RFC 3339 time", t.name)
		}
	}

	return filter, nil
}

// validAuditFilter says whether the kinds and the group a filter names exist.
func validAuditFilter(f *store.AuditFilter) bool {
	switch f.ActorKind {
	case "", fleet.AuditPeople, fleet.AuditScheduler, fleet.AuditAgents:
	default:
		return false
	}

	known := func(match func(a *fleet.AuditAction) bool) bool {
		for i := range fleet.AuditActions {
			if match(&fleet.AuditActions[i]) {
				return true
			}
		}

		return false
	}

	return (f.TargetKind == "" || known(func(a *fleet.AuditAction) bool { return a.Target == f.TargetKind })) &&
		(f.Group == "" || known(func(a *fleet.AuditAction) bool { return a.Group == f.Group }))
}
