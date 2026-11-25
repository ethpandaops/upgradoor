//go:build docker_integration

package agent

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
	"testing"
	"time"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Everything the test creates is named "upgradoor-it-<run>-..." (containers,
// image repositories) or labelled "upgradoor.test=<run>" (its image), and
// nothing else is touched.
const (
	itPrefix      = "upgradoor-it-"
	itLabel       = "upgradoor.test"
	itBaseImage   = "alpine:3.21"
	itRegistryRef = "registry:2"
	itSetup       = 5 * time.Minute
	itStopTimeout = 10 * time.Second
	itTeam        = "geth"
	// itLoop keeps a container alive and lets it stop at once on TERM.
	itLoop = "trap 'exit 0' TERM; while :; do sleep 0.2; done"
	// itOverrideLabels starts the labels that record an override.
	itOverrideLabels = "io.ethpandaops.upgradoor.override."
)

// dockerEnv is the local Docker daemon with a registry of the test's own and
// one image in several of its repositories.
type dockerEnv struct {
	t      *testing.T
	cli    *client.Client
	host   host.Host
	prefix string
	run    string
	// registry is "127.0.0.1:<port>"; Docker talks plain HTTP to it.
	registry string
	// digest is the manifest digest of the image in every repository.
	digest string
	refs   []string
}

func newDockerEnv(t *testing.T) *dockerEnv {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), itSetup)
	defer cancel()

	daemon := os.Getenv("DOCKER_HOST")
	if daemon == "" {
		daemon = "unix:///var/run/docker.sock"
	}

	cli, err := client.New(client.WithHost(daemon))
	require.NoError(t, err)

	_, err = cli.Ping(ctx, client.PingOptions{})
	require.NoError(t, err, "no Docker daemon at %s", daemon)

	id := make([]byte, 4)
	_, err = rand.Read(id)
	require.NoError(t, err)

	e := &dockerEnv{t: t, cli: cli, run: hex.EncodeToString(id)}
	e.prefix = itPrefix + e.run + "-"
	t.Cleanup(e.close)

	for _, ref := range []string{itBaseImage, itRegistryRef} {
		e.ensureImage(ctx, ref)
	}

	log := logrus.New()
	log.SetOutput(io.Discard)

	cfg := host.Config{}
	cfg.Docker.Host = daemon

	e.host, err = host.New(log, &cfg, nil, nil)
	require.NoError(t, err)

	e.startRegistry(ctx)
	e.buildImage(ctx)

	return e
}

// close removes the containers and image tags of this run.
func (e *dockerEnv) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if e.host != nil {
		_ = e.host.Close()
	}

	list, err := e.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err == nil {
		for _, c := range list.Items {
			for _, name := range c.Names {
				if strings.HasPrefix(strings.TrimPrefix(name, "/"), e.prefix) {
					opts := client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}
					_, _ = e.cli.ContainerRemove(ctx, c.ID, opts)

					break
				}
			}
		}
	}

	for _, ref := range e.refs {
		_, _ = e.cli.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: true})
	}

	_ = e.cli.Close()
}

func (e *dockerEnv) ensureImage(ctx context.Context, ref string) {
	if _, err := e.cli.ImageInspect(ctx, ref); err == nil {
		return
	}

	resp, err := e.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	require.NoError(e.t, err)

	defer func() { _ = resp.Close() }()

	require.NoError(e.t, resp.Wait(ctx), "pull %s", ref)
}

func (e *dockerEnv) startRegistry(ctx context.Context) {
	port := network.MustParsePort("5000/tcp")

	created, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: e.prefix + "registry",
		Config: &container.Config{
			Image: itRegistryRef, ExposedPorts: network.PortSet{port: {}},
			Labels: map[string]string{itLabel: e.run},
		},
		HostConfig: &container.HostConfig{
			PortBindings: network.PortMap{port: {{HostIP: netip.MustParseAddr("127.0.0.1")}}},
		},
	})
	require.NoError(e.t, err)

	_, err = e.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{})
	require.NoError(e.t, err)

	res, err := e.cli.ContainerInspect(ctx, created.ID, client.ContainerInspectOptions{})
	require.NoError(e.t, err)

	bindings := res.Container.NetworkSettings.Ports[port]
	require.NotEmpty(e.t, bindings, "the registry has no published port")

	e.registry = net.JoinHostPort("127.0.0.1", bindings[0].HostPort)

	require.Eventually(e.t, func() bool {
		url := "http://" + e.registry + "/v2/"

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return false
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, time.Minute, 100*time.Millisecond, "the registry did not come up")
}

// ref is the image's reference in a repository of the test registry.
func (e *dockerEnv) ref(repo string) string {
	return fmt.Sprintf("%s/%s%s:devnet", e.registry, e.prefix, repo)
}

