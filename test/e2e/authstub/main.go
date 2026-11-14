// Command authstub is a stand-in for the authenticatoor in local end-to-end
// runs. It serves a JWKS and mints real RS256 tokens that the controller
// verifies through the same code path as in production, plus a minimal
// client-v2.js so the web UI's login button works.
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
	"strings"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/issuer"
)

const clientJS = `(function () {
  var base = %q;
  var token = null, user = '', exp = 0, listeners = [];
  function info() {
    var ok = !!token && exp * 1000 > Date.now() + 10000;
    return { status: ok ? 'authenticated' : 'unauthenticated', authenticated: ok, user: ok ? user : '', exp: ok ? exp : 0 };
  }
  function emit() { var i = info(); listeners.forEach(function (l) { try { l(i); } catch (e) {} }); }
  function mint() {
    return fetch(base + '/mint?email=e2e%%40example.com').then(function (r) { return r.json(); }).then(function (d) {
      token = d.token; user = d.email; exp = d.exp; emit(); return true;
    });
  }
  window.ethpandaops = window.ethpandaops || {};
  window.ethpandaops.authenticatoor = {
    version: 2,
    authServiceURL: base,
    addEventListener: function (ev, cb) { if (ev === 'status') { listeners.push(cb); setTimeout(function () { cb(info()); }, 0); } },
    removeEventListener: function (ev, cb) { listeners = listeners.filter(function (l) { return l !== cb; }); },
    getStatus: function () { return Promise.resolve(info()); },
    getToken: function () { return Promise.resolve(info().authenticated ? token : null); },
    login: function () { return mint(); },
    logout: function () { token = null; emit(); return Promise.resolve(); },
  };
})();
`

func main() {
	listen := flag.String("listen", "127.0.0.1:18099", "loopback listen address")

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

	iss, err := issuer.NewRS256Issuer(issuer.Config{
		ActiveKey:       key,
		Issuer:          base,
		DefaultAudience: []string{audience},
		Scope:           "*",
		TTL:             2 * time.Hour,
	})
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
		email := r.URL.Query().Get("email")
		if email == "" {
			email = "e2e@example.com"
		}

		token, claims, err := iss.Issue(email, email, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		writeJSON(w, map[string]any{
			"token": token, "email": email, "exp": claims.ExpiresAt.Unix(),
		})
	})
	mux.HandleFunc("GET /client-v2.js", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, clientJS, base)
	})

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	log.Printf("authstub on %s (audience %q)", base, audience)
	log.Fatal(srv.ListenAndServe())
}
