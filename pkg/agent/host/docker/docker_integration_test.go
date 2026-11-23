//go:build docker_integration

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Everything the integration tests create is named "upgradoor-it-<run>-..."
// (containers, networks, volumes, image repositories) or labelled
// "upgradoor.test=<run>" (images), and nothing else is ever touched.
const (
	itPrefix     = "upgradoor-it-"
	itLabel      = "upgradoor.test"
	baseImage    = "alpine:3.21"
	registryRef  = "registry:2"
	staleAfter   = time.Hour
	setupTimeout = 5 * time.Minute

	// loopScript keeps a container alive and lets it stop at once on TERM.
	loopScript = "trap 'exit 0' TERM; while :; do sleep 0.2; done"
)

// itEnv is the shared environment: a Docker client and a throwaway registry.
type itEnv struct {
	cli  *client.Client
	host string
	run  string
	// prefix is itPrefix plus the run ID.
	prefix string
	// registry is "127.0.0.1:<port>"; Docker talks plain HTTP to loopback.
	registry string
	stage    atomic.Int64
}

var env *itEnv

func TestMain(m *testing.M) {
	os.Exit(runIntegration(m))
}

func runIntegration(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	e, err := newEnv(ctx)
	if e != nil {
		defer e.close()
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "docker integration setup: %v\n", err)

		return 1
	}

	env = e

	return m.Run()
}

func newEnv(ctx context.Context) (*itEnv, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}

	cli, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, err
	}

	if _, err := cli.Ping(ctx, client.PingOptions{}); err != nil {
		return nil, fmt.Errorf("no Docker daemon at %s: %w", host, err)
	}

	id := make([]byte, 4)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}

	e := &itEnv{cli: cli, host: host, run: hex.EncodeToString(id)}
	e.prefix = itPrefix + e.run + "-"

	// Leftovers of runs that died without cleaning up.
	e.sweep(ctx, itPrefix, itLabel, staleAfter)

	for _, ref := range []string{baseImage, registryRef} {
		if err := e.ensureImage(ctx, ref); err != nil {
			return e, err
		}
	}

	if err := e.startRegistry(ctx); err != nil {
		return e, err
	}

	return e, nil
}

// close removes everything this run created.
func (e *itEnv) close() {
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	e.sweep(ctx, e.prefix, itLabel+"="+e.run, 0)

	_ = e.cli.Close()
}

func (e *itEnv) ensureImage(ctx context.Context, ref string) error {
	if _, err := e.cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}

	resp, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}

	defer resp.Close()

	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}

	return nil
}

func (e *itEnv) startRegistry(ctx context.Context) error {
	port := network.MustParsePort("5000/tcp")

	created, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: e.prefix + "registry",
		Config: &container.Config{
			Image:        registryRef,
			ExposedPorts: network.PortSet{port: {}},
			Labels:       map[string]string{itLabel: e.run},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{port: {{HostIP: netip.MustParseAddr("127.0.0.1")}}},
		},
	})
	if err != nil {
		return fmt.Errorf("create registry: %w", err)
	}

	if _, err = e.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start registry: %w", err)
	}

	res, err := e.cli.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}

	bindings := res.Container.NetworkSettings.Ports[port]
	if len(bindings) == 0 {
		return errors.New("registry has no published port")
	}

	e.registry = net.JoinHostPort("127.0.0.1", bindings[0].HostPort)

	for {
		resp, err := e.registryDo(ctx, http.MethodGet, "/v2/", nil, nil)
		if err == nil {
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("registry did not come up: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (e *itEnv) registryDo(
	ctx context.Context, method, path string, header http.Header, body []byte,
) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+e.registry+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for key, values := range header {
		req.Header[key] = values
	}

	return http.DefaultClient.Do(req)
}

// sweep removes test resources with the prefix or image label older than minAge.
func (e *itEnv) sweep(ctx context.Context, prefix, imageLabel string, minAge time.Duration) {
	cutoff := time.Now().Add(-minAge)
	warn := func(what string, err error) {
		if err != nil && !cerrdefs.IsNotFound(err) {
			fmt.Fprintf(os.Stderr, "docker integration cleanup: %s: %v\n", what, err)
		}
	}

	containers, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	warn("list containers", err)

	var volumes []string

	for _, c := range containers.Items {
		if !hasNamePrefix(c.Names, prefix) || time.Unix(c.Created, 0).After(cutoff) {
			continue
		}

		for _, m := range c.Mounts {
			if m.Type == mount.TypeVolume && m.Name != "" {
				volumes = append(volumes, m.Name)
			}
		}

		_, rmErr := e.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		warn("remove container "+strings.Join(c.Names, ","), rmErr)
	}

	// Anonymous volumes Recreate re-attached survive their container.
	for _, name := range volumes {
		_, rmErr := e.cli.VolumeRemove(ctx, name, client.VolumeRemoveOptions{Force: true})
		warn("remove volume "+name, rmErr)
	}

	named, err := e.cli.VolumeList(ctx, client.VolumeListOptions{})
	warn("list volumes", err)

	for _, v := range named.Items {
		created, _ := time.Parse(time.RFC3339, v.CreatedAt)
		if strings.HasPrefix(v.Name, prefix) && !created.After(cutoff) {
			_, rmErr := e.cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true})
			warn("remove volume "+v.Name, rmErr)
		}
	}

	networks, err := e.cli.NetworkList(ctx, client.NetworkListOptions{})
	warn("list networks", err)

	for _, n := range networks.Items {
		if strings.HasPrefix(n.Name, prefix) && !n.Created.After(cutoff) {
			_, rmErr := e.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
			warn("remove network "+n.Name, rmErr)
		}
	}

	if imageLabel == "" {
		return
	}

	images, err := e.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("label", imageLabel),
	})
	warn("list images", err)

	for _, img := range images.Items {
		if time.Unix(img.Created, 0).After(cutoff) {
			continue
		}

		_, rmErr := e.cli.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{Force: true, PruneChildren: true})
		warn("remove image "+img.ID, rmErr)
	}
}

