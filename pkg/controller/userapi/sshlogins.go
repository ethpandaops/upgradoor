package userapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// handleSSHLogins serves GET /api/v1/ssh-logins: a page of SSH login groups
// or of their clusters across the fleet, newest first. It needs the feature
// audit.
func (s *Handler) handleSSHLogins(w http.ResponseWriter, r *http.Request, u *authz.User) {
	if _, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: authz.FeatureAudit}); !ok {
		return
	}

	filter, err := sshLoginFilter(r)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	page, err := s.opts.Store.ListSSHLogins(r.Context(), filter)

	switch {
	case errors.Is(err, store.ErrBadCursor):
		httpjson.Error(s.log, w, http.StatusBadRequest, "cursor is not the next of a page")

		return
	case err != nil:
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	out := views.SSHLogins{
		Groups: views.NonNil(page.Groups), Clusters: views.NonNil(page.Clusters), Next: page.Next,
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}

// sshLoginFilter reads the filter of GET /api/v1/ssh-logins.
func sshLoginFilter(r *http.Request) (*store.SSHLoginFilter, error) {
	q := r.URL.Query()

	limit, err := queryLimit(r, store.DefaultSSHLoginLimit, store.MaxSSHLoginLimit)
	if err != nil {
		return nil, err
	}

	f := &store.SSHLoginFilter{
		View:        store.SSHLoginView(q.Get("view")),
		Host:        q.Get("host"),
		Identity:    q.Get("identity"),
		Fingerprint: q.Get("fingerprint"),
		User:        q.Get("user"),
		Unknown:     q.Get("unknown") == "1",
		Open:        q.Get("open") == "1",
		Cursor:      q.Get("cursor"),
		Limit:       limit,
	}

	switch f.View {
	case "":
		f.View = store.SSHViewFleet
	case store.SSHViewFleet, store.SSHViewGroups:
	default:
		return nil, errors.New("view must be fleet or groups")
	}

	for _, v := range []string{f.Host, f.Identity, f.Fingerprint, f.User, f.Cursor} {
		if len(v) > maxAuditFilter {
			return nil, errors.New("a filter is too long")
		}
	}

	for _, t := range []struct {
		name string
		into *time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		raw := q.Get(t.name)
		if raw == "" {
			continue
		}

		if *t.into, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return nil, fmt.Errorf("%s must be an RFC 3339 time", t.name)
		}
	}

	return f, nil
}
