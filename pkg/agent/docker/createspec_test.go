package docker

import (
	"net/netip"
	"testing"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestOwnEnv(t *testing.T) {
	imageEnv := []string{"PATH=/usr/bin", "MODE=default", "EMPTY="}

	tests := []struct {
		name string
		env  []string
		want []string
	}{
		{name: "nothing set", env: nil, want: nil},
		{name: "only inherited", env: []string{"PATH=/usr/bin", "MODE=default", "EMPTY="}, want: []string{}},
		{
			name: "own variables keep their order, inherited ones go",
			env:  []string{"B=2", "A=1", "PATH=/usr/bin", "MODE=default"},
			want: []string{"B=2", "A=1"},
		},
		{
			name: "an overridden default is the container's own",
			env:  []string{"MODE=custom", "PATH=/usr/bin"},
			want: []string{"MODE=custom"},
		},
		{
			name: "value comparison is exact",
			env:  []string{"EMPTY", "PATH=/usr/bin "},
			want: []string{"EMPTY", "PATH=/usr/bin "},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ownEnv(tt.env, imageEnv))
		})
	}
}

func TestOwnCommand(t *testing.T) {
	tests := []struct {
		name            string
		entrypoint, cmd []string
		imageEP, imgCmd []string
		wantEP, wantCmd []string
	}{
		{
			name:       "everything inherited",
			entrypoint: []string{"/entry"}, cmd: []string{"serve"},
			imageEP: []string{"/entry"}, imgCmd: []string{"serve"},
		},
		{
			name: "no entrypoint anywhere, inherited command",
			cmd:  []string{"sh"}, imgCmd: []string{"sh"},
		},
		{
			name:       "command override with inherited entrypoint",
			entrypoint: []string{"/entry"}, cmd: []string{"--datadir", "/data"},
			imageEP: []string{"/entry"}, imgCmd: []string{"serve"},
			wantCmd: []string{"--datadir", "/data"},
		},
		{
			name:       "entrypoint override without command",
			entrypoint: []string{"/custom"},
			imageEP:    []string{"/entry"}, imgCmd: []string{"serve"},
			wantEP: []string{"/custom"},
		},
		{
			// With an own entrypoint Docker does not inherit the command,
			// so one that equals the image's was given and must stay.
			name:       "entrypoint override, command equal to the image's",
			entrypoint: []string{"/custom"}, cmd: []string{"serve"},
			imageEP: []string{"/entry"}, imgCmd: []string{"serve"},
			wantEP: []string{"/custom"}, wantCmd: []string{"serve"},
		},
		{
			// Without a command the image's would have been inherited,
			// had the entrypoint not been given.
			name:       "entrypoint given as the image has it, no command",
			entrypoint: []string{"/entry"},
			imageEP:    []string{"/entry"}, imgCmd: []string{"serve"},
			wantEP: []string{"/entry"},
		},
		{
			name:       "entrypoint inherited from an image without command",
			entrypoint: []string{"/entry"},
			imageEP:    []string{"/entry"},
		},
		{
			name:    "entrypoint reset",
			cmd:     []string{"sleep", "1"},
			imageEP: []string{"/entry"}, imgCmd: []string{"serve"},
			wantEP: []string{""}, wantCmd: []string{"sleep", "1"},
		},
		{
			name:       "entrypoint and command override",
			entrypoint: []string{"/custom"}, cmd: []string{"x"},
			imgCmd: []string{"sh"},
			wantEP: []string{"/custom"}, wantCmd: []string{"x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, cmd := ownCommand(tt.entrypoint, tt.cmd, tt.imageEP, tt.imgCmd)
			assert.Equal(t, tt.wantEP, ep, "entrypoint")
			assert.Equal(t, tt.wantCmd, cmd, "command")
		})
	}
}

func TestOwnLabels(t *testing.T) {
	imageLabels := map[string]string{
		"org.opencontainers.image.revision": "abc",
		"maintainer":                        "team",
	}

	got := ownLabels(map[string]string{
		"org.opencontainers.image.revision": "abc",
		"maintainer":                        "someone else",
		"role":                              "el",
	}, imageLabels)

	assert.Equal(t, map[string]string{"maintainer": "someone else", "role": "el"}, got)
	assert.Empty(t, ownLabels(imageLabels, imageLabels))
	assert.Empty(t, ownLabels(nil, imageLabels))
}

