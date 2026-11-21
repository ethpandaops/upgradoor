package docker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const (
	// replacedSuffix marks the old container while its replacement is being
	// built; whatever carries the real name next to it is unfinished.
	replacedSuffix = "_upgradoor_old"

	// retiredSuffix marks the old container once its replacement is complete
	// (created, networks connected); it only awaits removal.
	retiredSuffix = "_upgradoor_done"

	// cleanupTimeout bounds rollback and cleanup, which run even after the
	// caller's context is cancelled.
	cleanupTimeout = 2 * time.Minute
)

// errLeftover says a replaced container could not be removed; its
// replacement is in place, so it never blocks a start or stop.
var errLeftover = errors.New("replaced container cannot be removed")

// discardReplacement removes a given-up replacement with the anonymous
// volumes it created. Volumes taken over from the old container are attached
// by name, which the daemon never removes with a container.
var discardReplacement = client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}

// recreateStage says how far a replacement got, for the rollback.
type recreateStage int

const (
	// stageStopped: the old container still carries its name.
	stageStopped recreateStage = iota
	// stageReplaced: the old container carries replacedSuffix.
	stageReplaced
	// stageRetired: the old container carries retiredSuffix.
	stageRetired
)

// recreatePlan is a verified, ready-to-run replacement of one container.
type recreatePlan struct {
	name string
	old  *container.InspectResponse
	spec *createSpec
	// imageID is the verified local image of the replacement.
	imageID    string
	wasRunning bool
}

// Recreate implements Runtime.
func (r *dockerRuntime) Recreate(
	ctx context.Context,
	name, digest string,
	stopTimeout time.Duration,
	start bool,
	change *Change,
) error {
	if change == nil {
		change = &Change{}
	}

	r.recreateMu.Lock()
	defer r.recreateMu.Unlock()

	// The digest comes first: a refused Recreate changes nothing, not even
	// the leftovers of an interrupted one.
	ref, err := r.referenceOf(ctx, name)
	if err != nil {
		return fmt.Errorf("recreate container %s: %w", name, err)
	}

	if ref != "" && change.Image != "" {
		ref = change.Image
	}

	if ref != "" {
		if _, err = r.verifyDigest(ctx, ref, digest); err != nil {
			return fmt.Errorf("recreate container %s: %w", name, err)
		}
	}

	if err = r.recoverInterrupted(ctx, name, true); err != nil {
		return fmt.Errorf("recreate container %s: %w", name, err)
	}

	plan, err := r.planRecreate(ctx, name, digest, change)
	if err != nil {
		return fmt.Errorf("recreate container %s: %w", name, err)
	}

	if err := r.replace(ctx, plan, stopTimeout, start); err != nil {
		return fmt.Errorf("recreate container %s: %w", name, err)
	}

	return nil
}

// lookup inspects the container with exactly that name; nil when none.
func (r *dockerRuntime) lookup(ctx context.Context, name string) (*container.InspectResponse, error) {
	res, err := r.engine.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, nil //nolint:nilnil // "no such container" is a regular answer here
		}

		return nil, fmt.Errorf("inspect container %s: %w", name, err)
	}

	if strings.TrimPrefix(res.Container.Name, "/") != name {
		// The daemon resolved the name as the prefix of an ID.
		return nil, nil //nolint:nilnil // see above
	}

	return &res.Container, nil
}

// referenceOf returns the image reference of the container or of what an
// interrupted Recreate left of it; "" when there is none.
func (r *dockerRuntime) referenceOf(ctx context.Context, name string) (string, error) {
	for _, candidate := range []string{name, name + replacedSuffix, name + retiredSuffix} {
		ctr, err := r.lookup(ctx, candidate)
		if err != nil {
			return "", err
		}

		if ctr != nil && ctr.Config != nil {
			return ctr.Config.Image, nil
		}
	}

	return "", nil
}

