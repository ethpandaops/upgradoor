// Package webui serves the single-page web UI from an fs.FS (the embedded
// Vite build, or a directory during development).
//
// Rules:
//   - Files are served as they are; files below assets/ whose name carries a
//     content hash are cached forever, everything else is revalidated.
//   - Unknown paths get index.html, so client-side routes survive a reload,
//     unless they are below assets/ or end in a static file extension. A dot
//     alone does not make a path a file: host names may contain dots.
//   - Nothing below /api/ is ever answered with HTML: unknown API paths get a
//     JSON 404.
//   - Directories are never listed.
//   - Without an index.html (binary built without the UI) a placeholder page
//     is served.
//
// The UI reads its configuration from GET /api/v1/config; nothing is injected
// into the HTML.
package webui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	indexFile = "index.html"
	// assetsDir is where Vite writes content-hashed files.
	assetsDir = "assets/"

	cacheImmutable  = "public, max-age=31536000, immutable"
	cacheRevalidate = "no-cache"

	headerCacheControl = "Cache-Control"
	headerContentType  = "Content-Type"

	contentTypeHTML = "text/html; charset=utf-8"
	contentTypeJSON = "application/json; charset=utf-8"

	notFoundJSON = `{"error":"not found"}` + "\n"
)

// errDirectory is returned by readFile for directories, which are never
// listed and never replaced by the index page.
var errDirectory = errors.New("webui: is a directory")

// hashLen is the length of the content hash Vite puts into file names.
const hashLen = 8

// staticExtensions are the extensions of files a UI build can contain. A
// missing path with one of them is a missing file, never a client-side
// route.
var staticExtensions = map[string]struct{}{
	".js": {}, ".mjs": {}, ".css": {}, ".map": {}, ".json": {}, ".txt": {}, ".xml": {},
	".html": {}, ".htm": {}, ".wasm": {}, ".webmanifest": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".svg": {}, ".ico": {}, ".webp": {},
	".avif": {}, ".woff": {}, ".woff2": {}, ".ttf": {}, ".otf": {}, ".eot": {},
}

// isStaticFile reports whether name (a cleaned path without leading slash)
// can only be a file of the UI build.
func isStaticFile(name string) bool {
	if strings.HasPrefix(name, assetsDir) {
		return true
	}

	_, ok := staticExtensions[strings.ToLower(path.Ext(name))]

	return ok
}

// hashedName reports whether a file name carries a content hash before its
// extension the way Vite writes it: a "-" or "." and then exactly hashLen
// characters of [A-Za-z0-9_-], e.g. "index-BX3k2a9d.js", "index-BX3k2a9d.js.map"
// or "logo.3f9a1c0e.svg".
//
// A hash is told from a word by a digit or an upper-case letter, so a
// hand-named "my-component.js" or "app-settings.js" is not taken for hashed
// and cached forever. The rare hash of lower-case letters only is merely
// revalidated.
func hashedName(name string) bool {
	stem := strings.TrimSuffix(name, ".map")

	ext := path.Ext(stem)
	if ext == "" || ext == stem {
		return false
	}

	stem = strings.TrimSuffix(stem, ext)

	// A base name, the separator and the hash.
	if len(stem) < hashLen+2 {
		return false
	}

	sep := stem[len(stem)-hashLen-1]
	if sep != '-' && sep != '.' {
		return false
	}

	mixed := false

	for _, c := range []byte(stem[len(stem)-hashLen:]) {
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'Z':
			mixed = true
		case c >= 'a' && c <= 'z', c == '_', c == '-':
		default:
			return false
		}
	}

	return mixed
}

