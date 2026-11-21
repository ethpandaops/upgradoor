package docker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestRepositoryOf(t *testing.T) {
	tests := []struct {
		ref     string
		want    string
		wantErr bool
	}{
		{ref: "alpine:3.21", want: "docker.io/library/alpine"},
		{ref: "docker.io/library/alpine@" + digestA, want: "docker.io/library/alpine"},
		{ref: "registry-1.docker.io/alpine", want: "docker.io/library/alpine"},
		{ref: "docker.io/ethpandaops/lighthouse:unstable@" + digestA, want: "docker.io/ethpandaops/lighthouse"},
		{ref: "localhost:5000/app:latest", want: "localhost:5000/app"},
		{ref: "", wantErr: true},
		// A bare image ID is not a repository.
		{ref: digestA[len("sha256:"):], wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, err := repositoryOf(tt.ref)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSelectDigest(t *testing.T) {
	tests := []struct {
		name        string
		ref         string
		repoDigests []string
		preferred   []string
		want        string
	}{
		{
			name:        "official image, familiar repo digest",
			ref:         "alpine:3.21",
			repoDigests: []string{"alpine@" + digestA},
			want:        digestA,
		},
		{
			name:        "fully qualified reference against familiar repo digest",
			ref:         "docker.io/library/alpine:3.21",
			repoDigests: []string{"alpine@" + digestA},
			want:        digestA,
		},
		{
			name:        "hub alias in the reference",
			ref:         "registry-1.docker.io/ethpandaops/geth:master",
			repoDigests: []string{"ethpandaops/geth@" + digestA},
			want:        digestA,
		},
		{
			name: "picks the entry of the reference's repository",
			ref:  "ghcr.io/ethpandaops/geth:master",
			repoDigests: []string{
				"ethpandaops/geth@" + digestA,
				"ghcr.io/ethpandaops/geth@" + digestB,
				"ghcr.io/ethpandaops/geth-fork@" + digestC,
			},
			want: digestB,
		},
		{
			name:        "same path on another registry does not match",
			ref:         "ghcr.io/ethpandaops/geth:master",
			repoDigests: []string{"ethpandaops/geth@" + digestA},
			want:        "",
		},
		{
			name:        "registry port is part of the repository",
			ref:         "localhost:5000/app:latest",
			repoDigests: []string{"localhost:5001/app@" + digestA, "localhost:5000/app@" + digestB},
			want:        digestB,
		},
		{
			name:        "never pushed or pulled",
			ref:         "app:dev",
			repoDigests: nil,
			want:        "",
		},
		{
			name:        "several digests: first without a preference",
			ref:         "app",
			repoDigests: []string{"app@" + digestB, "app@" + digestA},
			want:        digestB,
		},
		{
			name:        "several digests: preference wins",
			ref:         "app",
			repoDigests: []string{"app@" + digestB, "app@" + digestA},
			preferred:   []string{"", digestA},
			want:        digestA,
		},
		{
			name:        "several digests: first matching preference wins",
			ref:         "app",
			repoDigests: []string{"app@" + digestB, "app@" + digestA},
			preferred:   []string{digestC, digestB, digestA},
			want:        digestB,
		},
		{
			name:        "preference of another repository is ignored",
			ref:         "app",
			repoDigests: []string{"app@" + digestB, "other@" + digestA},
			preferred:   []string{digestA},
			want:        digestB,
		},
		{
			name:        "digest-pinned reference",
			ref:         "app@" + digestA,
			repoDigests: []string{"app@" + digestA},
			want:        digestA,
		},
		{
			name:        "garbage entries are skipped",
			ref:         "app",
			repoDigests: []string{"<none>@<none>", "app:latest", "app@sha256:short", "app@" + digestA},
			want:        digestA,
		},
		{
			name:        "image ID as reference",
			ref:         digestA,
			repoDigests: []string{"app@" + digestA},
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, selectDigest(tt.ref, tt.repoDigests, tt.preferred...))
		})
	}
}

func TestHasDigest(t *testing.T) {
	repoDigests := []string{"app@" + digestA, "app@" + digestB, "other@" + digestC}

	assert.True(t, hasDigest("app:latest", repoDigests, digestA))
	assert.True(t, hasDigest("docker.io/library/app", repoDigests, digestB))
	assert.False(t, hasDigest("app:latest", repoDigests, digestC), "digest of another repository")
	assert.False(t, hasDigest("app:latest", repoDigests, ""), "an empty digest never matches")
	assert.False(t, hasDigest("app:latest", nil, digestA))
	assert.False(t, hasDigest("APP", repoDigests, digestA), "invalid reference")
}
