// Command authstub is a stand-in for the authenticatoor in local end-to-end
// runs. It serves a JWKS and mints real RS256 tokens that the controller
// verifies through the same code path as in production, plus a minimal
// client-v2.js so the web UI's login button works.
//
// GET /mint?email=<email>&services=<directives>&teams=<a,b> mints a token
// with that "services" claim and those teams. Without "services" the token
// is one of a staff member (--services, by default every feature of
// upgradoor); "services=none" mints a token without the claim, which has
// basic access and no feature. The login button of the stub's client mints
// with --services and --teams.
//
// It hands a token to anyone who asks. Never run it anywhere but on a
// loopback address for a local test.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/ethpandaops/service-authenticatoor/pkg/issuer"

	"github.com/ethpandaops/upgradoor/pkg/controller/api"
	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
)

// noServices asks /mint for a token without the "services" claim.
const noServices = "none"

// clientJS is the stub's client-v2.js. Its hasFeature answers for the
// features of the controller's service only, from a list the stub computed
// with the real evaluator when it minted the token; the real client
// evaluates the claim itself.
const clientJS = `(function () {
  var base = %q, query = %q, service = %q;
  var token = null, user = '', exp = 0, teams = [], services = '', features = [], listeners = [];
  function ok() { return !!token && exp * 1000 > Date.now() + 10000; }
  function info() {
    var a = ok();
    return { status: a ? 'authenticated' : 'unauthenticated', authenticated: a, user: a ? user : '',
             exp: a ? exp : 0, teams: a ? teams.slice() : [], services: a ? services : '' };
  }
  function emit() { var i = info(); listeners.forEach(function (l) { try { l(i); } catch (e) {} }); }
  function mint() {
    return fetch(base + '/mint?' + query).then(function (r) { return r.json(); }).then(function (d) {
      token = d.token; user = d.email; exp = d.exp; teams = d.teams || []; services = d.services || '';
      features = d.features || []; emit(); return true;
    });
  }
  window.ethpandaops = window.ethpandaops || {};
  window.ethpandaops.authenticatoor = {
    version: 2,
    authServiceURL: base,
    addEventListener: function (ev, cb) { if (ev === 'status') { listeners.push(cb); setTimeout(function () { cb(info()); }, 0); } },
    removeEventListener: function (ev, cb) { listeners = listeners.filter(function (l) { return l !== cb; }); },
    getStatus: function () { return Promise.resolve(info()); },
    getToken: function () { return Promise.resolve(ok() ? token : null); },
    login: function () { return mint(); },
    logout: function () { token = null; emit(); return Promise.resolve(); },
    hasService: function (svc) { return ok() && svc === service; },
    hasFeature: function (svc, feature) { return ok() && svc === service && features.indexOf(feature) >= 0; },
    getTeams: function () { return ok() ? teams.slice() : []; },
  };
})();
`

// features lists the features the stub's client can answer for: the ones of
// the controller and their team twins.
func features() []string {
	twinned := []string{
		authz.FeatureRoll, authz.FeatureReboot, authz.FeatureHold, authz.FeatureRecover,
		authz.FeatureClientPolicy, authz.FeatureImageOverride, authz.FeatureFlagOverride,
	}
	out := make([]string, 0, 2+2*len(twinned))
	out = append(out, authz.FeatureSupervise, authz.FeatureAdmin)

	for _, f := range twinned {
		out = append(out, f, "team_"+f)
	}

	return out
}

// splitTeams reads a comma-separated team list.
func splitTeams(raw string) []string {
	out := make([]string, 0, 4)

	for _, team := range strings.Split(raw, ",") {
		if team = strings.TrimSpace(team); team != "" {
			out = append(out, team)
		}
	}

	return out
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18099", "loopback listen address")
	service := flag.String("service", api.DefaultAuthService,
		"the controller's --auth-service; the stub's client answers hasFeature for it")
	services := flag.String("services", "+"+api.DefaultAuthService+":all",
		`"services" claim of a token minted without a services parameter`)
	teams := flag.String("teams", "", "teams of a token minted by the login button, comma-separated")

	flag.Parse()

	host, _, err := net.SplitHostPort(*listen)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		log.Fatalf("authstub only listens on a loopback address, got %q", *listen)
	}

	base := "http://" + *listen

	// The controller expects the audience to be the parent zone of the
	// provider URL's host: everything after the first dot.
	audience := host
	if i := strings.IndexByte(host, '.'); i > 0 {
		audience = host[i+1:]
	}

	key, err := issuer.GenerateRSAKey()
	if err != nil {
		log.Fatal(err)
	}

	// One issuer per "services" claim: the claim is part of an issuer's
	// configuration. All of them sign with the one key.
	newIssuer := func(claim string) (*issuer.RS256Issuer, error) {
		return issuer.NewRS256Issuer(issuer.Config{
			ActiveKey:       key,
			Issuer:          base,
			DefaultAudience: []string{audience},
			Scope:           "*",
			Services:        claim,
			TTL:             2 * time.Hour,
		})
	}

	iss, err := newIssuer(*services)
	if err != nil {
		log.Fatal(err)
	}

	jwks, err := iss.JWKS()
	if err != nil {
		log.Fatal(err)
	}

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Print(err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if _, err := w.Write(jwks); err != nil {
			log.Print(err)
		}
	})
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"issuer": base, "jwks_uri": base + "/jwks.json"})
	})
	mux.HandleFunc("GET /mint", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		email := q.Get("email")
		if email == "" {
			email = "e2e@example.com"
		}

		claim := *services

		switch {
		case q.Get("services") == noServices:
			claim = ""
		case q.Has("services"):
			claim = q.Get("services")
		}

		minter, err := newIssuer(claim)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		token, claims, err := minter.IssueFor(issuer.Identity{
			Subject: email, Email: email, Teams: splitTeams(q.Get("teams")),
		}, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		granted := make([]string, 0, 16)

		for _, f := range features() {
			if claims.HasFeature(*service, f, auth.FeatureDefaultDeny) {
				granted = append(granted, f)
			}
		}

		teams := claims.Teams
		if teams == nil {
			teams = []string{}
		}

		writeJSON(w, map[string]any{
			"token": token, "email": email, "exp": claims.ExpiresAt.Unix(),
			"services": claims.Services, "teams": teams, "features": granted,
		})
	})
	mux.HandleFunc("GET /client-v2.js", func(w http.ResponseWriter, _ *http.Request) {
		login := url.Values{"email": {"e2e@example.com"}, "teams": {*teams}}

		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, clientJS, base, login.Encode(), *service)
	})

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	log.Printf("authstub on %s (audience %q, service %q)", base, audience, *service)
	log.Fatal(srv.ListenAndServe())
}
