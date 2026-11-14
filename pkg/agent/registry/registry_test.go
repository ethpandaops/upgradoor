package registry

import (
	"context"
	"crypto/x509"
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
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	repoPath    = "org/app"
	tagMulti    = "multi"
	tagSingle   = "single"
	revAMD64    = "rev-amd64"
	revARM64    = "rev-arm64"
	testToken   = "tok-1"
	testService = "fake-registry"
	testBasic   = "user:secret"
	plainAMD64  = "linux/amd64"
	kindManifst = "/manifests/"
	kindBlob    = "/blobs/"
)

// fakeRegistry is an OCI distribution registry with one repository, org/app.
// Content is addressed by its real sha256 digest.
type fakeRegistry struct {
	srv *httptest.Server

	mu       sync.Mutex
	hits     map[string]int    // "METHOD path" -> count
	content  map[string][]byte // digest -> body, manifests and blobs alike
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

	indexDigest, amd64Digest, arm64Digest, amd64Config, arm64Config string
}

type fakePlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type fakeDescriptor struct {
	Digest   string        `json:"digest"`
	Platform *fakePlatform `json:"platform,omitempty"`
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	raw, err := json.Marshal(v)
	require.NoError(t, err)

	return raw
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

	f.amd64Config = f.add(configBody(t, revAMD64))
	f.arm64Config = f.add(configBody(t, revARM64))
	f.amd64Digest = f.add(mustJSON(t, map[string]any{"config": fakeDescriptor{Digest: f.amd64Config}}))
	f.arm64Digest = f.add(mustJSON(t, map[string]any{"config": fakeDescriptor{Digest: f.arm64Config}}))
	f.indexDigest = f.add(indexBody(t,
		fakeDescriptor{Digest: f.arm64Digest, Platform: &fakePlatform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
		fakeDescriptor{Digest: f.amd64Digest, Platform: &fakePlatform{OS: "linux", Architecture: "amd64"}},
		fakeDescriptor{
			Digest:   "sha256:" + strings.Repeat("9", 64),
			Platform: &fakePlatform{OS: platformUnknown, Architecture: platformUnknown},
		},
	))
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

func configBody(t *testing.T, revision string) []byte {
	t.Helper()

	labels := map[string]string{"other": "x"}
	if revision != "" {
		labels[RevisionLabel] = revision
	}

	return mustJSON(t, map[string]any{"config": map[string]any{"Labels": labels}})
}

func indexBody(t *testing.T, manifests ...fakeDescriptor) []byte {
	t.Helper()

	return mustJSON(t, map[string]any{
		"mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": manifests,
	})
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

// handle replaces the answer for one "/v2/org/app/..." suffix, e.g.
// "/blobs/sha256:…". The override runs after authentication.
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
	case strings.HasPrefix(suffix, kindBlob):
		f.serveContent(w, r, strings.TrimPrefix(suffix, kindBlob), "")
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

// newResolver builds a resolver that talks plain http to the fake registry.
func newResolver(t *testing.T, f *fakeRegistry, mod func(o *Options)) *Resolver {
	t.Helper()

	opts := Options{PlainHTTP: []string{f.host()}, Timeout: 5 * time.Second, Platform: plainAMD64}
	if mod != nil {
		mod(&opts)
	}

	r, err := New(testLogger(), opts)
	require.NoError(t, err)
	t.Cleanup(r.Close)

	return r
}

func writeAuthFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func authFileFor(t *testing.T, host, userPass string) string {
	t.Helper()

	return writeAuthFile(t, `{"auths":{"`+host+`":{"auth":"`+
		base64.StdEncoding.EncodeToString([]byte(userPass))+`"}}}`)
}

func TestResolveMultiArchHeadOnly(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	digest, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest, "the index digest, not a platform manifest")
	require.Equal(t, revAMD64, revision)

	// First sight of the digest: one HEAD, then index, platform manifest and
	// config, each once.
	require.Equal(t, 1, f.heads())
	require.Equal(t, 3, f.gets())
	require.Equal(t, 0, f.count(http.MethodGet, kindManifst+tagMulti), "the tag itself is never fetched with GET")
	require.Equal(t, 1, f.count(http.MethodGet, kindManifst+f.indexDigest))
	require.Equal(t, 1, f.count(http.MethodGet, kindManifst+f.amd64Digest))
	require.Equal(t, 1, f.count(http.MethodGet, kindBlob+f.amd64Config))

	// Unchanged tag: exactly one HEAD per resolution, no GET at all.
	for i := range 5 {
		digest, revision, err = r.Resolve(t.Context(), f.ref(tagMulti))
		require.NoError(t, err)
		require.Equal(t, f.indexDigest, digest)
		require.Equal(t, revAMD64, revision)
		require.Equal(t, 2+i, f.heads())
		require.Equal(t, 3, f.gets())
	}

	// The tag moves: the new digest is seen with the next HEAD and its
	// revision is looked up once.
	moved := f.tag(tagMulti, mustJSON(t, map[string]any{"config": fakeDescriptor{Digest: f.arm64Config}, "x": 1}))

	digest, revision, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, moved, digest)
	require.Equal(t, revARM64, revision)
	require.Equal(t, 7, f.heads())
	require.Equal(t, 5, f.gets())
}

func TestResolveSingleManifest(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	digest, revision, err := r.Resolve(t.Context(), f.ref(tagSingle))
	require.NoError(t, err)
	require.Equal(t, f.amd64Digest, digest)
	require.Equal(t, revAMD64, revision)
	require.Equal(t, 1, f.heads())
	require.Equal(t, 2, f.gets())
}

func TestResolveSkipRevision(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, func(o *Options) { o.SkipRevision = true })

	for range 3 {
		digest, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.NoError(t, err)
		require.Equal(t, f.indexDigest, digest)
		require.Empty(t, revision)
	}

	require.Equal(t, 3, f.heads())
	require.Equal(t, 0, f.gets())
}

