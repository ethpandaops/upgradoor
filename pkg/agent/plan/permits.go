package plan

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Origin is where a request to change the overrides file comes from.
type Origin string

// Origins; local also uses them as audit sources.
const (
	// FromController: a directive in a verified answer of the controller.
	FromController Origin = "controller"
	// FromLocal: the CLI socket.
	FromLocal Origin = "local"
)

// Permits checks a change of one container's override before it is
// written: next is the entry to write (nil removes it), asked the part asked
// for now (nil when a part is only taken away). An override from the
// controller needs an end within MaxOverride; one set on the host may have
// none.
func Permits(
	policy *Policy, view *View, container string, next, asked *Entry, origin Origin, now time.Time,
) error {
	c := view.Container(container)
	if c == nil || !c.Managed {
		return Refusal("container %s is not managed on this host", container)
	}

	if next != nil {
		if err := next.Validate(); err != nil {
			return Refusal("%s", err)
		}
	}

	if asked == nil {
		return nil
	}

	g := policy.group(c.Group)
	if err := partsPermitted(&g, c.Group, container, asked); err != nil {
		return Refusal("%s", err)
	}

	if origin != FromController {
		return nil
	}

	switch {
	case asked.Until == nil:
		return Refusal("an override from the controller needs an end")
	case !asked.Until.After(now):
		return Refusal("the override's end %s has passed", asked.Until.UTC().Format(time.RFC3339))
	case asked.Until.After(now.Add(MaxOverride + overrideClockSlack)):
		return Refusal("the override ends more than %s from now", MaxOverride)
	default:
		return nil
	}
}

// overrideClockSlack allows for the controller's clock differing from ours.
const overrideClockSlack = time.Minute

// partsPermitted checks an entry's parts against the group's policy; it
// bounds the controller, the local commands and hand edits alike.
func partsPermitted(g *Group, group, container string, entry *Entry) error {
	switch {
	case entry.Empty():
		return nil
	case group == protocol.DefaultGroup:
		return fmt.Errorf("container %s cannot be overridden: it is in the default group, "+
			"which allows no overrides", container)
	case len(g.Overrides) == 0:
		return fmt.Errorf("container %s cannot be overridden: its group %q allows no overrides "+
			"(groups.%s.overrides in agent.yaml)", container, group, group)
	case entry.Image != "" && !g.mayOverride(protocol.OverrideImage):
		return fmt.Errorf("the group %q of container %s does not allow image overrides", group, container)
	case len(entry.Add)+len(entry.Remove) > 0 && !g.mayOverride(protocol.OverrideFlags):
		return fmt.Errorf("the group %q of container %s does not allow flag overrides", group, container)
	default:
		return nil
	}
}

// Instruct decides an override directive against the current entry prev.
// It returns the entry to write (nil removes it) or a Refusal, checked in
// this order: no container or no override; container not managed; roll verb
// not allowed; unknown part; Permits on what is asked, whose refusal comes
// with the refused entry. The part not named keeps prev's value.
func Instruct(
	policy *Policy, view *View, container string, inst *protocol.OverrideInstruction,
	prev *Entry, by, reason string, now time.Time,
) (*Entry, error) {
	switch c := view.Container(container); {
	case inst == nil || container == "":
		return nil, Refusal("the override directive names no container or no override")
	case c == nil || !c.Managed:
		return nil, Refusal("container %s is not managed on this host", container)
	case !policy.Allows(protocol.VerbRoll):
		return nil, Refusal("verb roll is not allowed on this host")
	}

	next := Entry{By: by, Reason: Line(reason)}
	if prev != nil {
		// Copies: the returned entry must not share the caller's lists.
		next.Image, next.Until = prev.Image, prev.Until
		next.Add, next.Remove = slices.Clone(prev.Add), slices.Clone(prev.Remove)
	}

	asked := &Entry{Until: inst.Until}

	switch inst.Part {
	case protocol.OverrideImage:
		next.Image = strings.TrimSpace(inst.Image)
		asked.Image = next.Image
	case protocol.OverrideFlags:
		next.Add, next.Remove = slices.Clone(inst.AddFlags), slices.Clone(inst.RemoveFlags)
		asked.Add, asked.Remove = next.Add, next.Remove
	default:
		return nil, Refusal("unknown part %q of an override", inst.Part)
	}

	if asked.Empty() {
		// Taking a part away needs no opt-in, and no end.
		asked = nil
	} else {
		next.Until = inst.Until
	}

	entry := &next
	if next.Empty() {
		entry = nil
	}

	if err := Permits(policy, view, container, entry, asked, FromController, now); err != nil {
		return entry, err
	}

	return entry, nil
}

// LocalRequest is an override asked for on the host.
type LocalRequest struct {
	Container string
	Image     string
	Add       []string
	Remove    []string
	Until     *time.Time
	// User is the socket's caller, "local:<name>".
	User   string
	Reason string
	// Clear removes the override.
	Clear bool
}

// LocalEntry decides an override asked for on the host: the entry to write
// (nil for a clear) and Permits' refusal, if any.
func LocalEntry(policy *Policy, view *View, req *LocalRequest, now time.Time) (*Entry, error) {
	var entry *Entry
	if !req.Clear {
		entry = &Entry{
			Image: strings.TrimSpace(req.Image), Add: slices.Clone(req.Add), Remove: slices.Clone(req.Remove),
			By: bareUser(req.User), Reason: Line(req.Reason), Until: req.Until,
		}
	}

	return entry, Permits(policy, view, req.Container, entry, entry, FromLocal, now)
}

// defaultLocalHoldReason is the reason of a local hold set without one.
const defaultLocalHoldReason = "held"

// LocalHold is the local hold a socket caller sets on the host.
func LocalHold(user, reason string) Hold {
	return Hold{By: bareUser(user), Reason: cmp.Or(Line(reason), defaultLocalHoldReason)}
}

func bareUser(user string) string {
	return strings.TrimPrefix(user, string(FromLocal)+":")
}

// HoldSource is the one hold source an origin may write or lift; nothing
// the controller sends reaches the local hold.
func HoldSource(origin Origin) string {
	if origin == FromLocal {
		return HoldLocal
	}

	return HoldController
}

// ControllerHold is the controller hold a hold directive writes; local cuts
// its fields to one line when writing it.
func ControllerHold(reason string, by *protocol.Actor) Hold {
	hold := Hold{By: "controller", Reason: reason}
	if by != nil {
		hold.By, hold.Teams, hold.Feature = by.User, protocol.CleanOwners(by.Teams), by.Feature
	}

	return hold
}
