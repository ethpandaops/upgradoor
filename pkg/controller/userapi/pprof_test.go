package userapi

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

func TestPprof(t *testing.T) {
	h, provider := authHarness(t, true)

	gate := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("gate"))
	})
	handler := h.srv.Pprof(gate)

	serve := func(path string, mod func(r *http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, controllertest.BaseURL+path, http.NoBody)
		if mod != nil {
			mod(req)
		}

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		return rec
	}
	bearer := func(v string) func(r *http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", v) }
	}
	pprofs := func() []fleet.AuditEntry {
		out := make([]fleet.AuditEntry, 0, 4)

		for _, e := range h.store.AuditEntries() {
			if e.Action == AuditActionPprof {
				out = append(out, e)
			}
		}

		return out
	}

	admin := provider.Grant(t, "+upgradoor:admin")

	// Without a token: 401 with the challenge; a browser gets the gate.
	rec := serve("/debug/pprof/heap", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")

	rec = serve("/debug/pprof/heap", func(r *http.Request) { r.Header.Set("Accept", "text/html,*/*") })
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "gate", rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))

	assert.Equal(t, http.StatusUnauthorized, serve("/debug/pprof/heap", bearer("Bearer nonsense")).Code)

	// A login without admin is refused and the refusal audited.
	rec = serve("/debug/pprof/heap", bearer(provider.Grant(t, "+upgradoor:details:manage:audit")))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "upgradoor:admin")

	refused := false

	for _, e := range h.store.AuditEntries() {
		refused = refused || (e.Action == "auth.refused" && e.Target == "/debug/pprof/heap")
	}

	assert.True(t, refused)

	// The index is served and not audited.
	rec = serve("/debug/pprof/", bearer(admin))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "goroutine")
	assert.Empty(t, pprofs())

	// A profile is served and audited.
	rec = serve("/debug/pprof/goroutine?debug=1", bearer(admin))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "goroutine profile")

	entries := pprofs()
	require.Len(t, entries, 1)
	assert.Equal(t, controllertest.UserEmail, entries[0].Actor)
	assert.Equal(t, "/debug/pprof/goroutine", entries[0].Target)
	assert.Equal(t, map[string]any{"profile": "goroutine", "query": "debug=1", "status": http.StatusOK},
		entries[0].Detail)

	// The cookie of the gate page works like the header.
	cookie := strings.TrimPrefix(admin, "Bearer ")
	rec = serve("/debug/pprof/cmdline", func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: PprofTokenCookie, Value: cookie})
	})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, pprofs(), 2)

	// An unknown profile is audited with its status.
	rec = serve("/debug/pprof/nope", bearer(admin))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	entries = pprofs()
	require.Len(t, entries, 3)
	assert.Equal(t, http.StatusNotFound, entries[2].Detail["status"])
}

// TestNoServerServesDefaultServeMux keeps the profiles behind the gate:
// net/http/pprof sits on http.DefaultServeMux, so no server in pkg or cmd
// may serve that mux or register on it, and only pprof.go may name it.
func TestNoServerServesDefaultServeMux(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	fset := token.NewFileSet()
	checked := 0

	for _, dir := range []string{"pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}

			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}

			checked++

			ast.Inspect(file, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if isHTTP(n, "DefaultServeMux") && filepath.Base(path) != "pprof.go" {
						t.Errorf("%s: names http.DefaultServeMux", fset.Position(n.Pos()))
					}
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}

					switch {
					case isHTTP(sel, "Handle"), isHTTP(sel, "HandleFunc"):
						t.Errorf("%s: registers on http.DefaultServeMux", fset.Position(n.Pos()))
					case isHTTP(sel, "ListenAndServe"), isHTTP(sel, "ListenAndServeTLS"),
						isHTTP(sel, "Serve"), isHTTP(sel, "ServeTLS"):
						if len(n.Args) >= 2 && isNil(n.Args[1]) {
							t.Errorf("%s: serves http.DefaultServeMux", fset.Position(n.Pos()))
						}
					}
				case *ast.CompositeLit:
					if sel, ok := n.Type.(*ast.SelectorExpr); ok && isHTTP(sel, "Server") && !hasHandler(n) {
						t.Errorf("%s: http.Server without a Handler serves http.DefaultServeMux",
							fset.Position(n.Pos()))
					}
				}

				return true
			})

			return nil
		})
		require.NoError(t, err)
	}

	assert.Positive(t, checked)
}

func isHTTP(sel *ast.SelectorExpr, name string) bool {
	pkg, ok := sel.X.(*ast.Ident)

	return ok && pkg.Name == "http" && sel.Sel.Name == name
}

func isNil(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)

	return ok && id.Name == "nil"
}

// hasHandler reports whether an http.Server literal sets a Handler that is
// not nil.
func hasHandler(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Handler" {
				return !isNil(kv.Value)
			}
		}
	}

	return false
}
