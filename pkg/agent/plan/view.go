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

// View is what one cycle saw of the host, classified by the policy and the
// overrides file.
type View struct {
	// Containers are all containers, enriched by the policy; an unmanaged
	// one keeps no command line.
	Containers []protocol.Container
	// Fresh is Facts.Fresh.
	Fresh bool
	// Targets are the targets of labelled managed containers, by name.
	Targets map[string]Target
	// Tags are the tags to follow, by normalised reference: every managed
	// container's tag and the tag of each pending valid target.
	Tags map[string]string
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
		Tags:           make(map[string]string, len(list)),
		Hold:           HoldState(files, now),
		OverridesError: files.Err,
	}

	for i := range list {
		c := &list[i]
		// Deep copy: nothing written through the view may reach the facts.
		c.Command = slices.Clone(c.Command)

		if c.Base != nil {
			base := *c.Base
			base.Command = slices.Clone(base.Command)
			c.Base = &base
		}

		resolveLabels(policy, c)
		c.Impact, c.ConfiguredImage, c.Registry, c.NotRolled = "", "", nil, ""
		c.Override, c.Overridable = nil, false

		if !c.Managed {
			// An unmanaged container's command line is nobody's business.
			c.Command = nil

			continue
		}

		c.Impact = protocol.ImpactNode
		c.ConfiguredImage = c.Image
		c.NotRolled = notRolled(c)

		target := Target{Image: c.Image, InSync: true}

		if c.Kind == "" {
			c.Impact = protocol.ImpactIndependent
		} else {
			target = classifyTarget(c, files, now)
			view.Targets[c.Name] = target

			if d, ok := files.Deployed[c.Name]; ok {
				c.ConfiguredImage = d.Image
			} else if c.Base != nil && c.Base.Image != "" {
				c.ConfiguredImage = c.Base.Image
			}

			c.Overridable = c.Team != "" && !files.Disabled
			c.Override = target.report(c, currentEntry(files, c.Name, now))
		}

		if !imageref.IsTag(c.Image) {
			continue
		}

		key := imageref.Normalize(c.Image)
		follow(view.Tags, key, c.Image)

		if rc, ok := checks[key]; ok {
			c.Registry = &rc
		}

		// The target image is followed too: no container runs it yet.
		if next := imageref.Normalize(target.Image); next != key && !target.InSync &&
			target.Err == "" && imageref.IsTag(target.Image) {
			follow(view.Tags, next, target.Image)
		}
	}

	return view
}

// follow adds a tag, keeping the first reference written for it.
func follow(tags map[string]string, key, ref string) {
	if _, ok := tags[key]; !ok {
		tags[key] = ref
	}
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
func classifyTarget(c *protocol.Container, files *Overrides, now time.Time) Target {
	var deployed *Deployed
	if d, ok := files.Deployed[c.Name]; ok {
		deployed = &d
	}

	target := targetOf(deployed, c, currentEntry(files, c.Name, now))

	if !files.Known && !target.InSync {
		return Target{
			Image: c.Image, Command: slices.Clone(c.Command), InSync: true,
			Err: files.Err + "; nothing is changed",
		}
	}

	return target
}

// resolveLabels sets Managed, Kind and Team from the labels: a valid kind
// label other than none manages the container with its team label, if any;
// without a kind label defaultPolicy decides. A label that is not a name
// leaves the container out.
func resolveLabels(policy *Policy, c *protocol.Container) {
	if c.Kind == "" {
		c.Managed, c.Team = policy.rollsUnlabelled(), ""
	} else {
		c.Managed = c.Kind != protocol.KindNone && protocol.ValidKind(c.Kind) &&
			(c.Team == "" || protocol.ValidOwner(c.Team))
	}

	if !c.Managed {
		c.Kind, c.Team = "", ""
	}
}

// notRolled says why a managed container is never rolled; empty when it is.
func notRolled(c *protocol.Container) string {
	switch {
	case !imageref.IsTag(c.Image):
		return "pinned to a digest"
	case c.Linked:
		return "created with links (--link), which a recreate cannot keep"
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

// HoldState is the hold state at now. A file that cannot be read holds the
// host locally: when in doubt the agent does nothing.
func HoldState(files *Overrides, now time.Time) protocol.Hold {
	if !files.Known {
		return protocol.Hold{Local: true, LocalReason: Line(files.Err)}
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

// OverrideKeys says which containers the overrides file has a key for, with
// the default its default lines show (managed.yaml's, else what the container
// was created with), and which lose their default lines. A container the view
// does not show is in neither.
func OverrideKeys(view *View, files *Overrides) (keep map[string]Deployed, drop []string) {
	keep = make(map[string]Deployed, len(view.Containers))
	drop = make([]string, 0, len(view.Containers))

	for i := range view.Containers {
		c := &view.Containers[i]
		if !c.Managed || !c.Overridable {
			drop = append(drop, c.Name)

			continue
		}

		image, command := created(c)
		d := Deployed{Image: image, Command: slices.Clone(command), HasCommand: true}

		if m, ok := files.Deployed[c.Name]; ok {
			d.Image = m.Image

			if m.HasCommand {
				d.Command = slices.Clone(m.Command)
			}
		}

		keep[c.Name] = d
	}

	slices.Sort(drop)

	return keep, drop
}

// DeployState is what a deploy finds of a container: whether it exists,
// whether the agent rolls it to its target (labelled, not linked, the target
// a tag, the roll verb allowed), and whether it runs another image, digest or
// command line than that target. An unlabelled container is not rolled to a
// target: Ansible recreates it, with its labels.
func DeployState(policy *Policy, view *View, name string) (exists, managed, differs bool) {
	c := view.Container(name)
	if c == nil {
		return false, false, false
	}

	t, ok := view.Targets[name]
	if !ok || c.Linked || !imageref.IsTag(t.Image) || !policy.Allows(protocol.VerbRoll) {
		return true, false, false
	}

	if !t.InSync {
		return true, true, true
	}

	rc, ok := view.Checks[imageref.Normalize(t.Image)]

	return true, true, ok && rc.Digest != "" && rc.Digest != c.Digest
}
