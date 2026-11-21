// Package needs says what a host needs: new images for its tags, the specs
// and overrides of its containers, upgrades of its blocked packages, a
// reboot, and its validator range.
package needs

import (
	"cmp"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/hostapi"
	"github.com/ethpandaops/upgradoor/pkg/agent/imagewatch"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Observation is what one cycle saw of the host.
type Observation struct {
	Hold protocol.Hold
	// Containers are all containers, enriched with the managed-container
	// configuration and the registry checks.
	Containers []protocol.Container
	// ContainersFresh is false when the runtime could not be asked and
	// Containers is the previous observation.
	ContainersFresh bool
	Node            protocol.NodeHealth
	// Sys is nil until the first successful hostapi.System.Status.
	Sys *hostapi.SystemStatus
	// Targets is what every managed container is to run, by the
	// configuration and the overrides file.
	Targets map[string]overrides.Target
	// Check is the registry check of a tag, by normalised reference.
	Check func(key string) (protocol.RegistryCheck, bool)
}

// RegistryCheck is the registry check of a tag; none when the Observation
// carries no checks.
func (o *Observation) RegistryCheck(key string) (protocol.RegistryCheck, bool) {
	if o.Check == nil {
		return protocol.RegistryCheck{}, false
	}

	return o.Check(key)
}

// Container returns the observed container of that name, or nil.
func (o *Observation) Container(name string) *protocol.Container {
	return FindContainer(o.Containers, name)
}

// FindContainer returns the container of that name in list, or nil.
func FindContainer(list []protocol.Container, name string) *protocol.Container {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}

	return nil
}

// groupImpacts is the impact class of every group with a managed container
// on the host: what agent.yaml says, or node when the group has an el, cl or
// vc container and independent when it has none. The default group has only
// containers of kind other and no entry: it is independent.
func groupImpacts(cfg *config.Config, list []protocol.Container) map[string]protocol.Impact {
	out := make(map[string]protocol.Impact, len(cfg.Groups)+2)

	for i := range list {
		c := &list[i]
		if !c.Managed {
			continue
		}

		if g := cfg.Group(c.Group); g.Impact != "" {
			out[c.Group] = g.Impact

			continue
		}

		if out[c.Group] == "" {
			out[c.Group] = protocol.ImpactIndependent
		}

		out[c.Group] = out[c.Group].Heavier(protocol.DefaultImpact(c.Kind))
	}

	return out
}

// resolveGroup puts a container as the runtime observed it in its group: by
// its labels (the runtime filled Group and Kind from them), else by its name
// in agent.yaml, else in the default group unless agent.yaml leaves it out.
// The label value protocol.GroupNone leaves it out of everything. It sets
// Managed, Group, Kind and Unlabelled.
func resolveGroup(cfg *config.Config, c *protocol.Container) {
	c.Managed, c.Unlabelled = true, false
	m, mapped := cfg.Containers[c.Name]

	switch {
	case c.Group == protocol.GroupNone:
		c.Managed = false
	case c.Group != "":
		c.Kind = protocol.ParseKind(string(c.Kind))
	case mapped:
		c.Group, c.Kind, c.Unlabelled = m.Group, protocol.ParseKind(string(m.Kind)), true
	case cfg.DefaultGroup.Excludes(c.Name):
		c.Managed = false
	default:
		c.Group = protocol.DefaultGroup
	}

	switch {
	case !c.Managed:
		c.Group, c.Kind = "", ""
	case c.Group == protocol.DefaultGroup:
		c.Kind = protocol.KindOther
	}
}

// notRolled says why the agent rolls a managed container to nothing; empty
// when it rolls it.
func notRolled(cfg *config.Config, c *protocol.Container) string {
	switch {
	case !imageref.IsTag(c.Image):
		return "pinned to a digest"
	case c.Linked:
		return "created with links (--link), which a recreate cannot keep"
	case c.Group == protocol.DefaultGroup && !cfg.DefaultGroup.Roll:
		return "in the default group, which this host does not roll (defaultGroup.roll)"
	default:
		return ""
	}
}

