package agenttest

import (
	"net/http"
	"strings"
	"sync"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// Registry is a registry for the controller's lookups: an http.Handler that
// answers the manifest HEAD and GET of the distribution API with the digest
// of a tag. It serves no images; a fake host pulls by asking it for the
// digest (Host.PullFrom).
type Registry struct {
	mu   sync.Mutex
	tags map[string]string // by "path:tag"
}

var _ http.Handler = (*Registry)(nil)

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tags: make(map[string]string, 8)}
}

// Set points the tag of ref at digest; the registry host of ref does not
// matter. It reports whether ref is a tag reference.
func (r *Registry) Set(ref, digest string) bool {
	parsed, err := imageref.Parse(ref)
	if err != nil || parsed.Digest != "" {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.tags[parsed.Path+":"+parsed.Tag] = digest

	return true
}

// ServeHTTP answers /v2/ and /v2/<path>/manifests/<tag>.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok || (req.Method != http.MethodGet && req.Method != http.MethodHead) {
		http.NotFound(w, req)

		return
	}

	if rest == "" {
		w.WriteHeader(http.StatusOK)

		return
	}

	path, tag, ok := strings.Cut(rest, "/manifests/")

	r.mu.Lock()
	digest := r.tags[path+":"+tag]
	r.mu.Unlock()

	if !ok || digest == "" {
		http.NotFound(w, req)

		return
	}

	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	w.WriteHeader(http.StatusOK)
}
