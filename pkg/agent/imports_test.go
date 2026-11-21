package agent

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const module = "github.com/ethpandaops/upgradoor/"

// sourceFile is one parsed .go file; dir is relative to the module.
type sourceFile struct {
	path    string
	pkg     string
	test    bool
	imports []string
	text    string
}

// parseTree parses every .go file below dir (relative to the module root).
func parseTree(t *testing.T, root, dir string) []sourceFile {
	t.Helper()

	var out []sourceFile

	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		f, err := parser.ParseFile(token.NewFileSet(), path, raw, parser.ImportsOnly)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		sf := sourceFile{
			path: filepath.ToSlash(rel), pkg: filepath.ToSlash(filepath.Dir(rel)),
			test: strings.HasSuffix(path, "_test.go"), text: string(raw),
		}

		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}

			sf.imports = append(sf.imports, p)
		}

		out = append(out, sf)

		return nil
	})
	require.NoError(t, err)

	return out
}

// TestImportRules enforces the dependency rules of imports.go.
func TestImportRules(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	files := parseTree(t, root, pkgRoot)
	require.NotEmpty(t, files)

	for i := range files {
		f := &files[i]

		if !f.test {
			allowed, known := mayImport[f.pkg]
			require.True(t, known, "package %s has no import rules in imports.go", f.pkg)

			for _, imp := range f.imports {
				checkImport(t, f, imp, allowed)
			}
		}

		if f.pkg == pkgPlan && !f.test {
			for _, call := range planClock {
				assert.False(t, strings.Contains(f.text, call+"("),
					"%s: plan never reads the clock or waits (%s)", f.path, call)
			}
		}
	}

	// agenttest is test support: no production code imports it.
	cmd := parseTree(t, root, "cmd/upgradoor")
	for _, f := range slices.Concat(files, cmd) {
		if f.test {
			continue
		}

		assert.NotContains(t, f.imports, module+pkgAgenttest,
			"%s imports agenttest", f.path)
	}

	// Test files included: only host imports a host sub-package, and no
	// sub-package imports host or a sibling.
	everything := slices.Concat(parseTree(t, root, "pkg"), parseTree(t, root, "cmd"))
	for i := range everything {
		checkHostSubpackages(t, &everything[i])
	}
}

// TestHostBinaryDeps checks the released host binary against hostForbids and hostModules.
func TestHostBinaryDeps(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)

	for _, arch := range []string{"amd64", "arm64"} {
		for _, tags := range []string{"", "webui"} {
			build := "linux/" + arch + " tags=" + tags

			cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-tags="+tags,
				"-f", "{{.ImportPath}} {{with .Module}}{{.Path}}{{end}}", hostBinary)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")

			out, err := cmd.Output()
			require.NoError(t, err, "go list -deps %s (%s)", hostBinary, build)
			require.Contains(t, string(out), module+pkgRoot+" ", "go list returned no closure")

			for line := range strings.Lines(string(out)) {
				pkg, mod, _ := strings.Cut(strings.TrimSpace(line), " ")

				if own, ok := strings.CutPrefix(pkg, module); ok {
					for _, f := range hostForbids {
						assert.False(t, own == f.path || strings.HasPrefix(own, f.path+"/"),
							"%s (%s) depends on %s: %s", hostBinary, build, pkg, f.what)
					}
				}

				assert.True(t, mod == "" || slices.Contains(hostModules, mod),
					"%s (%s) depends on %s of module %s, which is not in hostModules",
					hostBinary, build, pkg, mod)
			}
		}
	}
}

// hostSubpackages are the packages only host imports.
var hostSubpackages = []string{pkgDocker, pkgSystem, pkgProbe}

// checkHostSubpackages checks any file's imports, tests included, against host's sub-packages.
func checkHostSubpackages(t *testing.T, f *sourceFile) {
	t.Helper()

	for _, imp := range f.imports {
		own, ok := strings.CutPrefix(imp, module)
		if !ok {
			continue
		}

		if slices.Contains(hostSubpackages, own) {
			assert.True(t, f.pkg == pkgHost || f.pkg == own,
				"%s imports %s: only host imports its sub-packages", f.path, own)
		}

		if own == pkgHost {
			assert.NotContains(t, hostSubpackages, f.pkg,
				"%s imports host: a sub-package does not import its parent", f.path)
		}
	}
}

// onlyInOwner returns the one package that may import imp, or "" when any may.
func onlyInOwner(imp string) string {
	for prefix, owner := range onlyIn {
		if strings.HasPrefix(imp, prefix) {
			return owner
		}
	}

	return ""
}

// checkImport checks one import of a non-test file.
func checkImport(t *testing.T, f *sourceFile, imp string, allowed []string) {
	t.Helper()

	owner := onlyInOwner(imp)

	switch {
	case strings.HasPrefix(imp, module):
		own := strings.TrimPrefix(imp, module)
		assert.Contains(t, allowed, own, "%s may not import %s", f.path, own)
	case f.pkg == pkgPlan && slices.ContainsFunc(planForbids, func(p string) bool {
		return imp == p || strings.HasPrefix(imp, p+"/")
	}):
		assert.Fail(t, "plan does no I/O", "%s imports %s", f.path, imp)
	case owner != "":
		assert.Equal(t, owner, f.pkg, "%s: only %s imports %s", f.path, owner, imp)
	case imp == "net/http" || strings.HasPrefix(imp, "net/http/"):
		assert.Contains(t, httpAllowed, f.pkg, "%s may not import %s", f.path, imp)
	case strings.Contains(strings.Split(imp, "/")[0], "."):
		// Third-party: the agent's dependencies are logrus and yaml (none in
		// plan); testify only in test support.
		ok := (f.pkg != pkgPlan && (imp == "github.com/sirupsen/logrus" || imp == "gopkg.in/yaml.v3")) ||
			(f.pkg == pkgAgenttest && strings.HasPrefix(imp, "github.com/stretchr/testify/"))
		assert.True(t, ok, "%s imports the third-party %s", f.path, imp)
	}
}
