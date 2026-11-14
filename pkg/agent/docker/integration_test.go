//go:build docker_integration

package docker

import (
	"encoding/json"
	"net/netip"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func observed(t *testing.T, r *Runtime, name string) protocol.Container {
	t.Helper()

	all, err := r.Observe(t.Context())
	require.NoError(t, err)

	idx := slices.IndexFunc(all, func(c protocol.Container) bool { return c.Name == name })
	require.GreaterOrEqual(t, idx, 0, "container %s is not observed", name)

	return all[idx]
}

// TestIntegrationDigests covers digest reporting and that a pull moves the
// tag without touching the running container.
func TestIntegrationDigests(t *testing.T) {
	env.cleanup(t, "digests")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("digests")
	name := env.prefix + "digests"

	digest1 := env.publish(t, ref, variantOne)

	local, err := r.LocalDigest(ctx, ref)
	require.NoError(t, err)
	assert.Empty(t, local, "nothing pulled yet")

	pulled, err := r.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest1, pulled)

	local, err = r.LocalDigest(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest1, local)

	created, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: ref},
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx, name))

	before := observed(t, r, name)
	assert.Equal(t, created.ID, before.ID)
	assert.Equal(t, ref, before.Image)
	assert.NotEmpty(t, before.ImageID)
	assert.Equal(t, digest1, before.Digest)
	assert.Equal(t, "rev1", before.Revision)
	assert.Equal(t, "1.0-rev1", before.Version)
	assert.Equal(t, "running", before.State)
	assert.Contains(t, []string{"starting", "healthy"}, before.Health)
	assert.Zero(t, before.RestartCount)
	assert.WithinDuration(t, time.Now(), before.StartedAt, time.Minute)
	assert.False(t, before.Managed)

	// Every container is observed, the test registry (pulled from Docker
	// Hub under its short name) included.
	reg := observed(t, r, env.prefix+"registry")
	assert.Equal(t, registryRef, reg.Image)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, reg.Digest)

	// A new image appears under the tag.
	digest2 := env.publish(t, ref, variantTwo)
	require.NotEqual(t, digest1, digest2)

	local, err = r.LocalDigest(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest1, local, "the registry moved, the local tag did not")

	pulled, err = r.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest2, pulled)

	local, err = r.LocalDigest(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest2, local)

	after := observed(t, r, name)
	assert.Equal(t, before.ID, after.ID, "the pull does not touch the container")
	assert.Equal(t, before.StartedAt, after.StartedAt)
	assert.Equal(t, "running", after.State)
	assert.Equal(t, before.ImageID, after.ImageID)
	assert.Equal(t, digest1, after.Digest, "the container still runs the old image")
	assert.Equal(t, "rev1", after.Revision)

	// Pulling again changes nothing.
	pulled, err = r.Pull(ctx, ref)
	require.NoError(t, err)
	assert.Equal(t, digest2, pulled)

	// Errors of the pull are reported.
	_, err = r.Pull(ctx, env.repoRef("digests-absent"))
	require.Error(t, err)

	local, err = r.LocalDigest(ctx, env.repoRef("digests-absent"))
	require.NoError(t, err)
	assert.Empty(t, local)

	// Stop and Start are idempotent.
	require.NoError(t, r.Stop(ctx, name, time.Second))
	require.NoError(t, r.Stop(ctx, name, time.Second))
	assert.Equal(t, "exited", observed(t, r, name).State)
	require.NoError(t, r.Start(ctx, name))
	require.NoError(t, r.Start(ctx, name))
	assert.Equal(t, "running", observed(t, r, name).State)
	require.Error(t, r.Start(ctx, env.prefix+"digests-absent"))
	require.Error(t, r.Stop(ctx, env.prefix+"digests-absent", time.Second))
}

// hostConfigWithoutMounts renders a host configuration for comparison. The
// mounts are compared separately: Recreate adds the anonymous volumes.
func hostConfigWithoutMounts(t *testing.T, hc *container.HostConfig) map[string]any {
	t.Helper()

	raw, err := json.Marshal(hc)
	require.NoError(t, err)

	var out map[string]any

	require.NoError(t, json.Unmarshal(raw, &out))
	delete(out, "Mounts")

	return out
}

