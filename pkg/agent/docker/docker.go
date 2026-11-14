// Package docker implements agent.ContainerRuntime against the Docker Engine
// API: it observes containers, pulls images and replaces a container by one
// with the same configuration on the image its tag now names.
//
// # Digests
//
// A digest is the registry manifest digest recorded in the image's
// RepoDigests for the repository of the reference in question (the index
// digest for multi-arch tags). References are compared by repository after
// normalisation, so "alpine", "docker.io/library/alpine:3" and
// "index.docker.io/library/alpine" are the same repository. One image can
// carry several digests of one repository (the same content reached through
// different manifests). Reporting then prefers the digest the last Pull of
// this process saw for the tag, then the daemon's own descriptor of the
// image, then the first entry; Recreate accepts the lease digest when it is
// any of them, which is exact because a digest names immutable content.
//
// # Recreate
//
// Recreate verifies the digest, then stops the container, renames it to
// "<name>_upgradoor_old", creates the replacement under the original name
// and connects its further networks. The replacement is then complete,
// which the next rename of the old container, to "<name>_upgradoor_done",
// records. Then the replacement is started and the old container removed
// (never its volumes). If anything fails after the stop, the replacement is
// removed, the old container gets its name back and is started again if it
// was running, and the original error is returned.
//
// The digest check comes before everything else: a refused Recreate changes
// nothing. After it, and at the beginning of Start and Stop, the leftovers
// of an interrupted Recreate (a crash or a power loss) are resolved by the
// name of the old container. Next to "<name>_upgradoor_old" whatever
// carries the name is unfinished: it is stopped with its own stop timeout
// should the daemon have started it, removed, and the old container gets
// its name back (Recreate also starts it again if the unfinished one was
// running). Next to "<name>_upgradoor_done" the replacement is the
// container and is never touched; the old one is removed. If that removal
// fails, at the end of a Recreate or later, the Recreate still succeeded:
// the failure is logged, the leftover's restart policy is disabled so the
// daemon does not start it next to its replacement, the removal is tried
// again by the next Recreate, Start or Stop of that name, and Observe does
// not report the leftover while its replacement exists.
//
// The replacement is created from the old container's inspect output. The
// host configuration is passed on as it is. The container configuration is
// first reduced to what was given for the container: Docker copies image
// defaults (Env, Cmd, Entrypoint, Labels, ExposedPorts, Volumes, WorkingDir,
// User, Healthcheck, StopSignal, Shell) into every container, and a value
// that merely equals the old image's default must not be pinned, or a new
// image's changed default would be masked. The result is the container that
// "docker run" or Ansible's docker_container would have created from the
// new image with the original arguments; a run of
// community.docker.docker_container against it reports no change.
//
// Known limits, all following from what the Engine API reports:
//
//   - A value that was given explicitly but equals the old image's default
//     cannot be told from an inherited one and follows the new image. If
//     the new image changes that default, a later Ansible run sees a
//     difference and recreates the container with the explicit value.
//   - Anonymous volumes are re-attached by name through HostConfig.Mounts
//     so their data survives (Ansible itself would start with empty ones).
//     From then on Docker treats them as named: "docker rm -v" keeps them.
//   - A fixed MAC address is kept for the network the container was
//     created on (Docker 25 or newer). Fixed MAC addresses of networks
//     connected later are not reported by the daemon and are lost.
//   - The log driver, IPC mode, cgroup namespace mode, runtime and the
//     masked and read-only paths are reported with the daemon defaults
//     filled in and are passed on as such.
//   - Containers created with auto-remove are refused.
//   - Credential helpers (credsStore, credHelpers) are not run; only
//     credentials stored in the "auths" section of the Docker config file
//     are used for pulls.
package docker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

var _ agent.ContainerRuntime = (*Runtime)(nil)

// legacyInspectVersion is the newest API version whose container inspect
// still has Config.MacAddress, the only place where the daemon tells a
// user-specified MAC address from a generated one.
const legacyInspectVersion = "1.51"

