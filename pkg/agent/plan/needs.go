package plan

import (
	"cmp"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Needs returns the host's open needs sorted by ID, none while on hold:
// allowed manual needs (covering the observed need they replace), a target
// or image need per managed container, the blocked packages, the reboot
// need and the actions that are due. PullError is set while a lease's pull of the need's image has just
// failed.
func Needs(policy *Policy, view *View, manual []protocol.Need) []protocol.Need {
	needs := make([]protocol.Need, 0, len(manual)+len(view.Containers)+2)

	if view.Hold.Active() {
		return needs
	}

	manualContainers := make(map[string]struct{}, len(manual))
	manualActions := make(map[string]struct{}, len(manual))
	manualReboot := false

	for i := range manual {
		n := &manual[i]
		if !policy.Allows(n.Kind.Verb()) {
			continue
		}

		switch {
		case n.Kind == protocol.NeedImage && n.Image != nil:
			manualContainers[n.Image.Container] = struct{}{}
		case n.Kind == protocol.NeedReboot && n.Reboot != nil:
			manualReboot = true
		case n.Kind == protocol.NeedAction && n.Action != nil:
			if _, ok := policy.Actions[n.Action.Name]; !ok {
				continue
			}

			manualActions[n.Action.Name] = struct{}{}
		default:
			continue
		}

		needs = append(needs, cloneNeed(n))
	}

	if policy.Allows(protocol.VerbRoll) {
		for i := range view.Containers {
			c := &view.Containers[i]

			// A container the recreate refuses has no need of any kind.
			if c.NotRolled != "" {
				continue
			}

			// A container off its target moves there first; its tag is
			// followed again once it is in sync.
			if t, ok := view.Targets[c.Name]; ok && !t.InSync && c.Managed {
				if n, ok := targetNeed(c, &t, view.Checks); ok {
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

	if pkgs := blockedChanges(policy, view.System); policy.Allows(protocol.VerbPackages) && len(pkgs) > 0 {
		needs = append(needs, protocol.Need{
			ID:       protocol.PackagesNeedID(pkgs),
			Kind:     protocol.NeedPackages,
			Packages: &protocol.PackagesNeed{Packages: pkgs},
		})
	}

	if reasons := rebootReasons(view.System); policy.Allows(protocol.VerbReboot) &&
		len(reasons) > 0 && !manualReboot {
		needs = append(needs, protocol.Need{
			ID:     protocol.RebootNeedID,
			Kind:   protocol.NeedReboot,
			Reboot: &protocol.RebootNeed{Reasons: reasons},
		})
	}

	needs = append(needs, actionNeeds(policy, view, manualActions)...)

	for i := range needs {
		needs[i].PullError = pullError(&needs[i], view.Checks)
	}

	slices.SortFunc(needs, func(a, b protocol.Need) int { return cmp.Compare(a.ID, b.ID) })

	return needs
}

// targetNeed returns the spec or override need that brings a container to
// its target, once the registry has resolved the target's tag.
func targetNeed(
	c *protocol.Container, t *Target, checks map[string]protocol.RegistryCheck,
) (protocol.Need, bool) {
	if t.InSync || t.Err != "" || !imageref.IsTag(t.Image) {
		return protocol.Need{}, false
	}

	rc, ok := checks[imageref.Normalize(t.Image)]
	if !ok || rc.Digest == "" {
		return protocol.Need{}, false
	}

	fromImage := ""
	if !imageref.Same(c.Image, t.Image) {
		fromImage = c.Image
	}

	if t.Spec {
		key := t.specKey(c, rc.Digest)

		return protocol.Need{
			ID: protocol.SpecNeedID(c.Name, key), Kind: protocol.NeedSpec,
			Spec: &protocol.SpecNeed{
				Container: c.Name, Kind: c.Kind, Team: c.Team, Key: key,
				Image: t.Image, FromImage: fromImage, FromDigest: c.Digest,
				ToDigest: rc.Digest, ToRevision: rc.Revision, Flags: t.Flags,
			},
		}, true
	}

	return protocol.Need{
		ID: protocol.OverrideNeedID(c.Name, t.key(rc.Digest)), Kind: protocol.NeedOverride, Local: true,
		Override: &protocol.OverrideNeed{
			Container: c.Name, Image: t.Image, FromImage: fromImage, FromDigest: c.Digest,
			ToDigest: rc.Digest, ToRevision: rc.Revision, Flags: t.Flags, Restore: t.Restore,
		},
	}, true
}

// imageNeed returns a container's image need when its tag points at another
// digest than it runs.
func imageNeed(c *protocol.Container) (protocol.Need, bool) {
	rc := c.Registry
	if !c.Managed || c.NotRolled != "" || rc == nil || rc.Digest == "" || rc.Digest == c.Digest {
		return protocol.Need{}, false
	}

	return protocol.Need{
		ID:   protocol.ImageNeedID(c.Name, rc.Digest),
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: c.Name, Kind: c.Kind, Team: c.Team, Image: c.Image,
			FromDigest: c.Digest, ToDigest: rc.Digest, ToRevision: rc.Revision,
		},
	}, true
}

// pullError is why the last lease could not pull the need's image, while
// the check of its tag says PullFailed for the need's digest.
func pullError(n *protocol.Need, checks map[string]protocol.RegistryCheck) string {
	var ref, digest string

	switch {
	case n.Image != nil:
		ref, digest = n.Image.Image, n.Image.ToDigest
	case n.Spec != nil:
		ref, digest = n.Spec.Image, n.Spec.ToDigest
	case n.Override != nil:
		ref, digest = n.Override.Image, n.Override.ToDigest
	}

	if rc, ok := checks[imageref.Normalize(ref)]; ok && ref != "" && rc.PullFailed && rc.Digest == digest {
		return rc.Error
	}

	return ""
}

func rebootReasons(sys *System) []string {
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

// blockedChanges returns the pending upgrades within the configured blocked
// set, sorted by name; the policy bounds the verb.
func blockedChanges(policy *Policy, sys *System) []protocol.PackageChange {
	if sys == nil {
		return nil
	}

	out := make([]protocol.PackageChange, 0, len(sys.Blocked))

	for _, p := range sys.Blocked {
		if slices.Contains(policy.Packages.Blocked, p.Name) {
			out = append(out, p)
		}
	}

	slices.SortFunc(out, func(a, b protocol.PackageChange) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// CloneNeeds returns a deep copy of a need list; nil stays nil.
func CloneNeeds(in []protocol.Need) []protocol.Need {
	if in == nil {
		return nil
	}

	out := make([]protocol.Need, len(in))
	for i := range in {
		out[i] = cloneNeed(&in[i])
	}

	return out
}

// cloneNeed returns a deep copy, so stored manual needs are never shared.
func cloneNeed(n *protocol.Need) protocol.Need {
	out := *n

	if n.Image != nil {
		img := *n.Image
		out.Image = &img
	}

	if n.Override != nil {
		o := *n.Override
		out.Override = &o
	}

	if n.Spec != nil {
		s := *n.Spec
		out.Spec = &s
	}

	if n.Packages != nil {
		out.Packages = &protocol.PackagesNeed{Packages: slices.Clone(n.Packages.Packages)}
	}

	if n.Reboot != nil {
		out.Reboot = &protocol.RebootNeed{Reasons: slices.Clone(n.Reboot.Reasons)}
	}

	if n.Action != nil {
		a := *n.Action
		out.Action = &a
	}

	return out
}

// Reconciled is the outcome of Reconcile.
type Reconciled struct {
	// Kept are the manual needs that still qualify, image needs updated to
	// the current digests.
	Kept []protocol.Need
	// Dropped are the reasons of the needs that stopped qualifying, by ID.
	Dropped map[string]string
	// Changed reports whether Kept differs from the input.
	Changed bool
}

// Reconcile brings the stored manual needs in line with the host. An image
// need is kept unchanged while the view is not Fresh.
func Reconcile(policy *Policy, view *View, manual []protocol.Need) Reconciled {
	out := Reconciled{
		Kept:    make([]protocol.Need, 0, len(manual)),
		Dropped: make(map[string]string, len(manual)),
	}

	for i := range manual {
		n := cloneNeed(&manual[i])

		switch {
		case !policy.Allows(n.Kind.Verb()):
			out.Dropped[n.ID] = "verb " + string(n.Kind.Verb()) + " is no longer allowed"
		case n.Kind == protocol.NeedReboot && n.Reboot != nil:
			out.Kept = append(out.Kept, n)
		case n.Kind == protocol.NeedAction && n.Action != nil:
			if _, ok := policy.Actions[n.Action.Name]; !ok {
				out.Dropped[n.ID] = "action " + n.Action.Name + " is no longer configured"

				continue
			}

			out.Kept = append(out.Kept, n)
		case n.Kind == protocol.NeedImage && n.Image != nil:
			// A stale container list must not lose a request.
			if !view.Fresh {
				out.Kept = append(out.Kept, n)

				continue
			}

			c := view.Container(n.Image.Container)

			if reason, _ := rollable(c, n.Image.Container); reason != "" {
				out.Dropped[n.ID] = reason

				continue
			}

			if !imageref.Same(c.Image, n.Image.Image) {
				out.Dropped[n.ID] = "container " + c.Name + " moved to another tag"

				continue
			}

			if followTag(n.Image, c) {
				out.Changed = true
			}

			out.Kept = append(out.Kept, n)
		default:
			out.Dropped[n.ID] = "malformed manual need"
		}
	}

	out.Changed = out.Changed || len(out.Dropped) > 0

	return out
}

// Withdrawn returns the IDs of the manual needs that the directive created.
func Withdrawn(manual []protocol.Need, directive string) []string {
	ids := make([]string, 0, 1)

	for i := range manual {
		if directive != "" && manual[i].DirectiveID == directive {
			ids = append(ids, manual[i].ID)
		}
	}

	return ids
}

// followTag moves a manual image need to the running and the tag's current
// digest, so an older roll request is served with the current image.
func followTag(n *protocol.ImageNeed, c *protocol.Container) bool {
	changed := false

	if n.FromDigest != c.Digest {
		n.FromDigest = c.Digest
		changed = true
	}

	// The revision is known once the image is local.
	if rc := c.Registry; rc != nil && rc.Digest != "" &&
		(rc.Digest != n.ToDigest || rc.Revision != n.ToRevision) {
		n.ToDigest, n.ToRevision = rc.Digest, rc.Revision
		changed = true
	}

	return changed
}

// Served returns the IDs of the needs whose step is done or failed: a
// manual request is good for one attempt.
func Served(op *protocol.Operation) []string {
	if op == nil {
		return nil
	}

	out := make([]string, 0, len(op.Steps))

	for _, s := range op.Steps {
		if s.Status == protocol.StepDone || s.Status == protocol.StepFailed {
			out = append(out, s.NeedID)
		}
	}

	return out
}