func mountAt(t *testing.T, c container.InspectResponse, dest string) container.MountPoint {
	t.Helper()

	idx := slices.IndexFunc(c.Mounts, func(m container.MountPoint) bool { return m.Destination == dest })
	require.GreaterOrEqual(t, idx, 0, "no mount at %s", dest)

	return c.Mounts[idx]
}

// TestIntegrationRecreate creates a container with about every setting
// there is and checks that its replacement has them all, on the new image.
func TestIntegrationRecreate(t *testing.T) {
	env.cleanup(t, "recreate")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("recreate")
	name := env.prefix + "recreate"
	netA, netB := name+"-net-a", name+"-net-b"
	namedVolume := name + "-data"
	markers := []string{"/anon/marker", "/imgvol/marker", "/named/marker", "/bind/marker"}

	digest1 := env.publish(t, ref, variantOne)
	_, err := r.Pull(ctx, ref)
	require.NoError(t, err)

	subnet := env.createNetwork(t, netA)
	_, err = env.cli.NetworkCreate(ctx, netB, client.NetworkCreateOptions{Driver: "bridge"})
	require.NoError(t, err)
	_, err = env.cli.VolumeCreate(ctx, client.VolumeCreateOptions{Name: namedVolume})
	require.NoError(t, err)

	bindDir := t.TempDir()
	require.NoError(t, os.Chmod(bindDir, 0o777))

	hostPort := freePort(t)
	published := network.MustParsePort("7000/tcp")
	staticIP := netip.MustParseAddr(subnet + ".10")
	fixedMAC := network.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0x10}
	stopTimeout := 7
	pidsLimit := int64(200)
	initProcess := true

	// Every container instance writes its own random ID into each volume,
	// unless a marker is already there.
	command := []string{"sh", "-c",
		"for d in /anon /imgvol /named /bind; do " +
			"[ -f $d/marker ] || cat /proc/sys/kernel/random/uuid > $d/marker; done; " + loopScript}

	created, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:        ref,
			Hostname:     "it-host",
			User:         "0:0",
			Env:          []string{"OWN=1", "IMAGE_OVERRIDDEN=by-container"},
			Cmd:          command,
			Labels:       map[string]string{"own": "yes"},
			ExposedPorts: network.PortSet{published: {}},
			Volumes:      map[string]struct{}{"/anon": {}},
			StopTimeout:  &stopTimeout,
			Healthcheck: &container.HealthConfig{
				Test:     []string{"CMD-SHELL", "exit 0"},
				Interval: 2 * time.Second,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(netA),
			Binds:       []string{bindDir + ":/bind", namedVolume + ":/named"},
			PortBindings: network.PortMap{
				published: {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: hostPort}},
			},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyOnFailure, MaximumRetryCount: 3},
			CapAdd:        []string{"NET_ADMIN"},
			CapDrop:       []string{"MKNOD"},
			SecurityOpt:   []string{"no-new-privileges:true"},
			LogConfig: container.LogConfig{
				Type:   "json-file",
				Config: map[string]string{"max-size": "5m", "max-file": "2"},
			},
			ExtraHosts:  []string{"it.example:10.1.2.3"},
			DNS:         []netip.Addr{netip.MustParseAddr("1.1.1.1")},
			GroupAdd:    []string{"1234"},
			ShmSize:     32 << 20,
			Tmpfs:       map[string]string{"/scratch": "size=1m"},
			Sysctls:     map[string]string{"net.core.somaxconn": "1024"},
			OomScoreAdj: 100,
			Init:        &initProcess,
			Resources: container.Resources{
				Memory:    256 << 20,
				NanoCPUs:  500_000_000,
				PidsLimit: &pidsLimit,
				Ulimits:   []*container.Ulimit{{Name: "nofile", Soft: 2048, Hard: 4096}},
				Devices: []container.DeviceMapping{
					{PathOnHost: "/dev/null", PathInContainer: "/dev/it-null", CgroupPermissions: "rwm"},
				},
			},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netA: {
					IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: staticIP},
					Aliases:    []string{"it-alias"},
					MacAddress: fixedMAC,
				},
			},
		},
	})
	require.NoError(t, err)

	_, err = env.cli.NetworkConnect(ctx, netB, client.NetworkConnectOptions{
		Container:      created.ID,
		EndpointConfig: &network.EndpointSettings{Aliases: []string{"second"}},
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx, name))

	want := make(map[string]string, len(markers))
	for _, path := range markers {
		want[path] = env.waitFile(t, name, path)
	}

	old := env.inspect(t, name)
	require.True(t, old.State.Running)

	assertUntouched := func(msg string) {
		t.Helper()

		now := env.inspect(t, name)
		assert.Equal(t, old.ID, now.ID, msg)
		assert.True(t, now.State.Running, msg)
		assert.Equal(t, old.State.StartedAt, now.State.StartedAt, msg)
		assert.False(t, env.exists(t, name+replacedSuffix), msg)
	}

	// The registry moves on. Without a pull the local image is still the
	// old one, so a lease for the new digest is refused.
	digest2 := env.publish(t, ref, variantTwo)

	err = r.Recreate(ctx, name, digest2, 5*time.Second, true)
	require.ErrorIs(t, err, agent.ErrDigestMismatch)
	assertUntouched("refused: the lease digest is not pulled")

	pulled, err := r.Pull(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, digest2, pulled)
	assertUntouched("pull")

	// A lease for the previous digest is refused as well.
	err = r.Recreate(ctx, name, digest1, 5*time.Second, true)
	require.ErrorIs(t, err, agent.ErrDigestMismatch)
	assertUntouched("refused: the lease digest is outdated")

	err = r.Recreate(ctx, name, "", 5*time.Second, true)
	require.ErrorIs(t, err, agent.ErrDigestMismatch)
	assertUntouched("refused: no digest")

	require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, true))

	check := func(now container.InspectResponse) {
		t.Helper()

		newImage, err := env.cli.ImageInspect(ctx, ref)
		require.NoError(t, err)

		assert.Equal(t, "/"+name, now.Name)
		assert.Equal(t, newImage.ID, now.Image, "on the image the tag names now")
		assert.Equal(t, ref, now.Config.Image, "same reference")
		assert.False(t, env.exists(t, name+replacedSuffix), "the old container is removed")
		assert.False(t, env.exists(t, old.ID))

		// Settings of the container.
		assert.Equal(t, "it-host", now.Config.Hostname)
		assert.Equal(t, "0:0", now.Config.User)
		assert.Equal(t, command, now.Config.Cmd)
		assert.Empty(t, now.Config.Entrypoint)
		assert.Equal(t, &stopTimeout, now.Config.StopTimeout)
		assert.Contains(t, now.Config.Env, "OWN=1")
		assert.Contains(t, now.Config.Env, "IMAGE_OVERRIDDEN=by-container")
		assert.Equal(t, "yes", now.Config.Labels["own"])
		assert.Contains(t, now.Config.ExposedPorts, published)
		assert.Contains(t, now.Config.Volumes, "/anon")

		// Defaults follow the new image.
		assert.Contains(t, now.Config.Env, "IMAGE_DEFAULT=two")
		assert.NotContains(t, now.Config.Env, "IMAGE_DEFAULT=one")
		assert.NotContains(t, now.Config.Env, "IMAGE_OVERRIDDEN=one")
		assert.NotContains(t, now.Config.Env, "IMAGE_OVERRIDDEN=two")
		assert.Equal(t, "two", now.Config.Labels["image.shared"])
		assert.Equal(t, "yes", now.Config.Labels["image.only-two"])
		assert.NotContains(t, now.Config.Labels, "image.only-one")
		assert.Equal(t, "rev2", now.Config.Labels[labelImageRevision])
		assert.Equal(t, "/two", now.Config.WorkingDir)
		assert.Contains(t, now.Config.ExposedPorts, network.MustParsePort("8545/tcp"))
		assert.Contains(t, now.Config.ExposedPorts, network.MustParsePort("9000/udp"))
		assert.Contains(t, now.Config.Volumes, "/imgvol")

		// Healthcheck: the container's test and interval, the rest from
		// the new image.
		require.NotNil(t, now.Config.Healthcheck)
		assert.Equal(t, []string{"CMD-SHELL", "exit 0"}, now.Config.Healthcheck.Test)
		assert.Equal(t, 2*time.Second, now.Config.Healthcheck.Interval)
		assert.Equal(t, 7, now.Config.Healthcheck.Retries)

		// Host configuration: restart policy, capabilities, resources, log
		// configuration, ports, binds and everything else, unchanged.
		assert.Equal(t, hostConfigWithoutMounts(t, old.HostConfig), hostConfigWithoutMounts(t, now.HostConfig))
		assert.Equal(t, container.RestartPolicyOnFailure, now.HostConfig.RestartPolicy.Name)
		assert.Equal(t, 3, now.HostConfig.RestartPolicy.MaximumRetryCount)
		assert.Equal(t, []string{"CAP_NET_ADMIN"}, now.HostConfig.CapAdd)
		assert.Equal(t, int64(256<<20), now.HostConfig.Memory)
		assert.Equal(t, "5m", now.HostConfig.LogConfig.Config["max-size"])
		assert.Equal(t, old.HostConfig.Binds, now.HostConfig.Binds)

		// Volumes: the same ones, with their data.
		for _, dest := range []string{"/anon", "/imgvol", "/named", "/bind"} {
			before, after := mountAt(t, old, dest), mountAt(t, now, dest)
			assert.Equal(t, before.Type, after.Type, dest)
			assert.Equal(t, before.Name, after.Name, dest)
			assert.Equal(t, before.Source, after.Source, dest)
			assert.Equal(t, before.RW, after.RW, dest)
		}

		assert.ElementsMatch(t, []mount.Mount{
			{Type: mount.TypeVolume, Source: mountAt(t, old, "/anon").Name, Target: "/anon"},
			{Type: mount.TypeVolume, Source: mountAt(t, old, "/imgvol").Name, Target: "/imgvol"},
		}, now.HostConfig.Mounts, "the anonymous volumes are re-attached by name")

		// Networks: both, with alias, static address and fixed MAC.
		require.Len(t, now.NetworkSettings.Networks, 2)

		first := now.NetworkSettings.Networks[netA]
		require.NotNil(t, first)
		require.NotNil(t, first.IPAMConfig)
		assert.Equal(t, staticIP, first.IPAMConfig.IPv4Address)
		assert.Contains(t, first.Aliases, "it-alias")
		assert.NotContains(t, first.Aliases, old.ID[:12])

		second := now.NetworkSettings.Networks[netB]
		require.NotNil(t, second)
		assert.Contains(t, second.Aliases, "second")
	}

	now := env.inspect(t, name)
	require.NotEqual(t, old.ID, now.ID)
	require.True(t, now.State.Running)
	check(now)

	// What only shows while the container runs.
	assert.Equal(t, staticIP, now.NetworkSettings.Networks[netA].IPAddress)
	assert.Equal(t, fixedMAC, now.NetworkSettings.Networks[netA].MacAddress)
	assert.NotEqual(t,
		old.NetworkSettings.Networks[netB].MacAddress, now.NetworkSettings.Networks[netB].MacAddress,
		"a generated MAC address is not pinned")
	assert.Equal(t,
		[]network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: hostPort}},
		now.NetworkSettings.Ports[published])

	for _, path := range markers {
		got, err := env.readFile(ctx, name, path)
		require.NoError(t, err)
		assert.Equal(t, want[path], got, "data in %s survives", path)
	}

	obs := observed(t, r, name)
	assert.Equal(t, digest2, obs.Digest)
	assert.Equal(t, "rev2", obs.Revision)

	// Once more, now without starting: the result is the same container
	// again, created but not running.
	require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, false))

	stopped := env.inspect(t, name)
	require.NotEqual(t, now.ID, stopped.ID)
	assert.Equal(t, container.StateCreated, stopped.State.Status)
	assert.False(t, stopped.State.Running)
	assert.Equal(t, command, stopped.Config.Cmd)
	assert.False(t, env.exists(t, name+replacedSuffix))

	require.NoError(t, r.Start(ctx, name))

	for _, path := range markers {
		assert.Equal(t, want[path], env.waitFile(t, name, path), "data in %s survives", path)
	}

	started := env.inspect(t, name)
	assert.Equal(t, stopped.ID, started.ID)
	assert.True(t, started.State.Running)
	// The full comparison needs a container that was started: until then
	// the daemon reports some host settings (OomKillDisable) unresolved.
	check(started)
	assert.Equal(t, fixedMAC, started.NetworkSettings.Networks[netA].MacAddress)
	assert.Equal(t, staticIP, started.NetworkSettings.Networks[netA].IPAddress)

	// A stopped container is recreated just the same, and started.
	require.NoError(t, r.Stop(ctx, name, 5*time.Second))
	require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, true))

	again := env.inspect(t, name)
	assert.True(t, again.State.Running)
	check(again)
}