func TestOwnExposedPorts(t *testing.T) {
	p := network.MustParsePort

	imagePorts := map[string]struct{}{"8545/tcp": {}, "30303/udp": {}, "30303": {}}
	bindings := network.PortMap{p("30303/tcp"): {{HostPort: "30303"}}}

	got := ownExposedPorts(network.PortSet{
		p("8545/tcp"):  {}, // inherited, not published: follows the image
		p("30303/udp"): {}, // inherited, not published
		p("30303/tcp"): {}, // inherited but published: stays
		p("9000/tcp"):  {}, // own
	}, imagePorts, bindings)

	assert.Equal(t, network.PortSet{p("30303/tcp"): {}, p("9000/tcp"): {}}, got)
	assert.Nil(t, ownExposedPorts(network.PortSet{p("8545/tcp"): {}}, imagePorts, nil))
	assert.Nil(t, ownExposedPorts(nil, imagePorts, bindings))
}

func TestOwnVolumes(t *testing.T) {
	imageVolumes := map[string]struct{}{"/data": {}}

	assert.Equal(t,
		map[string]struct{}{"/anon": {}},
		ownVolumes(map[string]struct{}{"/data": {}, "/anon": {}}, imageVolumes))
	assert.Nil(t, ownVolumes(map[string]struct{}{"/data": {}}, imageVolumes))
	assert.Nil(t, ownVolumes(nil, imageVolumes))
}

func TestOwnHealthcheck(t *testing.T) {
	imageCheck := &container.HealthConfig{
		Test:     []string{"CMD", "true"},
		Interval: 30 * time.Second,
		Timeout:  5 * time.Second,
		Retries:  3,
	}

	tests := []struct {
		name  string
		check *container.HealthConfig
		image *container.HealthConfig
		want  *container.HealthConfig
	}{
		{name: "none", check: nil, image: imageCheck, want: nil},
		{name: "inherited", check: imageCheck, image: imageCheck, want: nil},
		{
			name:  "image without healthcheck",
			check: &container.HealthConfig{Test: []string{"CMD", "x"}, Retries: 2},
			image: nil,
			want:  &container.HealthConfig{Test: []string{"CMD", "x"}, Retries: 2},
		},
		{
			// Docker fills unset fields from the image; only the given
			// ones are the container's.
			name: "partial override",
			check: &container.HealthConfig{
				Test: []string{"CMD", "true"}, Interval: time.Second, Timeout: 5 * time.Second, Retries: 3,
			},
			image: imageCheck,
			want:  &container.HealthConfig{Interval: time.Second},
		},
		{
			name: "full override",
			check: &container.HealthConfig{
				Test: []string{"CMD-SHELL", "exit 0"}, Interval: time.Second, Timeout: time.Second,
				StartPeriod: time.Second, StartInterval: time.Second, Retries: 9,
			},
			image: imageCheck,
			want: &container.HealthConfig{
				Test: []string{"CMD-SHELL", "exit 0"}, Interval: time.Second, Timeout: time.Second,
				StartPeriod: time.Second, StartInterval: time.Second, Retries: 9,
			},
		},
		{
			name:  "disabled",
			check: &container.HealthConfig{Test: []string{"NONE"}, Interval: 30 * time.Second, Timeout: 5 * time.Second, Retries: 3},
			image: imageCheck,
			want:  &container.HealthConfig{Test: []string{"NONE"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ownHealthcheck(tt.check, tt.image))
		})
	}
}

func TestBindDestination(t *testing.T) {
	tests := map[string]string{
		"/host/data:/data":       "/data",
		"/host/data:/data:ro":    "/data",
		"named:/var/lib/app/:rw": "/var/lib/app",
		"/anonymous":             "/anonymous",
		"/":                      "/",
	}

	for bind, want := range tests {
		assert.Equal(t, want, bindDestination(bind), bind)
	}
}

