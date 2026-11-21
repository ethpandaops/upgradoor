package agent

import (
	"cmp"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// observation is what one cycle saw of the host.
type observation struct {
	hold protocol.Hold
	// containers are all containers, enriched with the managed-container
	// configuration and the registry checks.
	containers []protocol.Container
	// containersFresh is false when the runtime could not be asked and
	// containers is the previous observation.
	containersFresh bool
	node            protocol.NodeHealth
	// sys is nil until the first successful System.Status.
	sys *SystemStatus
	// targets is what every managed container is to run, by the
	// configuration and the overrides file.
	targets map[string]overrideTarget
	// check is the registry check of a tag, by normalised reference.
	check func(key string) (protocol.RegistryCheck, bool)
}

// registryCheck is the registry check of a tag; none when the observation
// carries no checks.
func (o *observation) registryCheck(key string) (protocol.RegistryCheck, bool) {
	if o.check == nil {
		return protocol.RegistryCheck{}, false
	}

	return o.check(key)
}

// container returns the observed container of that name, or nil.
func (o *observation) container(name string) *protocol.Container {
	return findContainer(o.containers, name)
}

func findContainer(list []protocol.Container, name string) *protocol.Container {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}

	return nil
}

// enrichContainers fills Managed, Kind, Impact, ConfiguredImage, Registry,
// Owners, Client, Override and Overridable of the observed containers. It
// returns the tags to follow and, for every managed container, what it is to
// run by the configuration and the overrides file.
//
// A managed container that does not run what agent.yaml and the overrides
// file say is moved there under a lease (a spec or an override need). A
// container on an override is followed on the override's tag like any other,
// and put back when the override is gone.
func enrichContainers(
	cfg *Config, list []protocol.Container, set *overrideSet,
	check func(key string) (protocol.RegistryCheck, bool),
) (tags map[string]trackedTag, targets map[string]overrideTarget) {
	tags = make(map[string]trackedTag, len(cfg.Containers))
	targets = make(map[string]overrideTarget, len(cfg.Containers))

	for i := range list {
		c := &list[i]
		c.Managed, c.Kind, c.Impact, c.ConfiguredImage, c.Registry = false, "", "", "", nil
		c.Owners, c.Client, c.Override, c.Overridable = nil, "", nil, nil

		managed := cfg.Managed(c.Name)
		if managed == nil {
			// The command line of a container that is not managed is nobody's
			// business.
			c.Command = nil

			continue
		}

		m := cfg.current(managed)
		target := overridesTarget(&m, c, set)
		targets[c.Name] = target

		c.Managed = true
		c.Kind = m.Kind
		c.Impact = m.Impact
		c.ConfiguredImage = m.Image
		c.Owners = protocol.CleanOwners(m.Owners)
		c.Client = m.Client
		c.Overridable = m.overridable()
		c.Override = target.report(c, set.entry(c.Name))

		if !isTagReference(c.Image) {
			continue
		}

		key := NormalizeReference(c.Image)

		t := tags[key]
		if t.ref == "" {
			t.ref = c.Image
		}

		t.running = append(t.running, c.Digest)
		tags[key] = t

		if rc, ok := check(key); ok {
			c.Registry = &rc
		}

		// The image the container is to move to is followed too, and
		// pulled: no container runs it yet.
		if next := NormalizeReference(target.image); next != key && !target.inSync &&
			target.err == "" && isTagReference(target.image) {
			t := tags[next]
			if t.ref == "" {
				t.ref = target.image
			}

			t.running = append(t.running, "")
			tags[next] = t
		}
	}

	return tags, targets
}

// withMissing returns the observed containers and, for every managed
// container that does not exist, an entry in state missing. Such a
// container is reported and never created: the agent only recreates what is
// there, with the configuration it has.
func withMissing(cfg *Config, list []protocol.Container) []protocol.Container {
	out := make([]protocol.Container, 0, len(list)+1)
	out = append(out, list...)

	for i := range cfg.Containers {
		if findContainer(list, cfg.Containers[i].Name) != nil {
			continue
		}

		m := cfg.current(&cfg.Containers[i])
		out = append(out, protocol.Container{
			Name: m.Name, State: protocol.StateMissing, Managed: true, Kind: m.Kind, Impact: m.Impact,
			ConfiguredImage: m.Image, Owners: protocol.CleanOwners(m.Owners), Client: m.Client,
		})
	}

	return out
}

