package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/registryauth"
)

const (
	repoPath    = "org/app"
	tagMulti    = "multi"
	tagSingle   = "single"
	testToken   = "tok-1"
	testService = "fake-registry"
	testBasic   = "user:secret"
	kindManifst = "/manifests/"
)

// fakeRegistry serves org/app with tag "multi" at an index and "single" at
// an image manifest, addressed by real sha256 digests.
type fakeRegistry struct {
	srv *httptest.Server

	mu       sync.Mutex
	hits     map[string]int    // "METHOD path" -> count
	content  map[string][]byte // digest -> manifest body
	tags     map[string]string // tag -> digest
	override map[string]func(w http.ResponseWriter, r *http.Request)

	// knobs, set before use or under mu via set()
	noDigestHeader bool   // never send Docker-Content-Digest
	noHead         bool   // answer HEAD with 405
	headDigest     string // send this digest header on HEAD of a tag instead of the real one
	getDigest      string // same for GET of a tag
	bearer         string // when set, /v2/ wants this bearer token
	basic          string // when set, /v2/ wants these basic credentials (user:pass)
	challenge      string // WWW-Authenticate to send; default is a Bearer challenge to /token
	tokenBody      func() string
	tokenStatus    int
	tokenAuth      []string // Authorization headers seen by /token
	tokenAgents    []string // User-Agent headers seen by /token
	v2Agents       map[string]struct{}
	tokenQueries   []url.Values
	unauthorized   int // 401 answers given by /v2/

	indexDigest, amd64Digest string
}

func newFakeRegistry(t *testing.T, tlsServer bool) *fakeRegistry {
	t.Helper()

	f := &fakeRegistry{
		hits:     map[string]int{},
		content:  map[string][]byte{},
		tags:     map[string]string{},
		override: map[string]func(http.ResponseWriter, *http.Request){},
		v2Agents: map[string]struct{}{},
	}

	f.amd64Digest = f.add([]byte(`{"config":{"digest":"sha256:c0ffee"}}`))
	f.indexDigest = f.add([]byte(`{"manifests":[{"digest":"` + f.amd64Digest + `"}]}`))
	f.tags[tagMulti] = f.indexDigest
	f.tags[tagSingle] = f.amd64Digest

	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.serveToken)
	mux.HandleFunc("/v2/", f.serveV2)

	if tlsServer {
		f.srv = httptest.NewTLSServer(mux)
	} else {
		f.srv = httptest.NewServer(mux)
	}

	t.Cleanup(f.srv.Close)

	return f
}

// add stores content and returns its digest.
func (f *fakeRegistry) add(body []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	digest := sha256Digest(body)
	f.content[digest] = body

	return digest
}

// tag points a tag at new content and returns its digest.
func (f *fakeRegistry) tag(tag string, body []byte) string {
	digest := f.add(body)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.tags[tag] = digest

	return digest
}

// set changes knobs under the lock.
func (f *fakeRegistry) set(fn func(f *fakeRegistry)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

// handle replaces the answer for one "/v2/org/app" suffix, after authentication.
func (f *fakeRegistry) handle(suffix string, fn func(w http.ResponseWriter, r *http.Request)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.override[suffix] = fn
}

func (f *fakeRegistry) host() string {
	u, _ := url.Parse(f.srv.URL)

	return u.Host
}

func (f *fakeRegistry) ref(tag string) string {
	return f.host() + "/" + repoPath + ":" + tag
}

// count sums the requests of a method whose path contains part.
func (f *fakeRegistry) count(method, part string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for key, c := range f.hits {
		m, path, _ := strings.Cut(key, " ")
		if m == method && strings.Contains(path, part) {
			n += c
		}
	}

	return n
}

func (f *fakeRegistry) heads() int { return f.count(http.MethodHead, "/v2/") }
func (f *fakeRegistry) gets() int  { return f.count(http.MethodGet, "/v2/") }
func (f *fakeRegistry) tokens() int {
	return f.count(http.MethodGet, "/token")
}

func (f *fakeRegistry) serveToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.Method+" "+r.URL.Path]++
	f.tokenAuth = append(f.tokenAuth, r.Header.Get("Authorization"))
	f.tokenAgents = append(f.tokenAgents, r.Header.Get("User-Agent"))
	f.tokenQueries = append(f.tokenQueries, r.URL.Query())
	status, body := f.tokenStatus, f.tokenBody
	bearer := f.bearer
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)

		return
	}

	if body != nil {
		_, _ = io.WriteString(w, body())

		return
	}

	_ = json.NewEncoder(w).Encode(map[string]string{"token": bearer})
}

