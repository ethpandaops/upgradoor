// Package registry resolves image tags to manifest digests against OCI
// distribution registries, for the agent's registry checks.
//
// The digest returned for a tag is the digest of the manifest the tag points
// at: the index digest for a multi-arch tag, which is what Docker records in
// RepoDigests after a pull. Comparing the two tells whether a container is
// behind its tag.
//
// # Request budget
//
// Docker Hub does not count manifest HEAD requests against the pull rate
// limit, but it does count manifest GETs. A resolution therefore costs one
// HEAD on /v2/<path>/manifests/<tag>, answered from the Docker-Content-Digest
// header. A GET of the tag happens only when a registry sends no such header
// (or refuses HEAD), in which case the body is hashed.
//
// The revision label (org.opencontainers.image.revision) needs the platform
// manifest and the image config. It is looked up once per repository and
// digest, the first time that digest is seen, and cached for the life of the
// Resolver. A failed lookup never fails Resolve: it yields an empty revision
// and is tried again later, at most three times per digest and no more often
// than every ten minutes, so an unchanged tag still costs one HEAD per poll.
// A lookup interrupted by the caller's context does not count. The lookup
// spends counted GETs; Options.SkipRevision turns it off altogether, which is
// the right setting when the revision is read from the local image instead.
//
// # Authentication
//
// Credentials come from the "auths" map of a Docker config.json (an "auth"
// value or username/password; docker.io, index.docker.io and
// registry-1.docker.io are the same registry; when several entries name one
// registry the most specific key wins, deterministically). They are sent as HTTP Basic to
// the registry, and to the token service when the registry answers with a
// Bearer challenge. Tokens are requested with the scope
// repository:<path>:pull, cached per repository, and fetched again once when
// the registry rejects a cached one. Credential helpers ("credHelpers",
// "credsStore") and identity tokens are NOT supported: a registry whose login
// lives in a helper is accessed anonymously.
//
// # Provenance
//
// Adapted from rolloor's internal/registry (github.com/ethpandaops/rolloor):
// reference parsing, the token flow and the auth file reader started there.
// Changed here: HEAD-only resolution, the revision lookup decoupled from the
// digest, reference validation, token expiry, logrus.
package registry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent"
)

const (
	// acceptManifests lists the manifest types a tag may resolve to, index
	// types first so that a multi-arch tag answers with its index.
	acceptManifests = "application/vnd.oci.image.index.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, " +
		"application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.v2+json"

	headerContentDigest = "Docker-Content-Digest"
	userAgent           = "upgradoor-agent"

	defaultTimeout = 30 * time.Second
	maxRedirects   = 10

	maxManifestBytes = 4 << 20
	maxConfigBytes   = 4 << 20
	maxTokenBytes    = 1 << 20
	// maxDrainBytes bounds what is read from a body that is thrown away, so
	// that the connection can be reused.
	maxDrainBytes = 64 << 10
)

// errBodyTooLarge is returned when a response exceeds its size cap.
var errBodyTooLarge = errors.New("response body exceeds the size limit")

// StatusError is a registry or token service answer other than 200.
type StatusError struct {
	Method string
	// URL is the request URL without query.
	URL  string
	Code int
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("registry: %s %s: %d %s", e.Method, e.URL, e.Code, http.StatusText(e.Code))
}

// Options configure a Resolver.
type Options struct {
	// AuthFile is a Docker config.json whose "auths" map holds registry
	// credentials. Empty or missing means anonymous access; a file that
	// cannot be read or parsed is an error. credHelpers and credsStore are
	// not supported.
	AuthFile string
	// PlainHTTP lists registry hosts (as written in image references,
	// host[:port]) that are spoken to over http instead of https.
	PlainHTTP []string
	// CAFile is a PEM bundle trusted in addition to the system roots, for
	// registries behind a private certificate authority.
	CAFile string
	// Timeout bounds every single request, body included (default 30s).
	Timeout time.Duration
	// Platform ("os/arch" or "os/arch/variant") selects the manifest whose
	// config carries the revision label when a tag is a multi-arch index.
	// It defaults to linux/<GOARCH of the agent> and has no influence on the
	// digest.
	Platform string
	// SkipRevision disables the revision lookup: Resolve then never sends
	// more than its HEAD (or the fallback GET) and returns no revision.
	//
	// With the lookup on (the zero value), every digest seen for the first
	// time costs up to two GETs that Docker Hub counts against the pull rate
	// limit: the platform manifest of a multi-arch tag and the image config.
	// A caller that reads the revision label from the local image after the
	// pull (the upgradoor design, PLAN.md section 13) does not need the
	// registry's answer and should set SkipRevision to stay at zero counted
	// requests.
	SkipRevision bool
}

