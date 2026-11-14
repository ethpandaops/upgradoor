package agent

import (
	"cmp"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// containerRunning is the Docker state of a running container.
const containerRunning = "running"

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

// enrichContainers fills Managed, Kind, ConfiguredImage, Custom and Registry
// of the observed containers and returns the tags to follow.
//
// A managed container whose image reference differs from the configured one
// is custom: somebody swapped the image on purpose. It is reported, never
// followed and never rolled.
func enrichContainers(
	cfg *Config, list []protocol.Container, check func(key string) (protocol.RegistryCheck, bool),
) map[string]trackedTag {
	tags := make(map[string]trackedTag, len(cfg.Containers))

	for i := range list {
		c := &list[i]
		c.Managed, c.Kind, c.ConfiguredImage, c.Custom, c.Registry = false, "", "", false, nil

		m := cfg.Managed(c.Name)
		if m == nil {
			continue
		}

		c.Managed = true
		c.Kind = m.Kind
		c.ConfiguredImage = m.Image
		c.Custom = !SameReference(c.Image, m.Image)

		if c.Custom || !isTagReference(c.Image) {
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
	}

	return tags
}

// rollable reports whether the agent would roll this container at all, and
// if not, why. The reason for an unmanaged container is a refusal (local
// policy); the others mean the need no longer applies.
func rollable(cfg *Config, c *protocol.Container, name string) (reason string, refused bool) {
	switch {
	case cfg.Managed(name) == nil:
		return "container " + name + " is not managed", true
	case c == nil:
		return "container " + name + " does not exist", false
	case c.Custom || !SameReference(c.Image, cfg.Managed(name).Image):
		return "container " + name + " runs a custom image (" + c.Image + ")", false
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
	if !c.Managed || c.Custom || c.Registry == nil {
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
