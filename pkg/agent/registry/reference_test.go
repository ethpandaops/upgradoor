package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseReference(t *testing.T) {
	cases := []struct {
		in   string
		want Reference
		api  string
	}{
		{"nginx", Reference{dockerHubHost, "library/nginx", defaultTag}, dockerHubAPIHost},
		{"nginx:1.27", Reference{dockerHubHost, "library/nginx", "1.27"}, dockerHubAPIHost},
		{"acme/app:unstable", Reference{dockerHubHost, "acme/app", "unstable"}, dockerHubAPIHost},
		{"docker.io/geth", Reference{dockerHubHost, "library/geth", defaultTag}, dockerHubAPIHost},
		{"docker.io/library/geth:latest", Reference{dockerHubHost, "library/geth", defaultTag}, dockerHubAPIHost},
		{
			"index.docker.io/ethpandaops/geth:master",
			Reference{dockerHubHost, "ethpandaops/geth", "master"}, dockerHubAPIHost,
		},
		{"registry-1.docker.io/geth:v1", Reference{dockerHubHost, "library/geth", "v1"}, dockerHubAPIHost},
		{"Docker.IO/geth", Reference{dockerHubHost, "library/geth", defaultTag}, dockerHubAPIHost},
		{"ghcr.io/org/app:v1", Reference{"ghcr.io", "org/app", "v1"}, "ghcr.io"},
		{"GHCR.io/org/app:V1_x.y-z", Reference{"ghcr.io", "org/app", "V1_x.y-z"}, "ghcr.io"},
		{"localhost:5000/app:latest", Reference{"localhost:5000", "app", defaultTag}, "localhost:5000"},
		{"localhost:5000/app", Reference{"localhost:5000", "app", defaultTag}, "localhost:5000"},
		{"localhost/app:x", Reference{localhost, "app", "x"}, localhost},
		{"my.registry:443/a/b/c:t", Reference{"my.registry:443", "a/b/c", "t"}, "my.registry:443"},
		{"127.0.0.1:5000/a__b/c-d.e:t", Reference{"127.0.0.1:5000", "a__b/c-d.e", "t"}, "127.0.0.1:5000"},
		{"[::1]:5000/app:t", Reference{"[::1]:5000", "app", "t"}, "[::1]:5000"},
		// Not a host: no dot, no colon, not localhost.
		{
			"ethpandaops/lighthouse",
			Reference{dockerHubHost, "ethpandaops/lighthouse", defaultTag}, dockerHubAPIHost,
		},
		// Docker reads this as repository "localhost" with tag "5000".
		{"localhost:5000", Reference{dockerHubHost, "library/localhost", "5000"}, dockerHubAPIHost},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseReference(c.in)
			require.NoError(t, err)
			require.Equal(t, c.want, got)
			require.Equal(t, c.api, got.APIHost())
			require.Equal(t, c.want.Host+"/"+c.want.Path+":"+c.want.Tag, got.String())
			require.Equal(t, c.want.Host+"/"+c.want.Path, got.Repository())

			again, err := ParseReference(got.String())
			require.NoError(t, err)
			require.Equal(t, got, again)
		})
	}
}

func TestParseReferenceErrors(t *testing.T) {
	bad := []string{
		"",
		"org/app:",
		"localhost:5000/",
		"localhost:5000/:tag",
		"org/App",
		"org//app",
		"/app",
		"app/",
		"org/app:bad tag",
		"org/app:-tag",
		"org/app:" + strings.Repeat("a", 129),
		"ghcr.io/org/app?x=1",
		"ghcr.io/org/app#frag",
		"ghcr.io/../app",
		"ghcr.io/org/app:t/",
		"bad_host.io/app",
		"-bad.io/app",
		"host.io:port/app",
		"[::1/app",
		"ghcr.io/" + strings.Repeat("a", 256),
		"org/app\n",
		"org/app:tag\n",
	}

	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			_, err := ParseReference(in)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrDigestReference)
		})
	}

	for _, in := range []string{
		"org/app@sha256:" + strings.Repeat("a", 64),
		"ghcr.io/org/app:v1@sha256:" + strings.Repeat("a", 64),
		"@",
	} {
		_, err := ParseReference(in)
		require.ErrorIs(t, err, ErrDigestReference, in)
	}
}

