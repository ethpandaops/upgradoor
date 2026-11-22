// Package docker lists containers, describes and pulls images, and recreates
// a container with the same configuration through the Docker Engine API. It
// decides nothing, imports no other agent package and is the only one that
// imports the Docker client.
//
// A recreate creates no container that does not replace one of the same
// name, changes only image, command and labels, and stops nothing unless the
// local image has the granted digest.
package docker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// defaultHost is the daemon address when agent.yaml names none.
const defaultHost = "unix:///var/run/docker.sock"

// Config is the docker section of agent.yaml.
type Config struct {
	// Host is the daemon address.
	Host string `yaml:"host"`
}

// ApplyDefaults sets the default daemon address when none is given.
func (c *Config) ApplyDefaults() {
	if c.Host == "" {
		c.Host = defaultHost
	}
}

// ErrDigestMismatch is returned by Runtime.Recreate, before anything is
// stopped, when the local image does not have the granted digest.
var ErrDigestMismatch = errors.New("host: local image digest differs from the granted digest")

// ErrImageMoved is returned by Runtime.Recreate when the tag moved to
// another image between the digest check and the create, after the old
// container was stopped; the rollback has run.
var ErrImageMoved = errors.New("host: the image of the tag changed after the old container was stopped")

// Runtime is the Docker daemon.
type Runtime interface {
	// Containers lists all containers with the raw Docker facts; policy
	// fields are left to plan.Classify and recreate leftovers are not listed.
	Containers(ctx context.Context) ([]protocol.Container, error)
	// LocalImage returns the registry digest ("" if none) and revision label
	// of the local image ref names.
	LocalImage(ctx context.Context, ref string) (digest, revision string, err error)
	// Pull pulls ref by its tag and returns what LocalImage then would.
	Pull(ctx context.Context, ref string) (digest, revision string, err error)
	// Recreate replaces container name by one with the same configuration
	// and change applied, left stopped unless start. It fails with
	// ErrDigestMismatch before stopping anything unless digest is a registry
	// digest of the image, with ErrImageMoved when the tag moved after the
	// stop, rolls back on failure and resolves a recreate an earlier run left
	// half done.
	Recreate(
		ctx context.Context, name, digest string, stopTimeout time.Duration, start bool, change *Change,
	) error
	// Stop stops a container, waiting up to timeout before killing it. A
	// container that is not running is not an error.
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Start starts a container. A running container is not an error.
	Start(ctx context.Context, name string) error
	// Close releases the Docker client.
	Close() error
}

// Change is what a recreate changes about a container; the zero Change
// changes nothing.
type Change struct {
	// Image is the replacement's image reference, against which the digest
	// is verified; empty keeps the container's own.
	Image string
	// Command replaces the arguments after the entrypoint when SetCommand.
	Command    []string
	SetCommand bool
	// Base, when SetBase, replaces the override labels; nil removes them.
	Base    *protocol.OverrideBase
	SetBase bool
	// Group and Kind, when Group is set, are written as the group labels.
	Group string
	Kind  protocol.ContainerKind
}

// Login returns the pull credentials for a registry host; ok false pulls
// anonymously.
type Login func(registryHost string) (username, password string, ok bool)

// New returns the runtime for the daemon at host without connecting.
func New(log logrus.FieldLogger, host string, login Login) (Runtime, error) {
	r, err := newDockerRuntime(log, host, login)
	if err != nil {
		return nil, err
	}

	return r, nil
}

var _ Runtime = (*dockerRuntime)(nil)

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
	Close() error
}

// dockerRuntime is the Runtime against the Docker Engine API; a replacement is
// what Ansible's docker_container would create from the new image. Limits: a
// value equal to the old image's default follows the new image, re-attached
// anonymous volumes count as named, a fixed MAC address is not kept.
type dockerRuntime struct {
	log    logrus.FieldLogger
	engine engine
	login  Login

	// recreateMu serialises Recreate and the recovery of interrupted ones.
	recreateMu sync.Mutex

	// pulled is the digest the last Pull saw per repository and image ID;
	// it breaks ties between several digests of one image.
	pulledMu sync.Mutex
	pulled   map[string]string
}