func TestResolvePlatformSelection(t *testing.T) {
	cases := []struct {
		name, platform, want string
	}{
		{"amd64", plainAMD64, revAMD64},
		{"arm64", "linux/arm64", revARM64},
		{"arm64 with variant", "linux/arm64/v8", revARM64},
		{"variant mismatch falls back to the first image", "linux/arm64/v7", revARM64},
		{"unknown platform falls back to the first image", "plan9/mips", revARM64},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRegistry(t, false)
			r := newResolver(t, f, func(o *Options) { o.Platform = c.platform })

			digest, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
			require.NoError(t, err)
			require.Equal(t, f.indexDigest, digest)
			require.Equal(t, c.want, revision)
		})
	}
}

func TestParsePlatform(t *testing.T) {
	p, err := parsePlatform("")
	require.NoError(t, err)
	require.Equal(t, "linux", p.os)
	require.NotEmpty(t, p.arch)

	p, err = parsePlatform("linux/arm/v7")
	require.NoError(t, err)
	require.Equal(t, platform{os: "linux", arch: "arm", variant: "v7"}, p)

	for _, bad := range []string{"linux", "linux/", "/amd64", "a/b/c/d", "linux//v7"} {
		_, err = parsePlatform(bad)
		require.Error(t, err, bad)

		_, err = New(testLogger(), Options{Platform: bad})
		require.Error(t, err, bad)
	}
}

func TestResolveWithoutDigestHeader(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.noDigestHeader = true })

	r := newResolver(t, f, nil)

	digest, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest, "computed from the body")
	require.Equal(t, revAMD64, revision)

	// HEAD, GET of the tag (reused for the revision), platform manifest, config.
	require.Equal(t, 1, f.heads())
	require.Equal(t, 1, f.count(http.MethodGet, kindManifst+tagMulti))
	require.Equal(t, 0, f.count(http.MethodGet, kindManifst+f.indexDigest))
	require.Equal(t, 3, f.gets())

	// Later resolutions need the GET for the digest but nothing else.
	digest, revision, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
	require.Equal(t, revAMD64, revision)
	require.Equal(t, 2, f.heads())
	require.Equal(t, 4, f.gets())
}

func TestResolveWithoutHead(t *testing.T) {
	f := newFakeRegistry(t, false)
	f.set(func(f *fakeRegistry) { f.noHead = true })

	r := newResolver(t, f, func(o *Options) { o.SkipRevision = true })

	digest, _, err := r.Resolve(t.Context(), f.ref(tagSingle))
	require.NoError(t, err)
	require.Equal(t, f.amd64Digest, digest, "from the digest header of the GET")
	require.Equal(t, 1, f.heads())
	require.Equal(t, 1, f.gets())

	f.set(func(f *fakeRegistry) { f.getDigest = "not-a-digest" })

	_, _, err = r.Resolve(t.Context(), f.ref(tagSingle))
	require.ErrorContains(t, err, "invalid "+headerContentDigest)
}

func TestResolveInvalidDigestHeader(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	for _, bad := range []string{"sha256:../../x", "sha256:abc", "nonsense", "sha256:" + strings.Repeat("G", 64)} {
		f.set(func(f *fakeRegistry) { f.headDigest = bad })

		_, _, err := r.Resolve(t.Context(), f.ref(tagMulti))
		require.ErrorContains(t, err, "invalid "+headerContentDigest, bad)
	}

	require.Equal(t, 0, f.gets())
}

