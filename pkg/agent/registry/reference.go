package registry

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// dockerHubAPIHost is the host that serves Docker Hub's registry API.
const dockerHubAPIHost = "registry-1.docker.io"

// errDigestReference is returned for a reference pinned to a digest: there
// is no tag to resolve.
var errDigestReference = errors.New("registry: reference is pinned to a digest")

// digestPattern is algorithm:hex. Only hex encodings are accepted, which
// covers every algorithm registries use and keeps a digest safe to put in a
// URL path.
var digestPattern = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-f0-9]{32,128}$`)

// parseTagReference parses [host/]path[:tag]. A reference with a digest is
// refused with errDigestReference.
func parseTagReference(ref string) (imageref.Reference, error) {
	parsed, err := imageref.Parse(ref)
	if err != nil {
		return imageref.Reference{}, fmt.Errorf("registry: %w", err)
	}

	if parsed.Digest != "" {
		return imageref.Reference{}, fmt.Errorf("%w: %q", errDigestReference, ref)
	}

	return parsed, nil
}

// apiHost is the host to send requests for ref to.
func apiHost(ref imageref.Reference) string {
	if ref.Host == imageref.DockerHubHost {
		return dockerHubAPIHost
	}

	return ref.Host
}
