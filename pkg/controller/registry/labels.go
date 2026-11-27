package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

const (
	// maxConfigBytes bounds an image configuration.
	maxConfigBytes = 4 << 20

	platformOS      = "linux"
	platformArch    = "amd64"
	platformUnknown = "unknown"
)

// manifest is what Labels reads of an image manifest or an index.
type manifest struct {
	// Manifests are the images of an index; empty for an image manifest.
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

// IsNotFound reports whether err is a registry's answer that it does not
// have what was asked for.
func IsNotFound(err error) bool {
	var status *statusError

	return errors.As(err, &status) && status.Code == http.StatusNotFound
}

// Labels implements Resolver. Of an index it reads the linux/amd64 image, or
// the first one that is an image.
func (r *resolver) Labels(ctx context.Context, ref, digest string) (map[string]string, error) {
	parsed, err := parseTagReference(ref)
	if err != nil {
		return nil, err
	}

	m, err := r.manifest(ctx, parsed, digest)
	if err != nil {
		return nil, err
	}

	if len(m.Manifests) > 0 {
		pick := ""

		for i := range m.Manifests {
			p := &m.Manifests[i]
			if p.Platform.OS == platformUnknown {
				continue
			}

			if pick == "" || (p.Platform.OS == platformOS && p.Platform.Architecture == platformArch) {
				pick = p.Digest
			}

			if p.Platform.OS == platformOS && p.Platform.Architecture == platformArch {
				break
			}
		}

		if pick == "" {
			return nil, fmt.Errorf("registry: the index of %s lists no image", parsed)
		}

		if m, err = r.manifest(ctx, parsed, pick); err != nil {
			return nil, err
		}
	}

	if !digestPattern.MatchString(m.Config.Digest) {
		return nil, fmt.Errorf("registry: the manifest of %s names no configuration", parsed)
	}

	resp, err := r.do(ctx, parsed, http.MethodGet, r.baseURL(parsed)+"/blobs/"+m.Config.Digest)
	if err != nil {
		return nil, err
	}

	defer closeBody(resp)

	body, err := readCapped(resp.Body, maxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("registry: read configuration of %s: %w", parsed, err)
	}

	var config struct {
		Config struct {
			Labels map[string]string `json:"Labels"` //nolint:tagliatelle // the OCI image configuration
		} `json:"config"`
	}

	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("registry: configuration of %s: %w", parsed, err)
	}

	return config.Config.Labels, nil
}

// manifest GETs the manifest or index of a digest.
func (r *resolver) manifest(ctx context.Context, ref imageref.Reference, digest string) (*manifest, error) {
	if !digestPattern.MatchString(digest) {
		return nil, fmt.Errorf("registry: %q is not a digest", digest)
	}

	resp, err := r.do(ctx, ref, http.MethodGet, r.baseURL(ref)+"/manifests/"+digest)
	if err != nil {
		return nil, err
	}

	defer closeBody(resp)

	body, err := readCapped(resp.Body, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("registry: read manifest of %s: %w", ref, err)
	}

	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("registry: manifest of %s: %w", ref, err)
	}

	return &m, nil
}
