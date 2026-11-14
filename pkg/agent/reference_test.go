package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeReference(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"x", "docker.io/library/x:latest"},
		{"x:latest", "docker.io/library/x:latest"},
		{"docker.io/library/x:latest", "docker.io/library/x:latest"},
		{"docker.io/x", "docker.io/library/x:latest"},
		{"index.docker.io/library/x", "docker.io/library/x:latest"},
		{"registry-1.docker.io/x:1", "docker.io/library/x:1"},
		{"ethpandaops/geth:master", "docker.io/ethpandaops/geth:master"},
		{"docker.io/ethpandaops/geth:master", "docker.io/ethpandaops/geth:master"},
		{"ethpandaops/geth", "docker.io/ethpandaops/geth:latest"},
		{"ghcr.io/org/app:v1", "ghcr.io/org/app:v1"},
		{"ghcr.io/org/app", "ghcr.io/org/app:latest"},
		{"localhost/app", "localhost/app:latest"},
		{"localhost:5000/app", "localhost:5000/app:latest"},
		{"localhost:5000/app:dev", "localhost:5000/app:dev"},
		{"registry.example:5000/a/b/c:tag", "registry.example:5000/a/b/c:tag"},
		{" x:1 ", "docker.io/library/x:1"},
		{"x@sha256:abc", "docker.io/library/x@sha256:abc"},
		{"x:1@sha256:abc", "docker.io/library/x:1@sha256:abc"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, NormalizeReference(tt.in))
			assert.Equal(t, tt.want, NormalizeReference(tt.want), "normalising is idempotent")
		})
	}
}

func TestSameReference(t *testing.T) {
	same := [][2]string{
		{"docker.io/library/x:latest", "x"},
		{"ethpandaops/lighthouse:unstable", "docker.io/ethpandaops/lighthouse:unstable"},
		{"index.docker.io/sigp/lighthouse", "sigp/lighthouse:latest"},
	}
	for _, p := range same {
		assert.True(t, SameReference(p[0], p[1]), "%s == %s", p[0], p[1])
	}

	different := [][2]string{
		{"x:1", "x:2"},
		{"x", "y"},
		{"x", "ghcr.io/library/x"},
		{"x:latest", "x@sha256:abc"},
		{"org/x:latest", "x:latest"},
		{"sha256:0123456789ab", "x"},
		{"X", "x"},
	}
	for _, p := range different {
		assert.False(t, SameReference(p[0], p[1]), "%s != %s", p[0], p[1])
	}
}

func TestIsTagReference(t *testing.T) {
	assert.True(t, isTagReference("x"))
	assert.True(t, isTagReference("ghcr.io/org/x:v1"))
	assert.False(t, isTagReference("x@sha256:abc"))
	assert.False(t, isTagReference("x:v1@sha256:abc"))
	assert.False(t, isTagReference("sha256:abc"))
	assert.False(t, isTagReference(""))
}
