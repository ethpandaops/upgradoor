package fleet

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// declaredStates type-checks the package from its sources and returns the
// values of all constants of the named type, by constant name: whichever
// file declares them and however (`const X LeaseState = "x"`,
// `const X = LeaseState("x")`, in a block or alone). It is the list of states
// as the package declares them, whatever the tables say.
func declaredStates(t *testing.T, typeName string) map[string]string {
	t.Helper()

	return constantsOfType(t, ".", typeName)
}

// constantsOfType returns the package-level constants of the named type in
// the package of the directory (its test files left out).
func constantsOfType(t *testing.T, dir, typeName string) map[string]string {
	t.Helper()

	fset := token.NewFileSet()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	files := make([]*ast.File, 0, len(entries))

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err)

		files = append(files, file)
	}

	require.NotEmpty(t, files)

	// What the package imports is not needed to know its own constants:
	// every import is an empty package, and what cannot be resolved for that
	// is passed over.
	conf := types.Config{Importer: emptyImporter{}, Error: func(error) {}}

	pkg, _ := conf.Check(files[0].Name.Name, fset, files, nil)
	require.NotNil(t, pkg)

	out := make(map[string]string, 8)

	for _, name := range pkg.Scope().Names() {
		c, ok := pkg.Scope().Lookup(name).(*types.Const)
		if !ok {
			continue
		}

		named, ok := c.Type().(*types.Named)
		if !ok || named.Obj().Name() != typeName || named.Obj().Pkg() != pkg {
			continue
		}

		require.Equal(t, constant.String, c.Val().Kind(), name)

		out[name] = constant.StringVal(c.Val())
	}

	return out
}

// emptyImporter resolves every import to a package without declarations.
type emptyImporter struct{}

func (emptyImporter) Import(path string) (*types.Package, error) {
	pkg := types.NewPackage(path, filepath.Base(path))
	pkg.MarkComplete()

	return pkg, nil
}

// The search finds a state wherever and however it is declared.
func TestDeclaredStatesAreFoundInEveryFileAndForm(t *testing.T) {
	dir := t.TempDir()

	for name, src := range map[string]string{
		"a.go": "package p\n\ntype State string\n\nconst (\n\tOne State = \"one\"\n\tTwo State = \"two\"\n)\n",
		"b.go": "package p\n\nimport \"time\"\n\nconst Three = State(\"three\")\n\nconst Other = \"other\"\n\n" +
			"var _ = time.Second\n",
		"c_test.go": "package p\n\nconst InATest State = \"test\"\n",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
	}

	assert.Equal(t, map[string]string{"One": "one", "Two": "two", "Three": "three"}, constantsOfType(t, dir, "State"))

	// And in this package: what the tables are held against.
	assert.Len(t, declaredStates(t, "LeaseState"), 6)
	assert.Len(t, declaredStates(t, "RolloutState"), 5)
}

// Every state that is declared is classified, once, as open or not; the
// table names no state that is not declared. Terminal, the list of all
// states and the list of the open ones follow from the table. A state that
// is added without a row here fails by name.
func TestEveryStateIsClassified(t *testing.T) {
	check := func(t *testing.T, typeName string, table map[string]bool, terminal func(string) bool,
		all, open []string, wantOpen []string,
	) {
		t.Helper()

		declared := declaredStates(t, typeName)
		require.NotEmpty(t, declared)

		for name, value := range declared {
			_, classified := table[value]
			assert.True(t, classified, "%s (%q) is declared and not classified as open or terminal", name, value)
		}

		assert.Len(t, table, len(declared), "the table names a state that is not declared, or one twice")
		assert.Len(t, all, len(declared), "every state is listed once")

		for value, isOpen := range table {
			assert.Equal(t, !isOpen, terminal(value), value)
			assert.Contains(t, all, value)
			assert.Equal(t, isOpen, slices.Contains(open, value), value)
		}

		assert.Equal(t, wantOpen, open, "the open states, as the queries of the store name them")
		assert.False(t, terminal("no such state"), "an unknown state is not terminal")
	}

	t.Run("lease", func(t *testing.T) {
		table := make(map[string]bool, len(leaseStates))
		for _, c := range leaseStates {
			table[string(c.State)] = c.Open
		}

		check(t, "LeaseState", table, func(s string) bool { return LeaseState(s).Terminal() },
			stateNames(LeaseStates()), stateNames(OpenLeaseStates()), []string{"granted", "running", "verifying"})
		assert.Len(t, leaseStates, len(table), "a state is classified twice")
	})

	t.Run("rollout", func(t *testing.T) {
		table := make(map[string]bool, len(rolloutStates))
		for _, c := range rolloutStates {
			table[string(c.State)] = c.Open
		}

		check(t, "RolloutState", table, func(s string) bool { return RolloutState(s).Terminal() },
			stateNames(RolloutStates()), stateNames(OpenRolloutStates()), []string{"active", "halted"})
		assert.Len(t, rolloutStates, len(table), "a state is classified twice")
	})
}

func stateNames[S ~string](in []S) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, string(s))
	}

	return out
}