func (f *fakeRegistry) authorized(r *http.Request) bool {
	switch {
	case f.bearer != "":
		return r.Header.Get("Authorization") == "Bearer "+f.bearer
	case f.basic != "":
		return r.Header.Get("Authorization") == "Basic "+base64.StdEncoding.EncodeToString([]byte(f.basic))
	default:
		return true
	}
}

func (f *fakeRegistry) serveV2(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.Method+" "+r.URL.Path]++
	f.v2Agents[r.Header.Get("User-Agent")] = struct{}{}

	if !f.authorized(r) {
		f.unauthorized++

		challenge := f.challenge
		if challenge == "" {
			challenge = `Bearer realm="` + f.srv.URL + `/token",service="` + testService +
				`",scope="repository:` + repoPath + `:pull,push"`
		}

		f.mu.Unlock()

		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)

		return
	}

	suffix, ok := strings.CutPrefix(r.URL.Path, "/v2/"+repoPath)
	override := f.override[suffix]
	f.mu.Unlock()

	switch {
	case !ok:
		http.NotFound(w, r)
	case override != nil:
		override(w, r)
	case strings.HasPrefix(suffix, kindManifst):
		f.serveManifest(w, r, strings.TrimPrefix(suffix, kindManifst))
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRegistry) serveManifest(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	digest, isTag := f.tags[name]
	noHead, headDigest, getDigest := f.noHead, f.headDigest, f.getDigest
	f.mu.Unlock()

	if !isTag {
		f.serveContent(w, r, name, "")

		return
	}

	if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") ||
		!strings.Contains(r.Header.Get("Accept"), "application/vnd.docker.distribution.manifest.v2+json") {
		http.Error(w, "missing Accept", http.StatusNotAcceptable)

		return
	}

	if r.Method == http.MethodHead && noHead {
		w.WriteHeader(http.StatusMethodNotAllowed)

		return
	}

	header := ""
	if r.Method == http.MethodHead {
		header = headDigest
	} else {
		header = getDigest
	}

	f.serveContent(w, r, digest, header)
}

func (f *fakeRegistry) serveContent(w http.ResponseWriter, r *http.Request, digest, header string) {
	f.mu.Lock()
	body, ok := f.content[digest]
	noHeader := f.noDigestHeader
	f.mu.Unlock()

	if !ok {
		http.NotFound(w, r)

		return
	}

	if header == "" {
		header = digest
	}

	if !noHeader {
		w.Header().Set(headerContentDigest, header)
	}

	if r.Method == http.MethodHead {
		return
	}

	_, _ = w.Write(body)
}

func testLogger() logrus.FieldLogger {
	log := logrus.New()
	log.SetOutput(io.Discard)
	log.SetLevel(logrus.DebugLevel)

	return log
}

// buildResolver builds a resolver for cfg and logins.
func buildResolver(t *testing.T, cfg Config, logins registryauth.Logins) *resolver {
	t.Helper()

	cfg.AllowPrivate = true

	r, err := NewResolver(testLogger(), &cfg, logins)
	require.NoError(t, err)

	res, ok := r.(*resolver)
	require.True(t, ok)

	return res
}