func hasNamePrefix(names []string, prefix string) bool {
	for _, name := range names {
		if strings.HasPrefix(strings.TrimPrefix(name, "/"), prefix) {
			return true
		}
	}

	return false
}

// imageVariant is a busybox test image; variants differ in what a container inherits.
type imageVariant struct {
	Revision string
	// Default is the value of the image's IMAGE_DEFAULT variable.
	Default string
	Labels  map[string]string
	Expose  []string
	Workdir string
	// HealthRetries is the retries value of the image healthcheck.
	HealthRetries int
	// Cmd is the image's default command (exec form).
	Cmd []string
	// Entrypoint is the image's entrypoint (exec form), if any.
	Entrypoint []string
}

var (
	variantOne = imageVariant{
		Revision: "rev1", Default: "one",
		Labels:  map[string]string{"image.only-one": "yes", "image.shared": "one"},
		Expose:  []string{"8545"},
		Workdir: "/one", HealthRetries: 4,
		Cmd: []string{"sh", "-c", loopScript + " # one"},
	}
	variantTwo = imageVariant{
		Revision: "rev2", Default: "two",
		Labels:  map[string]string{"image.only-two": "yes", "image.shared": "two"},
		Expose:  []string{"8545", "9000/udp"},
		Workdir: "/two", HealthRetries: 7,
		Cmd: []string{"sh", "-c", loopScript + " # two"},
	}
	// variantNoCommand needs a command of the container's own to be created.
	variantNoCommand = imageVariant{
		Revision: "nocommand", Default: "nocommand",
		Workdir: "/", HealthRetries: 1,
	}
	// variantEntryOne and variantEntryTwo have an entrypoint and an ignored default command.
	variantEntryOne = imageVariant{
		Revision: "entry1", Default: "one",
		Workdir: "/", HealthRetries: 1,
		Entrypoint: []string{"sh", "-c", loopScript, "--"},
		Cmd:        []string{"one"},
	}
	variantEntryTwo = imageVariant{
		Revision: "entry2", Default: "two",
		Workdir: "/", HealthRetries: 1,
		Entrypoint: []string{"sh", "-c", loopScript, "--"},
		Cmd:        []string{"two"},
	}
	// variantBroken cannot start: its command does not exist.
	variantBroken = imageVariant{
		Revision: "broken", Default: "broken",
		Workdir: "/", HealthRetries: 1,
		Cmd: []string{"/does/not/exist"},
	}
)