// TestIntegrationRecreateRollback injects failures after the stop.
func TestIntegrationRecreateRollback(t *testing.T) {
	t.Run("the new image cannot start", func(t *testing.T) {
		env.cleanup(t, "rollback-start")

		ctx := t.Context()
		r := env.newRuntime(t)
		ref := env.repoRef("rollback-start")
		name := env.prefix + "rollback-start"

		env.publish(t, ref, variantOne)
		_, err := r.Pull(ctx, ref)
		require.NoError(t, err)

		_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref, Volumes: map[string]struct{}{"/anon": {}}},
		})
		require.NoError(t, err)
		require.NoError(t, r.Start(ctx, name))

		old := env.inspect(t, name)

		// The container uses the image's command; the new image's does
		// not exist, so the replacement is created but cannot start.
		broken := env.publish(t, ref, variantBroken)
		pulled, err := r.Pull(ctx, ref)
		require.NoError(t, err)
		require.Equal(t, broken, pulled)

		err = r.Recreate(ctx, name, broken, 5*time.Second, true)
		require.Error(t, err)
		require.NotErrorIs(t, err, agent.ErrDigestMismatch)
		assert.ErrorContains(t, err, "start new container")
		assert.ErrorContains(t, err, "/does/not/exist", "the original error is returned")

		now := env.inspect(t, name)
		assert.Equal(t, old.ID, now.ID, "the old container is back under its name")
		assert.Equal(t, old.Image, now.Image)
		assert.True(t, now.State.Running, "and running again")
		assert.NotEqual(t, old.State.StartedAt, now.State.StartedAt, "it was stopped in between")
		assert.Equal(t, mountAt(t, old, "/anon").Name, mountAt(t, now, "/anon").Name)
		assert.False(t, env.exists(t, name+replacedSuffix), "no leftover")

		all, err := env.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
		require.NoError(t, err)

		for _, c := range all.Items {
			if hasNamePrefix(c.Names, name) {
				assert.Equal(t, old.ID, c.ID, "no half-created container is left: %v", c.Names)
			}
		}

		// With start false the same image is no problem: nothing is run.
		require.NoError(t, r.Recreate(ctx, name, broken, 5*time.Second, false))
		assert.Equal(t, container.StateCreated, env.inspect(t, name).State.Status)
	})

	t.Run("a stopped container whose replacement cannot start", func(t *testing.T) {
		env.cleanup(t, "rollback-stopped")

		ctx := t.Context()
		r := env.newRuntime(t)
		ref := env.repoRef("rollback-stopped")
		name := env.prefix + "rollback-stopped"
		netName := name + "-net"

		env.publish(t, ref, variantOne)
		_, err := r.Pull(ctx, ref)
		require.NoError(t, err)

		_, err = env.cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{Driver: "bridge"})
		require.NoError(t, err)

		_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:       name,
			Config:     &container.Config{Image: ref},
			HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(netName)},
		})
		require.NoError(t, err)
		require.NoError(t, r.Start(ctx, name))
		require.NoError(t, r.Stop(ctx, name, 5*time.Second))

		old := env.inspect(t, name)

		// The network of the (stopped) container disappears: a container
		// on it can still be created, but not started.
		_, err = env.cli.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{})
		require.NoError(t, err)

		digest2 := env.publish(t, ref, variantTwo)
		_, err = r.Pull(ctx, ref)
		require.NoError(t, err)

		err = r.Recreate(ctx, name, digest2, 5*time.Second, true)
		require.Error(t, err)
		assert.ErrorContains(t, err, "start new container")
		assert.ErrorContains(t, err, netName)

		now := env.inspect(t, name)
		assert.Equal(t, old.ID, now.ID, "the old container is back under its name")
		assert.Equal(t, container.StateExited, now.State.Status, "it was not running, so it is not started")
		assert.False(t, env.exists(t, name+replacedSuffix), "no leftover")
	})

	t.Run("the new container cannot be created", func(t *testing.T) {
		env.cleanup(t, "rollback-create")

		ctx := t.Context()
		r := env.newRuntime(t)
		ref := env.repoRef("rollback-create")
		name := env.prefix + "rollback-create"

		env.publish(t, ref, variantOne)
		_, err := r.Pull(ctx, ref)
		require.NoError(t, err)

		_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref},
		})
		require.NoError(t, err)
		require.NoError(t, r.Start(ctx, name))

		old := env.inspect(t, name)

		// The container runs the image's command and the new image has
		// none: the daemon refuses to create a container without command.
		digest := env.publish(t, ref, variantNoCommand)
		_, err = r.Pull(ctx, ref)
		require.NoError(t, err)

		err = r.Recreate(ctx, name, digest, 5*time.Second, true)
		require.Error(t, err)
		assert.ErrorContains(t, err, "create new container")

		now := env.inspect(t, name)
		assert.Equal(t, old.ID, now.ID, "the old container is back under its name")
		assert.True(t, now.State.Running, "and running again")
		assert.NotEqual(t, old.State.StartedAt, now.State.StartedAt, "it was stopped in between")
		assert.False(t, env.exists(t, name+replacedSuffix), "no leftover")
	})

	t.Run("an interrupted recreate is recovered", func(t *testing.T) {
		env.cleanup(t, "interrupted")

		ctx := t.Context()
		r := env.newRuntime(t)
		ref := env.repoRef("interrupted")
		name := env.prefix + "interrupted"

		digest1 := env.publish(t, ref, variantOne)
		_, err := r.Pull(ctx, ref)
		require.NoError(t, err)

		created, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref, Env: []string{"OWN=1"}},
		})
		require.NoError(t, err)

		// The state a crash between rename and create leaves behind.
		_, err = env.cli.ContainerRename(ctx, created.ID, client.ContainerRenameOptions{
			NewName: name + replacedSuffix,
		})
		require.NoError(t, err)

		require.NoError(t, r.Start(ctx, name))

		restored := env.inspect(t, name)
		assert.Equal(t, created.ID, restored.ID, "Start finds the container again")
		assert.True(t, restored.State.Running)

		// And the state a crash between create and remove leaves behind.
		_, err = env.cli.ContainerRename(ctx, created.ID, client.ContainerRenameOptions{
			NewName: name + replacedSuffix,
		})
		require.NoError(t, err)

		half, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref},
		})
		require.NoError(t, err)

		require.NoError(t, r.Recreate(ctx, name, digest1, 5*time.Second, true))

		now := env.inspect(t, name)
		assert.NotEqual(t, half.ID, now.ID)
		assert.NotEqual(t, created.ID, now.ID)
		assert.True(t, now.State.Running)
		assert.Contains(t, now.Config.Env, "OWN=1", "built from the old container, not the unfinished one")
		assert.False(t, env.exists(t, name+replacedSuffix))
		assert.False(t, env.exists(t, half.ID))
	})

	t.Run("a running replacement next to a leftover old container", func(t *testing.T) {
		env.cleanup(t, "leftover")

		ctx := t.Context()
		r := env.newRuntime(t)
		ref := env.repoRef("leftover")
		name := env.prefix + "leftover"

		digest1 := env.publish(t, ref, variantOne)
		_, err := r.Pull(ctx, ref)
		require.NoError(t, err)

		create := func(containerName, marker string) string {
			res, cerr := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
				Name: containerName,
				Config: &container.Config{
					Image: ref, Env: []string{"MARKER=" + marker}, StopTimeout: new(1),
				},
				HostConfig: &container.HostConfig{
					RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyAlways},
				},
			})
			require.NoError(t, cerr)

			return res.ID
		}

		// What a Recreate leaves when it dies after the start, or cannot
		// remove the old container: the replacement runs, the old
		// container is stopped under the retired name.
		oldID := create(name+retiredSuffix, "old")
		newID := create(name, "new")
		require.NoError(t, r.Start(ctx, newID))

		started := env.inspect(t, name).State.StartedAt

		untouched := func(msg string) {
			now := env.inspect(t, name)
			assert.Equal(t, newID, now.ID, msg)
			assert.True(t, now.State.Running, msg)
			assert.Equal(t, started, now.State.StartedAt, msg)
			assert.Equal(t, container.RestartPolicyAlways, now.HostConfig.RestartPolicy.Name, msg)
		}

		err = r.Recreate(ctx, name, digestC, 5*time.Second, true)
		require.ErrorIs(t, err, agent.ErrDigestMismatch)
		untouched("a refused recreate")
		assert.True(t, env.exists(t, oldID), "a refused recreate changes nothing")

		list, err := r.Observe(ctx)
		require.NoError(t, err)

		for _, c := range list {
			assert.NotEqual(t, name+retiredSuffix, c.Name, "the leftover is not observed")
		}

		require.NoError(t, r.Start(ctx, name))
		untouched("start")
		assert.False(t, env.exists(t, oldID), "the leftover is removed")

		// An unfinished replacement that the daemon started is displaced by
		// the old container, but not by a refused recreate.
		_, err = env.cli.ContainerRename(ctx, newID, client.ContainerRenameOptions{
			NewName: name + replacedSuffix,
		})
		require.NoError(t, err)

		_, err = env.cli.ContainerStop(ctx, newID, client.ContainerStopOptions{})
		require.NoError(t, err)

		halfID := create(name, "half")
		require.NoError(t, r.Start(ctx, halfID))

		err = r.Recreate(ctx, name, digestC, 5*time.Second, true)
		require.ErrorIs(t, err, agent.ErrDigestMismatch)
		assert.True(t, env.inspect(t, halfID).State.Running, "a refused recreate stops nothing")
		assert.True(t, env.exists(t, name+replacedSuffix))

		require.NoError(t, r.Recreate(ctx, name, digest1, 5*time.Second, true))

		now := env.inspect(t, name)
		assert.True(t, now.State.Running)
		assert.Contains(t, now.Config.Env, "MARKER=new", "built from the old container")
		assert.Equal(t, container.RestartPolicyAlways, now.HostConfig.RestartPolicy.Name)
		assert.False(t, env.exists(t, halfID))
		assert.False(t, env.exists(t, newID))
		assert.False(t, env.exists(t, name+replacedSuffix))
		assert.False(t, env.exists(t, name+retiredSuffix))
	})
}