func TestNormalize(t *testing.T) {
	digest := "sha256:" + strings.Repeat("ab", 32)

	cases := []struct{ in, want string }{
		{"geth", "docker.io/library/geth:latest"},
		{"geth:latest", "docker.io/library/geth:latest"},
		{"docker.io/geth", "docker.io/library/geth:latest"},
		{"docker.io/library/geth:latest", "docker.io/library/geth:latest"},
		{"index.docker.io/library/geth", "docker.io/library/geth:latest"},
		{"registry-1.docker.io/library/geth:latest", "docker.io/library/geth:latest"},
		{"ethpandaops/geth:master", "docker.io/ethpandaops/geth:master"},
		{"GHCR.IO/org/app", "ghcr.io/org/app:latest"},
		{"geth@" + digest, "docker.io/library/geth@" + digest},
		{"geth:v1@" + digest, "docker.io/library/geth:v1@" + digest},
		{"ghcr.io/org/app@" + digest, "ghcr.io/org/app@" + digest},
		// Unparsable references come back unchanged.
		{"", ""},
		{"Not A Ref", "Not A Ref"},
		{"geth@nodigest", "geth@nodigest"},
		{"Bad Name@" + digest, "Bad Name@" + digest},
		{"geth:@" + digest, "geth:@" + digest},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := Normalize(c.in)
			require.Equal(t, c.want, got)
			require.Equal(t, got, Normalize(got), "idempotent")
		})
	}

	require.Equal(t, Normalize("geth"), Normalize("docker.io/library/geth:latest"))
	require.NotEqual(t, Normalize("geth"), Normalize("geth:v1"))
	require.NotEqual(t, Normalize("geth"), Normalize("ghcr.io/library/geth"))
}

func FuzzParseReference(f *testing.F) {
	for _, seed := range []string{
		"nginx", "nginx:1.27", "acme/app:unstable", "ghcr.io/org/app:v1", "localhost:5000/app",
		"localhost/app:x", "[::1]:5000/app:t", "index.docker.io/library/geth", "a/b/c/d:e",
		"org/app@sha256:" + strings.Repeat("a", 64), "", ":", "/", "a:b:c", "A/b", "a.b/c:d/e",
		"geth:v1@sha256:" + strings.Repeat("0", 64), "host.io:1/a?b#c", "x/y\n",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		norm := Normalize(in)
		if Normalize(norm) != norm {
			t.Fatalf("Normalize is not idempotent: %q -> %q -> %q", in, norm, Normalize(norm))
		}

		ref, err := ParseReference(in)
		if err != nil {
			return
		}

		if ref.Host == "" || ref.Path == "" || ref.Tag == "" {
			t.Fatalf("%q parsed with an empty part: %+v", in, ref)
		}

		// Whatever is accepted must be safe to place in a URL.
		if strings.ContainsAny(ref.Host+ref.Path+ref.Tag, " ?#%\\@\r\n\t") || strings.Contains(ref.Path, "..") {
			t.Fatalf("%q parsed into unsafe parts: %+v", in, ref)
		}

		again, err := ParseReference(ref.String())
		if err != nil {
			t.Fatalf("%q: canonical form %q does not parse: %v", in, ref.String(), err)
		}

		if again != ref {
			t.Fatalf("%q: round trip changed %+v into %+v", in, ref, again)
		}

		if norm != ref.String() {
			t.Fatalf("%q: Normalize gave %q, want %q", in, norm, ref.String())
		}
	})
}