func (v imageVariant) dockerfile(run, repo string) string {
	cmd, _ := json.Marshal(v.Cmd)

	var b strings.Builder

	fmt.Fprintf(&b, "FROM %s\n", baseImage)
	// The repository label makes the image unique to one test.
	fmt.Fprintf(&b, "LABEL %s=%q upgradoor.repo=%q\n", itLabel, run, repo)
	fmt.Fprintf(&b, "LABEL %s=%q %s=%q\n", labelImageRevision, v.Revision, labelImageVersion, "1.0-"+v.Revision)

	for key, value := range v.Labels {
		fmt.Fprintf(&b, "LABEL %s=%q\n", key, value)
	}

	fmt.Fprintf(&b, "ENV IMAGE_DEFAULT=%s\nENV IMAGE_OVERRIDDEN=%s\n", v.Default, v.Default)

	for _, port := range v.Expose {
		fmt.Fprintf(&b, "EXPOSE %s\n", port)
	}

	fmt.Fprintf(&b, "WORKDIR %s\nVOLUME /imgvol\n", v.Workdir)
	fmt.Fprintf(&b, "HEALTHCHECK --interval=30s --retries=%d CMD true\n", v.HealthRetries)

	switch {
	case len(v.Cmd) == 0:
		b.WriteString("ENTRYPOINT []\nCMD []\n")
	case len(v.Entrypoint) > 0:
		entrypoint, _ := json.Marshal(v.Entrypoint)
		fmt.Fprintf(&b, "ENTRYPOINT %s\nCMD %s\n", entrypoint, cmd)
	default:
		fmt.Fprintf(&b, "CMD %s\n", cmd)
	}

	return b.String()
}

// repoRef returns a fresh image reference in the test registry.
func (e *itEnv) repoRef(name string) string {
	return fmt.Sprintf("%s/%s%s/app:devnet", e.registry, e.prefix, name)
}

// publish pushes a variant under ref's tag, leaving the local tag alone, and
// returns its manifest digest.
func (e *itEnv) publish(t *testing.T, ref string, variant imageVariant) string {
	t.Helper()

	ctx := t.Context()
	repo, tag, found := strings.Cut(ref[strings.Index(ref, "/")+1:], ":")
	require.True(t, found, "reference needs a tag")

	stage := fmt.Sprintf("%s/%s:stage-%d", e.registry, repo, e.stage.Add(1))

	// Build under a staging tag.
	var buildContext bytes.Buffer

	dockerfile := variant.dockerfile(e.run, repo)
	tw := tar.NewWriter(&buildContext)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))}))
	_, err := tw.Write([]byte(dockerfile))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	built, err := e.cli.ImageBuild(ctx, &buildContext, client.ImageBuildOptions{
		Tags:        []string{stage},
		Dockerfile:  "Dockerfile",
		Remove:      true,
		ForceRemove: true,
		Version:     build.BuilderV1,
	})
	require.NoError(t, err)
	require.NoError(t, drainStream(built.Body), "build")

	// Push the staging tag, then point the real tag at the same manifest.
	anonymous := base64.URLEncoding.EncodeToString([]byte("{}"))

	pushed, err := e.cli.ImagePush(ctx, stage, client.ImagePushOptions{RegistryAuth: anonymous})
	require.NoError(t, err)
	require.NoError(t, pushed.Wait(ctx), "push")
	require.NoError(t, pushed.Close())

	accept := http.Header{"Accept": {
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}}

	resp, err := e.registryDo(ctx, http.MethodGet,
		"/v2/"+repo+"/manifests/"+stage[strings.LastIndex(stage, ":")+1:], accept, nil)
	require.NoError(t, err)

	manifest, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode, string(manifest))

	digest := resp.Header.Get("Docker-Content-Digest")
	require.NotEmpty(t, digest)

	resp, err = e.registryDo(ctx, http.MethodPut, "/v2/"+repo+"/manifests/"+tag,
		http.Header{"Content-Type": {resp.Header.Get("Content-Type")}}, manifest)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, digest, resp.Header.Get("Docker-Content-Digest"))

	// Forget the image locally, so that the pull is a real one.
	_, err = e.cli.ImageRemove(ctx, stage, client.ImageRemoveOptions{Force: true, PruneChildren: true})
	require.NoError(t, err)

	return digest
}

// drainStream reads a build progress stream and returns its first error.
func drainStream(body io.ReadCloser) error {
	defer body.Close()

	dec := json.NewDecoder(body)

	for {
		var msg jsonstream.Message

		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		if msg.Error != nil {
			return msg.Error
		}
	}
}

// newRuntime returns the runtime under test, logging to the test log.
func (e *itEnv) newRuntime(t *testing.T) *dockerRuntime {
	t.Helper()

	log := logrus.New()
	log.SetOutput(testLogWriter{t})
	log.SetLevel(logrus.DebugLevel)

	r, err := newDockerRuntime(log, e.host, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })

	return r
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))

	return len(p), nil
}

