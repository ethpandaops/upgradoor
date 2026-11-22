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
func Permits(view *View, container string, next, asked *Entry, origin Origin, now time.Time) error {
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

	if err := overridable(c, asked); err != nil {
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

// overridable checks that a container may take an entry: only a labelled
// container with a team may be overridden, image and command line alike. It
// bounds the controller, the local commands and hand edits alike.
func overridable(c *protocol.Container, entry *Entry) error {
	switch {
	case entry.Empty():
		return nil
	case c.Kind == "":
		return fmt.Errorf("container %s cannot be overridden: it has no %s label", c.Name, protocol.LabelKind)
	case c.Team == "":
		return fmt.Errorf("container %s cannot be overridden: it has no %s label", c.Name, protocol.LabelTeam)
	default:
		return nil
	}
}

// Instruct decides an override directive against the current entry prev
// and the container's deployed entry (nil when it has none). It returns the
// entry to write (nil removes it) or a Refusal, checked in this order: no
// container or no override; container not managed; roll verb not allowed;
// unknown part; malformed flags; Permits on what is asked, whose refusal
// comes with the refused entry. The part not named keeps prev's value. Flags
// are applied to the default command line, and the result is the entry's
// command.
func Instruct(
	policy *Policy, view *View, container string, inst *protocol.OverrideInstruction,
	prev *Entry, deployed *Deployed, by, reason string, now time.Time,
) (*Entry, error) {
	c := view.Container(container)

	switch {
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
		next.Command, next.HasCommand = slices.Clone(prev.Command), prev.HasCommand
	}

	asked := &Entry{Until: inst.Until}

	switch inst.Part {
	case protocol.OverrideImage:
		next.Image = strings.TrimSpace(inst.Image)
		asked.Image = next.Image
	case protocol.OverrideFlags:
		if err := validateFlags(inst.AddFlags, inst.RemoveFlags); err != nil {
			return nil, Refusal("%s", err)
		}

		next.Command, next.HasCommand = nil, false

		if len(inst.AddFlags)+len(inst.RemoveFlags) > 0 {
			next.Command = applyFlags(defaultCommand(deployed, c), inst.AddFlags, inst.RemoveFlags)
			next.HasCommand = true
		}

		asked.Command, asked.HasCommand = next.Command, next.HasCommand
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

	if err := Permits(view, container, entry, asked, FromController, now); err != nil {
		return entry, err
	}

	return entry, nil
}

// LocalRequest is an override asked for on the host.
type LocalRequest struct {
	Container string
	Image     string
	// Add and Remove change the default command line (applyFlags).
	Add    []string
	Remove []string
	Until  *time.Time
	// User is the socket's caller, "local:<name>".
	User   string
	Reason string
	// Clear removes the override.
	Clear bool
}

// LocalEntry decides an override asked for on the host against the
// container's deployed entry (nil when it has none): the entry to write (nil
// for a clear) and the refusal of malformed flags or of Permits, if any.
// Flags are applied to the default command line, and the result is the
// entry's command.
func LocalEntry(view *View, req *LocalRequest, deployed *Deployed, now time.Time) (*Entry, error) {
	var entry *Entry
	if !req.Clear {
		entry = &Entry{
			Image: strings.TrimSpace(req.Image), By: bareUser(req.User), Reason: Line(req.Reason), Until: req.Until,
		}

		if c := view.Container(req.Container); c != nil && len(req.Add)+len(req.Remove) > 0 {
			if err := validateFlags(req.Add, req.Remove); err != nil {
				return nil, Refusal("%s", err)
			}

			entry.Command, entry.HasCommand = applyFlags(defaultCommand(deployed, c), req.Add, req.Remove), true
		}
	}

	return entry, Permits(view, req.Container, entry, entry, FromLocal, now)
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