// newDockerRuntime returns the runtime without connecting; the API version
// is negotiated on the first request.
func newDockerRuntime(log logrus.FieldLogger, host string, login Login) (*dockerRuntime, error) {
	if host == "" {
		return nil, errors.New("docker: daemon host is required")
	}

	cli, err := client.New(client.WithHost(host), client.WithUserAgent("upgradoor-agent"))
	if err != nil {
		return nil, fmt.Errorf("docker: create client for %s: %w", host, err)
	}

	r := &dockerRuntime{
		log:    log.WithField("package", "docker"),
		engine: cli,
		login:  login,
		pulled: make(map[string]string, 8),
	}

	return r, nil
}

// Close releases the connection to the daemon.
func (r *dockerRuntime) Close() error {
	if err := r.engine.Close(); err != nil {
		return fmt.Errorf("close docker client: %w", err)
	}

	return nil
}

// Containers implements Runtime.
func (r *dockerRuntime) Containers(ctx context.Context) ([]protocol.Container, error) {
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

// withoutLeftovers drops the old container of a recreate while its
// replacement exists.
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
func (r *dockerRuntime) describe(
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
		out.Command = slices.Clone(ctr.Config.Cmd)
		out.Base = baseFromLabels(ctr.Config.Labels)

		// Raw; plan.Classify interprets them.
		if group := ctr.Config.Labels[protocol.LabelGroup]; group != "" {
			out.Group = group
			out.Kind = protocol.ContainerKind(ctr.Config.Labels[protocol.LabelKind])
		}

		if ctr.Config.StopTimeout != nil && *ctr.Config.StopTimeout > 0 {
			out.StopTimeout = time.Duration(*ctr.Config.StopTimeout) * time.Second
		}
	}

	out.Linked = ctr.HostConfig != nil && hasLinks(ctr)

	if ctr.State != nil {
		out.State = string(ctr.State.Status)
		out.StartedAt = parseTime(ctr.State.StartedAt)

		if !ctr.State.Running && !ctr.State.Restarting {
			out.ExitCode = ctr.State.ExitCode
		}

		if ctr.State.Health != nil {
			out.Health = string(ctr.State.Health.Status)
		}
	}

	if img != nil {
		out.Digest = r.digestOf(out.Image, img)

		out.Revision = revisionOf(img)

		if img.Config != nil {
			out.Version = img.Config.Labels[labelImageVersion]
		}
	}

	return out
}

// parseTime parses a daemon timestamp; zero or unparsable gives the zero time.
func parseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || t.Year() <= 1 {
		return time.Time{}
	}

	return t.UTC()
}

// inspectImage inspects an image by reference or ID; nil when it is missing.
func (r *dockerRuntime) inspectImage(ctx context.Context, ref string) (*client.ImageInspectResult, error) {
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
func (r *dockerRuntime) digestOf(ref string, img *client.ImageInspectResult) string {
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

	digest := selectDigest(ref, img.RepoDigests, pulled, descriptor)
	if digest == "" && len(img.RepoTags) == 0 {
		// The containerd image store drops all names of an image whose tag
		// moved on; its descriptor is still the manifest it was pulled as.
		return descriptor
	}

	return digest
}

func pulledKey(repo, imageID string) string { return repo + "\x00" + imageID }

// LocalImage implements Runtime.
func (r *dockerRuntime) LocalImage(ctx context.Context, ref string) (digest, revision string, err error) {
	if _, err = repositoryOf(ref); err != nil {
		return "", "", err
	}

	img, err := r.inspectImage(ctx, ref)
	if err != nil || img == nil {
		return "", "", err
	}

	return r.digestOf(ref, img), revisionOf(img), nil
}

// revisionOf returns the revision label of an image, "" when it has none.
func revisionOf(img *client.ImageInspectResult) string {
	if img.Config == nil {
		return ""
	}

	return img.Config.Labels[labelImageRevision]
}

// Stop implements Runtime.
func (r *dockerRuntime) Stop(ctx context.Context, name string, timeout time.Duration) error {
	if err := r.recoverFor(ctx, name); err != nil {
		return err
	}

	if err := r.stop(ctx, name, timeout); err != nil {
		return fmt.Errorf("stop container %s: %w", name, err)
	}

	r.log.WithField("container", name).Info("Container stopped")

	return nil
}

// Start implements Runtime.
func (r *dockerRuntime) Start(ctx context.Context, name string) error {
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

// stop stops a container, killing it after timeout; a stopped container is
// not an error.
func (r *dockerRuntime) stop(ctx context.Context, id string, timeout time.Duration) error {
	seconds := 0
	if timeout > 0 {
		seconds = int(math.Ceil(timeout.Seconds()))
	}

	_, err := r.engine.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &seconds})

	return err
}
