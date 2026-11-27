package upstream

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	commitC = "cccccccccccccccccccccccccccccccccccccccc"
)

// refsAnswer renders an ls-refs answer of the given lines.
func refsAnswer(lines ...string) []byte {
	var b bytes.Buffer
	for _, line := range lines {
		writePkt(&b, line+"\n")
	}

	b.WriteString("0000")

	return b.Bytes()
}

func TestParseRefs(t *testing.T) {
	heads, err := parseRefs(refsAnswer(
		commitA+" refs/tags/main",
		commitA+" refs/heads/main",
		commitB+" refs/heads/main-2",
		commitB+" refs/tags/v1 peeled:"+commitC,
		"junk",
	), []string{"main", "v1", "absent"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"main": commitA, "v1": commitC}, heads)

	// A branch wins over a tag of its name, whichever comes first.
	heads, err = parseRefs(refsAnswer(commitA+" refs/heads/x", commitB+" refs/tags/x"), []string{"x"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"x": commitA}, heads)

	_, err = parseRefs([]byte("00"), nil)
	require.Error(t, err)

	_, err = parseRefs([]byte("zzzz"), nil)
	require.Error(t, err)

	_, err = parseRefs([]byte("0040short"), nil)
	require.Error(t, err)
}

func TestHeadsAsksGitHubForTheRefs(t *testing.T) {
	var got struct {
		path, protocol, body string
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.path, got.protocol, got.body = r.URL.Path, r.Header.Get("Git-Protocol"), string(body)

		if strings.Contains(r.URL.Path, "missing") {
			http.Error(w, "Repository not found.", http.StatusUnauthorized)

			return
		}

		_, _ = w.Write(refsAnswer(commitA + " refs/heads/pq-devnet-0"))
	}))
	t.Cleanup(srv.Close)

	g := newGitHub(srv.URL, "test")

	heads, err := g.Heads(t.Context(), "org/client", []string{"pq-devnet-0"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"pq-devnet-0": commitA}, heads)
	require.Equal(t, "/org/client.git/git-upload-pack", got.path)
	require.Equal(t, "version=2", got.protocol)
	require.Equal(t, "0014command=ls-refs\n0024agent=upgradoor-controller/test\n00010009peel\n"+
		"0026ref-prefix refs/heads/pq-devnet-0\n0025ref-prefix refs/tags/pq-devnet-0\n0000", got.body)

	_, err = g.Heads(t.Context(), "org/missing", []string{"main"})
	require.ErrorContains(t, err, "github.com/org/missing: 401")
}