// buildImage builds the test image, pushes it to every repository and pulls
// it back, so every tag is local at its registry digest.
func (e *dockerEnv) buildImage(ctx context.Context) {
	dockerfile := fmt.Sprintf("FROM %s\nLABEL %s=%q\nCMD [\"sh\", \"-c\", %q]\n",
		itBaseImage, itLabel, e.run, itLoop)

	var buildContext bytes.Buffer

	tw := tar.NewWriter(&buildContext)
	require.NoError(e.t, tw.WriteHeader(&tar.Header{
		Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile)),
	}))
	_, err := tw.Write([]byte(dockerfile))
	require.NoError(e.t, err)
	require.NoError(e.t, tw.Close())

	first := e.ref("app")
	e.refs = append(e.refs, first)

	built, err := e.cli.ImageBuild(ctx, &buildContext, client.ImageBuildOptions{
		Tags: []string{first}, Dockerfile: "Dockerfile", Remove: true, ForceRemove: true,
		Version: build.BuilderV1,
	})
	require.NoError(e.t, err)
	require.NoError(e.t, drainStream(built.Body), "build")

	anonymous := base64.URLEncoding.EncodeToString([]byte("{}"))

	for _, repo := range []string{"app", "fix", "none", "excluded"} {
		ref := e.ref(repo)

		if ref != first {
			e.refs = append(e.refs, ref)
			_, err = e.cli.ImageTag(ctx, client.ImageTagOptions{Source: first, Target: ref})
			require.NoError(e.t, err)
		}

		pushed, err := e.cli.ImagePush(ctx, ref, client.ImagePushOptions{RegistryAuth: anonymous})
		require.NoError(e.t, err)
		require.NoError(e.t, pushed.Wait(ctx), "push %s", ref)
		require.NoError(e.t, pushed.Close())

		digest, _, err := e.host.Pull(ctx, ref, nil)
		require.NoError(e.t, err)

		if e.digest == "" {
			e.digest = digest
		}

		require.Equal(e.t, e.digest, digest, "one image in every repository")
	}
}

// drainStream reads a build progress stream and returns its first error.
func drainStream(body io.ReadCloser) error {
	defer func() { _ = body.Close() }()

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

// create creates and starts a container of the run running image with the
// command and labels, and returns its name.
func (e *dockerEnv) create(
	t *testing.T, name, image string, cmd []string, labels map[string]string,
) string {
	t.Helper()

	full := e.prefix + name
	ctx := t.Context()

	_, err := e.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: full, Config: &container.Config{Image: image, Cmd: cmd, Labels: labels},
	})
	require.NoError(t, err)

	_, err = e.cli.ContainerStart(ctx, full, client.ContainerStartOptions{})
	require.NoError(t, err)

	return full
}

// classify classifies the run's containers with their tags at the image's digest.
func (e *dockerEnv) classify(t *testing.T, policy *plan.Policy, files *plan.Overrides) plan.View {
	t.Helper()

	all, err := e.host.Containers(t.Context())
	require.NoError(t, err)

	facts := plan.Facts{Fresh: true}

	for i := range all {
		if strings.HasPrefix(all[i].Name, e.prefix) {
			facts.Containers = append(facts.Containers, all[i])
		}
	}

	checks := make(map[string]protocol.RegistryCheck, len(e.refs))
	for _, ref := range e.refs {
		checks[imageref.Normalize(ref)] = protocol.RegistryCheck{
			Digest: e.digest, Pulled: true,
		}
	}

	return plan.Classify(policy, &facts, files, checks, time.Now())
}

// recreate rechecks a recreate on a fresh view and carries it out with host.
func (e *dockerEnv) recreate(
	t *testing.T, policy *plan.Policy, files *plan.Overrides, task *plan.Recreate,
) {
	t.Helper()

	view := e.classify(t, policy, files)

	rec, reason := plan.Recheck(task, &view)
	require.Empty(t, reason)

	change := host.Change{
		Image: rec.Image, Command: rec.Command, SetCommand: rec.SetCommand, Base: rec.Base, SetBase: rec.SetBase,
	}
	err := e.host.Recreate(t.Context(), task.Container, task.Digest, itStopTimeout, true, &change)
	require.NoError(t, err)
}

// labels are the labels of a container of the run.
func (e *dockerEnv) labels(t *testing.T, name string) map[string]string {
	t.Helper()

	res, err := e.cli.ContainerInspect(t.Context(), name, client.ContainerInspectOptions{})
	require.NoError(t, err)

	return res.Container.Config.Labels
}

// targetNeed is the spec or override need of a container, failing the test
// when there is none.
func targetNeed(t *testing.T, policy *plan.Policy, view *plan.View, name string) protocol.Need {
	t.Helper()

	for _, n := range plan.Needs(policy, view, nil) {
		if (n.Spec != nil && n.Spec.Container == name) ||
			(n.Override != nil && n.Override.Container == name) {
			return n
		}
	}

	require.FailNow(t, "no spec or override need for "+name)

	return protocol.Need{}
}

func kindLabels(kind protocol.ContainerKind, team string) map[string]string {
	return map[string]string{protocol.LabelKind: string(kind), protocol.LabelTeam: team}
}

