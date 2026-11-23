package webui

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const indexHTML = "<!doctype html><title>upgradoor ui</title><div id=root></div>"

func testFS() fs.FS {
	return fstest.MapFS{
		"index.html":               {Data: []byte(indexHTML)},
		"favicon.svg":              {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
		"assets/index-BX3k2a9d.js": {Data: []byte("console.log('ui')")},
		"assets/index-Qw8eRt7y.css": {
			Data: []byte("body{margin:0}"),
		},
		"assets/plain.js":        {Data: []byte("console.log('plain')")},
		"assets/my-component.js": {Data: []byte("console.log('component')")},
	}
}

func newHandler(t *testing.T, fsys fs.FS, opts Options) *Handler {
	t.Helper()

	log := logrus.New()
	log.SetOutput(io.Discard)

	h, err := New(log, fsys, opts)
	require.NoError(t, err)

	return h
}

// answer is a recorded response.
type answer struct {
	StatusCode int
	Header     http.Header
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) (
	answer, string,
) {
	t.Helper()

	req := httptest.NewRequest(method, target, http.NoBody)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return answer{StatusCode: rec.Code, Header: rec.Header()}, rec.Body.String()
}

func TestServeRules(t *testing.T) {
	h := newHandler(t, testFS(), Options{})

	tests := []struct {
		name      string
		method    string
		target    string
		status    int
		ctype     string
		cache     string
		body      string
		wantETag  bool
		wantNoCSP bool
	}{
		{
			name: "root serves index", method: http.MethodGet, target: "/",
			status: 200, ctype: "text/html; charset=utf-8", cache: cacheRevalidate,
			body: indexHTML, wantETag: true,
		},
		{
			name: "index by name", method: http.MethodGet, target: "/index.html",
			status: 200, ctype: "text/html; charset=utf-8", cache: cacheRevalidate,
			body: indexHTML, wantETag: true,
		},
		{
			name: "client route falls back to index", method: http.MethodGet,
			target: "/rollouts", status: 200, ctype: "text/html; charset=utf-8",
			cache: cacheRevalidate, body: indexHTML, wantETag: true,
		},
		{
			name: "nested client route falls back to index", method: http.MethodGet,
			target: "/hosts/lighthouse-geth-1?tab=packages", status: 200,
			ctype: "text/html; charset=utf-8", cache: cacheRevalidate, body: indexHTML,
			wantETag: true,
		},
		{
			name:   "client route with a dotted host name falls back to index",
			method: http.MethodGet, target: "/hosts/lighthouse-geth-1.devnet", status: 200,
			ctype: "text/html; charset=utf-8", cache: cacheRevalidate, body: indexHTML,
			wantETag: true,
		},
		{
			name:   "client route with dots and a version falls back to index",
			method: http.MethodGet, target: "/hosts/node-1.example.v2/packages", status: 200,
			ctype: "text/html; charset=utf-8", cache: cacheRevalidate, body: indexHTML,
			wantETag: true,
		},
		{
			name: "hand-named asset is revalidated", method: http.MethodGet,
			target: "/assets/my-component.js", status: 200,
			ctype: "text/javascript; charset=utf-8", cache: cacheRevalidate,
			body: "console.log('component')", wantETag: true,
		},
		{
			name:   "missing script outside assets is not answered with html",
			method: http.MethodGet, target: "/sw.JS", status: 404,
			ctype: "text/plain; charset=utf-8", body: "not found\n",
		},
		{
			name:   "missing dotted path below assets is not answered with html",
			method: http.MethodGet, target: "/assets/gone.devnet", status: 404,
			ctype: "text/plain; charset=utf-8", body: "not found\n",
		},
		{
			name: "hashed asset is immutable", method: http.MethodGet,
			target: "/assets/index-BX3k2a9d.js", status: 200,
			ctype: "text/javascript; charset=utf-8", cache: cacheImmutable,
			body: "console.log('ui')",
		},
		{
			name: "hashed stylesheet is immutable", method: http.MethodGet,
			target: "/assets/index-Qw8eRt7y.css", status: 200,
			ctype: "text/css; charset=utf-8", cache: cacheImmutable, body: "body{margin:0}",
		},
		{
			name: "unhashed asset is revalidated", method: http.MethodGet,
			target: "/assets/plain.js", status: 200,
			ctype: "text/javascript; charset=utf-8", cache: cacheRevalidate,
			body: "console.log('plain')", wantETag: true,
		},
		{
			name: "static file outside assets is revalidated", method: http.MethodGet,
			target: "/favicon.svg", status: 200, ctype: "image/svg+xml",
			cache: cacheRevalidate, body: "<svg xmlns='http://www.w3.org/2000/svg'/>",
			wantETag: true,
		},
		{
			name: "missing asset is not answered with html", method: http.MethodGet,
			target: "/assets/gone-AAAAAAAA.js", status: 404,
			ctype: "text/plain; charset=utf-8", body: "not found\n",
		},
		{
			name: "missing file with extension is 404", method: http.MethodGet,
			target: "/robots.txt", status: 404, ctype: "text/plain; charset=utf-8",
			body: "not found\n",
		},
		{
			name: "directory is not listed", method: http.MethodGet, target: "/assets/",
			status: 404, ctype: "text/plain; charset=utf-8", body: "not found\n",
		},
		{
			name: "directory without slash is not listed", method: http.MethodGet,
			target: "/assets", status: 404, ctype: "text/plain; charset=utf-8",
			body: "not found\n",
		},
		{
			name: "path traversal is cleaned", method: http.MethodGet,
			target: "/assets/../../etc/passwd", status: 200,
			ctype: "text/html; charset=utf-8", cache: cacheRevalidate, body: indexHTML,
			wantETag: true,
		},
		{
			name: "unknown api path is json 404", method: http.MethodGet,
			target: "/api/v1/nope", status: 404, ctype: contentTypeJSON,
			cache: "no-store", body: notFoundJSON, wantNoCSP: true,
		},
		{
			name: "api root is json 404", method: http.MethodGet, target: "/api",
			status: 404, ctype: contentTypeJSON, cache: "no-store", body: notFoundJSON,
			wantNoCSP: true,
		},
		{
			name: "api path with other method is json 404", method: http.MethodPost,
			target: "/api/v1/nope", status: 404, ctype: contentTypeJSON,
			cache: "no-store", body: notFoundJSON, wantNoCSP: true,
		},
		{
			name:   "api path reached through dot segments is json 404",
			method: http.MethodGet, target: "/x/../api/v1/hosts", status: 404,
			ctype: contentTypeJSON, cache: "no-store", body: notFoundJSON, wantNoCSP: true,
		},
		{
			name: "post is refused", method: http.MethodPost, target: "/",
			status: 405, ctype: "text/plain; charset=utf-8", body: "method not allowed\n",
			wantNoCSP: true,
		},
		{
			name: "head has no body", method: http.MethodHead, target: "/",
			status: 200, ctype: "text/html; charset=utf-8", cache: cacheRevalidate,
			wantETag: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, body := do(t, h, tt.method, tt.target, nil)

			assert.Equal(t, tt.status, res.StatusCode)
			assert.Equal(t, tt.ctype, res.Header.Get("Content-Type"))
			assert.Equal(t, tt.cache, res.Header.Get("Cache-Control"))
			assert.Equal(t, tt.body, body)
			assert.Equal(t, tt.wantETag, res.Header.Get("ETag") != "")
			assert.Equal(t, "nosniff", res.Header.Get("X-Content-Type-Options"))

			if tt.wantNoCSP {
				assert.Empty(t, res.Header.Get("Content-Security-Policy"))
			} else {
				assert.NotEmpty(t, res.Header.Get("Content-Security-Policy"))
				assert.Equal(t, "DENY", res.Header.Get("X-Frame-Options"))
				assert.Equal(t, "no-referrer", res.Header.Get("Referrer-Policy"))
			}
		})
	}
}

