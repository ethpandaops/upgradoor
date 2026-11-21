package registry

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/ethpandaops/upgradoor/pkg/agent"
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
func parseTagReference(ref string) (agent.Reference, error) {
	parsed, err := agent.ParseReference(ref)
	if err != nil {
		return agent.Reference{}, fmt.Errorf("registry: %w", err)
	}

	if parsed.Digest != "" {
		return agent.Reference{}, fmt.Errorf("%w: %q", errDigestReference, ref)
	}

	return parsed, nil
}

// apiHost is the host to send requests for ref to.
func apiHost(ref agent.Reference) string {
	if ref.Host == agent.DockerHubHost {
		return dockerHubAPIHost
	}

	return ref.Host
}
