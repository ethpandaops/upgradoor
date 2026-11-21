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

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// hostConfigWithoutMounts renders a host configuration without mounts, which Recreate extends.
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

// A replacement keeps every setting of the container, on the new image.
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
	_, _, err := r.Pull(ctx, ref)
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
	stopTimeout := 7
	pidsLimit := int64(200)
	initProcess := true

	// Each instance writes its ID into each volume unless a marker is there.
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

	// Without a pull, a recreate for the new digest is refused.
	digest2 := env.publish(t, ref, variantTwo)

	err = r.Recreate(ctx, name, digest2, 5*time.Second, true, nil)
	require.ErrorIs(t, err, ErrDigestMismatch)
	assertUntouched("refused: the lease digest is not pulled")

	pulled, _, err := r.Pull(ctx, ref)
	require.NoError(t, err)
	require.Equal(t, digest2, pulled)
	assertUntouched("pull")

	// A lease for the previous digest is refused as well.
	err = r.Recreate(ctx, name, digest1, 5*time.Second, true, nil)
	require.ErrorIs(t, err, ErrDigestMismatch)
	assertUntouched("refused: the lease digest is outdated")

	err = r.Recreate(ctx, name, "", 5*time.Second, true, nil)
	require.ErrorIs(t, err, ErrDigestMismatch)
	assertUntouched("refused: no digest")

	require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, true, nil))

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

		// Healthcheck: the container's test and interval, the rest from the image.
		require.NotNil(t, now.Config.Healthcheck)
		assert.Equal(t, []string{"CMD-SHELL", "exit 0"}, now.Config.Healthcheck.Test)
		assert.Equal(t, 2*time.Second, now.Config.Healthcheck.Interval)
		assert.Equal(t, 7, now.Config.Healthcheck.Retries)

		// The host configuration is unchanged.
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

		// Networks: both, with alias and static address.
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

	// Once more without starting: created but not running.
	require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, false, nil))

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
	// The daemon resolves some host settings (OomKillDisable) only on start.
	check(started)
	assert.Equal(t, staticIP, started.NetworkSettings.Networks[netA].IPAddress)

	// A stopped container is recreated just the same, and started.
	require.NoError(t, r.Stop(ctx, name, 5*time.Second))
	require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, true, nil))

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
		_, _, err := r.Pull(ctx, ref)
		require.NoError(t, err)

		_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref, Volumes: map[string]struct{}{"/anon": {}}},
		})
		require.NoError(t, err)
		require.NoError(t, r.Start(ctx, name))

		old := env.inspect(t, name)

		// The new image's command does not exist: created, cannot start.
		broken := env.publish(t, ref, variantBroken)
		pulled, _, err := r.Pull(ctx, ref)
		require.NoError(t, err)
		require.Equal(t, broken, pulled)

		err = r.Recreate(ctx, name, broken, 5*time.Second, true, nil)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrDigestMismatch)
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
		require.NoError(t, r.Recreate(ctx, name, broken, 5*time.Second, false, nil))
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
		_, _, err := r.Pull(ctx, ref)
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

		// Without its network the replacement is created but cannot start.
		_, err = env.cli.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{})
		require.NoError(t, err)

		digest2 := env.publish(t, ref, variantTwo)
		_, _, err = r.Pull(ctx, ref)
		require.NoError(t, err)

		err = r.Recreate(ctx, name, digest2, 5*time.Second, true, nil)
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
		_, _, err := r.Pull(ctx, ref)
		require.NoError(t, err)

		_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Name:   name,
			Config: &container.Config{Image: ref},
		})
		require.NoError(t, err)
		require.NoError(t, r.Start(ctx, name))

		old := env.inspect(t, name)

		// The new image has no command: the daemon refuses the create.
		digest := env.publish(t, ref, variantNoCommand)
		_, _, err = r.Pull(ctx, ref)
		require.NoError(t, err)

		err = r.Recreate(ctx, name, digest, 5*time.Second, true, nil)
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
		_, _, err := r.Pull(ctx, ref)
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

		require.NoError(t, r.Recreate(ctx, name, digest1, 5*time.Second, true, nil))

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
		_, _, err := r.Pull(ctx, ref)
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

		// The replacement runs; the old container is stopped under the retired name.
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

		err = r.Recreate(ctx, name, digestC, 5*time.Second, true, nil)
		require.ErrorIs(t, err, ErrDigestMismatch)
		untouched("a refused recreate")
		assert.True(t, env.exists(t, oldID), "a refused recreate changes nothing")

		list, err := r.Containers(ctx)
		require.NoError(t, err)

		for _, c := range list {
			assert.NotEqual(t, name+retiredSuffix, c.Name, "the leftover is not observed")
		}

		require.NoError(t, r.Start(ctx, name))
		untouched("start")
		assert.False(t, env.exists(t, oldID), "the leftover is removed")

		// A started unfinished replacement is displaced, but not by a refused recreate.
		_, err = env.cli.ContainerRename(ctx, newID, client.ContainerRenameOptions{
			NewName: name + replacedSuffix,
		})
		require.NoError(t, err)

		_, err = env.cli.ContainerStop(ctx, newID, client.ContainerStopOptions{})
		require.NoError(t, err)

		halfID := create(name, "half")
		require.NoError(t, r.Start(ctx, halfID))

		err = r.Recreate(ctx, name, digestC, 5*time.Second, true, nil)
		require.ErrorIs(t, err, ErrDigestMismatch)
		assert.True(t, env.inspect(t, halfID).State.Running, "a refused recreate stops nothing")
		assert.True(t, env.exists(t, name+replacedSuffix))

		require.NoError(t, r.Recreate(ctx, name, digest1, 5*time.Second, true, nil))

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

