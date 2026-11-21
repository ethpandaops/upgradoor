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

	"github.com/ethpandaops/upgradoor/pkg/imageref"
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
	// tlsHandshakeTimeout bounds the TLS handshake of a new connection.
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout and maxIdleConns bound the kept-alive connections.
	idleConnTimeout = 90 * time.Second
	maxIdleConns    = 16
	maxRedirects    = 10

	maxManifestBytes = 4 << 20
	maxTokenBytes    = 1 << 20
	// maxDrainBytes bounds a discarded body, so the connection is reused.
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

// Resolver looks tags up at OCI distribution registries. Safe for concurrent use.
type Resolver interface {
	// Resolve returns the manifest digest a tag reference points at; the
	// index digest for a multi-arch tag.
	Resolve(ctx context.Context, ref string) (string, error)
}

// resolver is the Resolver of real registries. It resolves with a HEAD and
// falls back to a GET only without a digest header, since Docker Hub counts
// manifest GETs, not HEADs, against the pull rate limit.
type resolver struct {
	client    *http.Client
	logins    Logins
	plainHTTP map[string]struct{}
	now       func() time.Time

	mu     sync.Mutex
	tokens map[string]token // by API host/path
}

var _ Resolver = (*resolver)(nil)

// NewResolver returns the resolver for cfg and logins; it reads the CA file
// once and does not touch the network.
func NewResolver(_ logrus.FieldLogger, cfg *Config, logins Logins) (Resolver, error) {
	r := &resolver{
		logins:    logins,
		plainHTTP: make(map[string]struct{}, len(cfg.PlainHTTP)),
		now:       time.Now,
		tokens:    make(map[string]token, 8),
	}

	for _, host := range cfg.PlainHTTP {
		if host = imageref.RegistryHost(host); host != "" {
			r.plainHTTP[host] = struct{}{}
		}
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.CAFile != "" {
		pool, err := loadRoots(cfg.CAFile)
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
			MaxIdleConns:          maxIdleConns,
			IdleConnTimeout:       idleConnTimeout,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
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

// checkRedirect refuses a downgrade from https and drops the credentials on
// a redirect to another host; net/http alone still sends them to a subdomain.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}

	if via[0].URL.Scheme == schemeHTTPS && req.URL.Scheme != schemeHTTPS {
		return errors.New("refusing a redirect from https to " + req.URL.Scheme)
	}

	if req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}

	return nil
}

// Resolve implements Resolver; a registry answer other than 200 is a *statusError.
func (r *resolver) Resolve(ctx context.Context, ref string) (string, error) {
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
func (r *resolver) head(ctx context.Context, ref imageref.Reference) (string, error) {
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

// tagManifest GETs the tag's manifest and returns the digest header, or the
// hash of the body without one.
func (r *resolver) tagManifest(ctx context.Context, ref imageref.Reference) (string, error) {
	resp, err := r.do(ctx, ref, http.MethodGet, r.baseURL(ref)+"/manifests/"+ref.Tag)
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
func (r *resolver) baseURL(ref imageref.Reference) string {
	scheme := schemeHTTPS
	if r.isPlainHTTP(ref) {
		scheme = schemeHTTP
	}

	return scheme + "://" + apiHost(ref) + "/v2/" + ref.Path
}

func (r *resolver) isPlainHTTP(ref imageref.Reference) bool {
	_, ok := r.plainHTTP[ref.Host]

	return ok
}

// do performs a request and returns its 200 response. On 401 it drops the
// cached token, follows the Bearer challenge and retries once.
func (r *resolver) do(ctx context.Context, ref imageref.Reference, method, u string) (*http.Response, error) {
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
func (r *resolver) send(
	ctx context.Context, ref imageref.Reference, method, u, bearer string,
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

// closeBody drains a bounded rest of the body, so the connection is reused,
// and closes it.
func closeBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBytes))
	_ = resp.Body.Close()
}

// sha256Digest renders the sha256 digest of data.
func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)

	return "sha256:" + hex.EncodeToString(sum[:])
}
