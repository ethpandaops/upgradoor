package imageref

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDigest = "sha256:abababababababababababababababababababababababababababababababab"

func TestParse(t *testing.T) {
	hub := DockerHubHost

	cases := []struct {
		in   string
		want Reference
	}{
		{"nginx", Reference{Host: hub, Path: "library/nginx", Tag: defaultTag}},
		{"nginx:1.27", Reference{Host: hub, Path: "library/nginx", Tag: "1.27"}},
		{"acme/app:unstable", Reference{Host: hub, Path: "acme/app", Tag: "unstable"}},
		{"docker.io/geth", Reference{Host: hub, Path: "library/geth", Tag: defaultTag}},
		{"docker.io/library/geth:latest", Reference{Host: hub, Path: "library/geth", Tag: defaultTag}},
		{"index.docker.io/ethpandaops/geth:master", Reference{Host: hub, Path: "ethpandaops/geth", Tag: "master"}},
		{"registry-1.docker.io/geth:v1", Reference{Host: hub, Path: "library/geth", Tag: "v1"}},
		{"registry.hub.docker.com/geth", Reference{Host: hub, Path: "library/geth", Tag: defaultTag}},
		{"Docker.IO/geth", Reference{Host: hub, Path: "library/geth", Tag: defaultTag}},
		{"GHCR.io/org/app:V1_x.y-z", Reference{Host: "ghcr.io", Path: "org/app", Tag: "V1_x.y-z"}},
		{"localhost:5000/app", Reference{Host: "localhost:5000", Path: "app", Tag: defaultTag}},
		{"localhost/app:x", Reference{Host: "localhost", Path: "app", Tag: "x"}},
		{"127.0.0.1:5000/a__b/c-d.e:t", Reference{Host: "127.0.0.1:5000", Path: "a__b/c-d.e", Tag: "t"}},
		{"[::1]:5000/app:t", Reference{Host: "[::1]:5000", Path: "app", Tag: "t"}},
		// Not a host: no dot, no colon, not localhost.
		{"ethpandaops/lighthouse", Reference{Host: hub, Path: "ethpandaops/lighthouse", Tag: defaultTag}},
		// Docker reads this as repository "localhost" with tag "5000".
		{"localhost:5000", Reference{Host: hub, Path: "library/localhost", Tag: "5000"}},
		// No tag is invented for a pinned reference.
		{"geth@" + testDigest, Reference{Host: hub, Path: "library/geth", Digest: testDigest}},
		{
			"ghcr.io/org/app:v1@" + testDigest,
			Reference{Host: "ghcr.io", Path: "org/app", Tag: "v1", Digest: testDigest},
		},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := Parse(c.in)
			require.NoError(t, err)
			require.Equal(t, c.want, got)
			require.Equal(t, c.want.Host+"/"+c.want.Path, got.Repository())

			again, err := Parse(got.String())
			require.NoError(t, err)
			require.Equal(t, got, again, "the canonical form parses to itself")
		})
	}

	bad := []string{
		"",
		"org/app:",
		"localhost:5000/",
		"localhost:5000/:tag",
		"org/App",
		"Org/app",
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
		" org/app",
		"geth@sha256:abc",
		"@",
		// A bare image ID is not a repository.
		strings.Repeat("a", 64),
	}

	for _, in := range bad {
		t.Run("bad "+in, func(t *testing.T) {
			_, err := Parse(in)
			require.Error(t, err)
		})
	}
}

func TestRegistryHost(t *testing.T) {
	tests := map[string]string{
		"https://index.docker.io/v1/": DockerHubHost,
		"index.docker.io":             DockerHubHost,
		"registry-1.docker.io":        DockerHubHost,
		"registry.hub.docker.com":     DockerHubHost,
		"docker.io":                   DockerHubHost,
		"https://Docker.IO":           DockerHubHost,
		" GHCR.io ":                   "ghcr.io",
		"http://localhost:5000/v2/":   "localhost:5000",
		"registry.example:5000":       "registry.example:5000",
		"":                            "",
	}

	for in, want := range tests {
		assert.Equal(t, want, RegistryHost(in), in)
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"x", "docker.io/library/x:latest"},
		{"x:latest", "docker.io/library/x:latest"},
		{"docker.io/x", "docker.io/library/x:latest"},
		{"index.docker.io/library/x", "docker.io/library/x:latest"},
		{"ethpandaops/geth:master", "docker.io/ethpandaops/geth:master"},
		{"ghcr.io/org/app", "ghcr.io/org/app:latest"},
		{"registry.example:5000/a/b/c:tag", "registry.example:5000/a/b/c:tag"},
		{" x:1 ", "docker.io/library/x:1"},
		{"x@" + testDigest, "docker.io/library/x@" + testDigest},
		{"x:1@" + testDigest, "docker.io/library/x:1@" + testDigest},
		// What is not a reference comes back as it is.
		{"", ""},
		{"Not A Ref", "Not A Ref"},
		{"x@sha256:abc", "x@sha256:abc"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, Normalize(tt.in))
			assert.Equal(t, tt.want, Normalize(tt.want), "normalising is idempotent")
		})
	}
}

func TestSame(t *testing.T) {
	same := [][2]string{
		{"docker.io/library/x:latest", "x"},
		{"ethpandaops/lighthouse:unstable", "docker.io/ethpandaops/lighthouse:unstable"},
		{"index.docker.io/sigp/lighthouse", "sigp/lighthouse:latest"},
	}
	for _, p := range same {
		assert.True(t, Same(p[0], p[1]), "%s == %s", p[0], p[1])
	}

	different := [][2]string{
		{"x:1", "x:2"},
		{"x", "y"},
		{"x", "ghcr.io/library/x"},
		{"x:latest", "x@" + testDigest},
		{"org/x:latest", "x:latest"},
		{"sha256:0123456789ab", "x"},
		{strings.Repeat("a", 64), "x"},
		{"X", "x"},
	}
	for _, p := range different {
		assert.False(t, Same(p[0], p[1]), "%s != %s", p[0], p[1])
	}
}

func TestIsTag(t *testing.T) {
	assert.True(t, IsTag("x"))
	assert.True(t, IsTag("ghcr.io/org/x:v1"))
	assert.False(t, IsTag("x@sha256:abc"))
	assert.False(t, IsTag("x:v1@sha256:abc"))
	assert.False(t, IsTag("sha256:abc"))
	assert.False(t, IsTag(""))
}
