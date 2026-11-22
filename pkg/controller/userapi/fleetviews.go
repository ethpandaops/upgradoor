package userapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
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
// is looked up at its registry now instead of at its next interval, which
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

	var done discovery.Refreshed
	if s.opts.Lookups != nil {
		done = s.opts.Lookups.Refresh()
	}

	httpjson.Write(s.log, w, http.StatusOK, views.NewImagesRefresh(done.Tags, done.Recent, done.Next,
		discovery.RefreshFloor))
}

// handlePackages serves GET /api/v1/packages?q=<part of a name>&limit=&after=:
// one page of the packages installed across the fleet, one entry per name
// with its versions and the hosts on each. Without q it lists every package
// in alphabetical order; with q the names that contain it, the best match
// first. "after" is the "next" of the page before. It needs the plain
// feature details.
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

	page, err := s.opts.Store.ListPackages(r.Context(), store.PackageFilter{
		Query: query, After: after, Limit: limit,
	})

	switch {
	case errors.Is(err, store.ErrBadCursor):
		httpjson.Error(s.log, w, http.StatusBadRequest, "after is not the next of a page")

		return
	case err != nil:
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	out := views.Packages{Query: query, Matches: views.NonNil(page.Matches), Next: page.Next}
	if page.Total >= 0 {
		out.Total = &page.Total
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
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

	httpjson.Write(s.log, w, http.StatusOK, views.SettingsBody{
		Settings: settings,
		Defaults: s.opts.DefaultSettings,
	})
}

// handleAudit serves GET /api/v1/audit, newest first, paged by "before".
func (s *Handler) handleAudit(w http.ResponseWriter, r *http.Request, u *authz.User) {
	q := r.URL.Query()

	limit, err := queryLimit(r, store.DefaultAuditLimit, store.MaxAuditLimit)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	filter := store.AuditFilter{
		Target:       q.Get("target"),
		ActionPrefix: q.Get("action"),
		// One more than asked tells whether another page exists.
		Limit: limit + 1,
	}

	if len(filter.Target) > maxAuditFilter || len(filter.ActionPrefix) > maxAuditFilter {
		httpjson.Error(s.log, w, http.StatusBadRequest, "target or action filter too long")

		return
	}

	if raw := q.Get("before"); raw != "" {
		filter.BeforeID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || filter.BeforeID < 0 {
			httpjson.Error(s.log, w, http.StatusBadRequest, "before must be a non-negative integer")

			return
		}
	}

	entries, err := s.opts.Store.ListAudit(r.Context(), filter)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	access := s.Access(u)
	out := views.Audit{Entries: views.NonNil(access.Audit(entries))}

	if len(out.Entries) > limit {
		out.Entries = out.Entries[:limit]
		out.NextBefore = out.Entries[limit-1].ID
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}
