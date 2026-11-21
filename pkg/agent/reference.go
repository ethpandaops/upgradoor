package agent

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/distribution/reference"
)

const (
	// DockerHubHost is the registry host Docker Hub is known by, under
	// whatever alias it was written.
	DockerHubHost = "docker.io"

	defaultTag = "latest"
)

// dockerHubAliases are the registry hosts that all mean Docker Hub:
// index.docker.io is what Docker writes into credential files,
// registry-1.docker.io is the host the daemon really talks to.
var dockerHubAliases = []string{
	"index.docker.io", "registry-1.docker.io", "registry.hub.docker.com",
}

var errInvalidHost = errors.New("invalid registry host")

// RegistryHost maps a registry host as written in an image reference, or a
// key of the "auths" map of a Docker config.json (such as
// "https://index.docker.io/v1/"), to the host as Reference.Host spells it.
func RegistryHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host, _, _ = strings.Cut(host, "/")

	if slices.Contains(dockerHubAliases, host) {
		return DockerHubHost
	}

	return host
}

// Reference is a parsed image reference in canonical form. Every part is
// safe to place in a URL path.
type Reference struct {
	// Host is the registry in lower case; DockerHubHost when omitted or
	// written as one of the Docker Hub aliases.
	Host string
	// Path is the repository path, with library/ added for Docker Hub
	// official images.
	Path string
	// Tag is the tag: latest when the reference has neither tag nor digest,
	// empty when it has only a digest.
	Tag string
	// Digest is the digest the reference is pinned to, if any.
	Digest string
}

// ParseReference parses [host/]path[:tag][@digest] the way Docker does: the
// first component is a registry host when it contains a dot or a colon or is
// "localhost"; otherwise the reference names a Docker Hub repository.
func ParseReference(ref string) (Reference, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return Reference{}, fmt.Errorf("image reference %q: %w", ref, err)
	}

	out := Reference{
		Host: RegistryHost(reference.Domain(named)),
		Path: reference.Path(named),
	}

	// The library reads a first component with a dot that is no valid host
	// as a part of the path, with no host at all, and one in upper case as a
	// host whatever it looks like. Neither has a canonical form that parses
	// to itself.
	if !strings.ContainsAny(out.Host, ".:") && out.Host != "localhost" {
		return Reference{}, fmt.Errorf("image reference %q: %w", ref, errInvalidHost)
	}

	if out.Host == DockerHubHost && !strings.Contains(out.Path, "/") {
		out.Path = "library/" + out.Path
	}

	if tagged, ok := named.(reference.Tagged); ok {
		out.Tag = tagged.Tag()
	}

	if digested, ok := named.(reference.Digested); ok {
		out.Digest = digested.Digest().String()
	}

	if out.Tag == "" && out.Digest == "" {
		out.Tag = defaultTag
	}

	return out, nil
}

// Repository renders host/path.
func (r Reference) Repository() string {
	return r.Host + "/" + r.Path
}

// String renders host/path[:tag][@digest].
func (r Reference) String() string {
	out := r.Repository()

	if r.Tag != "" {
		out += ":" + r.Tag
	}

	if r.Digest != "" {
		out += "@" + r.Digest
	}

	return out
}

// NormalizeReference brings an image reference into the canonical
// "host/path:tag" form so that two spellings of the same image compare
// equal: "x", "x:latest" and "docker.io/library/x:latest" are one image.
//
// A digest is kept (a pinned reference is a different thing from a tag), and
// no default tag is added to a digest-only reference. Input that is not an
// image reference (an image ID, for instance) is returned as it is, which
// equals no canonical reference.
func NormalizeReference(ref string) string {
	ref = strings.TrimSpace(ref)

	parsed, err := ParseReference(ref)
	if err != nil {
		return ref
	}

	return parsed.String()
}

// SameReference reports whether two image references name the same image
// and tag.
func SameReference(a, b string) bool {
	return NormalizeReference(a) == NormalizeReference(b)
}

// isTagReference reports whether ref follows a tag, i.e. is not pinned to a
// digest. Only tag references are polled and rolled.
func isTagReference(ref string) bool {
	return ref != "" && !strings.Contains(ref, "@") && !strings.HasPrefix(ref, "sha256:")
}
