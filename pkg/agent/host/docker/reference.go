package docker

import (
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

const (
	labelImageRevision = "org.opencontainers.image.revision"
	labelImageVersion  = "org.opencontainers.image.version"
)

// repositoryOf returns the fully qualified repository of an image reference,
// e.g. "alpine:3.21" gives "docker.io/library/alpine".
func repositoryOf(ref string) (string, error) {
	parsed, err := imageref.Parse(ref)
	if err != nil {
		return "", err
	}

	return parsed.Repository(), nil
}

// repositoryDigests returns the unique RepoDigests digests of repo, in order.
func repositoryDigests(repo string, repoDigests []string) []string {
	var out []string

	for _, entry := range repoDigests {
		parsed, err := imageref.Parse(entry)
		if err != nil || parsed.Digest == "" || parsed.Repository() != repo {
			continue
		}

		if !slices.Contains(out, parsed.Digest) {
			out = append(out, parsed.Digest)
		}
	}

	return out
}

// selectDigest picks an image's registry digest for ref's repository. With
// several (an index and a platform manifest, or a re-pushed tag), the first
// of preferred among them wins, else the first entry.
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

// hasDigest reports whether digest is a registry digest of ref's repository.
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
