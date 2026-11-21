package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// pullDigestPrefix starts the status line with which the daemon reports the
// digest a pulled tag points at.
const pullDigestPrefix = "Digest: "

// Pull implements hostapi.ContainerRuntime.
func (r *Runtime) Pull(ctx context.Context, ref string) (digest, revision string, err error) {
	parsed, err := imageref.Parse(ref)
	if err != nil {
		return "", "", err
	}

	repo := parsed.Repository()
	log := r.log.WithField("image", ref)

	auth, err := r.registryAuth(parsed.Host)
	if err != nil {
		return "", "", fmt.Errorf("pull %s: %w", ref, err)
	}

	resp, err := r.engine.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: auth})
	if err != nil {
		return "", "", fmt.Errorf("pull %s: %w", ref, err)
	}

	defer func() {
		if cerr := resp.Close(); cerr != nil {
			log.WithError(cerr).Debug("Closing the pull stream failed")
		}
	}()

	// The request succeeds as soon as the daemon starts pulling; failures
	// arrive as messages in the stream, which must be read to the end.
	var tagDigest string

	for msg, err := range resp.JSONMessages(ctx) {
		if err != nil {
			return "", "", fmt.Errorf("pull %s: read progress: %w", ref, err)
		}

		if msg.Error != nil {
			return "", "", fmt.Errorf("pull %s: %w", ref, msg.Error)
		}

		if seen, ok := strings.CutPrefix(msg.Status, pullDigestPrefix); ok {
			tagDigest = strings.TrimSpace(seen)
		}
	}

	img, err := r.inspectImage(ctx, ref)
	if err != nil {
		return "", "", err
	}

	if img == nil {
		return "", "", fmt.Errorf("pull %s: image is absent after the pull", ref)
	}

	if hasDigest(ref, img.RepoDigests, tagDigest) {
		r.pulledMu.Lock()
		r.pulled[pulledKey(repo, img.ID)] = tagDigest
		r.pulledMu.Unlock()
	}

	digest = r.digestOf(ref, img)

	log.WithField("digest", digest).Info("Image pulled")

	return digest, revisionOf(img), nil
}

// registryAuth returns the X-Registry-Auth value for a registry host, or ""
// when there is no login for it.
func (r *Runtime) registryAuth(host string) (string, error) {
	if r.login == nil {
		return "", nil
	}

	username, password, ok := r.login(host)
	if !ok {
		return "", nil
	}

	// The daemon takes the login as JSON in the X-Registry-Auth header.
	//nolint:gosec // G117: sending the password is the purpose
	encoded, err := json.Marshal(registry.AuthConfig{
		Username: username, Password: password, ServerAddress: host,
	})
	if err != nil {
		return "", fmt.Errorf("encode registry login: %w", err)
	}

	return base64.URLEncoding.EncodeToString(encoded), nil
}