// cleanup removes one test's resources when it ends.
func (e *itEnv) cleanup(t *testing.T, name string) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		// The images stay for the final sweep.
		e.sweep(ctx, e.prefix+name, "", 0)
	})
}

func (e *itEnv) inspect(t *testing.T, name string) container.InspectResponse {
	t.Helper()

	res, err := e.cli.ContainerInspect(t.Context(), name, client.ContainerInspectOptions{})
	require.NoError(t, err)

	return res.Container
}

func (e *itEnv) exists(t *testing.T, name string) bool {
	t.Helper()

	_, err := e.cli.ContainerInspect(t.Context(), name, client.ContainerInspectOptions{})
	if err != nil {
		require.True(t, cerrdefs.IsNotFound(err), "%v", err)

		return false
	}

	return true
}

// readFile returns the content of a file in a container.
func (e *itEnv) readFile(ctx context.Context, name, path string) (string, error) {
	res, err := e.cli.CopyFromContainer(ctx, name, client.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return "", err
	}

	defer res.Content.Close()

	tr := tar.NewReader(res.Content)
	if _, err = tr.Next(); err != nil {
		return "", err
	}

	content, err := io.ReadAll(tr)

	return strings.TrimSpace(string(content)), err
}

// waitFile waits for a file to appear in a container and returns it.
func (e *itEnv) waitFile(t *testing.T, name, path string) string {
	t.Helper()

	var content string

	require.Eventually(t, func() bool {
		got, err := e.readFile(t.Context(), name, path)
		content = got

		return err == nil && got != ""
	}, 30*time.Second, 100*time.Millisecond, "file %s in %s", path, name)

	return content
}

// freePort returns a TCP port that is free on the loopback interface.
func freePort(t *testing.T) string {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	_, port, err := net.SplitHostPort(l.Addr().String())
	require.NoError(t, err)
	require.NoError(t, l.Close())

	return port
}

// createNetwork creates a bridge network on a free subnet and returns its first three octets.
func (e *itEnv) createNetwork(t *testing.T, name string) string {
	t.Helper()

	var lastErr error

	for range 30 {
		octet := make([]byte, 1)
		_, err := rand.Read(octet)
		require.NoError(t, err)

		base := fmt.Sprintf("10.213.%d", octet[0])

		_, lastErr = e.cli.NetworkCreate(t.Context(), name, client.NetworkCreateOptions{
			Driver: "bridge",
			IPAM: &network.IPAM{Config: []network.IPAMConfig{{
				Subnet:  netip.MustParsePrefix(base + ".0/24"),
				Gateway: netip.MustParseAddr(base + ".1"),
			}}},
			Labels: map[string]string{itLabel: e.run},
		})
		if lastErr == nil {
			return base
		}
	}

	require.NoError(t, lastErr, "no free subnet for a test network")

	return ""
}

func observed(t *testing.T, r *dockerRuntime, name string) protocol.Container {
	t.Helper()

	all, err := r.Containers(t.Context())
	require.NoError(t, err)

	idx := slices.IndexFunc(all, func(c protocol.Container) bool { return c.Name == name })
	require.GreaterOrEqual(t, idx, 0, "container %s is not observed", name)

	return all[idx]
}

// Kind, team and stop timeout come from the container's labels and config.
func TestIntegrationLabels(t *testing.T) {
	env.cleanup(t, "labelled")
	env.cleanup(t, "plain")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("labels")
	env.publish(t, ref, variantEntryOne)

	_, _, err := r.Pull(ctx, ref)
	require.NoError(t, err)

	timeout := 42

	for name, labels := range map[string]map[string]string{
		env.prefix + "labelled": {protocol.LabelKind: "whatever", protocol.LabelTeam: "geth"},
		env.prefix + "plain":    {"own": "yes"},
	} {
		_, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref, Labels: labels, StopTimeout: &timeout},
		})
		require.NoError(t, err)
	}

	labelled := observed(t, r, env.prefix+"labelled")
	assert.Equal(t, protocol.ContainerKind("whatever"), labelled.Kind)
	assert.Equal(t, "geth", labelled.Team)
	assert.Equal(t, 42*time.Second, labelled.StopTimeout)

	plain := observed(t, r, env.prefix+"plain")
	assert.Empty(t, plain.Kind)
	assert.Empty(t, plain.Team)
}
