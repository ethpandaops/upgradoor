package userapi

import (
	"net/http"

	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// handleConfig serves GET /api/v1/config.
func (s *Handler) handleConfig(w http.ResponseWriter, _ *http.Request) {
	httpjson.Write(s.log, w, http.StatusOK, views.Config{
		Network:         s.opts.Network,
		Version:         s.opts.Version,
		AuthProviderURL: s.opts.AuthProviderURL,
		AuthService:     s.opts.AuthService,
	})
}

// handlePublicHosts serves GET /api/v1/public/hosts.
func (s *Handler) handlePublicHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.opts.Store.ListHosts(r.Context())
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, views.NewPublicHosts(hosts))
}

// handlePublicImages serves GET /api/v1/public/images.
func (s *Handler) handlePublicImages(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.opts.Store.ListHosts(r.Context())
	if err != nil {
		httpjson.InternalError(s.log, w, r, err)

		return
	}

	httpjson.Write(s.log, w, http.StatusOK, views.NewPublicImages(hosts))
}

// handlePublicNetwork serves GET /api/v1/public/network.
func (s *Handler) handlePublicNetwork(w http.ResponseWriter, _ *http.Request) {
	httpjson.Write(s.log, w, http.StatusOK, s.views.PublicGate())
}