// Resolver resolves tags against OCI distribution registries. It is safe for
// concurrent use.
type Resolver struct {
	log          logrus.FieldLogger
	client       *http.Client
	creds        map[string]credentials // by Reference.Host
	plainHTTP    map[string]struct{}
	platform     platform
	skipRevision bool
	now          func() time.Time

	mu            sync.Mutex
	tokens        map[string]token          // by API host/path
	revisions     map[string]*revisionEntry // by API host/path@digest
	revisionOrder []string
}

var _ agent.Registry = (*Resolver)(nil)

// systemRoots is x509.SystemCertPool, replaceable so its failure is testable.
var systemRoots = x509.SystemCertPool

// New builds a Resolver. It reads the auth file and the CA file once; it does
// not touch the network. A nil log discards the package's log output.
//
//nolint:gocritic // hugeParam: Options by value is the agreed constructor signature; New runs once
func New(log logrus.FieldLogger, opts Options) (*Resolver, error) {
	plat, err := parsePlatform(opts.Platform)
	if err != nil {
		return nil, err
	}

	if isNilLogger(log) {
		discard := logrus.New()
		discard.SetOutput(io.Discard)

		log = discard
	}

	r := &Resolver{
		log:          log.WithField("package", "registry"),
		creds:        map[string]credentials{},
		plainHTTP:    make(map[string]struct{}, len(opts.PlainHTTP)),
		platform:     plat,
		skipRevision: opts.SkipRevision,
		now:          time.Now,
		tokens:       make(map[string]token, 8),
		revisions:    make(map[string]*revisionEntry, 16),
	}

	for _, host := range opts.PlainHTTP {
		if host = authHost(strings.TrimSpace(host)); host != "" {
			r.plainHTTP[host] = struct{}{}
		}
	}

	if opts.AuthFile != "" {
		creds, unsupported, err := loadCredentials(r.log, opts.AuthFile)
		if err != nil {
			return nil, err
		}

		r.creds = creds

		if unsupported {
			r.log.WithField("auth_file", opts.AuthFile).Warn(
				"auth file names a credential helper or store, which is not supported; " +
					"only its \"auths\" entries are used")
		}
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if opts.CAFile != "" {
		pool, err := loadRoots(opts.CAFile)
		if err != nil {
			return nil, err
		}

		tlsConfig.RootCAs = pool
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	r.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSClientConfig:       tlsConfig,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
		CheckRedirect: checkRedirect,
	}

	return r, nil
}

// isNilLogger reports whether log is nil or holds a nil logrus pointer, which
// would panic on first use.
func isNilLogger(log logrus.FieldLogger) bool {
	switch l := log.(type) {
	case nil:
		return true
	case *logrus.Logger:
		return l == nil
	case *logrus.Entry:
		return l == nil
	default:
		return false
	}
}

// loadRoots returns the system roots plus the certificates of a PEM file.
func loadRoots(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry: read CA file: %w", err)
	}

	pool, err := systemRoots()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("registry: CA file %s holds no PEM certificates", path)
	}

	return pool, nil
}

// checkRedirect follows redirects (blob downloads are redirected to object
// storage) but never from https to anything else. net/http already drops the
// Authorization header when a redirect leaves the host.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}

	if via[0].URL.Scheme == schemeHTTPS && req.URL.Scheme != schemeHTTPS {
		return errors.New("refusing a redirect from https to " + req.URL.Scheme)
	}

	return nil
}

// Close releases idle connections. The Resolver stays usable.
func (r *Resolver) Close() {
	r.client.CloseIdleConnections()
}

// Resolve returns the digest of the manifest that ref ([host/]path[:tag])
// points at, the index digest for a multi-arch tag, and the image's revision
// label when it is known. A revision that cannot be determined is returned
// empty; only a failure to determine the digest is an error. Registry answers
// other than 200 are reported as *StatusError.
func (r *Resolver) Resolve(ctx context.Context, ref string) (digest, revision string, err error) {
	parsed, err := ParseReference(ref)
	if err != nil {
		return "", "", err
	}

	digest, err = r.head(ctx, parsed)
	if err != nil {
		return "", "", err
	}

	var body []byte

	if digest == "" {
		body, digest, err = r.tagManifest(ctx, parsed)
		if err != nil {
			return "", "", err
		}
	}

	if r.skipRevision {
		return digest, "", nil
	}

	return digest, r.revision(ctx, parsed, digest, body), nil
}