// engine is the part of the Docker Engine API client the runtime uses.
type engine interface {
	ContainerList(
		ctx context.Context, options client.ContainerListOptions,
	) (client.ContainerListResult, error)
	ContainerInspect(
		ctx context.Context, containerID string, options client.ContainerInspectOptions,
	) (client.ContainerInspectResult, error)
	ContainerCreate(
		ctx context.Context, options client.ContainerCreateOptions,
	) (client.ContainerCreateResult, error)
	ContainerStart(
		ctx context.Context, containerID string, options client.ContainerStartOptions,
	) (client.ContainerStartResult, error)
	ContainerStop(
		ctx context.Context, containerID string, options client.ContainerStopOptions,
	) (client.ContainerStopResult, error)
	ContainerRename(
		ctx context.Context, containerID string, options client.ContainerRenameOptions,
	) (client.ContainerRenameResult, error)
	ContainerRemove(
		ctx context.Context, containerID string, options client.ContainerRemoveOptions,
	) (client.ContainerRemoveResult, error)
	ContainerUpdate(
		ctx context.Context, containerID string, options client.ContainerUpdateOptions,
	) (client.ContainerUpdateResult, error)
	ImageInspect(
		ctx context.Context, imageID string, options ...client.ImageInspectOption,
	) (client.ImageInspectResult, error)
	ImagePull(
		ctx context.Context, ref string, options client.ImagePullOptions,
	) (client.ImagePullResponse, error)
	NetworkConnect(
		ctx context.Context, networkID string, options client.NetworkConnectOptions,
	) (client.NetworkConnectResult, error)
	ClientVersion() string
	Close() error
}

// Runtime is the Docker implementation of agent.ContainerRuntime.
type Runtime struct {
	log    logrus.FieldLogger
	engine engine
	// legacy speaks legacyInspectVersion; nil when unavailable.
	legacy   engine
	authFile string

	// recreateMu serialises Recreate and the recovery of interrupted ones.
	recreateMu sync.Mutex

	// pulled remembers, per repository and image ID, the digest the last
	// Pull saw the tag at. It only breaks ties between several digests of
	// one image; see the package comment.
	pulledMu sync.Mutex
	pulled   map[string]string
}

// Option configures a Runtime.
type Option func(*Runtime)

// WithAuthFile sets the Docker config file (config.json) that holds the
// registry credentials used for pulls. A missing file means anonymous
// pulls. Without this option the file is $DOCKER_CONFIG/config.json or
// ~/.docker/config.json.
func WithAuthFile(path string) Option {
	return func(r *Runtime) {
		if path != "" {
			r.authFile = path
		}
	}
}

// New returns a Runtime for the daemon at host (for example
// unix:///var/run/docker.sock). It does not connect; the API version is
// negotiated with the daemon on the first request.
func New(log logrus.FieldLogger, host string, opts ...Option) (*Runtime, error) {
	if log == nil {
		return nil, errors.New("docker: logger is required")
	}

	if host == "" {
		return nil, errors.New("docker: daemon host is required")
	}

	cli, err := client.New(client.WithHost(host), client.WithUserAgent("upgradoor-agent"))
	if err != nil {
		return nil, fmt.Errorf("docker: create client for %s: %w", host, err)
	}

	r := &Runtime{
		log:      log.WithField("package", "docker"),
		engine:   cli,
		authFile: defaultAuthFile(),
		pulled:   make(map[string]string, 8),
	}

	legacy, err := client.New(
		client.WithHost(host),
		client.WithUserAgent("upgradoor-agent"),
		client.WithAPIVersion(legacyInspectVersion),
	)
	if err != nil {
		r.log.WithError(err).Warn("No legacy API client, fixed MAC addresses will not be kept")
	} else {
		r.legacy = legacy
	}

	for _, opt := range opts {
		opt(r)
	}

	return r, nil
}

// Close releases the connections to the daemon.
func (r *Runtime) Close() error {
	var errs []error

	if err := r.engine.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close docker client: %w", err))
	}

	if r.legacy != nil {
		if err := r.legacy.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close legacy docker client: %w", err))
		}
	}

	return errors.Join(errs...)
}

// Observe implements agent.ContainerRuntime.
func (r *Runtime) Observe(ctx context.Context) ([]protocol.Container, error) {
	list, err := r.engine.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]protocol.Container, 0, len(list.Items))
	images := make(map[string]*client.ImageInspectResult, len(list.Items))

	for i := range list.Items {
		res, err := r.engine.ContainerInspect(ctx, list.Items[i].ID, client.ContainerInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				// Removed between the list and the inspect.
				continue
			}

			return nil, fmt.Errorf("inspect container %s: %w", list.Items[i].ID, err)
		}

		ctr := &res.Container

		img, known := images[ctr.Image]
		if !known {
			img, err = r.inspectImage(ctx, ctr.Image)
			if err != nil {
				return nil, err
			}

			images[ctr.Image] = img
		}

		out = append(out, r.describe(ctr, img))
	}

	out = withoutLeftovers(out)

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, nil
}

