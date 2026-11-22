package userapi

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/beacon"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/validators"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// sourcesAuditTimeout bounds the write of the audit entry of a refresh.
const sourcesAuditTimeout = 5 * time.Second

// BeaconEndpoints is the part of beacon.Service the data sources view reads.
type BeaconEndpoints interface {
	Endpoints() []beacon.EndpointStatus
}

// ValidatorRanges is the part of validators.Service the data sources view
// reads and refreshes.
type ValidatorRanges interface {
	Status() validators.Status
	Refresh(ctx context.Context) validators.Refreshed
}

// handleSources serves GET /api/v1/settings/sources; it needs the feature
// admin.
func (s *Handler) handleSources(w http.ResponseWriter, r *http.Request, u *authz.User) {
	ctx, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: authz.FeatureAdmin})
	if !ok {
		return
	}

	out, err := s.dataSources(ctx)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}

// handleRefreshValidatorRanges serves POST
// /api/v1/settings/sources/validator-ranges/refresh: the inventory is fetched
// now, unless the last fetch is younger than validators.RefreshFloor or one
// is running. It needs the feature admin; a fetch that ran is audited.
func (s *Handler) handleRefreshValidatorRanges(w http.ResponseWriter, r *http.Request, u *authz.User) {
	ctx, ok := s.authz.Allow(w, r, u, &authz.Requirement{Feature: authz.FeatureAdmin})
	if !ok {
		return
	}

	var body struct{}
	if err := decodeBody(w, r, &body); err != nil {
		s.writeBodyError(w, err)

		return
	}

	done := validators.Refreshed{Reason: "no validator ranges URL is configured"}
	if s.opts.Validators != nil {
		done = s.opts.Validators.Refresh(ctx)
	}

	if done.Fetched {
		s.auditRefresh(ctx, u, &done)
	}

	out, err := s.dataSources(ctx)
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, views.NewValidatorRangesRefresh(&done, &out))
}

// auditRefresh records a refresh that fetched, with what allowed it.
func (s *Handler) auditRefresh(ctx context.Context, u *authz.User, done *validators.Refreshed) {
	detail := map[string]any{"changed": done.Changed}
	if done.Error != "" {
		detail["error"] = done.Error
	}

	if a, ok := scheduler.AuthorizationFrom(ctx); ok {
		teams := slices.Clone(a.Teams)
		if teams == nil {
			teams = []string{}
		}

		detail["authorizedBy"] = a.By
		detail["teams"] = teams
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sourcesAuditTimeout)
	defer cancel()

	err := s.opts.Store.AppendAudit(ctx, fleet.AuditEntry{
		Time: s.now(), Actor: u.Actor, Action: "sources.validator-ranges.refresh",
		Target: "validator-ranges", Detail: detail,
	})
	if err != nil {
		s.log.WithError(err).Warn("Could not audit a validator ranges refresh")

		return
	}

	s.opts.Notify()
}

// dataSources builds the data sources view.
func (s *Handler) dataSources(ctx context.Context) (views.DataSources, error) {
	var endpoints []beacon.EndpointStatus
	if s.opts.Endpoints != nil {
		endpoints = s.opts.Endpoints.Endpoints()
	}

	var ranges validators.Status
	if s.opts.Validators != nil {
		ranges = s.opts.Validators.Status()
	}

	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return views.DataSources{}, err
	}

	known := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		known[h.Name] = struct{}{}
	}

	return views.NewDataSources(endpoints, &ranges, func(host string) bool {
		_, ok := known[host]

		return ok
	}), nil
}