// Enrich fills Managed, Group, Kind, NotRolled, Impact, ConfiguredImage,
// Registry, Owners, Client, Override and Overridable of the containers as
// the runtime observed them (resolveGroup says which are managed); the
// group's entry in agent.yaml says the rest. It returns the tags to follow
// and, for every managed container outside the default group, what it is to
// run by the overrides file.
//
// A managed container that does not run what the deployed block and its
// override say is moved there under a lease (a spec or an override need). A
// container on an override is followed on the override's tag like any other,
// and put back when the override is gone. The default group knows no
// overrides: its containers only follow their tag.
func Enrich(
	cfg *config.Config, list []protocol.Container, set *overrides.Set,
	check func(key string) (protocol.RegistryCheck, bool),
) (tags map[string]imagewatch.Tag, targets map[string]overrides.Target) {
	tags = make(map[string]imagewatch.Tag, len(list))
	targets = make(map[string]overrides.Target, len(list))

	for i := range list {
		resolveGroup(cfg, &list[i])
	}

	impacts := groupImpacts(cfg, list)

	for i := range list {
		c := &list[i]
		c.Impact, c.ConfiguredImage, c.Registry, c.NotRolled = "", "", nil, ""
		c.Owners, c.Client, c.Override, c.Overridable = nil, "", nil, nil

		if !c.Managed {
			// The command line of a container that is not managed is nobody's
			// business.
			c.Command = nil

			continue
		}

		c.Impact = impacts[c.Group]
		c.ConfiguredImage = c.Image
		c.NotRolled = notRolled(cfg, c)

		target := overrides.Target{Image: c.Image, InSync: true}

		if c.Group != protocol.DefaultGroup {
			g := cfg.Group(c.Group)
			target = overridesTarget(&g, c, set)
			targets[c.Name] = target

			if d := set.DeployedOf(c.Name); d != nil {
				c.ConfiguredImage = d.Image
			} else if c.Base != nil && c.Base.Image != "" {
				c.ConfiguredImage = c.Base.Image
			}

			c.Owners = protocol.CleanOwners(g.Owners)
			c.Client = g.Client
			c.Overridable = g.Overridable()
			c.Override = target.Report(c, set.Entry(c.Name))
		}

		if !imageref.IsTag(c.Image) {
			continue
		}

		key := imageref.Normalize(c.Image)

		t := tags[key]
		if t.Ref == "" {
			t.Ref = c.Image
		}

		t.Running = append(t.Running, c.Digest)
		tags[key] = t

		if rc, ok := check(key); ok {
			c.Registry = &rc
		}

		// The image the container is to move to is followed too, and
		// pulled: no container runs it yet.
		if next := imageref.Normalize(target.Image); next != key && !target.InSync &&
			target.Err == "" && imageref.IsTag(target.Image) {
			t := tags[next]
			if t.Ref == "" {
				t.Ref = target.Image
			}

			t.Running = append(t.Running, "")
			tags[next] = t
		}
	}

	return tags, targets
}

// overridesTarget is overrides.TargetOf for the overrides file as last read.
// While the file cannot be read nothing is applied and nothing put back:
// what a container runs is left alone, and said.
func overridesTarget(g *config.Group, c *protocol.Container, set *overrides.Set) overrides.Target {
	if set == nil {
		set = overrides.NewSet()
	}

	target := overrides.TargetOf(set.DeployedOf(c.Name), c, set.Entry(c.Name), overrides.PolicyOf(g), c.Group)

	if !set.Known && !target.InSync {
		return overrides.Target{
			Image: c.Image, Command: slices.Clone(c.Command), InSync: true,
			Err: "the overrides file cannot be read (" + set.Err + "); nothing is changed",
		}
	}

	return target
}