func TestConditionalRequest(t *testing.T) {
	h := newHandler(t, testFS(), Options{})

	res, _ := do(t, h, http.MethodGet, "/", nil)
	etag := res.Header.Get("ETag")
	require.NotEmpty(t, etag)

	res, body := do(t, h, http.MethodGet, "/", map[string]string{"If-None-Match": etag})
	assert.Equal(t, http.StatusNotModified, res.StatusCode)
	assert.Empty(t, body)
}

func TestPlaceholder(t *testing.T) {
	tests := []struct {
		name string
		fsys fs.FS
	}{
		{name: "nil file system", fsys: nil},
		{name: "file system without index", fsys: fstest.MapFS{
			"assets/x-AAAAAAAA.js": {Data: []byte("x")},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHandler(t, tt.fsys, Options{})

			for _, target := range []string{"/", "/rollouts", "/index.html"} {
				res, body := do(t, h, http.MethodGet, target, nil)
				assert.Equal(t, http.StatusOK, res.StatusCode, target)
				assert.Equal(t, contentTypeHTML, res.Header.Get("Content-Type"), target)
				assert.Contains(t, body, "does not include the web UI", target)
			}

			res, body := do(t, h, http.MethodGet, "/api/v1/nope", nil)
			assert.Equal(t, http.StatusNotFound, res.StatusCode)
			assert.Equal(t, notFoundJSON, body)

			res, _ = do(t, h, http.MethodGet, "/missing.js", nil)
			assert.Equal(t, http.StatusNotFound, res.StatusCode)
		})
	}
}

