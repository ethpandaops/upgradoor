package registry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	dockerHubHost    = "docker.io"
	dockerHubAPIHost = "registry-1.docker.io"
	dockerHubIndex   = "index.docker.io"
	dockerHubLibrary = "library/"
	localhost        = "localhost"
	defaultTag       = "latest"
)

// ErrDigestReference is returned by ParseReference for a reference pinned to
// a digest: there is no tag to resolve.
var ErrDigestReference = errors.New("registry: reference is pinned to a digest")

var (
	// hostPattern is a DNS name or IPv4 address, or a bracketed IPv6 address,
	// with an optional port.
	hostPattern = regexp.MustCompile(
		`^(?:[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?|\[[0-9a-f:.]+\])(?::[0-9]+)?$`)
	// pathPattern is the distribution spec's repository name.
	pathPattern = regexp.MustCompile(
		`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*$`)
	// tagPattern is the distribution spec's tag.
	tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// digestPattern is algorithm:hex. Only hex encodings are accepted, which
	// covers every algorithm registries use and keeps a digest safe to put in
	// a URL path.
	digestPattern = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-f0-9]{32,128}$`)
)

// Reference is a parsed [host/]path[:tag] image reference in canonical form.
type Reference struct {
	// Host is the registry in lower case; docker.io when omitted or written
	// as one of the Docker Hub aliases.
	Host string
	// Path is the repository path, with library/ added for Docker Hub
	// official images.
	Path string
	// Tag is the tag, latest when omitted.
	Tag string
}

// ParseReference splits [host/]path[:tag] the way Docker does: the first
// component is a registry host when it contains a dot or a colon or is
// "localhost"; otherwise the reference names a Docker Hub repository. A
// reference with a digest is refused with ErrDigestReference.
func ParseReference(ref string) (Reference, error) {
	if ref == "" {
		return Reference{}, errors.New("registry: empty reference")
	}

	if strings.Contains(ref, "@") {
		return Reference{}, fmt.Errorf("%w: %q", ErrDigestReference, ref)
	}

	return parseName(ref, defaultTag)
}

// parseName parses a reference without digest. fallbackTag is used when the
// reference carries no tag.
func parseName(ref, fallbackTag string) (Reference, error) {
	host := dockerHubHost
	rest := ref

	if i := strings.Index(ref, "/"); i > 0 {
		first := ref[:i]
		if strings.ContainsAny(first, ".:") || first == localhost {
			host = strings.ToLower(first)
			rest = ref[i+1:]
		}
	}

	if host == dockerHubIndex || host == dockerHubAPIHost {
		host = dockerHubHost
	}

	path, tag := rest, fallbackTag

	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "/") {
		path, tag = rest[:i], rest[i+1:]

		if tag == "" {
			return Reference{}, fmt.Errorf("registry: %q: empty tag", ref)
		}
	}

	if host == dockerHubHost && !strings.Contains(path, "/") {
		path = dockerHubLibrary + path
	}

	switch {
	case !hostPattern.MatchString(host):
		return Reference{}, fmt.Errorf("registry: %q: invalid registry host", ref)
	case len(path) > 255 || !pathPattern.MatchString(path):
		return Reference{}, fmt.Errorf("registry: %q: invalid repository path", ref)
	case tag != "" && !tagPattern.MatchString(tag):
		return Reference{}, fmt.Errorf("registry: %q: invalid tag", ref)
	}

	return Reference{Host: host, Path: path, Tag: tag}, nil
}

// APIHost is the host to send requests to.
func (r Reference) APIHost() string {
	if r.Host == dockerHubHost {
		return dockerHubAPIHost
	}

	return r.Host
}

// Repository renders host/path.
func (r Reference) Repository() string {
	return r.Host + "/" + r.Path
}

// String renders host/path:tag.
func (r Reference) String() string {
	return r.Host + "/" + r.Path + ":" + r.Tag
}

// Normalize returns the canonical spelling of an image reference so that two
// references can be compared as strings: "geth", "docker.io/library/geth" and
// "index.docker.io/library/geth:latest" all become
// "docker.io/library/geth:latest".
//
// A digest is kept: "geth@sha256:…" becomes "docker.io/library/geth@sha256:…"
// and "geth:v1@sha256:…" keeps both tag and digest (no tag is invented for a
// digest reference, which names one manifest whatever latest points at). A
// reference that cannot be parsed is returned unchanged, so it only ever
// equals itself.
func Normalize(ref string) string {
	name, digest, pinned := strings.Cut(ref, "@")
	if !pinned {
		parsed, err := ParseReference(ref)
		if err != nil {
			return ref
		}

		return parsed.String()
	}

	if !digestPattern.MatchString(digest) {
		return ref
	}

	parsed, err := parseName(name, "")
	if err != nil {
		return ref
	}

	if parsed.Tag == "" {
		return parsed.Repository() + "@" + digest
	}

	return parsed.String() + "@" + digest
}