// verifyDigest returns the local image ref names if digest is one of its
// registry digests, ErrDigestMismatch otherwise.
func (r *dockerRuntime) verifyDigest(
	ctx context.Context,
	ref, digest string,
) (*client.ImageInspectResult, error) {
	img, err := r.inspectImage(ctx, ref)
	if err != nil {
		return nil, err
	}

	if img == nil {
		return nil, fmt.Errorf("%w: there is no local image %s", ErrDigestMismatch, ref)
	}

	if !hasDigest(ref, img.RepoDigests, digest) {
		local := r.digestOf(ref, img)
		if local == "" {
			local = "an image without registry digest"
		}

		return nil, fmt.Errorf("%w: %s is %s, wanted %q",
			ErrDigestMismatch, ref, local, digest)
	}

	return img, nil
}

// planRecreate inspects the container and its images, verifies the digest
// and derives the create request. It changes nothing.
func (r *dockerRuntime) planRecreate(
	ctx context.Context, name, digest string, change *Change,
) (*recreatePlan, error) {
	res, err := r.engine.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect: %w", err)
	}

	old := &res.Container

	if strings.TrimPrefix(old.Name, "/") != name {
		return nil, fmt.Errorf("%q is not a container name (it resolves to %s)", name, old.Name)
	}

	if old.Config == nil || old.HostConfig == nil {
		return nil, errors.New("incomplete container inspect data")
	}

	ref := cmp.Or(change.Image, old.Config.Image)

	newImage, err := r.verifyDigest(ctx, ref, digest)
	if err != nil {
		return nil, err
	}

	oldImage, err := r.inspectImage(ctx, old.Image)
	if err != nil {
		return nil, err
	}

	if oldImage == nil {
		// Without the old image's defaults the container's own settings
		// cannot be told apart; refuse rather than guess.
		return nil, fmt.Errorf("image %s of the container is gone, "+
			"its configuration cannot be separated from the image defaults", old.Image)
	}

	spec, err := buildCreateSpec(old, oldImage.Config)
	if err != nil {
		return nil, err
	}

	spec.apply(change)

	return &recreatePlan{
		name:       name,
		old:        old,
		spec:       spec,
		imageID:    newImage.ID,
		wasRunning: isUp(old),
	}, nil
}

// replace runs a plan: stop, rename, create, connect, retire the old
// container, start, remove. Any failure rolls back to the old container.
func (r *dockerRuntime) replace(
	ctx context.Context,
	plan *recreatePlan,
	stopTimeout time.Duration,
	start bool,
) error {
	log := r.log.WithFields(logrus.Fields{
		"container": plan.name,
		"image":     plan.spec.Config.Image,
	})

	if err := r.stop(ctx, plan.old.ID, stopTimeout); err != nil {
		return r.rollback(ctx, plan, "", stageStopped, fmt.Errorf("stop: %w", err))
	}

	_, err := r.engine.ContainerRename(ctx, plan.old.ID, client.ContainerRenameOptions{
		NewName: plan.name + replacedSuffix,
	})
	if err != nil {
		return r.rollback(ctx, plan, "", stageStopped, fmt.Errorf("rename old container: %w", err))
	}

	newID, err := r.create(ctx, plan, log)
	if err != nil {
		return r.rollback(ctx, plan, newID, stageReplaced, err)
	}

	// Renaming the old container records that the replacement is complete:
	// an interruption from here on is resolved in its favour.
	_, err = r.engine.ContainerRename(ctx, plan.old.ID, client.ContainerRenameOptions{
		NewName: plan.name + retiredSuffix,
	})
	if err != nil {
		return r.rollback(ctx, plan, newID, stageReplaced, fmt.Errorf("retire old container: %w", err))
	}

	if start {
		if _, err := r.engine.ContainerStart(ctx, newID, client.ContainerStartOptions{}); err != nil {
			return r.rollback(ctx, plan, newID, stageRetired, fmt.Errorf("start new container: %w", err))
		}
	}

	log = log.WithFields(logrus.Fields{
		"old_id":  plan.old.ID,
		"new_id":  newID,
		"started": start,
	})

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	if err := r.removeRetired(cleanupCtx, plan.old.ID); err != nil {
		// The next Recreate, Start or Stop of this name removes the leftover.
		log.WithError(err).Error("Container recreated, but the replaced container " +
			plan.name + retiredSuffix + " cannot be removed yet")

		return nil
	}

	log.Info("Container recreated")

	return nil
}