// placeholderPage is served when the binary carries no web UI.
const placeholderPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>upgradoor</title>
</head>
<body>
<h1>upgradoor</h1>
<p>This build does not include the web UI. The API is available below <code>/api/</code>.</p>
</body>
</html>
`

// Options configures the handler.
type Options struct {
	// AuthProviderURL is the authenticatoor base URL. Its origin is allowed
	// in the Content-Security-Policy so the UI can load its login client and
	// talk to it. Empty when the API runs open.
	AuthProviderURL string
}

// Handler serves the web UI.
type Handler struct {
	log  logrus.FieldLogger
	fsys fs.FS
	csp  string
}

var _ http.Handler = (*Handler)(nil)

// New returns a handler over fsys. A nil fsys, or one without index.html,
// yields the placeholder page.
func New(log logrus.FieldLogger, fsys fs.FS, opts Options) (*Handler, error) {
	csp, err := contentSecurityPolicy(opts.AuthProviderURL)
	if err != nil {
		return nil, err
	}

	return &Handler{
		log:  log.WithField("package", "webui"),
		fsys: fsys,
		csp:  csp,
	}, nil
}

// contentSecurityPolicy builds the policy for UI pages: everything from the
// own origin, plus the authenticatoor origin for its client script, its
// requests and its login frame.
func contentSecurityPolicy(authProviderURL string) (string, error) {
	authOrigin := ""

	if authProviderURL != "" {
		u, err := url.Parse(authProviderURL)
		if err != nil {
			return "", fmt.Errorf("webui: auth provider URL: %w", err)
		}

		if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return "", fmt.Errorf("webui: auth provider URL %q is not an http(s) URL",
				authProviderURL)
		}

		authOrigin = " " + u.Scheme + "://" + u.Host
	}

	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self'" + authOrigin,
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self' data:",
		"connect-src 'self'" + authOrigin,
		"frame-src 'self'" + authOrigin,
		"object-src 'none'",
		"base-uri 'self'",
		"form-action 'self'" + authOrigin,
		"frame-ancestors 'none'",
	}, "; "), nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean("/" + r.URL.Path)

	// API paths never fall back to the UI, whatever the method.
	if clean == "/api" || strings.HasPrefix(clean, "/api/") {
		w.Header().Set(headerContentType, contentTypeJSON)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set(headerCacheControl, "no-store")
		w.WriteHeader(http.StatusNotFound)
		h.write(w, []byte(notFoundJSON))

		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	h.setSecurityHeaders(w)

	name := strings.TrimPrefix(clean, "/")
	if name == "" {
		name = indexFile
	}

	content, err := h.readFile(name)

	switch {
	case err == nil:
		h.serveFile(w, r, name, content)
	case errors.Is(err, errDirectory):
		http.Error(w, "not found", http.StatusNotFound)
	case !errors.Is(err, fs.ErrNotExist):
		h.log.WithError(err).WithField("file", name).Warn("Cannot read web UI file")
		http.Error(w, "internal error", http.StatusInternalServerError)
	case name != indexFile && isStaticFile(name):
		// A missing file must not be answered with HTML: a browser would try
		// to run index.html as a script or stylesheet.
		http.Error(w, "not found", http.StatusNotFound)
	default:
		h.serveIndex(w, r)
	}
}

// serveIndex answers a client-side route with index.html, or with the
// placeholder when there is no UI.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	content, err := h.readFile(indexFile)

	switch {
	case err == nil:
		h.serveFile(w, r, indexFile, content)
	case errors.Is(err, fs.ErrNotExist):
		w.Header().Set(headerContentType, contentTypeHTML)
		w.Header().Set(headerCacheControl, cacheRevalidate)
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(placeholderPage))
	default:
		h.log.WithError(err).Warn("Cannot read web UI index")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// serveFile writes one file with its cache policy.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name string, content []byte) {
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = http.DetectContentType(content)
	}

	w.Header().Set(headerContentType, ctype)

	if strings.HasPrefix(name, assetsDir) && hashedName(path.Base(name)) {
		w.Header().Set(headerCacheControl, cacheImmutable)
	} else {
		sum := sha256.Sum256(content)

		w.Header().Set(headerCacheControl, cacheRevalidate)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:16])+`"`)
	}

	// ServeContent handles HEAD, ranges and If-None-Match.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(content))
}

// readFile returns a regular file's content. Invalid names and a missing file
// system report fs.ErrNotExist, directories errDirectory.
func (h *Handler) readFile(name string) ([]byte, error) {
	if h.fsys == nil || !fs.ValidPath(name) {
		return nil, fs.ErrNotExist
	}

	f, err := h.fsys.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid) {
			return nil, fs.ErrNotExist
		}

		return nil, fmt.Errorf("open %s: %w", name, err)
	}

	defer func() {
		if cerr := f.Close(); cerr != nil {
			h.log.WithError(cerr).WithField("file", name).Debug("Cannot close web UI file")
		}
	}()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", name, err)
	}

	if info.IsDir() {
		return nil, errDirectory
	}

	if !info.Mode().IsRegular() {
		return nil, fs.ErrNotExist
	}

	content, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}

	return content, nil
}

func (h *Handler) setSecurityHeaders(w http.ResponseWriter) {
	hdr := w.Header()
	hdr.Set("Content-Security-Policy", h.csp)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("Cross-Origin-Opener-Policy", "same-origin-allow-popups")
}

func (h *Handler) write(w http.ResponseWriter, body []byte) {
	if _, err := w.Write(body); err != nil {
		h.log.WithError(err).Debug("Cannot write response")
	}
}