// overridesTarget is targetOf for the overrides as last read. While the file
// cannot be read nothing is applied and nothing put back: what a container
// runs is left alone, and said.
func overridesTarget(m *ManagedContainer, c *protocol.Container, set *overrideSet) overrideTarget {
	if set == nil {
		set = noOverrides()
	}

	target := targetOf(m, c, set.entry(c.Name))

	if !set.known && !target.inSync {
		return overrideTarget{
			image: c.Image, command: slices.Clone(c.Command), inSync: true,
			err: "the overrides file cannot be read (" + set.err + "); nothing is changed",
		}
	}

	return target
}

// overrideNeed returns the need to bring a container in line with
// agent.yaml and the overrides file: it is not what they say, and the image
// it is to run is local. The pull comes first so that the slot is spent on
// the restart alone. Where agent.yaml says another image or command line
// than the container was created with, it is a spec need that hosts share a
// rollout of; otherwise an override need of the host's own.
func overrideNeed(
	c *protocol.Container, target *overrideTarget,
	check func(key string) (protocol.RegistryCheck, bool),
) (protocol.Need, bool) {
	if target.inSync || target.err != "" || !isTagReference(target.image) {
		return protocol.Need{}, false
	}

	rc, ok := check(NormalizeReference(target.image))
	if !ok || rc.Digest == "" || !rc.Pulled {
		return protocol.Need{}, false
	}

	if target.spec {
		s := &protocol.SpecNeed{
			Container: c.Name, Key: target.specKey(c, rc.Digest), Image: target.image,
			FromDigest: c.Digest, ToDigest: rc.Digest, ToRevision: rc.Revision, Flags: target.flags,
		}

		if !SameReference(c.Image, target.image) {
			s.FromImage = c.Image
		}

		return protocol.Need{ID: protocol.SpecNeedID(c.Name, s.Key), Kind: protocol.NeedSpec, Spec: s}, true
	}

	n := &protocol.OverrideNeed{
		Container: c.Name, Image: target.image, FromDigest: c.Digest,
		ToDigest: rc.Digest, ToRevision: rc.Revision,
		Flags: target.flags, Restore: target.restore,
	}

	if !SameReference(c.Image, target.image) {
		n.FromImage = c.Image
	}

	return protocol.Need{
		ID:       protocol.OverrideNeedID(c.Name, target.key(rc.Digest)),
		Kind:     protocol.NeedOverride,
		Local:    true,
		Override: n,
	}, true
}

// rollable reports whether the agent would roll this container at all, and
// if not, why. c is a container that enrichContainers has looked at. The reason for an unmanaged container is a refusal (local
// policy); the others mean the need no longer applies.
func rollable(cfg *Config, c *protocol.Container, name string) (reason string, refused bool) {
	switch {
	case cfg.Managed(name) == nil:
		return "container " + name + " is not managed", true
	case c == nil:
		return "container " + name + " does not exist", false
	case !isTagReference(c.Image):
		return "container " + name + " is pinned to a digest", true
	default:
		return "", false
	}
}

// rebootReasons returns why the host wants a reboot, from the OS state.
func rebootReasons(sys *SystemStatus) []string {
	if sys == nil {
		return nil
	}

	var reasons []string

	if sys.OS.RebootRequired {
		reasons = append(reasons, protocol.RebootReasonFlag)
	}

	if sys.OS.NewestKernel != "" && sys.OS.RunningKernel != "" &&
		sys.OS.NewestKernel != sys.OS.RunningKernel {
		reasons = append(reasons, protocol.RebootReasonKernel)
	}

	return reasons
}

