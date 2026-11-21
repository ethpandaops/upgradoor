package plan

import (
	"cmp"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// RollRefusal returns why a manual roll of a container is refused, or "".
func RollRefusal(policy *Policy, view *View, container string) string {
	switch {
	case !policy.Allows(protocol.VerbRoll):
		return "verb roll is not allowed on this host"
	case view.Hold.Active():
		return HoldReason(view.Hold)
	case container == "":
		return "roll directive names no container"
	}

	reason, _ := rollable(view.Container(container), container)

	return reason
}

// RollNeed is the manual image need of a roll once its registry lookup
// (check, nil while pending) has answered, or why the roll is rejected. Both
// are empty while the roll waits. The caller sets DirectiveID or Local.
func RollNeed(
	policy *Policy, view *View, container, ref, id string, check *protocol.RegistryCheck, gone bool,
) (need *protocol.Need, reject string) {
	if reason := RollRefusal(policy, view, container); reason != "" {
		return nil, reason
	}

	c := view.Container(container)

	switch {
	case gone || imageref.Normalize(c.Image) != imageref.Normalize(ref):
		return nil, "container " + container + " moved to another image"
	case check == nil:
		return nil, ""
	case check.Digest == "":
		// Only the lookup counts here; the image is pulled in a pull window.
		return nil, "the registry did not say where " + c.Image + " points: " +
			cmp.Or(check.Error, "no digest")
	}

	return &protocol.Need{
		ID:   id,
		Kind: protocol.NeedImage,
		Image: &protocol.ImageNeed{
			Container: c.Name, Group: c.Group, Client: c.Client, Image: c.Image,
			FromDigest: c.Digest, ToDigest: check.Digest, ToRevision: check.Revision,
		},
	}, ""
}

// ManualReboot answers a reboot directive with a manual reboot need, or why
// not. The directive never reboots by itself.
func ManualReboot(policy *Policy, view *View, directiveID string) (need protocol.Need, reject string) {
	switch {
	case !policy.Allows(protocol.VerbReboot):
		return protocol.Need{}, "verb reboot is not allowed on this host"
	case view.Hold.Active():
		return protocol.Need{}, HoldReason(view.Hold)
	}

	return protocol.Need{
		ID:          protocol.ManualNeedID(directiveID),
		Kind:        protocol.NeedReboot,
		DirectiveID: directiveID,
		Reboot:      &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}, ""
}