// create creates the replacement and attaches its networks; it returns the
// new ID once the container exists, also on failure.
func (r *dockerRuntime) create(
	ctx context.Context,
	plan *recreatePlan,
	log logrus.FieldLogger,
) (string, error) {
	spec := plan.spec
	options := client.ContainerCreateOptions{
		Name:       spec.Name,
		Config:     spec.Config,
		HostConfig: spec.HostConfig,
	}

	if spec.Primary != "" {
		options.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				spec.Primary: spec.PrimaryEndpoint,
			},
		}
	}

	created, err := r.engine.ContainerCreate(ctx, options)
	if err != nil {
		return "", fmt.Errorf("create new container: %w", err)
	}

	for _, warning := range created.Warnings {
		log.WithField("warning", warning).Warn("Daemon warning on container create")
	}

	// The tag may have moved between the digest check and the create.
	res, err := r.engine.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		return created.ID, fmt.Errorf("inspect new container: %w", err)
	}

	if res.Container.Image != plan.imageID {
		return created.ID, fmt.Errorf("%w: the new container is on image %s, verified was %s",
			ErrDigestMismatch, res.Container.Image, plan.imageID)
	}

	// Further networks are connected one by one: API versions before 1.44
	// accept a single endpoint in the create request.
	for _, extra := range spec.Extra {
		_, err := r.engine.NetworkConnect(ctx, extra.Network, client.NetworkConnectOptions{
			Container:      created.ID,
			EndpointConfig: extra.Endpoint,
		})
		if err != nil {
			return created.ID, fmt.Errorf("connect network %s: %w", extra.Network, err)
		}
	}

	return created.ID, nil
}