func TestResolveErrors(t *testing.T) {
	f := newFakeRegistry(t, false)
	r := newResolver(t, f, nil)

	_, _, err := r.Resolve(t.Context(), "")
	require.Error(t, err)

	_, _, err = r.Resolve(t.Context(), f.host()+"/org/app@sha256:"+strings.Repeat("a", 64))
	require.ErrorIs(t, err, ErrDigestReference)

	// Unknown tag: a StatusError with the registry's code.
	_, _, err = r.Resolve(t.Context(), f.ref("missing"))

	var status *StatusError

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

	_, _, err = r.Resolve(t.Context(), f.ref("broken"))
	require.ErrorAs(t, err, &status)
	require.Equal(t, http.StatusBadGateway, status.Code)
	require.Equal(t, http.MethodGet, status.Method)

	// Cancelled context.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = r.Resolve(ctx, f.ref(tagMulti))
	require.ErrorIs(t, err, context.Canceled)

	// Unreachable registry.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadHost := strings.TrimPrefix(dead.URL, "http://")
	dead.Close()

	r2, err := New(testLogger(), Options{PlainHTTP: []string{deadHost}, Timeout: 2 * time.Second})
	require.NoError(t, err)

	_, _, err = r2.Resolve(t.Context(), deadHost+"/org/app:x")
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

	_, _, err := r.Resolve(t.Context(), f.ref("huge"))
	require.ErrorIs(t, err, errBodyTooLarge)

	_, _, err = r.Resolve(t.Context(), f.ref("short"))
	require.ErrorContains(t, err, "read manifest")

	// A body that is not a manifest still has a digest; only the revision is lost.
	garbage := f.tag("garbage", []byte("this is not json"))

	digest, revision, err := r.Resolve(t.Context(), f.ref("garbage"))
	require.NoError(t, err)
	require.Equal(t, garbage, digest)
	require.Empty(t, revision)
}

func TestReadCapped(t *testing.T) {
	data, err := readCapped(strings.NewReader("12345"), 5)
	require.NoError(t, err)
	require.Equal(t, "12345", string(data))

	_, err = readCapped(strings.NewReader("123456"), 5)
	require.ErrorIs(t, err, errBodyTooLarge)
}

func TestPlainHTTPIsPerHost(t *testing.T) {
	f := newFakeRegistry(t, false)

	// Without the plain-http entry the resolver speaks TLS to an http server.
	r, err := New(testLogger(), Options{Timeout: 2 * time.Second})
	require.NoError(t, err)

	_, _, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.Error(t, err)
	require.Equal(t, 0, f.heads())

	// Entries are trimmed, lower-cased and may carry a scheme.
	r = newResolver(t, f, func(o *Options) {
		o.PlainHTTP = []string{"", "  http://" + strings.ToUpper(f.host()) + "/ "}
	})

	_, _, err = r.Resolve(t.Context(), f.ref(tagMulti))
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

	// The repository on the fake is org/app; Docker Hub official images live
	// under library/, so serve one there too.
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
			r, err := New(testLogger(), Options{
				AuthFile:     authFileFor(t, key, testBasic),
				SkipRevision: true,
			})
			require.NoError(t, err)

			transport := &hubTransport{target: strings.TrimPrefix(hub.URL, "http://"), base: http.DefaultTransport}
			r.client.Transport = transport

			for _, ref := range []string{"geth", "docker.io/library/geth:latest", "index.docker.io/geth"} {
				digest, _, err := r.Resolve(t.Context(), ref)
				require.NoError(t, err, ref)
				require.Equal(t, f.indexDigest, digest)
			}

			for _, host := range transport.hosts {
				require.Equal(t, "https://"+dockerHubAPIHost, host)
			}
		})
	}
}

func TestNewDefaultsAndClose(t *testing.T) {
	r, err := New(testLogger(), Options{})
	require.NoError(t, err)
	require.Equal(t, defaultTimeout, r.client.Timeout)
	require.Empty(t, r.creds)

	r, err = New(testLogger(), Options{Timeout: -time.Second})
	require.NoError(t, err)
	require.Equal(t, defaultTimeout, r.client.Timeout)

	r, err = New(testLogger(), Options{Timeout: 3 * time.Second})
	require.NoError(t, err)
	require.Equal(t, 3*time.Second, r.client.Timeout)
	r.Close()
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

	r := newResolver(t, f, func(o *Options) { o.Timeout = 100 * time.Millisecond })

	start := time.Now()
	_, _, err := r.Resolve(t.Context(), f.ref("slow"))
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
	r, err := New(testLogger(), Options{Timeout: 5 * time.Second, Platform: plainAMD64})
	require.NoError(t, err)

	_, _, err = r.Resolve(t.Context(), f.ref(tagMulti))
	require.ErrorContains(t, err, "certificate")
	r.Close()

	r, err = New(testLogger(), Options{CAFile: caFileFor(t, f.srv), Timeout: 5 * time.Second, Platform: plainAMD64})
	require.NoError(t, err)

	digest, revision, err := r.Resolve(t.Context(), f.ref(tagMulti))
	require.NoError(t, err)
	require.Equal(t, f.indexDigest, digest)
	require.Equal(t, revAMD64, revision)
	r.Close()
}

