package docker

import (
	"errors"
	"maps"
	"slices"
	"sort"
	"strings"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// shortIDLength is the length of the short container ID Docker uses as the
// default hostname and, before Docker 25, as an automatic network alias.
const shortIDLength = 12

// createSpec is everything needed to create a container again.
type createSpec struct {
	Name       string
	Config     *container.Config
	HostConfig *container.HostConfig
	// Primary is the network the container is created on; empty for host,
	// none and container network modes, where no endpoint is configured.
	Primary         string
	PrimaryEndpoint *network.EndpointSettings
	// Extra are the networks connected after the create, sorted by name.
	Extra []networkAttachment
}

// networkAttachment is one network endpoint to configure.
type networkAttachment struct {
	Network  string
	Endpoint *network.EndpointSettings
}

// buildCreateSpec derives the create request for a replacement of old, whose
// Config and HostConfig are set. Docker copies image defaults into the
// container at create time, so values equal to oldImage's defaults are
// dropped to let the new image's defaults apply.
func buildCreateSpec(
	old *container.InspectResponse,
	oldImage *dockerspec.DockerOCIImageConfig,
) (*createSpec, error) {
	if old.HostConfig.AutoRemove {
		return nil, errors.New("container is created with auto-remove and cannot be recreated")
	}

	if hasLinks(old) {
		return nil, errors.New("container has links (--link), which a recreate does not keep; " +
			"it cannot be recreated")
	}

	spec := &createSpec{
		Name:       strings.TrimPrefix(old.Name, "/"),
		Config:     ownConfig(old, oldImage),
		HostConfig: cloneHostConfig(old.HostConfig),
	}

	if spec.Name == "" {
		return nil, errors.New("container has no name")
	}

	nameAnonymousMounts(spec.HostConfig.Mounts, old.Mounts)
	spec.HostConfig.Mounts = append(spec.HostConfig.Mounts, anonymousVolumeMounts(old)...)
	spec.setNetworks(old)

	return spec, nil
}

// apply writes a recreate's change into the create request.
func (s *createSpec) apply(change *Change) {
	if change.Image != "" {
		s.Config.Image = change.Image
	}

	if change.SetCommand {
		s.Config.Cmd = slices.Clone(change.Command)
	}

	if !change.SetBase && change.Group == "" {
		return
	}

	labels := maps.Clone(s.Config.Labels)
	if labels == nil {
		labels = make(map[string]string, 4)
	}

	if change.SetBase {
		setBaseLabels(labels, change.Base)
	}

	if change.Group != "" {
		labels[protocol.LabelGroup] = change.Group
		labels[protocol.LabelKind] = string(change.Kind)
	}

	s.Config.Labels = labels
}

// hasLinks reports whether the container has legacy or per-network links.
func hasLinks(old *container.InspectResponse) bool {
	if len(old.HostConfig.Links) > 0 {
		return true
	}

	if old.NetworkSettings == nil {
		return false
	}

	for _, endpoint := range old.NetworkSettings.Networks {
		if endpoint != nil && len(endpoint.Links) > 0 {
			return true
		}
	}

	return false
}

// ownConfig returns the container configuration without image defaults.
func ownConfig(
	old *container.InspectResponse,
	img *dockerspec.DockerOCIImageConfig,
) *container.Config {
	if img == nil {
		img = &dockerspec.DockerOCIImageConfig{}
	}

	in := old.Config
	cfg := *in

	// The default hostname is the old short ID; a container in another
	// container's network namespace cannot have one.
	if isShortID(in.Hostname, old.ID) || old.HostConfig.NetworkMode.IsContainer() {
		cfg.Hostname = ""
		cfg.Domainname = ""
	}

	cfg.Env = ownEnv(in.Env, img.Env)
	cfg.Entrypoint, cfg.Cmd = ownCommand(in.Entrypoint, in.Cmd, img.Entrypoint, img.Cmd)
	cfg.Labels = ownLabels(in.Labels, img.Labels)
	cfg.ExposedPorts = ownExposedPorts(in.ExposedPorts, img.ExposedPorts, old.HostConfig.PortBindings)
	cfg.Volumes = ownVolumes(in.Volumes, img.Volumes)
	cfg.Healthcheck = ownHealthcheck(in.Healthcheck, img.Healthcheck)

	if in.WorkingDir == img.WorkingDir {
		cfg.WorkingDir = ""
	}

	if in.User == img.User {
		cfg.User = ""
	}

	if in.StopSignal == img.StopSignal {
		cfg.StopSignal = ""
	}

	if slices.Equal(in.Shell, img.Shell) {
		cfg.Shell = nil
	}

	if slices.Equal(in.OnBuild, img.OnBuild) {
		cfg.OnBuild = nil
	}

	if in.StopTimeout != nil {
		timeout := *in.StopTimeout
		cfg.StopTimeout = &timeout
	}

	return &cfg
}

// isShortID reports whether value is the short form of a container ID.
func isShortID(value, id string) bool {
	return len(value) >= shortIDLength && len(value) <= len(id) && strings.HasPrefix(id, value)
}

// ownEnv returns the variables that are not verbatim image entries, which
// Docker appends when the container does not set them.
func ownEnv(env, imageEnv []string) []string {
	if len(env) == 0 {
		return nil
	}

	out := make([]string, 0, len(env))

	for _, entry := range env {
		if !slices.Contains(imageEnv, entry) {
			out = append(out, entry)
		}
	}

	return out
}

// ownCommand returns the entrypoint and command to pass on. Docker inherits
// the command only when neither entrypoint nor command is given, so the
// command is dropped only together with the entrypoint, and an entrypoint
// without command is kept even when it equals the image's.
func ownCommand(entrypoint, cmd, imageEntrypoint, imageCmd []string) (ownEP, ownCmd []string) {
	switch {
	case slices.Equal(entrypoint, imageEntrypoint):
		if slices.Equal(cmd, imageCmd) {
			// Inherited (or both empty): let the new image decide.
			return nil, nil
		}

		if len(entrypoint) > 0 && len(cmd) == 0 {
			// Entrypoint given without command: nothing was inherited.
			return slices.Clone(entrypoint), nil
		}

		return nil, slices.Clone(cmd)
	case len(entrypoint) == 0:
		// Created with --entrypoint ""; [""] tells the daemon so.
		return []string{""}, slices.Clone(cmd)
	default:
		return slices.Clone(entrypoint), slices.Clone(cmd)
	}
}

// ownLabels returns the labels that are not verbatim image labels.
func ownLabels(labels, imageLabels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))

	for key, value := range labels {
		if imageValue, inherited := imageLabels[key]; inherited && imageValue == value {
			continue
		}

		out[key] = value
	}

	return out
}