// head returns the tag's digest from a HEAD request, or "" when the registry
// sends no digest header or does not serve HEAD.
func (r *Resolver) head(ctx context.Context, ref Reference) (string, error) {
	resp, err := r.do(ctx, ref, http.MethodHead, r.baseURL(ref)+"/manifests/"+ref.Tag, acceptManifests)
	if err != nil {
		var status *StatusError
		if errors.As(err, &status) &&
			(status.Code == http.StatusMethodNotAllowed || status.Code == http.StatusNotImplemented) {
			return "", nil
		}

		return "", err
	}

	closeBody(resp)

	digest := resp.Header.Get(headerContentDigest)
	if digest != "" && !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("registry: %s sent an invalid %s header", ref.APIHost(), headerContentDigest)
	}

	return digest, nil
}

// tagManifest fetches the manifest of the tag with a GET. The digest is the
// registry's digest header when it sends one and the hash of the body
// otherwise.
func (r *Resolver) tagManifest(ctx context.Context, ref Reference) (body []byte, digest string, err error) {
	u := r.baseURL(ref) + "/manifests/" + ref.Tag

	resp, err := r.do(ctx, ref, http.MethodGet, u, acceptManifests)
	if err != nil {
		return nil, "", err
	}

	defer closeBody(resp)

	body, err = readCapped(resp.Body, maxManifestBytes)
	if err != nil {
		return nil, "", fmt.Errorf("registry: read manifest of %s: %w", ref, err)
	}

	digest = resp.Header.Get(headerContentDigest)

	switch {
	case digest == "":
		digest = sha256Digest(body)
	case !digestPattern.MatchString(digest):
		return nil, "", fmt.Errorf("registry: %s sent an invalid %s header", ref.APIHost(), headerContentDigest)
	}

	return body, digest, nil
}

// baseURL is the repository's /v2/ URL.
func (r *Resolver) baseURL(ref Reference) string {
	scheme := schemeHTTPS
	if r.isPlainHTTP(ref) {
		scheme = schemeHTTP
	}

	return scheme + "://" + ref.APIHost() + "/v2/" + ref.Path
}

// isPlainHTTP reports whether the registry of ref is configured as plain http.
func (r *Resolver) isPlainHTTP(ref Reference) bool {
	_, ok := r.plainHTTP[ref.Host]

	return ok
}

// do performs a request against a repository and returns its 200 response.
// On 401 it drops the cached token, follows the Bearer challenge and retries
// once; that covers both the first contact and an expired token. Every other
// outcome is an error, a *StatusError when the registry answered.
func (r *Resolver) do(ctx context.Context, ref Reference, method, u, accept string) (*http.Response, error) {
	key := ref.APIHost() + "/" + ref.Path
	bearer := r.cachedToken(key)

	resp, err := r.send(ctx, ref, method, u, accept, bearer)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := bearerChallenge(resp.Header)

		closeBody(resp)
		r.dropToken(key, bearer)

		if challenge == nil {
			return nil, &StatusError{Method: method, URL: u, Code: http.StatusUnauthorized}
		}

		fresh, err := r.fetchToken(ctx, ref, challenge)
		if err != nil {
			return nil, err
		}

		r.storeToken(key, fresh)

		resp, err = r.send(ctx, ref, method, u, accept, fresh.value)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusUnauthorized {
			r.dropToken(key, fresh.value)
		}
	}

	if resp.StatusCode != http.StatusOK {
		closeBody(resp)

		return nil, &StatusError{Method: method, URL: u, Code: resp.StatusCode}
	}

	return resp, nil
}

// send performs one request, with the bearer token when there is one and the
// registry's basic credentials otherwise.
func (r *Resolver) send(
	ctx context.Context, ref Reference, method, u, accept, bearer string,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("registry: build request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if creds, ok := r.creds[ref.Host]; ok {
		req.Header.Set("Authorization", creds.basic())
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}

	return resp, nil
}

// readCapped reads a whole body and fails when it is longer than limit.
func readCapped(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}

	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}

	return data, nil
}

// closeBody discards what is left of a response body, within reason, and
// closes it.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	_ = resp.Body.Close()
}

// sha256Digest renders the sha256 digest of data.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}

// platform is an os/arch[/variant] selector.
type platform struct {
	os, arch, variant string
}

// parsePlatform reads "os/arch" or "os/arch/variant"; empty means
// linux/<GOARCH>.
func parsePlatform(s string) (platform, error) {
	if s == "" {
		return platform{os: "linux", arch: runtime.GOARCH}, nil
	}

	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return platform{}, fmt.Errorf("registry: platform %q: want os/arch or os/arch/variant", s)
	}

	for _, part := range parts {
		if part == "" {
			return platform{}, fmt.Errorf("registry: platform %q: want os/arch or os/arch/variant", s)
		}
	}

	p := platform{os: parts[0], arch: parts[1]}
	if len(parts) == 3 {
		p.variant = parts[2]
	}

	return p, nil
}
