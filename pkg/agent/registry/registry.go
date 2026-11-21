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
// The registry is asked for nothing else: an unchanged tag costs one HEAD per
// poll. The revision label of an image is read from the local image after
// the pull (pkg/agent/docker).
//
// # Authentication
//
// Logins (see LoadLogins) are sent as HTTP Basic to the registry, and to the
// token service when the registry answers with a Bearer challenge. Tokens are
// requested with the scope repository:<path>:pull, cached per repository, and
// fetched again once when the registry rejects a cached one.
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

	// requestTimeout bounds every single request, body included.
	requestTimeout = 30 * time.Second
	maxRedirects   = 10

	maxManifestBytes = 4 << 20
	maxTokenBytes    = 1 << 20
	// maxDrainBytes bounds what is read from a body that is thrown away, so
	// that the connection can be reused.
	maxDrainBytes = 64 << 10
)

// errBodyTooLarge is returned when a response exceeds its size cap.
var errBodyTooLarge = errors.New("response body exceeds the size limit")

// statusError is a registry or token service answer other than 200.
type statusError struct {
	Method string
	// URL is the request URL without query.
	URL  string
	Code int
}

// Error implements error.
func (e *statusError) Error() string {
	return fmt.Sprintf("registry: %s %s: %d %s", e.Method, e.URL, e.Code, http.StatusText(e.Code))
}

// Options configure a Resolver.
type Options struct {
	// Logins are the registry logins; a registry without one is accessed
	// anonymously.
	Logins Logins
	// PlainHTTP lists registry hosts (as written in image references,
	// host[:port]) that are spoken to over http instead of https.
	PlainHTTP []string
	// CAFile is a PEM bundle trusted in addition to the system roots, for
	// registries behind a private certificate authority.
	CAFile string
}

// Resolver resolves tags against OCI distribution registries. It is safe for
// concurrent use.
type Resolver struct {
	log       logrus.FieldLogger
	client    *http.Client
	logins    Logins
	plainHTTP map[string]struct{}
	now       func() time.Time

	mu     sync.Mutex
	tokens map[string]token // by API host/path
}

var _ agent.Registry = (*Resolver)(nil)

// New builds a Resolver. It reads the CA file once; it does not touch the
// network.
func New(log logrus.FieldLogger, opts Options) (*Resolver, error) {
	r := &Resolver{
		log:       log.WithField("package", "registry"),
		logins:    opts.Logins,
		plainHTTP: make(map[string]struct{}, len(opts.PlainHTTP)),
		now:       time.Now,
		tokens:    make(map[string]token, 8),
	}

	for _, host := range opts.PlainHTTP {
		if host = agent.RegistryHost(host); host != "" {
			r.plainHTTP[host] = struct{}{}
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

	r.client = &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSClientConfig:       tlsConfig,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: requestTimeout,
		},
		CheckRedirect: checkRedirect,
	}

	return r, nil
}

// loadRoots returns the system roots plus the certificates of a PEM file.
func loadRoots(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry: read CA file: %w", err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("registry: CA file %s holds no PEM certificates", path)
	}

	return pool, nil
}

// checkRedirect follows redirects but never from https to anything else. net/http already drops the
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

// Resolve returns the digest of the manifest that ref ([host/]path[:tag])
// points at, the index digest for a multi-arch tag. Registry answers other
// than 200 are reported as *statusError.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	parsed, err := parseTagReference(ref)
	if err != nil {
		return "", err
	}

	digest, err := r.head(ctx, parsed)
	if err != nil || digest != "" {
		return digest, err
	}

	return r.tagManifest(ctx, parsed)
}

// head returns the tag's digest from a HEAD request, or "" when the registry
// sends no digest header or does not serve HEAD.
func (r *Resolver) head(ctx context.Context, ref agent.Reference) (string, error) {
	resp, err := r.do(ctx, ref, http.MethodHead, r.baseURL(ref)+"/manifests/"+ref.Tag)
	if err != nil {
		var status *statusError
		if errors.As(err, &status) &&
			(status.Code == http.StatusMethodNotAllowed || status.Code == http.StatusNotImplemented) {
			return "", nil
		}

		return "", err
	}

	closeBody(resp)

	digest := resp.Header.Get(headerContentDigest)
	if digest != "" && !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("registry: %s sent an invalid %s header", apiHost(ref), headerContentDigest)
	}

	return digest, nil
}

// tagManifest fetches the manifest of the tag with a GET and returns its
// digest: the registry's digest header when it sends one and the hash of the
// body otherwise.
func (r *Resolver) tagManifest(ctx context.Context, ref agent.Reference) (string, error) {
	u := r.baseURL(ref) + "/manifests/" + ref.Tag

	resp, err := r.do(ctx, ref, http.MethodGet, u)
	if err != nil {
		return "", err
	}

	defer closeBody(resp)

	body, err := readCapped(resp.Body, maxManifestBytes)
	if err != nil {
		return "", fmt.Errorf("registry: read manifest of %s: %w", ref, err)
	}

	digest := resp.Header.Get(headerContentDigest)

	switch {
	case digest == "":
		digest = sha256Digest(body)
	case !digestPattern.MatchString(digest):
		return "", fmt.Errorf("registry: %s sent an invalid %s header", apiHost(ref), headerContentDigest)
	}

	return digest, nil
}

// baseURL is the repository's /v2/ URL.
func (r *Resolver) baseURL(ref agent.Reference) string {
	scheme := schemeHTTPS
	if r.isPlainHTTP(ref) {
		scheme = schemeHTTP
	}

	return scheme + "://" + apiHost(ref) + "/v2/" + ref.Path
}

// isPlainHTTP reports whether the registry of ref is configured as plain http.
func (r *Resolver) isPlainHTTP(ref agent.Reference) bool {
	_, ok := r.plainHTTP[ref.Host]

	return ok
}

// do performs a request against a repository and returns its 200 response.
// On 401 it drops the cached token, follows the Bearer challenge and retries
// once; that covers both the first contact and an expired token. Every other
// outcome is an error, a *statusError when the registry answered.
func (r *Resolver) do(ctx context.Context, ref agent.Reference, method, u string) (*http.Response, error) {
	key := apiHost(ref) + "/" + ref.Path
	bearer := r.cachedToken(key)

	resp, err := r.send(ctx, ref, method, u, bearer)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := bearerChallenge(resp.Header)

		closeBody(resp)
		r.dropToken(key, bearer)

		if challenge == nil {
			return nil, &statusError{Method: method, URL: u, Code: http.StatusUnauthorized}
		}

		fresh, err := r.fetchToken(ctx, ref, challenge)
		if err != nil {
			return nil, err
		}

		r.storeToken(key, fresh)

		resp, err = r.send(ctx, ref, method, u, fresh.value)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusUnauthorized {
			r.dropToken(key, fresh.value)
		}
	}

	if resp.StatusCode != http.StatusOK {
		closeBody(resp)

		return nil, &statusError{Method: method, URL: u, Code: resp.StatusCode}
	}

	return resp, nil
}

// send performs one request, with the bearer token when there is one and the
// registry's basic credentials otherwise.
func (r *Resolver) send(
	ctx context.Context, ref agent.Reference, method, u, bearer string,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("registry: build request: %w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	req.Header.Set("Accept", acceptManifests)

	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	} else if login, ok := r.logins[ref.Host]; ok {
		req.Header.Set("Authorization", login.basic())
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
