package scheduler

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// A roll group is what Ansible labels a set of containers with
// (protocol.LabelGroup): a beacon node and its validator client, an
// execution client, a proxy and its companions. The image and spec needs of
// one group and client are one rollout, whatever moved in it: the new digests
// of the group's tags and what a soft deploy changed. A host's needs of the
// group are served together, by one lease: one canary, one readiness check,
// one halt for the group.

// prefixGroup starts the name of a group (GroupName) and the key of its
// rollouts.
const prefixGroup = "group:"

// GroupName names the rollouts of one roll group of one client, whatever
// they move it to. A host's broken mark for the rollouts carries the name.
func GroupName(group, client string) string {
	if client == "" {
		return prefixGroup + group
	}

	return prefixGroup + group + "#" + client
}

// DefaultGroupName names the rollouts of one image of the default group
// (protocol.DefaultGroup): its containers have nothing in common but their
// image, so all hosts' needs of one image are one rollout.
func DefaultGroupName(image string) string {
	return prefixGroup + protocol.DefaultGroup + "/" + imageref.Normalize(image)
}

// groupNameOf is GroupName, or DefaultGroupName for the default group.
func groupNameOf(group, client, image string) string {
	if group == protocol.DefaultGroup {
		return DefaultGroupName(image)
	}

	return GroupName(group, client)
}

// ContainerGroupName names the rollouts that roll a managed container.
func ContainerGroupName(c *protocol.Container) string {
	return groupNameOf(c.Group, c.Client, c.Image)
}

// rolloutGroupName names the rollouts of a group rollout's group.
func rolloutGroupName(r *fleet.Rollout) string {
	image := ""
	if len(r.Targets) > 0 {
		image = r.Targets[0].Image
	}

	return groupNameOf(r.Group, r.Client, image)
}

// needGroupName is the group name of an image or spec need; empty for
// another need.
func needGroupName(n *protocol.Need) string {
	switch {
	case n.Kind == protocol.NeedImage && n.Image != nil:
		return groupNameOf(n.Image.Group, n.Image.Client, n.Image.Image)
	case n.Kind == protocol.NeedSpec && n.Spec != nil:
		return groupNameOf(n.Spec.Group, n.Spec.Client, n.Spec.Image)
	default:
		return ""
	}
}

// isGroupNeed reports whether an observed need is served in a group rollout.
func isGroupNeed(n *protocol.Need) bool {
	return n.DirectiveID == "" && !isLocal(n) && needGroupName(n) != ""
}

// targetOf is the part of a group's target an image or spec need asks for.
func targetOf(n *protocol.Need) fleet.RolloutTarget {
	if s := n.Spec; s != nil {
		return fleet.RolloutTarget{
			Image: s.Image, Digest: s.ToDigest, Revision: s.ToRevision,
			Spec: s.Key, Container: s.Container, Flags: s.Flags,
		}
	}

	return fleet.RolloutTarget{Image: n.Image.Image, Digest: n.Image.ToDigest, Revision: n.Image.ToRevision}
}

// targetID identifies a part of a target: a spec by its key, a tag by its
// normalized reference (the digest is what it moves to).
func targetID(t *fleet.RolloutTarget) string {
	if t.Spec != "" {
		return "spec " + t.Spec
	}

	return "image " + imageref.Normalize(t.Image)
}

// groupTarget is what the hosts' needs of one group and client ask for.
type groupTarget struct {
	group, client string
	// parts by targetID; checked is, per tag, the freshest registry check
	// that named its digest.
	parts   map[string]fleet.RolloutTarget
	checked map[string]time.Time
}

// add offers one need. For a tag the freshest registry check wins; on a tie
// the smaller digest.
func (g *groupTarget) add(n *protocol.Need, checked time.Time) {
	t := targetOf(n)
	id := targetID(&t)

	cur, ok := g.parts[id]
	if ok && t.Spec == "" && t.Digest != cur.Digest {
		c := checked.Compare(g.checked[id])
		if c < 0 || (c == 0 && t.Digest > cur.Digest) {
			return
		}
	} else if ok {
		return
	}

	g.parts[id] = t
	g.checked[id] = checked
}

// sorted returns the parts in the order of their IDs.
func (g *groupTarget) sorted() []fleet.RolloutTarget {
	out := make([]fleet.RolloutTarget, 0, len(g.parts))
	for _, id := range sortedKeys(g.parts) {
		out = append(out, g.parts[id])
	}

	return out
}