// rollback undoes a failed replacement and returns cause, extended by what
// could not be undone.
func (r *dockerRuntime) rollback(
	ctx context.Context,
	plan *recreatePlan,
	newID string,
	stage recreateStage,
	cause error,
) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	log := r.log.WithField("container", plan.name)
	log.WithError(cause).Warn("Recreate failed, rolling back to the old container")

	var errs []error

	if stage == stageRetired {
		// The replacement loses its standing first, in case the rollback is
		// interrupted or cannot remove it.
		_, err := r.engine.ContainerRename(ctx, plan.old.ID, client.ContainerRenameOptions{
			NewName: plan.name + replacedSuffix,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("rename old container: %w", err))
		}
	}

	if newID != "" {
		_, err := r.engine.ContainerRemove(ctx, newID, discardReplacement)
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove new container: %w", err))
		}
	}

	if stage != stageStopped {
		_, err := r.engine.ContainerRename(ctx, plan.old.ID, client.ContainerRenameOptions{
			NewName: plan.name,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("rename old container back: %w", err))
		}
	}

	if plan.wasRunning {
		_, err := r.engine.ContainerStart(ctx, plan.old.ID, client.ContainerStartOptions{})
		if err != nil {
			errs = append(errs, fmt.Errorf("start old container: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%w (rollback incomplete: %w)", cause, errors.Join(errs...))
	}

	log.Info("Rolled back to the old container")

	return cause
}

// removeRetired removes a replaced container but not its volumes, which the
// replacement uses.
func (r *dockerRuntime) removeRetired(ctx context.Context, id string) error {
	var err error

	for range 2 {
		_, err = r.engine.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
		if err == nil || cerrdefs.IsNotFound(err) {
			return nil
		}
	}

	// Keep the daemon from starting it next to its replacement after a restart.
	_, uerr := r.engine.ContainerUpdate(ctx, id, client.ContainerUpdateOptions{
		RestartPolicy: &container.RestartPolicy{Name: container.RestartPolicyDisabled},
	})
	if uerr != nil {
		r.log.WithError(uerr).WithField("container_id", id).
			Error("Cannot disable the restart policy of the replaced container")
	}

	return fmt.Errorf("%w: %w", errLeftover, err)
}

// isUp reports whether the container has a process (running, paused or
// about to be restarted by the daemon).
func isUp(ctr *container.InspectResponse) bool {
	return ctr.State != nil && (ctr.State.Running || ctr.State.Restarting)
}

// stopGracefully stops a container with its own stop timeout.
func (r *dockerRuntime) stopGracefully(ctx context.Context, ctr *container.InspectResponse) {
	if !isUp(ctr) {
		return
	}

	if _, err := r.engine.ContainerStop(ctx, ctr.ID, client.ContainerStopOptions{}); err != nil {
		r.log.WithError(err).WithField("container", strings.TrimPrefix(ctr.Name, "/")).
			Warn("Cannot stop the container gracefully")
	}
}

// recoverInterrupted resolves a Recreate that did not finish, by the old
// container's suffix: next to retiredSuffix the replacement stays and the old
// one is removed; next to replacedSuffix the replacement is removed and the
// old one gets its name back, started again with restart if the replacement
// was running.
func (r *dockerRuntime) recoverInterrupted(ctx context.Context, name string, restart bool) error {
	if err := r.finishRetired(ctx, name); err != nil {
		return err
	}

	return r.restoreReplaced(ctx, name, restart)
}

// finishRetired removes the old container of a complete replacement.
func (r *dockerRuntime) finishRetired(ctx context.Context, name string) error {
	retired, err := r.lookup(ctx, name+retiredSuffix)
	if err != nil || retired == nil {
		return err
	}

	current, err := r.lookup(ctx, name)
	if err != nil {
		return err
	}

	log := r.log.WithField("container", name)

	if current == nil {
		// The replacement is gone (by hand, or an unfinished rollback).
		log.Warn("Found a replaced container without its replacement, restoring it " +
			"(its restart policy may have been disabled)")

		_, err = r.engine.ContainerRename(ctx, retired.ID, client.ContainerRenameOptions{NewName: name})
		if err != nil {
			return fmt.Errorf("restore replaced container: %w", err)
		}

		return nil
	}

	log.Info("Removing the container left over from an earlier recreate")

	// A daemon restart may have brought it up again.
	r.stopGracefully(ctx, retired)

	if err := r.removeRetired(ctx, retired.ID); err != nil {
		return fmt.Errorf("%s: %w", name+retiredSuffix, err)
	}

	return nil
}

// restoreReplaced puts the old container of an unfinished replacement back
// under its name.
func (r *dockerRuntime) restoreReplaced(ctx context.Context, name string, restart bool) error {
	old, err := r.lookup(ctx, name+replacedSuffix)
	if err != nil || old == nil {
		return err
	}

	unfinished, err := r.lookup(ctx, name)
	if err != nil {
		return err
	}

	log := r.log.WithField("container", name)
	log.Warn("Found an interrupted recreate, restoring the old container")

	wasUp := false

	if unfinished != nil {
		// Never started by Recreate, but the daemon may have started it.
		wasUp = isUp(unfinished)
		r.stopGracefully(ctx, unfinished)

		_, err = r.engine.ContainerRemove(ctx, unfinished.ID, discardReplacement)
		if err != nil && !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("remove unfinished replacement: %w", err)
		}
	}

	_, err = r.engine.ContainerRename(ctx, old.ID, client.ContainerRenameOptions{NewName: name})
	if err != nil {
		return fmt.Errorf("restore interrupted recreate: %w", err)
	}

	if restart && wasUp && !isUp(old) {
		if _, err := r.engine.ContainerStart(ctx, old.ID, client.ContainerStartOptions{}); err != nil {
			return fmt.Errorf("start restored container: %w", err)
		}
	}

	return nil
}

// recoverFor recovers an interrupted Recreate before Start and Stop; a
// leftover that cannot be removed does not fail them.
func (r *dockerRuntime) recoverFor(ctx context.Context, name string) error {
	r.recreateMu.Lock()
	defer r.recreateMu.Unlock()

	err := r.recoverInterrupted(ctx, name, false)
	if errors.Is(err, errLeftover) {
		r.log.WithError(err).WithField("container", name).
			Error("Cannot remove the container left over from an earlier recreate")

		return nil
	}

	if err != nil {
		return fmt.Errorf("container %s: %w", name, err)
	}

	return nil
}