// overrideNeed returns the need to bring a container in line with the
// overrides file (its deployed entry and its override): it is not what they
// say, and the registry said where the tag points. The need waits for the
// image to be pulled in a pull window (markUnpulled). Where the deployed
// block names another image or command line than the container was created
// with, it is a spec need, rolled in the group's rollout; otherwise an
// override need of the host's own.
func overrideNeed(
	c *protocol.Container, target *overrides.Target,
	check func(key string) (protocol.RegistryCheck, bool),
) (protocol.Need, bool) {
	if target.InSync || target.Err != "" || !imageref.IsTag(target.Image) {
		return protocol.Need{}, false
	}

	rc, ok := check(imageref.Normalize(target.Image))
	if !ok || rc.Digest == "" {
		return protocol.Need{}, false
	}

	if target.Spec {
		s := &protocol.SpecNeed{
			Container: c.Name, Group: c.Group, Client: c.Client, Key: target.SpecKey(c, rc.Digest),
			Image: target.Image, FromDigest: c.Digest, ToDigest: rc.Digest,
			ToRevision: rc.Revision, Flags: target.Flags,
		}

		if !imageref.Same(c.Image, target.Image) {
			s.FromImage = c.Image
		}

		return protocol.Need{ID: protocol.SpecNeedID(c.Name, s.Key), Kind: protocol.NeedSpec, Spec: s}, true
	}

	n := &protocol.OverrideNeed{
		Container: c.Name, Image: target.Image, FromDigest: c.Digest,
		ToDigest: rc.Digest, ToRevision: rc.Revision,
		Flags: target.Flags, Restore: target.Restore,
	}

	if !imageref.Same(c.Image, target.Image) {
		n.FromImage = c.Image
	}

	return protocol.Need{
		ID:       protocol.OverrideNeedID(c.Name, target.Key(rc.Digest)),
		Kind:     protocol.NeedOverride,
		Local:    true,
		Override: n,
	}, true
}

// Rollable reports whether the agent would roll this container at all, and
// if not, why. c is a container that Enrich has looked at. The
// reason for an unmanaged container is a refusal (local policy); the others
// mean the need no longer applies.
func Rollable(c *protocol.Container, name string) (reason string, refused bool) {
	switch {
	case c == nil:
		return "container " + name + " does not exist", false
	case !c.Managed:
		return "container " + name + " is not managed", true
	case c.NotRolled != "":
		return "container " + name + " is " + c.NotRolled, true
	default:
		return "", false
	}
}