func TestCAFileErrors(t *testing.T) {
	_, err := New(testLogger(), Options{CAFile: filepath.Join(t.TempDir(), "missing.pem")})
	require.ErrorContains(t, err, "read CA file")

	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))

	_, err = New(testLogger(), Options{CAFile: empty})
	require.ErrorContains(t, err, "no PEM certificates")
}

func TestCAFileWithoutSystemRoots(t *testing.T) {
	f := newFakeRegistry(t, true)

	orig := systemRoots
	systemRoots = func() (*x509.CertPool, error) { return nil, errors.New("no system roots") }

	t.Cleanup(func() { systemRoots = orig })

	r, err := New(testLogger(), Options{CAFile: caFileFor(t, f.srv), SkipRevision: true})
	require.NoError(t, err)

	digest, _, err := r.Resolve(t.Context(), f.ref(tagSingle))
	require.NoError(t, err)
	require.Equal(t, f.amd64Digest, digest)
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

func TestBlobRedirectIsFollowed(t *testing.T) {
	f := newFakeRegistry(t, false)

	var seenAuth []string

	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		_, _ = w.Write(configBody(t, revAMD64))
	}))
	t.Cleanup(storage.Close)

	// The storage server answers on "localhost", a different host than the
	// registry's 127.0.0.1, so credentials must not follow the redirect.
	storageURL := strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)

	f.set(func(f *fakeRegistry) { f.basic = testBasic })
	f.handle(kindBlob+f.amd64Config, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storageURL+"/blob", http.StatusTemporaryRedirect)
	})

	r := newResolver(t, f, func(o *Options) { o.AuthFile = authFileFor(t, f.host(), testBasic) })

	digest, revision, err := r.Resolve(t.Context(), f.ref(tagSingle))
	require.NoError(t, err)
	require.Equal(t, f.amd64Digest, digest)
	require.Equal(t, revAMD64, revision)
	require.Equal(t, []string{""}, seenAuth, "credentials are not sent to the redirect target")
}

func TestStatusError(t *testing.T) {
	err := error(&StatusError{Method: http.MethodHead, URL: "https://r.example/v2/a/manifests/t", Code: 429})
	require.Equal(t, "registry: HEAD https://r.example/v2/a/manifests/t: 429 Too Many Requests", err.Error())
}

func TestCredentialHelperWarning(t *testing.T) {
	log, hook := logrustest.NewNullLogger()

	for _, content := range []string{
		`{"credsStore":"desktop"}`,
		`{"credHelpers":{"ghcr.io":"gh"},"auths":{"ghcr.io":{}}}`,
	} {
		hook.Reset()

		r, err := New(log, Options{AuthFile: writeAuthFile(t, content)})
		require.NoError(t, err)
		require.Empty(t, r.creds)
		require.Len(t, hook.AllEntries(), 1)
		assert.Equal(t, logrus.WarnLevel, hook.LastEntry().Level)
		assert.Contains(t, hook.LastEntry().Message, "not supported")
		assert.Equal(t, "registry", hook.LastEntry().Data["package"])
	}

	hook.Reset()

	_, err := New(log, Options{AuthFile: writeAuthFile(t, `{"auths":{}}`)})
	require.NoError(t, err)
	require.Empty(t, hook.AllEntries())
}

func TestNewWithoutLogger(t *testing.T) {
	f := newFakeRegistry(t, false)

	// Both the constructor (credential store warning) and Resolve (failed
	// revision lookup) log.
	f.tag("broken", []byte("<html>"))

	r, err := New(nil, Options{
		PlainHTTP: []string{f.host()},
		AuthFile:  writeAuthFile(t, `{"credsStore":"desktop"}`),
	})
	require.NoError(t, err)
	t.Cleanup(r.Close)

	digest, revision, err := r.Resolve(t.Context(), f.ref("broken"))
	require.NoError(t, err)
	require.Equal(t, sha256Digest([]byte("<html>")), digest)
	require.Empty(t, revision)

	// Typed nil pointers in the interface are nil loggers too.
	var (
		nilLogger *logrus.Logger
		nilEntry  *logrus.Entry
	)

	for _, log := range []logrus.FieldLogger{nilLogger, nilEntry} {
		_, err = New(log, Options{AuthFile: writeAuthFile(t, `{"credsStore":"desktop"}`)})
		require.NoError(t, err)
	}
}
