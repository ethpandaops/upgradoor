package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// RevisionLabel is the OCI image label carrying the source commit.
const RevisionLabel = "org.opencontainers.image.revision"

const (
	// maxRevisions bounds the revision cache; the oldest entries go first.
	maxRevisions = 4096

	// maxRevisionAttempts bounds how often the lookup runs for one digest
	// when it keeps failing; after that the digest has no revision.
	maxRevisionAttempts = 3
	// revisionRetryInterval is the minimum time between two lookups for one
	// digest, so that a failing lookup never costs requests on every poll.
	revisionRetryInterval = 10 * time.Minute

	platformUnknown = "unknown"
)

// revisionEntry is one revision lookup. revision may be read once ready is
// closed; the other fields are guarded by Resolver.mu.
type revisionEntry struct {
	ready    chan struct{}
	revision string

	// failed is set when the lookup ended with an error.
	failed bool
	// attempts counts the lookups that ran for this digest, this one
	// included.
	attempts int
	// retryAt is when a failed lookup may run again.
	retryAt time.Time
}

// manifest is the part of an image manifest or index this package reads.
type manifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

// imageConfig is the part of an image config this package reads.
type imageConfig struct {
	Config struct {
		Labels map[string]string `json:"Labels"` //nolint:tagliatelle // image config wire format
	} `json:"config"`
}

// revision returns the revision label of the image behind digest. A lookup
// that succeeds runs once per repository and digest and is cached. One that
// fails yields an empty revision and is tried again by a later call, at most
// maxRevisionAttempts times in all and no sooner than revisionRetryInterval
// after the failure. A lookup cut short by the caller's context says nothing
// about the image and is not held against the digest. body is the manifest
// behind digest when the caller already has it.
func (r *Resolver) revision(ctx context.Context, ref Reference, digest string, body []byte) string {
	key := ref.APIHost() + "/" + ref.Path + "@" + digest

	entry, owner := r.claimRevision(key)
	if !owner {
		select {
		case <-entry.ready:
			return entry.revision
		case <-ctx.Done():
			return ""
		}
	}

	rev, err := r.fetchRevision(ctx, ref, digest, body)
	if err != nil {
		r.log.WithError(err).WithFields(logrus.Fields{
			"image":  ref.String(),
			"digest": digest,
		}).Debug("revision label unavailable")
	}

	r.settleRevision(key, entry, rev, err != nil, ctx.Err() != nil)

	return rev
}

// claimRevision returns the cache entry for key. owner is true when the
// caller has to run the lookup and settle the entry: for a new entry, and for
// a failed one that is due for another attempt.
func (r *Resolver) claimRevision(key string) (entry *revisionEntry, owner bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	attempts := 0

	if known, ok := r.revisions[key]; ok {
		if !known.failed || known.attempts >= maxRevisionAttempts || r.now().Before(known.retryAt) {
			return known, false
		}

		// The retry takes the place of the failed entry and keeps its slot
		// in the eviction order.
		attempts = known.attempts
	} else {
		for len(r.revisionOrder) >= maxRevisions {
			delete(r.revisions, r.revisionOrder[0])
			r.revisionOrder = r.revisionOrder[1:]
		}

		r.revisionOrder = append(r.revisionOrder, key)
	}

	entry = &revisionEntry{ready: make(chan struct{}), attempts: attempts + 1}
	r.revisions[key] = entry

	return entry, true
}

// settleRevision records the outcome of the lookup its owner ran and wakes
// the waiters. An interrupted lookup is forgotten, so that the next call
// starts over.
func (r *Resolver) settleRevision(key string, entry *revisionEntry, revision string, failed, interrupted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	defer close(entry.ready)

	if !failed {
		entry.revision = revision

		return
	}

	entry.failed = true
	entry.retryAt = r.now().Add(revisionRetryInterval)

	if !interrupted || r.revisions[key] != entry {
		return
	}

	delete(r.revisions, key)

	// The key was appended recently, so it is found near the end.
	for i := len(r.revisionOrder) - 1; i >= 0; i-- {
		if r.revisionOrder[i] == key {
			r.revisionOrder = append(r.revisionOrder[:i], r.revisionOrder[i+1:]...)

			break
		}
	}
}

// fetchRevision reads the revision label from the image config, descending
// through an index to the configured platform when needed.
func (r *Resolver) fetchRevision(ctx context.Context, ref Reference, digest string, body []byte) (string, error) {
	var err error

	if body == nil {
		body, err = r.fetchByDigest(ctx, ref, "manifests", digest, acceptManifests, maxManifestBytes)
		if err != nil {
			return "", err
		}
	}

	var m manifest
	if err = json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("registry: parse manifest %s: %w", digest, err)
	}

	if len(m.Manifests) > 0 {
		var child string

		child, err = r.platform.pick(&m)
		if err != nil {
			return "", err
		}

		body, err = r.fetchByDigest(ctx, ref, "manifests", child, acceptManifests, maxManifestBytes)
		if err != nil {
			return "", err
		}

		m = manifest{}
		if err = json.Unmarshal(body, &m); err != nil {
			return "", fmt.Errorf("registry: parse platform manifest %s: %w", child, err)
		}
	}

	if m.Config.Digest == "" {
		return "", errors.New("registry: manifest names no image config")
	}

	body, err = r.fetchByDigest(ctx, ref, "blobs", m.Config.Digest, "", maxConfigBytes)
	if err != nil {
		return "", err
	}

	var cfg imageConfig
	if err = json.Unmarshal(body, &cfg); err != nil {
		return "", fmt.Errorf("registry: parse image config %s: %w", m.Config.Digest, err)
	}

	return cfg.Config.Labels[RevisionLabel], nil
}

// fetchByDigest GETs a manifest or blob by digest and checks sha256 content
// against its address.
func (r *Resolver) fetchByDigest(
	ctx context.Context, ref Reference, kind, digest, accept string, limit int64,
) ([]byte, error) {
	if !digestPattern.MatchString(digest) {
		return nil, fmt.Errorf("registry: invalid digest %q", digest)
	}

	resp, err := r.do(ctx, ref, http.MethodGet, r.baseURL(ref)+"/"+kind+"/"+digest, accept)
	if err != nil {
		return nil, err
	}

	defer closeBody(resp)

	body, err := readCapped(resp.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", digest, err)
	}

	if strings.HasPrefix(digest, "sha256:") && sha256Digest(body) != digest {
		return nil, fmt.Errorf("registry: content of %s does not match its digest", digest)
	}

	return body, nil
}

// pick chooses the manifest of an index to read the image config from: the
// one for the platform, else the first that is an image (attestation
// manifests carry the platform unknown/unknown).
func (p platform) pick(index *manifest) (string, error) {
	fallback := ""

	for i := range index.Manifests {
		entry := &index.Manifests[i]
		plat := entry.Platform

		if plat.OS == p.os && plat.Architecture == p.arch && (p.variant == "" || plat.Variant == p.variant) {
			return entry.Digest, nil
		}

		if fallback == "" && plat.OS != platformUnknown && plat.Architecture != platformUnknown {
			fallback = entry.Digest
		}
	}

	if fallback == "" {
		return "", errors.New("registry: index holds no image manifest")
	}

	return fallback, nil
}
