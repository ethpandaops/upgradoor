package agent

import "strings"

const (
	defaultRegistryHost = "docker.io"
	defaultTag          = "latest"
)

// NormalizeReference brings an image reference into the canonical
// "host/path:tag" form so that two spellings of the same image compare
// equal: "x", "x:latest" and "docker.io/library/x:latest" are one image.
//
// A digest suffix is kept as written (a pinned reference is a different
// thing from a tag), and no default tag is added to a digest-only reference.
// Input that is not an image reference (an image ID, for instance) is
// returned in a form that equals no tag reference.
func NormalizeReference(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}

	name, digest, _ := strings.Cut(ref, "@")

	host, path := defaultRegistryHost, name

	if first, rest, ok := strings.Cut(name, "/"); ok && isRegistryHost(first) {
		host, path = first, rest
	}

	if host == "index.docker.io" || host == "registry-1.docker.io" {
		host = defaultRegistryHost
	}

	tag := ""

	if i := strings.LastIndex(path, ":"); i > strings.LastIndex(path, "/") {
		path, tag = path[:i], path[i+1:]
	}

	if host == defaultRegistryHost && !strings.Contains(path, "/") {
		path = "library/" + path
	}

	if tag == "" && digest == "" {
		tag = defaultTag
	}

	out := host + "/" + path

	if tag != "" {
		out += ":" + tag
	}

	if digest != "" {
		out += "@" + digest
	}

	return out
}

// isRegistryHost reports whether the first path component of a reference is
// a registry host, by the rule Docker uses: it contains a dot or a port, or
// it is "localhost".
func isRegistryHost(s string) bool {
	return s == "localhost" || strings.ContainsAny(s, ".:")
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