// inspected returns inspect data of a container the way the daemon reports
// one that was created from testImage with a few settings of its own.
func inspected() *container.InspectResponse {
	stopTimeout := 120

	return &container.InspectResponse{
		ID:    testContainerID,
		Name:  "/geth",
		Image: "sha256:old",
		State: &container.State{Running: true},
		Config: &container.Config{
			Hostname: testContainerID[:12],
			User:     "1000",
			Image:    "ethpandaops/geth:master",
			Env:      []string{"OWN=1", "PATH=/usr/bin", "MODE=default"},
			Cmd:      []string{"--datadir", "/data"},
			Entrypoint: []string{
				"/entry",
			},
			Labels: map[string]string{
				"role":                            "el",
				labelImageRevision:                "oldrev",
				"org.opencontainers.image.title":  "geth",
				"org.opencontainers.image.vendor": "overridden",
			},
			ExposedPorts: network.PortSet{
				network.MustParsePort("8545/tcp"):  {},
				network.MustParsePort("30303/tcp"): {},
				network.MustParsePort("30303/udp"): {},
			},
			Volumes:     map[string]struct{}{"/imgvol": {}, "/anon": {}},
			WorkingDir:  "/work",
			StopSignal:  "SIGTERM",
			StopTimeout: &stopTimeout,
			Healthcheck: &container.HealthConfig{Test: []string{"CMD", "true"}, Interval: time.Minute},
		},
		HostConfig: &container.HostConfig{
			Binds:       []string{"/host/data:/data", "named:/named:ro"},
			NetworkMode: "shared",
			PortBindings: network.PortMap{
				network.MustParsePort("30303/tcp"): {{HostIP: netip.MustParseAddr("0.0.0.0"), HostPort: "30303"}},
			},
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyAlways},
			Mounts: []mount.Mount{
				{Type: mount.TypeVolume, Source: "viamount", Target: "/viamount"},
			},
			Tmpfs:       map[string]string{"/scratch": ""},
			SecurityOpt: []string{"no-new-privileges:true"},
			CapAdd:      []string{"NET_ADMIN"},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/host/data", Destination: "/data", RW: true},
			{Type: mount.TypeVolume, Name: "named", Destination: "/named"},
			{Type: mount.TypeVolume, Name: "viamount", Destination: "/viamount", RW: true},
			{Type: mount.TypeVolume, Name: "anonvolume1", Destination: "/anon", RW: true},
			{Type: mount.TypeVolume, Name: "anonvolume2", Destination: "/imgvol"},
			// Inherited through volumes-from: not declared by the container.
			{Type: mount.TypeVolume, Name: "foreign", Destination: "/foreign", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"shared": {
					IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.9.0.10")},
					Aliases:    []string{"el", testContainerID[:12]},
					NetworkID:  "netid-shared",
					EndpointID: "ep1",
					IPAddress:  netip.MustParseAddr("10.9.0.10"),
					Gateway:    netip.MustParseAddr("10.9.0.1"),
					MacAddress: network.HardwareAddr{0x02, 0x42, 0x0a, 0x09, 0x00, 0x0a},
					DNSNames:   []string{"geth", "el"},
				},
				"extra-b": {NetworkID: "netid-b", IPAddress: netip.MustParseAddr("10.8.0.2")},
				"extra-a": {
					NetworkID:  "netid-a",
					Aliases:    []string{"alias-a"},
					DriverOpts: map[string]string{"opt": "1"},
					GwPriority: 5,
				},
			},
		},
	}
}

func testImage() *dockerspec.DockerOCIImageConfig {
	return &dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{
			User:         "",
			Env:          []string{"PATH=/usr/bin", "MODE=default"},
			Entrypoint:   []string{"/entry"},
			Cmd:          []string{"serve"},
			WorkingDir:   "/work",
			StopSignal:   "SIGTERM",
			ExposedPorts: map[string]struct{}{"8545/tcp": {}, "30303/tcp": {}},
			Volumes:      map[string]struct{}{"/imgvol": {}},
			Labels: map[string]string{
				labelImageRevision:                "oldrev",
				"org.opencontainers.image.title":  "geth",
				"org.opencontainers.image.vendor": "ethpandaops",
			},
		},
		DockerOCIImageConfigExt: dockerspec.DockerOCIImageConfigExt{
			Healthcheck: &container.HealthConfig{Test: []string{"CMD", "true"}, Interval: 30 * time.Second},
		},
	}
}