// plan's rules applied through the real host to containers of the local Docker daemon.
func TestRulesOnDocker(t *testing.T) {
	e := newDockerEnv(t)

	cmdV1 := []string{"sh", "-c", itLoop}
	cmdV2 := []string{"sh", "-c", itLoop + " # v2"}
	app, fix := e.ref("app"), e.ref("fix")

	policy := plan.Policy{
		Verbs:         []protocol.Verb{protocol.VerbRoll},
		DefaultPolicy: plan.DefaultIgnore,
	}
	policy.ApplyDefaults()
	require.NoError(t, policy.Validate())

	t.Run("soft deploy", func(t *testing.T) {
		name := e.create(t, "soft", app, cmdV1, kindLabels(protocol.KindEL, itTeam))
		files := plan.Overrides{Known: true, Deployed: map[string]plan.Deployed{
			name: {Image: app, Command: cmdV2, HasCommand: true},
		}}

		view := e.classify(t, &policy, &files)
		require.False(t, view.Targets[name].InSync)

		need := targetNeed(t, &policy, &view, name)
		require.Equal(t, protocol.NeedSpec, need.Kind)

		e.recreate(t, &policy, &files, &plan.Recreate{
			IDs: []string{need.ID}, Container: name, Digest: need.Spec.ToDigest, Target: need.ID,
		})

		view = e.classify(t, &policy, &files)
		assert.True(t, view.Targets[name].InSync)
		assert.Equal(t, cmdV2, view.Container(name).Command)
		assert.Nil(t, view.Container(name).Base)
	})

	t.Run("override and its restore", func(t *testing.T) {
		name := e.create(t, "override", app, cmdV1, kindLabels(protocol.KindEL, itTeam))
		files := plan.Overrides{
			Known: true,
			Deployed: map[string]plan.Deployed{
				name: {Image: app, Command: cmdV1, HasCommand: true},
			},
			Entries: map[string]plan.Entry{
				name: {Image: fix, Command: append(slices.Clone(cmdV1), "--debug"), HasCommand: true, By: "it"},
			},
		}

		view := e.classify(t, &policy, &files)
		need := targetNeed(t, &policy, &view, name)
		require.Equal(t, protocol.NeedOverride, need.Kind)

		e.recreate(t, &policy, &files, &plan.Recreate{
			IDs: []string{need.ID}, Container: name, Digest: need.Override.ToDigest,
			Target: need.ID,
		})

		view = e.classify(t, &policy, &files)
		c := view.Container(name)
		assert.True(t, view.Targets[name].InSync)
		assert.Equal(t, fix, c.Image)
		assert.Equal(t, append(append([]string{}, cmdV1...), "--debug"), c.Command)
		base := &protocol.OverrideBase{Image: app, Command: cmdV1, HasCommand: true}
		assert.Equal(t, base, c.Base, "the labels record the deployed form")

		files.Entries = nil
		view = e.classify(t, &policy, &files)
		need = targetNeed(t, &policy, &view, name)
		require.True(t, need.Override.Restore)

		e.recreate(t, &policy, &files, &plan.Recreate{
			IDs: []string{need.ID}, Container: name, Digest: need.Override.ToDigest,
			Target: need.ID,
		})

		view = e.classify(t, &policy, &files)
		c = view.Container(name)
		assert.True(t, view.Targets[name].InSync)
		assert.Equal(t, app, c.Image)
		assert.Equal(t, cmdV1, c.Command)
		assert.Nil(t, c.Base)

		for label := range e.labels(t, name) {
			assert.False(t, strings.HasPrefix(label, itOverrideLabels), "label %s is left", label)
		}
	})

	t.Run("none and unlabelled are left alone", func(t *testing.T) {
		noneLabels := kindLabels(protocol.KindNone, itTeam)
		none := e.create(t, "none", e.ref("none"), cmdV1, noneLabels)
		excluded := e.create(t, "excluded", e.ref("excluded"), cmdV1, nil)
		files := plan.Overrides{Known: true}

		view := e.classify(t, &policy, &files)

		lease := &protocol.Lease{ID: "lease-it"}

		refs := map[string]string{none: e.ref("none"), excluded: e.ref("excluded")}

		for name, ref := range refs {
			c := view.Container(name)
			require.NotNil(t, c, name)
			assert.False(t, c.Managed, name)
			assert.NotContains(t, view.Tags, imageref.Normalize(ref),
				"the tag of %s is not followed", name)

			lease.Needs = append(lease.Needs, protocol.Need{
				ID: protocol.ImageNeedID(name, e.digest), Kind: protocol.NeedImage,
				Image: &protocol.ImageNeed{
					Container: name, Image: ref, FromDigest: "sha256:old", ToDigest: e.digest,
				},
			})
		}

		lp, err := plan.Validate(&policy, lease, &view, nil)
		require.NoError(t, err)
		assert.Empty(t, lp.Recreates)

		for _, n := range lease.Needs {
			assert.Equal(t, protocol.StepRefused, lp.Verdicts[n.ID].Status, n.ID)
			assert.Contains(t, lp.Verdicts[n.ID].Error, "is not managed", n.ID)
		}
	})
}
