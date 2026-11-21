package plan

import (
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Facts are what host read of the machine for one cycle or lease look.
type Facts struct {
	// Containers are all containers as host returned them, without policy.
	Containers []protocol.Container
	// Fresh is false when Containers is the last good list; Reconcile then
	// keeps every manual image need.
	Fresh bool
	// System is nil until the system state has been read once.
	System *System
}

// System is the cheap, frequently read part of the OS state. It mirrors
// host.System field for field, which a test keeps equal.
type System struct {
	BootID  string
	Uptime  time.Duration
	OS      protocol.OSSummary
	Pending protocol.Pending
	// Blocked are the configured blocked packages with a newer candidate.
	Blocked []protocol.PackageChange
	// InventoryHash is the hash of the inventory host would return now.
	InventoryHash string
}

// Tag is one tag the registry tracker is to follow; it mirrors registry.Tag.
type Tag struct {
	// Ref is the reference as written on the container or in the target.
	Ref string
	// InUse: a container runs the tag. A tag only a target names is looked
	// up on the tracker's own schedule even while the controller hands out
	// lookups.
	InUse bool
}

// View is what one cycle saw of the host, classified by the policy and the
// overrides file.
type View struct {
	// Containers are all containers, enriched by the policy; an unmanaged
	// one keeps no command line.
	Containers []protocol.Container
	// Fresh is Facts.Fresh.
	Fresh bool
	// Targets are the targets of managed containers outside the default
	// group, by name.
	Targets map[string]Target
	// Tags are the tags to follow, by normalised reference: every managed
	// container's tag and the tag of each pending valid target.
	Tags map[string]Tag
	// Hold is the hold state at the time of the view.
	Hold protocol.Hold
	// OverridesError is Overrides.Err.
	OverridesError string
	// System is Facts.System.
	System *System
	// Checks are the registry checks the view was built with, by normalised
	// reference.
	Checks map[string]protocol.RegistryCheck
}

// Container returns the container of that name in the view, or nil.
func (v *View) Container(name string) *protocol.Container {
	for i := range v.Containers {
		if v.Containers[i].Name == name {
			return &v.Containers[i]
		}
	}

	return nil
}

// Classify builds the view of one cycle, hold state included, without
// changing facts. Entries whose end has passed at now count as gone.
func Classify(
	policy *Policy, facts *Facts, files *Overrides,
	checks map[string]protocol.RegistryCheck, now time.Time,
) View {
	// A copy: the root keeps facts as the last good observation.
	list := slices.Clone(facts.Containers)
	view := View{
		Containers: list, Fresh: facts.Fresh, System: facts.System, Checks: checks,
		Targets:        make(map[string]Target, len(list)),
		Tags:           make(map[string]Tag, len(list)),
		Hold:           HoldState(files, now),
		OverridesError: files.Err,
	}

	for i := range list {
		c := &list[i]
		// Deep copy: nothing written through the view may reach the facts.
		c.Command, c.Owners = slices.Clone(c.Command), slices.Clone(c.Owners)

		if c.Base != nil {
			base := *c.Base
			base.Command = slices.Clone(base.Command)
			c.Base = &base
		}

		resolveGroup(policy, c)
	}

	impacts := groupImpacts(policy, list)

	for i := range list {
		c := &list[i]
		c.Impact, c.ConfiguredImage, c.Registry, c.NotRolled = "", "", nil, ""
		c.Owners, c.Client, c.Override, c.Overridable = nil, "", nil, nil

		if !c.Managed {
			// An unmanaged container's command line is nobody's business.
			c.Command = nil

			continue
		}

		c.Impact = impacts[c.Group]
		c.ConfiguredImage = c.Image
		c.NotRolled = notRolled(policy, c)

		target := Target{Image: c.Image, InSync: true}

		if c.Group != protocol.DefaultGroup {
			target = classifyTarget(policy, c, files, now)
			view.Targets[c.Name] = target

			if d, ok := files.Deployed[c.Name]; ok {
				c.ConfiguredImage = d.Image
			} else if c.Base != nil && c.Base.Image != "" {
				c.ConfiguredImage = c.Base.Image
			}

			g := policy.group(c.Group)
			c.Owners = protocol.CleanOwners(g.Owners)
			c.Client = g.Client
			c.Overridable = g.overridable()
			c.Override = target.report(c, currentEntry(files, c.Name, now))
		}

		if !imageref.IsTag(c.Image) {
			continue
		}

		key := imageref.Normalize(c.Image)
		follow(view.Tags, key, c.Image, true)

		if rc, ok := checks[key]; ok {
			c.Registry = &rc
		}

		// The target image is followed too: no container runs it yet.
		if next := imageref.Normalize(target.Image); next != key && !target.InSync &&
			target.Err == "" && imageref.IsTag(target.Image) {
			follow(view.Tags, next, target.Image, false)
		}
	}

	return view
}

// follow adds a tag, keeping the first reference written for it.
func follow(tags map[string]Tag, key, ref string, inUse bool) {
	t := tags[key]
	if t.Ref == "" {
		t.Ref = ref
	}

	t.InUse = t.InUse || inUse
	tags[key] = t
}

// currentEntry is a container's unexpired override, or nil.
func currentEntry(files *Overrides, name string, now time.Time) *Entry {
	e, ok := files.Entries[name]
	if !ok || e.Expired(now) {
		return nil
	}

	return &e
}

// classifyTarget is targetOf for the overrides file as read; while the file
// cannot be read, the container is left as it is.
func classifyTarget(policy *Policy, c *protocol.Container, files *Overrides, now time.Time) Target {
	var deployed *Deployed
	if d, ok := files.Deployed[c.Name]; ok {
		deployed = &d
	}

	g := policy.group(c.Group)
	target := targetOf(deployed, c, currentEntry(files, c.Name, now), &g, c.Group)

	if !files.Known && !target.InSync {
		return Target{
			Image: c.Image, Command: slices.Clone(c.Command), InSync: true,
			Err: "the overrides file cannot be read (" + files.Err + "); nothing is changed",
		}
	}

	return target
}

// resolveGroup sets Managed, Group, Kind and Unlabelled: by label, else by
// the name map, else the default group unless excluded; the label value
// protocol.GroupNone leaves the container out of everything.
func resolveGroup(policy *Policy, c *protocol.Container) {
	c.Managed, c.Unlabelled = true, false
	m, mapped := policy.Containers[c.Name]

	switch {
	case c.Group == protocol.GroupNone:
		c.Managed = false
	case c.Group != "":
		c.Kind = protocol.ParseKind(string(c.Kind))
	case mapped:
		c.Group, c.Kind, c.Unlabelled = m.Group, protocol.ParseKind(string(m.Kind)), true
	case policy.DefaultGroup.excludes(c.Name):
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

// groupImpacts is each group's impact: agent.yaml's, else node when the
// group has an el, cl or vc container, else independent.
func groupImpacts(policy *Policy, list []protocol.Container) map[string]protocol.Impact {
	out := make(map[string]protocol.Impact, len(policy.Groups)+2)

	for i := range list {
		c := &list[i]
		if !c.Managed {
			continue
		}

		if g := policy.group(c.Group); g.Impact != "" {
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

// notRolled says why a managed container is never rolled; empty when it is.
func notRolled(policy *Policy, c *protocol.Container) string {
	switch {
	case !imageref.IsTag(c.Image):
		return "pinned to a digest"
	case c.Linked:
		return "created with links (--link), which a recreate cannot keep"
	case c.Group == protocol.DefaultGroup && !policy.DefaultGroup.Roll:
		return "in the default group, which this host does not roll (defaultGroup.roll)"
	default:
		return ""
	}
}

// rollable says why a container would not be rolled; refused is true when
// local policy forbids it rather than the container being gone.
func rollable(c *protocol.Container, name string) (reason string, refused bool) {
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

// HoldState is the hold state the overrides file says at now. A file that
// cannot be read holds the host locally: when in doubt the agent does nothing.
func HoldState(files *Overrides, now time.Time) protocol.Hold {
	if !files.Known {
		return protocol.Hold{Local: true, LocalReason: "the overrides file cannot be read: " + Line(files.Err)}
	}

	var out protocol.Hold

	if h, ok := files.Holds[HoldLocal]; ok && !h.Expired(now) {
		out.Local, out.LocalReason = true, h.Reason
	}

	if h, ok := files.Holds[HoldController]; ok && !h.Expired(now) {
		out.Controller = true
	}

	return out
}

// HoldReason is why a host on hold does nothing, as a step or refusal says it.
func HoldReason(h protocol.Hold) string {
	switch {
	case h.Local && h.LocalReason != "":
		return "host is on local hold: " + h.LocalReason
	case h.Local:
		return "host is on local hold"
	default:
		return "host is on controller hold"
	}
}

// Deployment is the generated block init-overrides writes: each managed
// container's configured image and its command line before any override.
func Deployment(view *View) map[string]Deployed {
	out := make(map[string]Deployed, len(view.Containers))

	for i := range view.Containers {
		c := &view.Containers[i]
		if !c.Managed {
			continue
		}

		command := c.Command
		if c.Base != nil && c.Base.HasCommand {
			command = c.Base.Command
		}

		out[c.Name] = Deployed{
			Image: c.ConfiguredImage, Command: append([]string{}, command...), HasCommand: true,
		}
	}

	return out
}
