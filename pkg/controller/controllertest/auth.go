package controllertest

import (
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/issuer"
	"github.com/stretchr/testify/require"
)

const (
	// DiscoveredIssuer differs from the provider URL, which shows that the
	// discovery document is used.
	DiscoveredIssuer = "https://auth.testnet.example"
	TokenScope       = "*.testnet.example"
	UserEmail        = "alice@ethpandaops.io"
	// StaffServices is the "services" claim of a staff token.
	StaffServices = "+upgradoor:all"
)

// AuthProvider is a stand-in authenticatoor: discovery document and JWKS.
type AuthProvider struct {
	Server    *httptest.Server
	Key       *rsa.PrivateKey
	Discovery atomic.Bool
	Clock     *Clock
}

func NewAuthProvider(t *testing.T, c *Clock, discovery bool) *AuthProvider {
	t.Helper()

	key, err := issuer.GenerateRSAKey()
	require.NoError(t, err)

	p := &AuthProvider{Key: key, Clock: c}
	p.Discovery.Store(discovery)

	jwks, err := p.Issuer(t, nil).JWKS()
	require.NoError(t, err)

	serveJWKS := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}

	mux := http.NewServeMux()
	// The discovery document points at a non-default JWKS path.
	mux.HandleFunc("/keys", serveJWKS)
	mux.HandleFunc("/jwks.json", serveJWKS)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		if !p.Discovery.Load() {
			http.NotFound(w, nil)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": DiscoveredIssuer, "jwks_uri": p.Server.URL + "/keys",
		})
	})

	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Server.Close)

	return p
}

// IssuerName is the "iss" the controller expects.
func (p *AuthProvider) IssuerName() string {
	if p.Discovery.Load() {
		return DiscoveredIssuer
	}

	return p.Server.URL
}

// Issuer returns a token issuer; mod can break its configuration.
func (p *AuthProvider) Issuer(t *testing.T, mod func(*issuer.Config)) *issuer.RS256Issuer {
	t.Helper()

	// Unless mod says otherwise the token is one of a staff member: every
	// feature of this service. A token with basic access only is minted with
	// Grant.
	cfg := issuer.Config{
		ActiveKey: p.Key,
		TTL:       30 * time.Minute,
		Scope:     TokenScope,
		Services:  StaffServices,
		Now:       p.Clock.Now,
	}

	if p.Server != nil {
		cfg.Issuer = p.IssuerName()
		cfg.DefaultAudience = []string{audienceOf(p.Server.URL)}
	} else {
		cfg.Issuer = DiscoveredIssuer
	}

	if mod != nil {
		mod(&cfg)
	}

	iss, err := issuer.NewRS256Issuer(cfg)
	require.NoError(t, err)

	return iss
}

// Token mints a token for subject and email.
func (p *AuthProvider) Token(t *testing.T, subject, email string, mod func(*issuer.Config)) string {
	t.Helper()

	tok, _, err := p.Issuer(t, mod).Issue(subject, email, nil)
	require.NoError(t, err)

	return tok
}

// Grant mints a token for UserEmail with the given "services" claim (empty:
// no claim) and teams, the way the authenticatoor mints one for an identity.
func (p *AuthProvider) Grant(t *testing.T, services string, teams ...string) string {
	t.Helper()

	tok, claims, err := p.Issuer(t, func(c *issuer.Config) { c.Services = services }).
		IssueFor(issuer.Identity{Subject: "alice", Email: UserEmail, Teams: teams}, nil)
	require.NoError(t, err)
	require.Equal(t, services, claims.Services)
	require.ElementsMatch(t, teams, claims.Teams)

	return "Bearer " + tok
}

// audienceOf is the token audience the controller expects for a provider
// URL: the parent DNS zone of its host.
func audienceOf(rawURL string) string {
	host := rawURL
	if _, rest, ok := strings.Cut(host, "://"); ok {
		host = rest
	}

	host, _, _ = strings.Cut(host, "/")
	host, _, _ = strings.Cut(host, ":")

	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[i+1:]
	}

	return host
}
