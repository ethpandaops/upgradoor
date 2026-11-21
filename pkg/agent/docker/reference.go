package docker

import (
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/agent"
)

const (
	labelImageRevision = "org.opencontainers.image.revision"
	labelImageVersion  = "org.opencontainers.image.version"
)

// repositoryOf returns the fully qualified repository an image reference
// names, without tag and digest: "alpine", "alpine:3.21",
// "docker.io/library/alpine@sha256:..." and "index.docker.io/library/alpine"
// all give "docker.io/library/alpine".
func repositoryOf(ref string) (string, error) {
	parsed, err := agent.ParseReference(ref)
	if err != nil {
		return "", err
	}

	return parsed.Repository(), nil
}

// repositoryDigests returns the digests ("sha256:...") of the entries of an
// image's RepoDigests that belong to repo, in their original order and
// without duplicates. Entries that do not parse are ignored.
func repositoryDigests(repo string, repoDigests []string) []string {
	var out []string

	for _, entry := range repoDigests {
		parsed, err := agent.ParseReference(entry)
		if err != nil || parsed.Digest == "" || parsed.Repository() != repo {
			continue
		}

		if !slices.Contains(out, parsed.Digest) {
			out = append(out, parsed.Digest)
		}
	}

	return out
}

// selectDigest picks the registry digest of an image for the repository of
// ref. An image usually has one digest per repository. It can have several
// when the same image content was pulled through different manifests (a
// multi-arch index and one of its platform manifests, or a re-pushed tag);
// then the first of preferred that is among them wins, and failing that the
// first entry.
func selectDigest(ref string, repoDigests []string, preferred ...string) string {
	repo, err := repositoryOf(ref)
	if err != nil {
		return ""
	}

	candidates := repositoryDigests(repo, repoDigests)
	if len(candidates) == 0 {
		return ""
	}

	for _, want := range preferred {
		if want != "" && slices.Contains(candidates, want) {
			return want
		}
	}

	return candidates[0]
}

// hasDigest reports whether digest is one of the registry digests of an
// image for the repository of ref.
func hasDigest(ref string, repoDigests []string, digest string) bool {
	if digest == "" {
		return false
	}

	repo, err := repositoryOf(ref)
	if err != nil {
		return false
	}

	return slices.Contains(repositoryDigests(repo, repoDigests), digest)
}