// TestIntegrationRecreateExplicitEntrypoint: a container created with an
// entrypoint that equals the image's and without a command does not run the
// image's command, before the roll or after it.
func TestIntegrationRecreateExplicitEntrypoint(t *testing.T) {
	env.cleanup(t, "explicit-ep")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("explicit-ep")

	env.publish(t, ref, variantEntryOne)
	_, err := r.Pull(ctx, ref)
	require.NoError(t, err)

	explicit := env.prefix + "explicit-ep-given"
	inherited := env.prefix + "explicit-ep-inherited"

	_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   explicit,
		Config: &container.Config{Image: ref, Entrypoint: variantEntryOne.Entrypoint},
	})
	require.NoError(t, err)

	_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   inherited,
		Config: &container.Config{Image: ref},
	})
	require.NoError(t, err)

	require.NoError(t, r.Start(ctx, explicit))
	require.NoError(t, r.Start(ctx, inherited))

	before := env.inspect(t, explicit)
	require.Equal(t, variantEntryOne.Entrypoint, before.Config.Entrypoint)
	require.Empty(t, before.Config.Cmd, "the daemon does not inherit the command next to an entrypoint")

	digest2 := env.publish(t, ref, variantEntryTwo)
	_, err = r.Pull(ctx, ref)
	require.NoError(t, err)

	require.NoError(t, r.Recreate(ctx, explicit, digest2, 5*time.Second, true))
	require.NoError(t, r.Recreate(ctx, inherited, digest2, 5*time.Second, true))

	now := env.inspect(t, explicit)
	assert.True(t, now.State.Running)
	assert.Equal(t, before.Path, now.Path)
	assert.Equal(t, before.Args, now.Args, "the process is the same, without the image's command")
	assert.Equal(t, variantEntryTwo.Entrypoint, now.Config.Entrypoint)
	assert.Empty(t, now.Config.Cmd)

	now = env.inspect(t, inherited)
	assert.True(t, now.State.Running)
	assert.Equal(t, variantEntryTwo.Cmd, now.Config.Cmd, "an inherited command follows the image")
}