func TestBuildCreateSpec(t *testing.T) {
	old := inspected()

	spec, err := buildCreateSpec(old, testImage())
	require.NoError(t, err)

	assert.Equal(t, "geth", spec.Name)

	cfg := spec.Config
	assert.Equal(t, "ethpandaops/geth:master", cfg.Image, "same reference")
	assert.Empty(t, cfg.Hostname, "the default hostname is the old container's ID")
	assert.Equal(t, "1000", cfg.User)
	assert.Equal(t, []string{"OWN=1"}, cfg.Env)
	assert.Nil(t, cfg.Entrypoint, "inherited entrypoint")
	assert.Equal(t, []string{"--datadir", "/data"}, cfg.Cmd)
	assert.Equal(t, map[string]string{
		"role":                            "el",
		"org.opencontainers.image.vendor": "overridden",
	}, cfg.Labels)
	assert.Equal(t, network.PortSet{
		network.MustParsePort("30303/tcp"): {},
		network.MustParsePort("30303/udp"): {},
	}, cfg.ExposedPorts)
	assert.Equal(t, map[string]struct{}{"/anon": {}}, cfg.Volumes)
	assert.Empty(t, cfg.WorkingDir)
	assert.Empty(t, cfg.StopSignal)
	require.NotNil(t, cfg.StopTimeout)
	assert.Equal(t, 120, *cfg.StopTimeout)
	assert.Equal(t, &container.HealthConfig{Interval: time.Minute}, cfg.Healthcheck)

	host := spec.HostConfig
	assert.Equal(t, old.HostConfig.Binds, host.Binds, "binds are passed on untouched")
	assert.Equal(t, container.RestartPolicyAlways, host.RestartPolicy.Name)
	assert.Equal(t, old.HostConfig.PortBindings, host.PortBindings)
	assert.Equal(t, []string{"no-new-privileges:true"}, host.SecurityOpt)
	assert.Equal(t, []string{"NET_ADMIN"}, host.CapAdd)
	assert.Equal(t, container.NetworkMode("shared"), host.NetworkMode)
	assert.Equal(t, []mount.Mount{
		{Type: mount.TypeVolume, Source: "viamount", Target: "/viamount"},
		{Type: mount.TypeVolume, Source: "anonvolume1", Target: "/anon"},
		{Type: mount.TypeVolume, Source: "anonvolume2", Target: "/imgvol", ReadOnly: true},
	}, host.Mounts, "anonymous volumes are re-attached, everything else is covered already")

	assert.Equal(t, "shared", spec.Primary)
	assert.Equal(t, &network.EndpointSettings{
		IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr("10.9.0.10")},
		Aliases:    []string{"el"},
	}, spec.PrimaryEndpoint, "only what the user specified: the MAC address is a new one")
	assert.Equal(t, []networkAttachment{
		{Network: "extra-a", Endpoint: &network.EndpointSettings{
			Aliases:    []string{"alias-a"},
			DriverOpts: map[string]string{"opt": "1"},
			GwPriority: 5,
		}},
		{Network: "extra-b", Endpoint: &network.EndpointSettings{}},
	}, spec.Extra)

	// The inspect data is not modified.
	assert.Equal(t, inspected(), old)
}

