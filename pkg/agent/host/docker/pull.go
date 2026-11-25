package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// pullDigestPrefix starts the daemon's status line naming the pulled digest.
	pullDigestPrefix = "Digest: "
	// pullFromPrefix starts the status line of the tag, not of a layer.
	pullFromPrefix = "Pulling from "
	// pullStall is how long a pull may go without progress.
	pullStall = 3 * time.Minute
)

// Pull implements Runtime.
func (r *dockerRuntime) Pull(
	ctx context.Context, ref string, progress func(*protocol.PullProgress),
) (digest, revision string, err error) {
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

	tagDigest, err := r.pullStream(ctx, ref, auth, progress)
	if err != nil {
		return "", "", err
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

// pullStream runs the pull and reads its messages to the end, cancelling it
// after r.stall without progress, and returns the digest the daemon named.
func (r *dockerRuntime) pullStream(
	ctx context.Context, ref, auth string, progress func(*protocol.PullProgress),
) (string, error) {
	pctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	watch := time.AfterFunc(r.stall, func() { cancel(ErrPullStalled) })
	defer watch.Stop()

	fail := func(err error) error {
		if ctx.Err() == nil && errors.Is(context.Cause(pctx), ErrPullStalled) {
			return fmt.Errorf("pull %s: %w for %s", ref, ErrPullStalled, r.stall)
		}

		return fmt.Errorf("pull %s: %w", ref, err)
	}

	resp, err := r.engine.ImagePull(pctx, ref, client.ImagePullOptions{RegistryAuth: auth})
	if err != nil {
		return "", fail(err)
	}

	defer func() {
		if cerr := resp.Close(); cerr != nil {
			r.log.WithError(cerr).WithField("image", ref).Debug("Closing the pull stream failed")
		}
	}()

	// The request succeeds as soon as the daemon starts pulling; failures
	// arrive as messages in the stream, which must be read to the end.
	var (
		tagDigest string
		layers    pullLayers
	)

	for msg, err := range resp.JSONMessages(pctx) {
		if err != nil {
			return "", fail(fmt.Errorf("read progress: %w", err))
		}

		if msg.Error != nil {
			return "", fail(msg.Error)
		}

		if seen, ok := strings.CutPrefix(msg.Status, pullDigestPrefix); ok {
			tagDigest = strings.TrimSpace(seen)
		}

		if layers.note(&msg) {
			watch.Reset(r.stall)

			if progress != nil {
				p := layers.progress()
				progress(&p)
			}
		}
	}

	if err := pctx.Err(); err != nil {
		return "", fail(err)
	}

	return tagDigest, nil
}

// layerStage is how far one layer got; it only grows.
type layerStage int

const (
	layerWaiting layerStage = iota
	layerDownloading
	layerDownloaded
	layerExtracting
	layerDone
)

// pullLayer is one layer of a pull. done counts the downloaded bytes.
type pullLayer struct {
	stage       layerStage
	done, total int64
	extracted   int64
}

// pullLayers follows the layers of a pull through the daemon's messages.
type pullLayers struct {
	order []string
	byID  map[string]*pullLayer
}

// note takes in one message and reports whether it is progress: a new
// layer, a layer further along or more bytes downloaded or extracted.
// Messages that change nothing, such as the daemon's own retry countdown,
// are not.
func (p *pullLayers) note(msg *jsonstream.Message) bool {
	if msg.ID == "" || strings.HasPrefix(msg.Status, pullFromPrefix) {
		return false
	}

	l, known := p.byID[msg.ID]
	if !known {
		if p.byID == nil {
			p.byID = make(map[string]*pullLayer, 16)
		}

		l = &pullLayer{}
		p.byID[msg.ID] = l
		p.order = append(p.order, msg.ID)
	}

	before := *l

	var cur, total int64
	if msg.Progress != nil {
		cur, total = msg.Progress.Current, msg.Progress.Total
	}

	switch msg.Status {
	case "Downloading":
		l.stage = max(l.stage, layerDownloading)
		l.done = cur

		if total > 0 {
			l.total = total
		}
	case "Verifying Checksum", "Download complete":
		l.stage = max(l.stage, layerDownloaded)
	case "Extracting":
		l.stage = max(l.stage, layerExtracting)
		l.extracted = cur
	case "Pull complete", "Already exists":
		l.stage = layerDone
	}

	if l.stage >= layerDownloaded && l.total > 0 {
		l.done = l.total
	}

	return !known || *l != before
}

// progress is the counts over the layers seen so far.
func (p *pullLayers) progress() protocol.PullProgress {
	out := protocol.PullProgress{LayersTotal: len(p.order)}

	for _, id := range p.order {
		l := p.byID[id]
		out.BytesDone += l.done
		out.BytesTotal += l.total

		if l.stage == layerDone {
			out.LayersDone++
		}
	}

	return out
}

// registryAuth returns the X-Registry-Auth value for a registry host, or "".
func (r *dockerRuntime) registryAuth(host string) (string, error) {
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
