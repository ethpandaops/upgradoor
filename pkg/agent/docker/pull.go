package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
)

// pullDigestPrefix starts the status line with which the daemon reports the
// digest a pulled tag points at.
const pullDigestPrefix = "Digest: "

// Pull implements agent.ContainerRuntime.
func (r *Runtime) Pull(ctx context.Context, ref string) (string, error) {
	repo, err := repositoryOf(ref)
	if err != nil {
		return "", err
	}

	log := r.log.WithField("image", ref)

	auth, err := r.registryAuth(ref)
	if err != nil {
		// Credentials are optional: a public image still pulls without.
		log.WithError(err).Warn("Cannot use the registry credentials file, pulling anonymously")
	}

	resp, err := r.engine.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: auth})
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", ref, err)
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
			return "", fmt.Errorf("pull %s: read progress: %w", ref, err)
		}

		if msg.Error != nil {
			return "", fmt.Errorf("pull %s: %w", ref, msg.Error)
		}

		if digest, ok := strings.CutPrefix(msg.Status, pullDigestPrefix); ok {
			tagDigest = strings.TrimSpace(digest)
		}
	}

	img, err := r.inspectImage(ctx, ref)
	if err != nil {
		return "", err
	}

	if img == nil {
		return "", fmt.Errorf("pull %s: image is absent after the pull", ref)
	}

	if hasDigest(ref, img.RepoDigests, tagDigest) {
		r.pulledMu.Lock()
		r.pulled[pulledKey(repo, img.ID)] = tagDigest
		r.pulledMu.Unlock()
	}

	digest := r.digestOf(ref, img)

	log.WithField("digest", digest).Info("Image pulled")

	return digest, nil
}

// dockerConfigFile is the part of a Docker config.json the runtime reads.
type dockerConfigFile struct {
	Auths map[string]dockerConfigAuth `json:"auths"`
}

// dockerConfigAuth is one entry of the "auths" section.
type dockerConfigAuth struct {
	// Auth is base64("username:password").
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

// defaultAuthFile is where the Docker CLI keeps its configuration.
func defaultAuthFile() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".docker", "config.json")
}

// registryAuth returns the X-Registry-Auth value for the registry of ref,
// or "" when there are no credentials for it.
func (r *Runtime) registryAuth(ref string) (string, error) {
	if r.authFile == "" {
		return "", nil
	}

	host, err := registryOf(ref)
	if err != nil {
		return "", err
	}

	raw, err := os.ReadFile(r.authFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}

		return "", fmt.Errorf("read %s: %w", r.authFile, err)
	}

	cfg, err := findRegistryAuth(raw, host)
	if err != nil {
		return "", fmt.Errorf("%s: %w", r.authFile, err)
	}

	if cfg == nil {
		return "", nil
	}

	// The daemon takes the credentials as JSON in the X-Registry-Auth header.
	encoded, err := json.Marshal(cfg) //nolint:gosec // G117: sending the password is the purpose
	if err != nil {
		return "", fmt.Errorf("encode registry credentials: %w", err)
	}

	return base64.URLEncoding.EncodeToString(encoded), nil
}

// findRegistryAuth looks up the credentials for a (normalised) registry
// host in the content of a Docker config.json. Keys are matched by host, so
// "https://index.docker.io/v1/", "registry-1.docker.io" and "docker.io" all
// serve Docker Hub; an exact key wins over an alias.
func findRegistryAuth(configJSON []byte, host string) (*registry.AuthConfig, error) {
	var file dockerConfigFile

	if err := json.Unmarshal(configJSON, &file); err != nil {
		return nil, fmt.Errorf("parse docker config: %w", err)
	}

	var (
		entry dockerConfigAuth
		key   string
	)

	for candidate, value := range file.Auths {
		if normalizeRegistry(candidate) != host {
			continue
		}

		// Prefer the exact host, then the smallest key, to be deterministic.
		if key == "" || (key != host && (candidate == host || candidate < key)) {
			entry, key = value, candidate
		}
	}

	if key == "" {
		return nil, nil //nolint:nilnil // no credentials for this registry is a regular answer
	}

	cfg := &registry.AuthConfig{
		Username:      entry.Username,
		Password:      entry.Password,
		IdentityToken: entry.IdentityToken,
		RegistryToken: entry.RegistryToken,
		ServerAddress: key,
	}

	if entry.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return nil, fmt.Errorf("auths[%q]: invalid auth value: %w", key, err)
		}

		user, password, found := strings.Cut(string(decoded), ":")
		if !found {
			return nil, fmt.Errorf("auths[%q]: auth value is not user:password", key)
		}

		cfg.Username = user
		cfg.Password = strings.TrimRight(password, "\x00")
	}

	if cfg.Username == "" && cfg.IdentityToken == "" && cfg.RegistryToken == "" {
		return nil, nil //nolint:nilnil // an empty entry (credential helper stub) carries no credentials
	}

	return cfg, nil
}
