package userapi

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	defaultClientHostLimit = 50
	maxClientHostLimit     = 200
	maxClientHostQuery     = 128
)

// handleClients serves GET /api/v1/clients.
func (s *Handler) handleClients(w http.ResponseWriter, r *http.Request, _ *authz.User) {
	idx, err := s.views.Clients(r.Context())
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, views.Clients{Clients: idx.Views})
}

// handleClientHosts serves
// GET /api/v1/clients/{client}/hosts?state=&q=&override=&offset=&limit=&names=:
// one page of the hosts that run the client, ordered by name. state is a list
// of views.ClientHostState separated by commas, q a part of the host name, override
// "image" or "flags" (hosts with that part of an override). names=true
// answers the names of every matched host instead of a page.
func (s *Handler) handleClientHosts(w http.ResponseWriter, r *http.Request, _ *authz.User) {
	query := r.URL.Query()
	needle := strings.ToLower(strings.TrimSpace(query.Get("q")))

	if len(needle) > maxClientHostQuery {
		httpjson.Error(s.log, w, http.StatusBadRequest, "q is too long")

		return
	}

	var states []views.ClientHostState

	for part := range strings.SplitSeq(query.Get("state"), ",") {
		switch state := views.ClientHostState(part); {
		case part == "":
		case slices.Contains(views.ClientHostStates(), state):
			states = append(states, state)
		default:
			httpjson.Error(s.log, w, http.StatusBadRequest, "unknown state in the state filter")

			return
		}
	}

	override := query.Get("override")
	if override != "" && override != protocol.OverrideImage && override != protocol.OverrideFlags {
		httpjson.Error(s.log, w, http.StatusBadRequest, "override must be image or flags")

		return
	}

	names := query.Get("names") == "true"

	limit, err := queryLimit(r, defaultClientHostLimit, maxClientHostLimit)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	offset := 0

	if raw := query.Get("offset"); raw != "" {
		if offset, err = strconv.Atoi(raw); err != nil || offset < 0 {
			httpjson.Error(s.log, w, http.StatusBadRequest, "offset must not be negative")

			return
		}
	}

	idx, err := s.views.Clients(r.Context())
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	name := r.PathValue("client")

	at := slices.IndexFunc(idx.Views, func(v views.ClientView) bool { return v.Name == name })
	if at < 0 {
		httpjson.Error(s.log, w, http.StatusNotFound, "unknown client")

		return
	}

	rows := idx.Hosts[name]
	out := views.ClientHosts{
		Client: name, Version: idx.Views[at].Version, Total: len(rows),
		Offset: offset, Limit: limit, Hosts: make([]views.ClientHost, 0, limit),
	}

	for i := range rows {
		row := &rows[i]

		if (len(states) > 0 && !slices.Contains(states, row.State)) ||
			(needle != "" && !strings.Contains(strings.ToLower(row.Host), needle)) ||
			!views.OverrideMatches(row, override) {
			continue
		}

		switch {
		case names:
			if len(out.Names) < maxActionHosts {
				out.Names = append(out.Names, row.Host)
			}
		case out.Matched >= offset && len(out.Hosts) < limit:
			out.Hosts = append(out.Hosts, *row)
		}

		out.Matched++
	}

	httpjson.Write(s.log, w, http.StatusOK, out)
}