func TestBuildCreateSpecNetworkModes(t *testing.T) {
	tests := []struct {
		name        string
		mode        container.NetworkMode
		networks    map[string]*network.EndpointSettings
		wantPrimary string
		wantMode    container.NetworkMode
		wantExtra   []string
	}{
		{
			name: "host", mode: "host",
			networks: map[string]*network.EndpointSettings{"host": {}},
			wantMode: "host",
		},
		{
			name: "none", mode: "none",
			networks: map[string]*network.EndpointSettings{"none": {}},
			wantMode: "none",
		},
		{name: "container", mode: "container:abc", wantMode: "container:abc"},
		{
			name: "default bridge", mode: "default",
			networks:    map[string]*network.EndpointSettings{"bridge": {}},
			wantPrimary: "bridge", wantMode: "default",
		},
		{
			name: "bridge and a second network", mode: "bridge",
			networks:    map[string]*network.EndpointSettings{"bridge": {}, "aaa": {}},
			wantPrimary: "bridge", wantMode: "bridge", wantExtra: []string{"aaa"},
		},
		{
			name: "network mode names the network by ID", mode: "netid-1",
			networks: map[string]*network.EndpointSettings{
				"aaa": {NetworkID: "netid-0"}, "zzz": {NetworkID: "netid-1"},
			},
			wantPrimary: "zzz", wantMode: "netid-1", wantExtra: []string{"aaa"},
		},
		{
			name: "disconnected from the network it was created on", mode: "gone",
			networks: map[string]*network.EndpointSettings{
				"bbb": {NetworkID: "netid-b"}, "ccc": {NetworkID: "netid-c"},
			},
			wantPrimary: "bbb", wantMode: "bbb", wantExtra: []string{"ccc"},
		},
		{name: "no endpoints", mode: "bridge", wantMode: "bridge"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := inspected()
			old.HostConfig.NetworkMode = tt.mode
			old.NetworkSettings.Networks = tt.networks

			spec, err := buildCreateSpec(old, testImage())
			require.NoError(t, err)

			assert.Equal(t, tt.wantPrimary, spec.Primary)
			assert.Equal(t, tt.wantMode, spec.HostConfig.NetworkMode)

			extra := make([]string, 0, len(spec.Extra))
			for _, att := range spec.Extra {
				extra = append(extra, att.Network)
			}

			assert.ElementsMatch(t, tt.wantExtra, extra)
			assert.IsIncreasing(t, append([]string{""}, extra...), "sorted by name")

			if tt.wantPrimary == "" {
				assert.Nil(t, spec.PrimaryEndpoint)
			} else {
				require.NotNil(t, spec.PrimaryEndpoint)
				assert.Empty(t, spec.PrimaryEndpoint.MacAddress)
			}

			if tt.mode.IsContainer() {
				assert.Empty(t, spec.Config.Hostname)
			}
		})
	}
}

func TestBuildCreateSpecKeepsExplicitHostname(t *testing.T) {
	old := inspected()
	old.Config.Hostname = "geth-1"
	old.Config.Domainname = "example.org"

	spec, err := buildCreateSpec(old, testImage())
	require.NoError(t, err)
	assert.Equal(t, "geth-1", spec.Config.Hostname)
	assert.Equal(t, "example.org", spec.Config.Domainname)
}

func TestBuildCreateSpecWithoutImageConfig(t *testing.T) {
	// An image without any configuration: everything is the container's.
	old := inspected()

	spec, err := buildCreateSpec(old, nil)
	require.NoError(t, err)
	assert.Equal(t, old.Config.Env, spec.Config.Env)
	assert.Equal(t, old.Config.Entrypoint, spec.Config.Entrypoint)
	assert.Equal(t, old.Config.Labels, spec.Config.Labels)
	assert.Equal(t, old.Config.Healthcheck, spec.Config.Healthcheck)
}

func TestBuildCreateSpecRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*container.InspectResponse)
		want   string
	}{
		{name: "no name", mutate: func(c *container.InspectResponse) { c.Name = "/" }, want: "no name"},
		{
			name:   "auto-remove",
			mutate: func(c *container.InspectResponse) { c.HostConfig.AutoRemove = true },
			want:   "auto-remove",
		},
		{
			name: "links on the default bridge",
			mutate: func(c *container.InspectResponse) {
				c.HostConfig.Links = []string{"/db:/geth/database"}
			},
			want: "links",
		},
		{
			name: "links on a network",
			mutate: func(c *container.InspectResponse) {
				c.NetworkSettings.Networks["extra-b"].Links = []string{"db:database"}
			},
			want: "links",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := inspected()
			tt.mutate(old)

			_, err := buildCreateSpec(old, testImage())
			require.ErrorContains(t, err, tt.want)
		})
	}
}