// RebootReasons returns why the host wants a reboot, from the OS state.
func RebootReasons(sys *hostapi.SystemStatus) []string {
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

// BlockedChanges returns the pending upgrades of configured blocked
// packages, sorted by name. Anything the system reports outside the
// configured set is dropped: the configuration bounds the verb.
func BlockedChanges(cfg *config.Config, sys *hostapi.SystemStatus) []protocol.PackageChange {
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

// Compute returns the open needs of the host: what it observed plus
// the open manual needs. While any hold is in effect there are none.
//
// One container has at most one image need and the host at most one kind of
// reboot need per directive: where a manual need covers the same thing as an
// observed one, the manual need is reported (the controller waits for its
// directive) and the observed one is left out.
func Compute(cfg *config.Config, obs *Observation, manual []protocol.Need) []protocol.Need {
	needs := make([]protocol.Need, 0, len(manual)+len(obs.Containers)+2)

	if obs.Hold.Active() {
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

		needs = append(needs, Clone([]protocol.Need{n})...)
	}

	if cfg.Allows(protocol.VerbRoll) {
		for i := range obs.Containers {
			c := &obs.Containers[i]

			// A container that is not what agent.yaml and the overrides file
			// say moves there first; its tag is followed again once it is.
			if t, ok := obs.Targets[c.Name]; ok && !t.InSync && c.Managed {
				if n, ok := overrideNeed(c, &t, obs.RegistryCheck); ok {
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

	if pkgs := BlockedChanges(cfg, obs.Sys); cfg.Allows(protocol.VerbPackages) && len(pkgs) > 0 {
		needs = append(needs, protocol.Need{
			ID:       protocol.PackagesNeedID(pkgs),
			Kind:     protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		})
	}

	if reasons := RebootReasons(obs.Sys); cfg.Allows(protocol.VerbReboot) &&
		len(reasons) > 0 && !manualReboot {
		needs = append(needs, protocol.Need{
			ID:     protocol.RebootNeedID,
			Kind:   protocol.NeedReboot,
			Reboot: &protocol.RebootNeed{Reasons: reasons},
		})
	}

	markUnpulled(needs, obs)
	slices.SortFunc(needs, func(a, b protocol.Need) int { return cmp.Compare(a.ID, b.ID) })

	return needs
}

// needImage is the image a need moves a container to: a tag reference at a
// digest; empty for a need about no image.
func needImage(n *protocol.Need) protocol.PullImage {
	switch {
	case n.Image != nil:
		return protocol.PullImage{Ref: n.Image.Image, Digest: n.Image.ToDigest}
	case n.Spec != nil:
		return protocol.PullImage{Ref: n.Spec.Image, Digest: n.Spec.ToDigest}
	case n.Override != nil:
		return protocol.PullImage{Ref: n.Override.Image, Digest: n.Override.ToDigest}
	default:
		return protocol.PullImage{}
	}
}

// markUnpulled says of every need whose image is not local yet that it waits
// for it (Need.Unpulled). The image is pulled in a pull window the controller
// grants, so that the slot is spent on the restart alone.
func markUnpulled(needs []protocol.Need, obs *Observation) {
	for i := range needs {
		n := &needs[i]
		n.Unpulled = nil

		img := needImage(n)
		if img.Ref == "" || img.Digest == "" {
			continue
		}

		if rc, ok := obs.RegistryCheck(imageref.Normalize(img.Ref)); ok && rc.Pulled && rc.Digest == img.Digest {
			continue
		}

		n.Unpulled = []protocol.PullImage{img}
	}
}

// imageNeed returns the observed image need of a container: its tag points
// at another digest than it runs. The image need not be local yet: the need
// says what it waits for (markUnpulled).
func imageNeed(c *protocol.Container) (protocol.Need, bool) {
	if !c.Managed || c.NotRolled != "" || c.Registry == nil {
		return protocol.Need{}, false
	}

	rc := c.Registry
	if rc.Digest == "" || rc.Digest == c.Digest {
		return protocol.Need{}, false
	}

	return protocol.Need{
		ID:   protocol.ImageNeedID(c.Name, rc.Digest),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container:  c.Name,
			Group:      c.Group,
			Client:     c.Client,
			Image:      c.Image,
			FromDigest: c.Digest,
			ToDigest:   rc.Digest,
			ToRevision: rc.Revision,
		},
	}, true
}

// Reconcile brings the stored manual needs in line with the
// host: needs whose target stopped qualifying are dropped (with the reason
// in dropped, by need ID), and manual image needs follow their tag, so that
// a manual roll requested a while ago is served with the current image.
func Reconcile(
	cfg *config.Config, obs *Observation, manual []protocol.Need,
) (kept []protocol.Need, dropped map[string]string, changed bool) {
	kept = make([]protocol.Need, 0, len(manual))
	dropped = make(map[string]string, len(manual))

	for _, n := range Clone(manual) {
		switch {
		case !cfg.Allows(n.Kind.Verb()):
			dropped[n.ID] = "verb " + string(n.Kind.Verb()) + " is no longer allowed"
		case n.Kind == protocol.NeedReboot && n.Reboot != nil:
			kept = append(kept, n)
		case n.Kind == protocol.NeedImage && n.Image != nil:
			if !obs.ContainersFresh {
				kept = append(kept, n)

				continue
			}

			c := obs.Container(n.Image.Container)

			if reason, _ := Rollable(c, n.Image.Container); reason != "" {
				dropped[n.ID] = reason

				continue
			}

			if !imageref.Same(c.Image, n.Image.Image) {
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
	if rc != nil && rc.Digest != "" && (rc.Digest != n.Image.ToDigest || rc.Revision != n.Image.ToRevision) {
		// The revision is known once the image is local.
		n.Image.ToDigest = rc.Digest
		n.Image.ToRevision = rc.Revision
		changed = true
	}

	return changed
}

// Clone returns a deep copy of needs.
func Clone(in []protocol.Need) []protocol.Need {
	if in == nil {
		return nil
	}

	out := make([]protocol.Need, len(in))

	for i, n := range in {
		out[i] = n

		if n.Image != nil {
			img := *n.Image
			out[i].Image = &img
		}

		if n.Packages != nil {
			pk := *n.Packages
			pk.Packages = append([]protocol.PackageChange(nil), n.Packages.Packages...)
			out[i].Packages = &pk
		}

		if n.Reboot != nil {
			rb := *n.Reboot
			rb.Reasons = append([]string(nil), n.Reboot.Reasons...)
			out[i].Reboot = &rb
		}
	}

	return out
}