// newResolver builds a resolver that talks plain http to the fake registry.
func newResolver(t *testing.T, f *fakeRegistry, logins registryauth.Logins) *resolver {
	t.Helper()

	return buildResolver(t, Config{PlainHTTP: []string{f.host()}}, logins)
}

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

// loginFor is the login of one registry, from "user:password".
func loginFor(host, userPass string) registryauth.Logins {
	user, pass, _ := strings.Cut(userPass, ":")

	return registryauth.Logins{host: {Username: user, Password: pass}}
}

func TestResolveIsOneHead(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	for i := range 3 {
		digest, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.NoError(t, err)
		require.Equal(t, f.indexDigest, digest, "the index digest, not a platform manifest")
		require.Equal(t, 1+i, f.heads())
	}

	digest, err := r.Resolve(t.Context(), f.ref(tagSingle))
	require.NoError(t, err)
	require.Equal(t, f.amd64Digest, digest)

	// The tag moves: the new digest is seen with the next HEAD.
	moved := f.tag(tagMulti, []byte(`{"manifests":[]}`))

	digest, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, moved, digest)
	require.Equal(t, 5, f.heads())
	require.Equal(t, 0, f.gets())
}

func TestResolveWithoutDigestHeader(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.noDigestHeader = true })

	r := newResolver(t, f, nil)

	digest, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest, "computed from the body")
	require.Equal(t, 1, f.heads())
	require.Equal(t, 1, f.count(http.MethodGet, kindManifst+tagMulti))
	require.Equal(t, 1, f.gets())
}

func TestResolveWithoutHead(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.noHead = true })

	r := newResolver(t, f, nil)

	digest, err := r.Resolve(t.Context(), f.ref(tagSingle))
	require.NoError(t, err)
	require.Equal(t, f.amd64Digest, digest, "from the digest header of the GET")
	require.Equal(t, 1, f.heads())
	require.Equal(t, 1, f.gets())

	f.set(func(f *fakeRegistry) { f.getDigest = "not-a-digest" })

	_, err = r.Resolve(t.Context(), f.ref(tagSingle))
	require.ErrorContains(t, err, "invalid "+headerContentDigest)
}

func TestResolveInvalidDigestHeader(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	for _, bad := range []string{"sha256:../../x", "sha256:abc", "nonsense", "sha256:" + strings.Repeat("G", 64)} {
		f.set(func(f *fakeRegistry) { f.headDigest = bad })

		_, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.ErrorContains(t, err, "invalid "+headerContentDigest, bad)
	}

	require.Equal(t, 0, f.gets())
}

func TestResolveErrors(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	_, err := r.Resolve(t.Context(), "")
	require.Error(t, err)

	_, err = r.Resolve(t.Context(), f.host()+"/org/app@sha256:"+strings.Repeat("a", 64))
	require.ErrorIs(t, err, errDigestReference)

	// Unknown tag: a statusError with the registry's code.
	_, err = r.Resolve(t.Context(), f.ref("missing"))

	var status *statusError

	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusNotFound, status.Code)
	require.Equal(t, http.MethodHead, status.Method)
	require.Contains(t, err.Error(), "404 Not Found")
	require.Contains(t, err.Error(), "/v2/org/app/manifests/missing")

	// Server errors on the fallback GET.
	f.set(func(f *fakeRegistry) { f.noHead = true })
	f.handle(kindManifst+"broken", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotImplemented)

			return
		}

		w.WriteHeader(http.StatusBadGateway)
	})

	_, err = r.Resolve(t.Context(), f.ref("broken"))
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusBadGateway, status.Code)
	require.Equal(t, http.MethodGet, status.Method)

	// Cancelled context.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = r.Resolve(ctx, f.ref(tagMulti))
	require.ErrorIs(t, err, context.Canceled)

	// Unreachable registry.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadHost := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	r2 := buildResolver(t, Config{PlainHTTP: []string{deadHost}}, nil)

	_, err = r2.Resolve(t.Context(), deadHost+"/org/app:x")
	require.Error(t, err)
	require.False(t, errors.As(err, &status))
}

