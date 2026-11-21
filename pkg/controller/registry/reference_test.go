package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

func TestParseTagReference(t *testing.T) {
	ref, err := parseTagReference("geth")
	require.NoError(t, err)
	require.Equal(t, imageref.Reference{Host: imageref.DockerHubHost, Path: "library/geth", Tag: "latest"}, ref)
	require.Equal(t, dockerHubAPIHost, apiHost(ref), "Docker Hub is served by another host")

	ref, err = parseTagReference("GHCR.io/org/app:v1")
	require.NoError(t, err)
	require.Equal(t, "ghcr.io", apiHost(ref))

	_, err = parseTagReference("org/App")
	require.Error(t, err)
	require.NotErrorIs(t, err, errDigestReference)

	for _, in := range []string{
		"org/app@sha256:" + strings.Repeat("a", 64),
		"ghcr.io/org/app:v1@sha256:" + strings.Repeat("a", 64),
	} {
		_, err = parseTagReference(in)
		require.ErrorIs(t, err, errDigestReference, in)
	}
}

func FuzzParseTagReference(f *testing.F) {
	for _, seed := range []string{
		"nginx", "nginx:1.27", "acme/app:unstable", "ghcr.io/org/app:v1", "localhost:5000/app",
		"localhost/app:x", "[::1]:5000/app:t", "index.docker.io/library/geth", "a/b/c/d:e",
		"org/app@sha256:" + strings.Repeat("a", 64), "", ":", "/", "a:b:c", "A/b", "a.b/c:d/e",
		"geth:v1@sha256:" + strings.Repeat("0", 64), "host.io:1/a?b#c", "x/y\n", "bad_host.io/app",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		ref, err := parseTagReference(in)
		if err != nil {
			return
		}

		if ref.Host == "" || ref.Path == "" || ref.Tag == "" || ref.Digest != "" {
			t.Fatalf("%q parsed with an empty part or a digest: %+v", in, ref)
		}

		// Whatever is accepted must be safe to place in a URL.
		if strings.ContainsAny(ref.Host+ref.Path+ref.Tag, " ?#%\\@\r\n\t") || strings.Contains(ref.Path, "..") {
			t.Fatalf("%q parsed into unsafe parts: %+v", in, ref)
		}

		again, err := parseTagReference(ref.String())
		if err != nil {
			t.Fatalf("%q: canonical form %q does not parse: %v", in, ref.String(), err)
		}

		if again != ref {
			t.Fatalf("%q: round trip changed %+v into %+v", in, ref, again)
		}
	})
}