// blockedChanges returns the pending upgrades of configured blocked
// packages, sorted by name. Anything the system reports outside the
// configured set is dropped: the configuration bounds the verb.
func blockedChanges(cfg *Config, sys *SystemStatus) []protocol.PackageChange {
	if sys == nil {
		return nil
	}

	out := make([]protocol.PackageChange, 0, len(sys.Blocked))

	for _, p := range sys.Blocked {
		if slices.Contains(cfg.Packages.Blocked, p.Name) {
			out = append(out, p)
		}
	}

	slices.SortFunc(out, func(a, b protocol.PackageChange) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// computeNeeds returns the open needs of the host: what it observed plus
// the open manual needs. While any hold is in effect there are none.
//
// One container has at most one image need and the host at most one kind of
// reboot need per directive: where a manual need covers the same thing as an
// observed one, the manual need is reported (the controller waits for its
// directive) and the observed one is left out.
func computeNeeds(cfg *Config, obs *observation, manual []protocol.Need) []protocol.Need {
	needs := make([]protocol.Need, 0, len(manual)+len(cfg.Containers)+2)

	if obs.hold.Active() {
		return needs
	}

	manualContainers := make(map[string]struct{}, len(manual))
	manualReboot := false

	for _, n := range manual {
		if !cfg.Allows(n.Kind.Verb()) {
			continue
		}

		switch {
		case n.Kind == protocol.NeedImage && n.Image != nil:
			manualContainers[n.Image.Container] = struct{}{}
		case n.Kind == protocol.NeedReboot && n.Reboot != nil:
			manualReboot = true
		default:
			continue
		}

		needs = append(needs, cloneNeeds([]protocol.Need{n})...)
	}

	if cfg.Allows(protocol.VerbRoll) {
		for i := range obs.containers {
			c := &obs.containers[i]

			// A container that is not what agent.yaml and the overrides file
			// say moves there first; its tag is followed again once it is.
			if t, ok := obs.targets[c.Name]; ok && !t.inSync && c.Managed {
				if n, ok := overrideNeed(c, &t, obs.registryCheck); ok {
					needs = append(needs, n)
				}

				continue
			}

			if _, covered := manualContainers[c.Name]; covered {
				continue
			}

			if n, ok := imageNeed(c); ok {
				needs = append(needs, n)
			}
		}
	}

	if pkgs := blockedChanges(cfg, obs.sys); cfg.Allows(protocol.VerbPackages) && len(pkgs) > 0 {
		needs = append(needs, protocol.Need{
			ID:       protocol.PackagesNeedID(pkgs),
			Kind:     protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		})
	}

	if reasons := rebootReasons(obs.sys); cfg.Allows(protocol.VerbReboot) &&
		len(reasons) > 0 && !manualReboot {
		needs = append(needs, protocol.Need{
			ID:     protocol.RebootNeedID,
			Kind:   protocol.NeedReboot,
			Reboot: &protocol.RebootNeed{Reasons: reasons},
		})
	}

	slices.SortFunc(needs, func(a, b protocol.Need) int { return cmp.Compare(a.ID, b.ID) })

	return needs
}

// imageNeed returns the observed image need of a container: its tag points
// at another digest than it runs, and that image is already local. The pull
// comes first so that the slot is spent on the restart alone.
func imageNeed(c *protocol.Container) (protocol.Need, bool) {
	if !c.Managed || c.Registry == nil {
		return protocol.Need{}, false
	}

	rc := c.Registry
	if rc.Digest == "" || !rc.Pulled || rc.Digest == c.Digest {
		return protocol.Need{}, false
	}

	return protocol.Need{
		ID:   protocol.ImageNeedID(c.Name, rc.Digest),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container:  c.Name,
			Image:      c.Image,
			FromDigest: c.Digest,
			ToDigest:   rc.Digest,
			ToRevision: rc.Revision,
		},
	}, true
}

// reconcileManualNeeds brings the stored manual needs in line with the
// host: needs whose target stopped qualifying are dropped (with the reason
// in dropped, by need ID), and manual image needs follow their tag, so that
// a manual roll requested a while ago is served with the current image.
func reconcileManualNeeds(
	cfg *Config, obs *observation, manual []protocol.Need,
) (kept []protocol.Need, dropped map[string]string, changed bool) {
	kept = make([]protocol.Need, 0, len(manual))
	dropped = make(map[string]string, len(manual))

	for _, n := range cloneNeeds(manual) {
		switch {
		case !cfg.Allows(n.Kind.Verb()):
			dropped[n.ID] = "verb " + string(n.Kind.Verb()) + " is no longer allowed"
		case n.Kind == protocol.NeedReboot && n.Reboot != nil:
			kept = append(kept, n)
		case n.Kind == protocol.NeedImage && n.Image != nil:
			if !obs.containersFresh {
				kept = append(kept, n)

				continue
			}

			c := obs.container(n.Image.Container)

			if reason, _ := rollable(cfg, c, n.Image.Container); reason != "" {
				dropped[n.ID] = reason

				continue
			}

			if !SameReference(c.Image, n.Image.Image) {
				dropped[n.ID] = "container " + c.Name + " moved to another tag"

				continue
			}

			if followTag(&n, c) {
				changed = true
			}

			kept = append(kept, n)
		default:
			dropped[n.ID] = "malformed manual need"
		}
	}

	return kept, dropped, changed || len(dropped) > 0
}

// followTag updates a manual image need to the container's running digest
// and the tag's current, locally present digest.
func followTag(n *protocol.Need, c *protocol.Container) bool {
	changed := false

	if n.Image.FromDigest != c.Digest {
		n.Image.FromDigest = c.Digest
		changed = true
	}

	rc := c.Registry
	if rc != nil && rc.Pulled && rc.Digest != "" && rc.Digest != n.Image.ToDigest {
		n.Image.ToDigest = rc.Digest
		n.Image.ToRevision = rc.Revision
		changed = true
	}

	return changed
}