func TestResolveOversizedOrBrokenTagManifest(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.noDigestHeader = true })

	f.handle(kindManifst+"huge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}

		chunk := []byte(strings.Repeat(" ", 1<<20))
		for range 5 {
			_, _ = w.Write(chunk)
		}
	})

	// Announces more than it sends, so reading the body fails.
	f.handle(kindManifst+"short", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			return
		}

		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "{")
	})

	r := newResolver(t, f, nil)

	_, err := r.Resolve(t.Context(), f.ref("huge"))
	require.ErrorIs(t, err, errBodyTooLarge)

	_, err = r.Resolve(t.Context(), f.ref("short"))
	require.ErrorContains(t, err, "read manifest")

	// The body is hashed, not read: one that is no manifest still has a digest.
	garbage := f.tag("garbage", []byte("this is not json"))

	digest, err := r.Resolve(t.Context(), f.ref("garbage"))
	require.NoError(t, err)
	require.Equal(t, garbage, digest)
}

func TestPlainHTTPIsPerHost(t *testing.T) {
	f := newFakeRegistry(t, false)

	// Without the plain-http entry the resolver speaks TLS to an http server.
	r := buildResolver(t, Config{}, nil)

	_, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.Error(t, err)
	require.Equal(t, 0, f.heads())

	// Entries are trimmed, lower-cased and may carry a scheme.
	r = buildResolver(t, Config{
		PlainHTTP: []string{"", "  http://" + strings.ToUpper(f.host()) + "/ "},
	}, nil)

	_, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
}

// hubTransport sends requests for Docker Hub to a fake registry.
type hubTransport struct {
	target string
	base   http.RoundTripper
	hosts  []string
	mu     sync.Mutex
}

func (h *hubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.hosts = append(h.hosts, req.URL.Scheme+"://"+req.URL.Host)
	h.mu.Unlock()

	clone := req.Clone(req.Context())
	clone.URL.Scheme = schemeHTTP
	clone.URL.Host = h.target

	return h.base.RoundTrip(clone)
}

func TestResolveDockerHub(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.basic = testBasic })

	// Docker Hub official images live under library/.
	hubMux := http.NewServeMux()
	hubMux.HandleFunc("/v2/library/geth/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte(testBasic)) {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		w.Header().Set(headerContentDigest, f.indexDigest)
	})

	hub := httptest.NewServer(hubMux)
	t.Cleanup(hub.Close)

	for _, key := range []string{"https://index.docker.io/v1/", "docker.io", "registry-1.docker.io", "INDEX.docker.io"} {
		t.Run(key, func(t *testing.T) {
			logins, err := registryauth.LoadLogins(testLogger(), writeAuthFile(t,
				`{"auths":{"`+key+`":{"auth":"`+base64.StdEncoding.EncodeToString([]byte(testBasic))+`"}}}`))
			require.NoError(t, err)

			r := buildResolver(t, Config{}, logins)

			transport := &hubTransport{target: strings.TrimPrefix(hub.URL, "http://"), base: http.DefaultTransport}
			r.client.Transport = transport

			for _, ref := range []string{"geth", "docker.io/library/geth:latest", "index.docker.io/geth"} {
				digest, err := r.Resolve(t.Context(), ref)
				require.NoError(t, err, ref)
				require.Equal(t, f.indexDigest, digest)
			}

			for _, host := range transport.hosts {
				require.Equal(t, "https://"+dockerHubAPIHost, host)
			}
		})
	}
}

