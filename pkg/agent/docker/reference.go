package docker

import (
	"fmt"
	"slices"
	"strings"

	"github.com/distribution/reference"
)

const (
	hubDomain          = "docker.io"
	hubOfficialPrefix  = "library/"
	labelImageRevision = "org.opencontainers.image.revision"
	labelImageVersion  = "org.opencontainers.image.version"
)

// hubAliases are the registry hosts that all mean Docker Hub. The reference
// parser already folds index.docker.io into docker.io; registry-1.docker.io
// is the host the daemon really talks to and shows up in hand-written
// references and credential files.
var hubAliases = []string{"index.docker.io", "registry-1.docker.io", "registry.hub.docker.com"}

// normalizeRegistry maps a registry host, or a credential file key such as
// "https://index.docker.io/v1/", to the host name used for comparisons.
func normalizeRegistry(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host, _, _ = strings.Cut(host, "/")

	if slices.Contains(hubAliases, host) {
		return hubDomain
	}

	return host
}

// repositoryOf returns the fully qualified repository an image reference
// names, without tag and digest: "alpine", "alpine:3.21",
// "docker.io/library/alpine@sha256:..." and "index.docker.io/library/alpine"
// all give "docker.io/library/alpine".
func repositoryOf(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(ref))
	if err != nil {
		return "", fmt.Errorf("parse image reference %q: %w", ref, err)
	}

	domain := normalizeRegistry(reference.Domain(named))
	path := reference.Path(named)

	if domain == hubDomain && !strings.Contains(path, "/") {
		path = hubOfficialPrefix + path
	}

	return domain + "/" + path, nil
}

// registryOf returns the normalised registry host of an image reference.
func registryOf(ref string) (string, error) {
	repo, err := repositoryOf(ref)
	if err != nil {
		return "", err
	}

	host, _, _ := strings.Cut(repo, "/")

	return host, nil
}

// repositoryDigests returns the digests ("sha256:...") of the entries of an
// image's RepoDigests that belong to repo, in their original order and
// without duplicates. Entries that do not parse are ignored.
func repositoryDigests(repo string, repoDigests []string) []string {
	var out []string

	for _, entry := range repoDigests {
		named, err := reference.ParseNormalizedNamed(entry)
		if err != nil {
			continue
		}

		canonical, ok := named.(reference.Canonical)
		if !ok {
			continue
		}

		entryRepo, err := repositoryOf(named.Name())
		if err != nil || entryRepo != repo {
			continue
		}

		if dgst := canonical.Digest().String(); !slices.Contains(out, dgst) {
			out = append(out, dgst)
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