// TestIntegrationRecreateEntrypoint covers the entrypoint and command
// inheritance rules against the daemon's real merge behaviour.
func TestIntegrationRecreateEntrypoint(t *testing.T) {
	env.cleanup(t, "entrypoint")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("entrypoint")

	env.publish(t, ref, variantOne)
	_, err := r.Pull(ctx, ref)
	require.NoError(t, err)

	tests := []struct {
		name       string
		entrypoint []string
		cmd        []string
		// wantCmd is the command after the roll to variant two; nil means
		// the new image's default.
		wantEntrypoint []string
		wantCmd        []string
	}{
		{name: "inherited", wantCmd: variantTwo.Cmd},
		{name: "own-cmd", cmd: []string{"sh", "-c", loopScript}, wantCmd: []string{"sh", "-c", loopScript}},
		{
			name:           "own-entrypoint",
			entrypoint:     []string{"sh", "-c", loopScript},
			wantEntrypoint: []string{"sh", "-c", loopScript},
		},
		{
			// The command equals the old image's default, but with an own
			// entrypoint it was given, not inherited.
			name:           "own-entrypoint-default-cmd",
			entrypoint:     []string{"sh", "-c", loopScript, "--"},
			cmd:            variantOne.Cmd,
			wantEntrypoint: []string{"sh", "-c", loopScript, "--"},
			wantCmd:        variantOne.Cmd,
		},
	}

	for _, tt := range tests {
		_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   env.prefix + "entrypoint-" + tt.name,
			Config: &container.Config{Image: ref, Entrypoint: tt.entrypoint, Cmd: tt.cmd},
		})
		require.NoError(t, err)
		require.NoError(t, r.Start(ctx, env.prefix+"entrypoint-"+tt.name))
	}

	digest2 := env.publish(t, ref, variantTwo)
	_, err = r.Pull(ctx, ref)
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := env.prefix + "entrypoint-" + tt.name

			require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, true))

			now := env.inspect(t, name)
			assert.True(t, now.State.Running)
			assert.Equal(t, tt.wantEntrypoint, now.Config.Entrypoint)
			assert.Equal(t, tt.wantCmd, now.Config.Cmd)
		})
	}
}