// covers reports whether the rollout's target holds the part: the same spec,
// or the same tag at the same digest.
func covers(r *fleet.Rollout, t *fleet.RolloutTarget) bool {
	id := targetID(t)

	return slices.ContainsFunc(r.Targets, func(o fleet.RolloutTarget) bool {
		return targetID(&o) == id && (t.Spec != "" || o.Digest == t.Digest)
	})
}

// groupKey is the rollout key of a group's target.
func groupKey(group, client string, targets []fleet.RolloutTarget) string {
	parts := make([]string, 0, len(targets))
	for i := range targets {
		parts = append(parts, targetID(&targets[i])+"@"+targets[i].Digest)
	}

	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))

	return groupNameOf(group, client, targets[0].Image) + "@" + hex.EncodeToString(sum[:8])
}

// groupTitle says what a group rollout does, for people.
func groupTitle(group, client string, targets []fleet.RolloutTarget) string {
	parts := make([]string, 0, len(targets))

	for i := range targets {
		t := &targets[i]
		part := t.Image + " to " + shortDigest(t.Digest)

		if t.Spec != "" {
			part = t.Container + " to " + part
			if t.Flags {
				part += ", command line changed"
			}
		}

		parts = append(parts, part)
	}

	title := cmp.Or(group, "(no group)") + ": " + strings.Join(parts, "; ")
	if client != "" {
		title = client + " " + title
	}

	return title
}

// groupRollout is the rollout of a group's target.
func groupRollout(g *groupTarget) *fleet.Rollout {
	targets := g.sorted()

	return &fleet.Rollout{
		Kind:    fleet.RolloutGroup,
		Key:     groupKey(g.group, g.client, targets),
		Title:   groupTitle(g.group, g.client, targets),
		Group:   g.group,
		Client:  g.client,
		Targets: targets,
	}
}

// supersedeGroup closes a group rollout whose group the hosts now ask more
// of, or something else: a tag of the group moved again, another tag of it
// moved, Ansible changed another container of it. The rollout that follows
// has the combined target of what the hosts ask for.
func (p *pass) supersedeGroup(r *fleet.Rollout, rep *reportedNeeds) {
	g := rep.groups[rolloutGroupName(r)]
	if g == nil {
		return
	}

	for _, id := range sortedKeys(g.parts) {
		t := g.parts[id]
		if !covers(r, &t) {
			p.closeRollout(r, fleet.RolloutSuperseded, "the group's target changed: "+groupTitle(r.Group, "", []fleet.RolloutTarget{t}))

			return
		}
	}
}

// assignGroupNeeds sorts the group needs of one host to the open rollouts of
// their groups: all needs of a group or none, so that one lease serves the
// whole group. A host that asks for a part the rollout does not hold (it
// has not heard of the newest digest yet) waits until it asks for the same.
func (p *pass) assignGroupNeeds(name string, all []protocol.Need, open map[string]*fleet.Rollout) {
	byGroup := make(map[string][]protocol.Need, 2)

	for i := range all {
		if isGroupNeed(&all[i]) {
			g := needGroupName(&all[i])
			byGroup[g] = append(byGroup[g], all[i])
		}
	}

	for g, needs := range byGroup {
		r := open[g]
		if r == nil {
			continue
		}

		if slices.ContainsFunc(needs, func(n protocol.Need) bool {
			t := targetOf(&n)

			return !covers(r, &t)
		}) {
			continue
		}

		p.addNeeds(r, name, needs)
	}
}

// addNeeds records needs of a host as served by a rollout.
func (p *pass) addNeeds(r *fleet.Rollout, host string, needs []protocol.Need) {
	byHost := p.needs[r.ID]
	if byHost == nil {
		byHost = make(map[string][]protocol.Need, len(p.names))
		p.needs[r.ID] = byHost
	}

	byHost[host] = append(byHost[host], needs...)
}

// groupContainer reports whether the host runs a managed container of the
// rollout's group and client on one of its tags, at the tag's target digest
// (onTarget) or at another.
func groupContainer(r *fleet.Rollout, hv *hostView, onTarget bool) bool {
	if hv.h.Status == nil {
		return false
	}

	for i := range hv.h.Status.Containers {
		c := &hv.h.Status.Containers[i]
		if !c.Managed || c.Group != r.Group || c.Client != r.Client {
			continue
		}

		for j := range r.Targets {
			t := &r.Targets[j]
			if t.Spec == "" && imageref.Same(c.Image, t.Image) && (c.Digest == t.Digest) == onTarget {
				return true
			}
		}
	}

	return false
}