// An explicit entrypoint equal to the image's, without command, stays without command.
func TestIntegrationRecreateExplicitEntrypoint(t *testing.T) {
	env.cleanup(t, "explicit-ep")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("explicit-ep")

	env.publish(t, ref, variantEntryOne)
	_, _, err := r.Pull(ctx, ref)
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
	_, _, err = r.Pull(ctx, ref)
	require.NoError(t, err)

	require.NoError(t, r.Recreate(ctx, explicit, digest2, 5*time.Second, true, nil))
	require.NoError(t, r.Recreate(ctx, inherited, digest2, 5*time.Second, true, nil))

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

// Entrypoint and command inheritance match the daemon's merge behaviour.
func TestIntegrationRecreateEntrypoint(t *testing.T) {
	env.cleanup(t, "entrypoint")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("entrypoint")

	env.publish(t, ref, variantOne)
	_, _, err := r.Pull(ctx, ref)
	require.NoError(t, err)

	tests := []struct {
		name       string
		entrypoint []string
		cmd        []string
		// wantCmd is the command after the roll; nil means the image default.
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
			// With an own entrypoint the default-equal command was given.
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
	_, _, err = r.Pull(ctx, ref)
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := env.prefix + "entrypoint-" + tt.name

			require.NoError(t, r.Recreate(ctx, name, digest2, 5*time.Second, true, nil))

			now := env.inspect(t, name)
			assert.True(t, now.State.Running)
			assert.Equal(t, tt.wantEntrypoint, now.Config.Entrypoint)
			assert.Equal(t, tt.wantCmd, now.Config.Cmd)
		})
	}
}

// An override recreate changes image and command, records labels, and restores.
func TestIntegrationRecreateWithChange(t *testing.T) {
	env.cleanup(t, "override")

	ctx := t.Context()
	r := env.newRuntime(t)
	name := env.prefix + "override"
	base, fix := env.repoRef("override-base"), env.repoRef("override-fix")

	baseDigest := env.publish(t, base, variantEntryOne)
	fixDigest := env.publish(t, fix, variantEntryTwo)

	for _, ref := range []string{base, fix} {
		_, _, err := r.Pull(ctx, ref)
		require.NoError(t, err)
	}

	command := []string{"--http", "--maxpeers=50"}

	_, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image: base, Cmd: command, Env: []string{"OWN=1"}, Labels: map[string]string{"own": "yes"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx, name))

	original := env.inspect(t, name)

	seen := observed(t, r, name)
	assert.Equal(t, command, seen.Command)
	assert.Nil(t, seen.Base, "a container nobody overrode records nothing")

	baseJSON, err := json.Marshal(command)
	require.NoError(t, err)

	baseLabel := string(baseJSON)
	changed := []string{"--http", "--pprof"}
	onto := &Change{
		Image: fix, Command: changed, SetCommand: true,
		Base: &protocol.OverrideBase{Image: base, Command: command, HasCommand: true}, SetBase: true,
	}

	// The digest is the replacement image's, verified before anything is touched.
	err = r.Recreate(ctx, name, baseDigest, 5*time.Second, true, onto)
	require.ErrorIs(t, err, ErrDigestMismatch)
	assert.Equal(t, original.ID, env.inspect(t, name).ID, "a refused recreate changes nothing")

	require.NoError(t, r.Recreate(ctx, name, fixDigest, 5*time.Second, true, onto))

	now := env.inspect(t, name)
	assert.NotEqual(t, original.ID, now.ID)
	assert.True(t, now.State.Running)
	assert.Equal(t, fix, now.Config.Image)
	assert.NotEqual(t, original.Image, now.Image, "on the other image")
	assert.Equal(t, changed, now.Config.Cmd)
	assert.Equal(t, variantEntryTwo.Entrypoint, now.Config.Entrypoint, "the entrypoint follows the image")
	assert.Contains(t, now.Config.Env, "OWN=1")
	assert.Equal(t, "yes", now.Config.Labels["own"])
	assert.Equal(t, base, now.Config.Labels[labelOverrideImage])
	assert.Equal(t, baseLabel, now.Config.Labels[labelOverrideCommand])
	assert.False(t, env.exists(t, name+replacedSuffix), "no leftover")

	seen = observed(t, r, name)
	assert.Equal(t, fix, seen.Image)
	assert.Equal(t, fixDigest, seen.Digest)
	assert.Equal(t, changed, seen.Command)
	assert.Equal(t, &protocol.OverrideBase{Image: base, Command: command, HasCommand: true}, seen.Base)

	// Without SetBase the override labels stay.
	more := []string{"--http", "--pprof", "--verbosity=4"}
	require.NoError(t, r.Recreate(ctx, name, fixDigest, 5*time.Second, true,
		&Change{Command: more, SetCommand: true}))

	now = env.inspect(t, name)
	assert.Equal(t, fix, now.Config.Image)
	assert.Equal(t, more, now.Config.Cmd)
	assert.Equal(t, baseLabel, now.Config.Labels[labelOverrideCommand], "labels it does not name stay")

	// A plain recreate keeps the override: image, command and labels.
	require.NoError(t, r.Recreate(ctx, name, fixDigest, 5*time.Second, true, nil))

	now = env.inspect(t, name)
	assert.Equal(t, fix, now.Config.Image)
	assert.Equal(t, more, now.Config.Cmd)
	assert.Equal(t, base, now.Config.Labels[labelOverrideImage])

	// And back: the configured image, the recorded command, no labels.
	require.NoError(t, r.Recreate(ctx, name, baseDigest, 5*time.Second, true, &Change{
		Image: base, Command: command, SetCommand: true, SetBase: true,
	}))

	now = env.inspect(t, name)
	assert.True(t, now.State.Running)
	assert.Equal(t, base, now.Config.Image)
	assert.Equal(t, original.Image, now.Image)
	assert.Equal(t, command, now.Config.Cmd)
	assert.Equal(t, original.Config.Labels, now.Config.Labels)
	assert.Equal(t, original.Config.Entrypoint, now.Config.Entrypoint)
	assert.Nil(t, observed(t, r, name).Base)
}

