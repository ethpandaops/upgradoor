package upstream

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

func TestSourceOf(t *testing.T) {
	configured := map[string]Source{
		"docker.io/acme/node":  {Repo: "acme/node-src"},
		"docker.io/acme/fixed": {Repo: "acme/fixed-src", Ref: "release"},
	}

	cases := []struct {
		name   string
		image  string
		labels map[string]string
		want   Source
		ok     bool
	}{
		{
			name:  "the builder's labels",
			image: "ethpandaops/nimbus-eth2:pq-devnet-0",
			labels: map[string]string{
				labelBuilderRepo: "status-im/nimbus-eth2", labelBuilderRef: "feat/pq", labelBuilderCommit: "6AF1517",
			},
			want: Source{Repo: "status-im/nimbus-eth2", Ref: "feat/pq", Commit: "6af1517"},
			ok:   true,
		},
		{
			name:  "the OCI labels, the ref is the tag",
			image: "ethpandaops/teku:pq-devnet-0",
			labels: map[string]string{
				labelOCISource: "https://github.com/Consensys/teku.git", labelOCIRevision: "20eea1a9",
			},
			want: Source{Repo: "Consensys/teku", Ref: "pq-devnet-0", Commit: "20eea1a9"},
			ok:   true,
		},
		{
			name:   "a revision that is no commit is left out",
			image:  "ethpandaops/ethlambda:pq-devnet-0",
			labels: map[string]string{labelOCIRevision: "unknown"},
			want:   Source{Repo: "lambdaclass/ethlambda", Ref: "pq-devnet-0"},
			ok:     true,
		},
		{
			name:  "the builder's table, without labels",
			image: "ghcr.io/ethpandaops/grandine:pq-devnet-0",
			want:  Source{Repo: "grandinetech/grandine", Ref: "pq-devnet-0"},
			ok:    true,
		},
		{
			name:   "configured wins over the labels",
			image:  "acme/node:main",
			labels: map[string]string{labelBuilderRepo: "other/repo"},
			want:   Source{Repo: "acme/node-src", Ref: "main"},
			ok:     true,
		},
		{
			name:  "configured with a ref",
			image: "acme/fixed:v3",
			want:  Source{Repo: "acme/fixed-src", Ref: "release"},
			ok:    true,
		},
		{
			name:   "nothing names the repository",
			image:  "ethereum/client-go:stable",
			labels: map[string]string{labelBuilderCommit: "1df30b18d9ed35ee9d28ffd4648442c2204554ad"},
			want:   Source{Commit: "1df30b18d9ed35ee9d28ffd4648442c2204554ad"},
		},
		{
			name:   "a repository label that is none",
			image:  "acme/other:main",
			labels: map[string]string{labelBuilderRepo: "../../etc", labelOCISource: "https://example.com/a/b"},
		},
		{
			name:  "an image of the builder's name at another registry",
			image: "registry.example/ethpandaops/grandine:main",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, err := imageref.Parse(c.image)
			require.NoError(t, err)

			got, ok := sourceOf(ref, c.labels, configured)
			require.Equal(t, c.ok, ok)
			require.Equal(t, c.want, got)
		})
	}
}

func TestParseSource(t *testing.T) {
	image, source, err := ParseSource("ethpandaops/grandine=grandinetech/grandine")
	require.NoError(t, err)
	require.Equal(t, "docker.io/ethpandaops/grandine", image)
	require.Equal(t, Source{Repo: "grandinetech/grandine"}, source)

	image, source, err = ParseSource("ghcr.io/acme/node=acme/src@release/1.x")
	require.NoError(t, err)
	require.Equal(t, "ghcr.io/acme/node", image)
	require.Equal(t, Source{Repo: "acme/src", Ref: "release/1.x"}, source)

	for _, bad := range []string{"acme/node", "acme/node=", "acme/node=https://github.com/a/b", "acme/node=a/b@ x", "=a/b"} {
		_, _, err = ParseSource(bad)
		require.Error(t, err, bad)
	}
}