// withoutLeftovers drops the old container of a Recreate that is under way
// or could not remove it yet, as long as its replacement is there: it is
// not a container of the host's configuration.
func withoutLeftovers(list []protocol.Container) []protocol.Container {
	names := make(map[string]struct{}, len(list))
	for i := range list {
		names[list[i].Name] = struct{}{}
	}

	out := list[:0]

	for i := range list {
		leftover := false

		for _, suffix := range []string{replacedSuffix, retiredSuffix} {
			if base, ok := strings.CutSuffix(list[i].Name, suffix); ok {
				_, leftover = names[base]
			}
		}

		if !leftover {
			out = append(out, list[i])
		}
	}

	return out
}

// describe converts inspect data to the protocol type. img is nil when the
// container's image is gone.
func (r *Runtime) describe(
	ctr *container.InspectResponse,
	img *client.ImageInspectResult,
) protocol.Container {
	out := protocol.Container{
		Name:         strings.TrimPrefix(ctr.Name, "/"),
		ID:           ctr.ID,
		ImageID:      ctr.Image,
		RestartCount: ctr.RestartCount,
	}

	if ctr.Config != nil {
		out.Image = ctr.Config.Image
	}

	if ctr.State != nil {
		out.State = string(ctr.State.Status)
		out.StartedAt = parseTime(ctr.State.StartedAt)

		if ctr.State.Health != nil {
			out.Health = string(ctr.State.Health.Status)
		}
	}

	if img != nil {
		out.Digest = r.digestOf(out.Image, img)

		if img.Config != nil {
			out.Revision = img.Config.Labels[labelImageRevision]
			out.Version = img.Config.Labels[labelImageVersion]
		}
	}

	return out
}

// parseTime parses a daemon timestamp; the daemon's zero value and anything
// unparsable give the zero time.
func parseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}

	return t.UTC()
}

// inspectImage inspects an image by reference or ID; a missing image gives
// nil without an error.
func (r *Runtime) inspectImage(ctx context.Context, ref string) (*client.ImageInspectResult, error) {
	if ref == "" {
		return nil, nil //nolint:nilnil // "no such image" is a regular answer here, not an error
	}

	img, err := r.engine.ImageInspect(ctx, ref)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, nil //nolint:nilnil // see above
		}

		return nil, fmt.Errorf("inspect image %s: %w", ref, err)
	}

	return &img, nil
}

// digestOf returns the registry digest of img for the repository of ref.
func (r *Runtime) digestOf(ref string, img *client.ImageInspectResult) string {
	repo, err := repositoryOf(ref)
	if err != nil {
		return ""
	}

	var descriptor string
	if img.Descriptor != nil {
		descriptor = img.Descriptor.Digest.String()
	}

	r.pulledMu.Lock()
	pulled := r.pulled[pulledKey(repo, img.ID)]
	r.pulledMu.Unlock()

	return selectDigest(ref, img.RepoDigests, pulled, descriptor)
}

func pulledKey(repo, imageID string) string { return repo + "\x00" + imageID }

// LocalDigest implements agent.ContainerRuntime.
func (r *Runtime) LocalDigest(ctx context.Context, ref string) (string, error) {
	if _, err := repositoryOf(ref); err != nil {
		return "", err
	}

	img, err := r.inspectImage(ctx, ref)
	if err != nil || img == nil {
		return "", err
	}

	return r.digestOf(ref, img), nil
}

// Stop implements agent.ContainerRuntime.
func (r *Runtime) Stop(ctx context.Context, name string, timeout time.Duration) error {
	if err := r.recoverFor(ctx, name); err != nil {
		return err
	}

	if err := r.stop(ctx, name, timeout); err != nil {
		return fmt.Errorf("stop container %s: %w", name, err)
	}

	r.log.WithField("container", name).Info("Container stopped")

	return nil
}

// Start implements agent.ContainerRuntime.
func (r *Runtime) Start(ctx context.Context, name string) error {
	if err := r.recoverFor(ctx, name); err != nil {
		return err
	}

	// The daemon answers "not modified" for a running container, which the
	// client does not treat as an error.
	if _, err := r.engine.ContainerStart(ctx, name, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start container %s: %w", name, err)
	}

	r.log.WithField("container", name).Info("Container started")

	return nil
}

// stop stops a container, waiting up to timeout before it is killed. A
// container that is not running is not an error (the daemon answers "not
// modified").
func (r *Runtime) stop(ctx context.Context, id string, timeout time.Duration) error {
	seconds := 0
	if timeout > 0 {
		seconds = int(math.Ceil(timeout.Seconds()))
	}

	_, err := r.engine.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &seconds})

	return err
}