// A soft deploy recreates the container onto the deployed block, keeping its configuration.
func TestIntegrationSoftDeployViaTheBlock(t *testing.T) {
	env.cleanup(t, "soft")

	ctx := t.Context()
	r := env.newRuntime(t)
	name := env.prefix + "soft"
	v1, v2 := env.repoRef("soft-v1"), env.repoRef("soft-v2")

	env.publish(t, v1, variantEntryOne)
	v2Digest := env.publish(t, v2, variantEntryTwo)

	for _, ref := range []string{v1, v2} {
		_, _, err := r.Pull(ctx, ref)
		require.NoError(t, err)
	}

	labels := map[string]string{protocol.LabelGroup: "consensus", protocol.LabelKind: "cl", "own": "yes"}
	_, err := env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   name,
		Config: &container.Config{Image: v1, Cmd: []string{"--http"}, Env: []string{"OWN=1"}, Labels: labels},
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx, name))

	seen := observed(t, r, name)
	require.Equal(t, "consensus", seen.Group, "found by its label")

	require.NoError(t, r.Recreate(ctx, name, v2Digest, 5*time.Second, true, &Change{
		Image: v2, Command: []string{"--http", "--new-flag"}, SetCommand: true, SetBase: true,
	}))

	now := env.inspect(t, name)
	assert.True(t, now.State.Running)
	assert.Equal(t, v2, now.Config.Image)
	assert.Equal(t, []string{"--http", "--new-flag"}, now.Config.Cmd)
	assert.Contains(t, now.Config.Env, "OWN=1", "its own configuration")
	for k, v := range labels {
		assert.Equal(t, v, now.Config.Labels[k], "its labels stay: %s", k)
	}

	assert.NotContains(t, now.Config.Labels, labelOverrideImage, "no override label")
	assert.NotContains(t, now.Config.Labels, labelOverrideCommand, "no override label")
	assert.Nil(t, observed(t, r, name).Base)
}

// The recreate of a name-mapped container adds its group labels.
func TestIntegrationRecreateLabelsMappedContainer(t *testing.T) {
	env.cleanup(t, "mapped")

	ctx := t.Context()
	r := env.newRuntime(t)
	ref := env.repoRef("mapped")
	digest := env.publish(t, ref, variantOne)

	_, _, err := r.Pull(ctx, ref)
	require.NoError(t, err)

	mapped := env.prefix + "mapped"
	_, err = env.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: mapped, Config: &container.Config{Image: ref, Labels: map[string]string{"own": "yes"}},
	})
	require.NoError(t, err)
	require.NoError(t, r.Start(ctx, mapped))

	before := observed(t, r, mapped)
	assert.Empty(t, before.Group, "no label yet")
	assert.Empty(t, before.Kind)

	require.NoError(t, r.Recreate(ctx, mapped, digest, 5*time.Second, true,
		&Change{Group: "execution", Kind: protocol.KindEL}))

	now := env.inspect(t, mapped)
	assert.True(t, now.State.Running)
	assert.Equal(t, "execution", now.Config.Labels[protocol.LabelGroup])
	assert.Equal(t, "el", now.Config.Labels[protocol.LabelKind])
	assert.Equal(t, "yes", now.Config.Labels["own"], "its own labels stay")

	after := observed(t, r, mapped)
	assert.Equal(t, "execution", after.Group)
	assert.Equal(t, protocol.KindEL, after.Kind)
}