// ownExposedPorts returns the exposed ports the image does not expose, plus
// every published port.
func ownExposedPorts(
	ports network.PortSet,
	imagePorts map[string]struct{},
	bindings network.PortMap,
) network.PortSet {
	if len(ports) == 0 {
		return nil
	}

	fromImage := make(map[network.Port]struct{}, len(imagePorts))

	for raw := range imagePorts {
		if port, err := network.ParsePort(raw); err == nil {
			fromImage[port] = struct{}{}
		}
	}

	out := make(network.PortSet, len(ports))

	for port := range ports {
		_, inherited := fromImage[port]
		_, published := bindings[port]

		if !inherited || published {
			out[port] = struct{}{}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// ownVolumes returns the volume declarations the image does not declare.
func ownVolumes(volumes, imageVolumes map[string]struct{}) map[string]struct{} {
	if len(volumes) == 0 {
		return nil
	}

	out := make(map[string]struct{}, len(volumes))

	for path := range volumes {
		if _, inherited := imageVolumes[path]; !inherited {
			out[path] = struct{}{}
		}
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// ownHealthcheck returns the healthcheck fields that differ from the image's;
// Docker fills unset fields per field.
func ownHealthcheck(check, imageCheck *container.HealthConfig) *container.HealthConfig {
	if check == nil {
		return nil
	}

	if imageCheck == nil {
		out := *check
		out.Test = slices.Clone(check.Test)

		return &out
	}

	var out container.HealthConfig

	if !slices.Equal(check.Test, imageCheck.Test) {
		out.Test = slices.Clone(check.Test)
	}

	if check.Interval != imageCheck.Interval {
		out.Interval = check.Interval
	}

	if check.Timeout != imageCheck.Timeout {
		out.Timeout = check.Timeout
	}

	if check.StartPeriod != imageCheck.StartPeriod {
		out.StartPeriod = check.StartPeriod
	}

	if check.StartInterval != imageCheck.StartInterval {
		out.StartInterval = check.StartInterval
	}

	if check.Retries != imageCheck.Retries {
		out.Retries = check.Retries
	}

	if len(out.Test) == 0 && out.Interval == 0 && out.Timeout == 0 &&
		out.StartPeriod == 0 && out.StartInterval == 0 && out.Retries == 0 {
		return nil
	}

	return &out
}

// cloneHostConfig copies the host configuration deep enough to edit the spec.
func cloneHostConfig(in *container.HostConfig) *container.HostConfig {
	out := *in
	out.Binds = slices.Clone(in.Binds)
	out.Mounts = slices.Clone(in.Mounts)
	out.PortBindings = maps.Clone(in.PortBindings)

	return &out
}

// anonymousVolumeMounts returns mounts that attach the old container's
// anonymous volumes to the replacement so their data survives. They are not
// added to HostConfig.Binds, which Ansible's docker_container compares.
func anonymousVolumeMounts(old *container.InspectResponse) []mount.Mount {
	covered := make(map[string]struct{}, len(old.HostConfig.Binds)+len(old.HostConfig.Mounts))

	for _, bind := range old.HostConfig.Binds {
		if dest := bindDestination(bind); dest != "" {
			covered[dest] = struct{}{}
		}
	}

	for _, m := range old.HostConfig.Mounts {
		covered[cleanPath(m.Target)] = struct{}{}
	}

	for dest := range old.HostConfig.Tmpfs {
		covered[cleanPath(dest)] = struct{}{}
	}

	declared := make(map[string]struct{}, len(old.Config.Volumes))

	for path := range old.Config.Volumes {
		declared[cleanPath(path)] = struct{}{}
	}

	var out []mount.Mount

	for _, mp := range old.Mounts {
		if mp.Type != mount.TypeVolume || mp.Name == "" {
			continue
		}

		dest := cleanPath(mp.Destination)

		if _, isCovered := covered[dest]; isCovered {
			continue
		}

		if _, isDeclared := declared[dest]; !isDeclared {
			continue
		}

		out = append(out, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   mp.Name,
			Target:   mp.Destination,
			ReadOnly: !mp.RW,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })

	return out
}

// nameAnonymousMounts gives each volume mount without a source the name of
// the volume the old container has at its target, so the data follows the
// container instead of a new empty volume; read-only and options stay.
func nameAnonymousMounts(mounts []mount.Mount, points []container.MountPoint) {
	for i := range mounts {
		m := &mounts[i]
		if m.Type != mount.TypeVolume || m.Source != "" {
			continue
		}

		target := cleanPath(m.Target)

		for _, mp := range points {
			if mp.Type == mount.TypeVolume && mp.Name != "" && cleanPath(mp.Destination) == target {
				m.Source = mp.Name

				break
			}
		}
	}
}

// bindDestination returns the container path of a bind specification
// ("source:destination[:options]" or a bare "destination").
func bindDestination(bind string) string {
	parts := strings.Split(bind, ":")

	switch len(parts) {
	case 0:
		return ""
	case 1:
		return cleanPath(parts[0])
	default:
		return cleanPath(parts[1])
	}
}

// cleanPath normalises a container path for comparisons.
func cleanPath(path string) string {
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}

	return path
}

// setNetworks fills the spec's networks from the old container's endpoints.
func (s *createSpec) setNetworks(old *container.InspectResponse) {
	mode := old.HostConfig.NetworkMode
	if mode.IsHost() || mode.IsNone() || mode.IsContainer() {
		return
	}

	if old.NetworkSettings == nil || len(old.NetworkSettings.Networks) == 0 {
		return
	}

	networks := old.NetworkSettings.Networks
	names := slices.Sorted(maps.Keys(networks))
	primary := primaryNetwork(mode, networks, names)

	if primary == "" {
		// Detached from the network it was created on: use its first one.
		primary = names[0]
		s.HostConfig.NetworkMode = container.NetworkMode(primary)
	}

	for _, name := range names {
		endpoint := ownEndpoint(networks[name], old.ID)

		if name != primary {
			s.Extra = append(s.Extra, networkAttachment{Network: name, Endpoint: endpoint})

			continue
		}

		s.Primary = name
		s.PrimaryEndpoint = endpoint
	}
}

// primaryNetwork finds the endpoint of the network mode, by name or ID.
func primaryNetwork(
	mode container.NetworkMode,
	networks map[string]*network.EndpointSettings,
	names []string,
) string {
	want := string(mode)
	if mode.IsDefault() {
		want = network.NetworkBridge
	}

	if _, ok := networks[want]; ok {
		return want
	}

	for _, name := range names {
		if ep := networks[name]; ep != nil && ep.NetworkID != "" && ep.NetworkID == want {
			return name
		}
	}

	return ""
}

// ownEndpoint returns the user-specified part of an endpoint, without
// assigned addresses, IDs and MAC address.
func ownEndpoint(ep *network.EndpointSettings, containerID string) *network.EndpointSettings {
	if ep == nil {
		return &network.EndpointSettings{}
	}

	out := &network.EndpointSettings{
		IPAMConfig: ep.IPAMConfig.Copy(),
		DriverOpts: maps.Clone(ep.DriverOpts),
		GwPriority: ep.GwPriority,
	}

	for _, alias := range ep.Aliases {
		// Docker before 25 listed the container's short ID as an alias.
		if !isShortID(alias, containerID) {
			out.Aliases = append(out.Aliases, alias)
		}
	}

	return out
}
