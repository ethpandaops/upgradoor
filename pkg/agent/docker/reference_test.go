package docker

import (
	"encoding/base64"
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
		{ref: "alpine", want: "docker.io/library/alpine"},
		{ref: "alpine:3.21", want: "docker.io/library/alpine"},
		{ref: "library/alpine:latest", want: "docker.io/library/alpine"},
		{ref: "docker.io/alpine", want: "docker.io/library/alpine"},
		{ref: "docker.io/library/alpine@" + digestA, want: "docker.io/library/alpine"},
		{ref: "index.docker.io/library/alpine:3", want: "docker.io/library/alpine"},
		{ref: "registry-1.docker.io/library/alpine", want: "docker.io/library/alpine"},
		{ref: "registry-1.docker.io/alpine", want: "docker.io/library/alpine"},
		{ref: "ethpandaops/lighthouse:unstable", want: "docker.io/ethpandaops/lighthouse"},
		{ref: "docker.io/ethpandaops/lighthouse:unstable@" + digestA, want: "docker.io/ethpandaops/lighthouse"},
		{ref: "ghcr.io/ethpandaops/geth:master", want: "ghcr.io/ethpandaops/geth"},
		{ref: "localhost:5000/app:latest", want: "localhost:5000/app"},
		{ref: "localhost/app", want: "localhost/app"},
		{ref: "127.0.0.1:5000/team/app:v1", want: "127.0.0.1:5000/team/app"},
		{ref: " alpine:3.21 ", want: "docker.io/library/alpine"},
		{ref: "", wantErr: true},
		{ref: "Alpine:3", wantErr: true},
		{ref: "alpine:", wantErr: true},
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

func TestRegistryOf(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{ref: "alpine", want: "docker.io"},
		{ref: "index.docker.io/library/alpine", want: "docker.io"},
		{ref: "registry-1.docker.io/ethpandaops/geth", want: "docker.io"},
		{ref: "ghcr.io/ethpandaops/geth:master", want: "ghcr.io"},
		{ref: "localhost:5000/app", want: "localhost:5000"},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got, err := registryOf(tt.ref)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeRegistry(t *testing.T) {
	tests := map[string]string{
		"https://index.docker.io/v1/": "docker.io",
		"index.docker.io":             "docker.io",
		"registry-1.docker.io":        "docker.io",
		"docker.io":                   "docker.io",
		"https://ghcr.io":             "ghcr.io",
		"GHCR.io":                     "ghcr.io",
		"http://localhost:5000/v2/":   "localhost:5000",
	}

	for in, want := range tests {
		assert.Equal(t, want, normalizeRegistry(in), in)
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

func TestFindRegistryAuth(t *testing.T) {
	basic := func(user, password string) string {
		return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	}

	config := `{
		"auths": {
			"https://index.docker.io/v1/": {"auth": "` + basic("hubuser", "hub:pass") + `"},
			"ghcr.io": {"username": "gh", "password": "token"},
			"https://ghcr.io/v2/": {"username": "other", "password": "other"},
			"registry.example.com": {"identitytoken": "idtoken"},
			"helper.example.com": {},
			"broken.example.com": {"auth": "!!!"},
			"nocolon.example.com": {"auth": "` + base64.StdEncoding.EncodeToString([]byte("nocolon")) + `"}
		},
		"credsStore": "desktop"
	}`

	tests := []struct {
		name       string
		host       string
		wantUser   string
		wantPass   string
		wantToken  string
		wantServer string
		wantNone   bool
		wantErr    bool
	}{
		{
			name: "docker hub through the legacy index key", host: "docker.io",
			wantUser: "hubuser", wantPass: "hub:pass", wantServer: "https://index.docker.io/v1/",
		},
		{
			name: "exact host wins over a URL key", host: "ghcr.io",
			wantUser: "gh", wantPass: "token", wantServer: "ghcr.io",
		},
		{
			name: "identity token", host: "registry.example.com",
			wantToken: "idtoken", wantServer: "registry.example.com",
		},
		{name: "credential helper stub has no credentials", host: "helper.example.com", wantNone: true},
		{name: "unknown registry", host: "quay.io", wantNone: true},
		{name: "invalid base64", host: "broken.example.com", wantErr: true},
		{name: "auth without colon", host: "nocolon.example.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findRegistryAuth([]byte(config), tt.host)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)

			if tt.wantNone {
				assert.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			assert.Equal(t, tt.wantUser, got.Username)
			assert.Equal(t, tt.wantPass, got.Password)
			assert.Equal(t, tt.wantToken, got.IdentityToken)
			assert.Equal(t, tt.wantServer, got.ServerAddress)
		})
	}

	_, err := findRegistryAuth([]byte("{not json"), "docker.io")
	require.Error(t, err)

	got, err := findRegistryAuth([]byte("{}"), "docker.io")
	require.NoError(t, err)
	assert.Nil(t, got)
}