func TestRequestTimeout(t *testing.T) {
	f := newFakeRegistry(t, false)
	release := make(chan struct{})

	f.handle(kindManifst+"slow", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	t.Cleanup(func() { close(release) })

	r := newResolver(t, f, nil)
	r.client.Timeout = 100 * time.Millisecond

	start := time.Now()
	_, err := r.Resolve(t.Context(), f.ref("slow"))
	require.Error(t, err)
	require.Less(t, time.Since(start), 3*time.Second)
}

func caFileFor(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(path, block, 0o600))

	return path
}

func TestCAFile(t *testing.T) {
	f := newFakeRegistry(t, true)

	// Without the CA the certificate is not trusted.
	r := buildResolver(t, Config{}, nil)

	_, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.ErrorContains(t, err, "certificate")

	r = buildResolver(t, Config{CAFile: caFileFor(t, f.srv)}, nil)

	digest, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
}

func TestCAFileErrors(t *testing.T) {
	_, err := NewResolver(testLogger(), &Config{CAFile: filepath.Join(t.TempDir(), "missing.pem")}, nil)
	require.ErrorContains(t, err, "read CA file")

	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))

	_, err = NewResolver(testLogger(), &Config{CAFile: empty}, nil)
	require.ErrorContains(t, err, "no PEM certificates")
}

func TestCheckRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, raw, http.NoBody)
		require.NoError(t, err)

		return r
	}

	secure, plain := req("https://a.example/x"), req("http://a.example/x")

	require.NoError(t, checkRedirect(secure, []*http.Request{secure}))
	require.NoError(t, checkRedirect(plain, []*http.Request{plain}))
	require.NoError(t, checkRedirect(secure, []*http.Request{plain}))
	require.ErrorContains(t, checkRedirect(plain, []*http.Request{secure}), "https to http")

	many := make([]*http.Request, maxRedirects)
	for i := range many {
		many[i] = secure
	}

	require.ErrorContains(t, checkRedirect(secure, many), "redirects")
}

// Credentials never cross a redirect to another host, port or subdomain.
func TestCredentialsNeverCrossARedirect(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) {
		f.basic = testBasic
		f.challenge = `Basic realm="registry"`
	})

	var (
		mu   sync.Mutex
		seen []string
	)

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()

		w.Header().Set(headerContentDigest, f.indexDigest)
	}))
	t.Cleanup(other.Close)

	f.handle(kindManifst+"away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v2/org/app/manifests/away", http.StatusTemporaryRedirect)
	})
	f.handle(kindManifst+"here", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v2/org/app/manifests/"+tagMulti, http.StatusTemporaryRedirect)
	})

	r := newResolver(t, f, loginFor(f.host(), testBasic))

	digest, err := r.Resolve(t.Context(), f.ref("away"))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
	require.Equal(t, []string{""}, seen, "no credentials for another host")

	// Within the registry the credentials go along.
	digest, err = r.Resolve(t.Context(), f.ref("here"))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
	require.Equal(t, 0, f.unauthorized)
}

func TestPublicOnly(t *testing.T) {
	for _, address := range []string{
		"127.0.0.1:443", "[::1]:443", "10.1.2.3:443", "172.16.0.1:5000", "192.168.1.1:443",
		"169.254.169.254:80", "100.64.0.1:443", "[fd00::1]:443", "[fe80::1]:443",
		"[::ffff:10.0.0.1]:443", "0.0.0.0:443", "224.0.0.1:443", "not-an-address",
	} {
		require.Error(t, publicOnly("tcp", address, nil), address)
	}

	for _, address := range []string{"1.1.1.1:443", "[2606:4700::1111]:443"} {
		require.NoError(t, publicOnly("tcp", address, nil), address)
	}

	f := newFakeRegistry(t, false)

	r, err := NewResolver(testLogger(), &Config{PlainHTTP: []string{f.host()}}, nil)
	require.NoError(t, err)

	_, err = r.Resolve(t.Context(), f.host()+"/ethpandaops/geth:latest")
	require.ErrorIs(t, err, errPrivateAddress)
}