func TestContentSecurityPolicy(t *testing.T) {
	tests := []struct {
		name    string
		authURL string
		extra   []string
		want    []string
		not     []string
		wantErr bool
	}{
		{
			name: "open mode allows only self",
			want: []string{"default-src 'self'", "script-src 'self';", "frame-ancestors 'none'"},
			not:  []string{"http://", "https://"},
		},
		{
			name:    "auth provider origin is allowed",
			authURL: "https://auth.devnet-0.example.io/some/path",
			want: []string{
				"script-src 'self' https://auth.devnet-0.example.io;",
				"connect-src 'self' https://auth.devnet-0.example.io;",
				"frame-src 'self' https://auth.devnet-0.example.io;",
			},
			not: []string{"/some/path"},
		},
		{
			name:    "extra origins for scripts and requests only",
			authURL: "https://auth.example.org",
			extra: []string{
				"https://menu.example.org/", "http://b.example.org:8080",
				"https://auth.example.org", "https://menu.example.org",
			},
			want: []string{
				"script-src 'self' https://auth.example.org " +
					"http://b.example.org:8080 https://menu.example.org;",
				"connect-src 'self' https://auth.example.org " +
					"http://b.example.org:8080 https://menu.example.org;",
				"frame-src 'self' https://auth.example.org;",
				"form-action 'self' https://auth.example.org;",
				"img-src 'self' data:;",
			},
		},
		{name: "extra origin with path", extra: []string{"https://menu.example.org/x"}, wantErr: true},
		{name: "extra origin with query", extra: []string{"https://menu.example.org/?a=1"}, wantErr: true},
		{name: "extra origin without scheme", extra: []string{"menu.example.org"}, wantErr: true},
		{name: "not a URL", authURL: "://nope", wantErr: true},
		{name: "wrong scheme", authURL: "javascript:alert(1)", wantErr: true},
		{name: "no host", authURL: "https://", wantErr: true},
	}

	log := logrus.New()
	log.SetOutput(io.Discard)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := New(log, testFS(), Options{AuthProviderURL: tt.authURL, ExtraOrigins: tt.extra})
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)

			res, _ := do(t, h, http.MethodGet, "/", nil)
			csp := res.Header.Get("Content-Security-Policy")

			for _, want := range tt.want {
				assert.Contains(t, csp, want)
			}

			for _, not := range tt.not {
				assert.NotContains(t, csp, not)
			}
		})
	}
}

func TestHashedName(t *testing.T) {
	tests := map[string]bool{
		"index-BX3k2a9d.js":        true,
		"index-BX3k2a9d.js.map":    true,
		"index-Qw8eRt7y.css":       true,
		"logo.3f9a1c0e.svg":        true,
		"chunk-B-_k2a9d.js":        true, // the hash alphabet includes - and _
		"vendor-react-D4fGh1Jk.js": true,
		"index.js":                 false,
		"plain.js":                 false,
		"apple-touch-icon.png":     false,
		"chunk-abc.js":             false,
		"my-component.js":          false, // a word of 8+ letters is not a hash
		"app-settings.js":          false, // exactly 8 lower-case letters
		"app-settings.js.map":      false,
		"user-preferences.js":      false,
		"index-BX3k2a9dXX.js":      false, // 10 characters
		"BX3k2a9d.js":              false, // no base name
		"-BX3k2a9d.js":             false,
		"index-BX3k2a9d":           false, // no extension
		"index-BX3k+a9d.js":        false,
		".BX3k2a9d":                false,
		"":                         false,
	}

	for name, want := range tests {
		assert.Equal(t, want, hashedName(name), name)
	}
}
