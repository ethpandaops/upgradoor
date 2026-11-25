package plan

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"unicode/utf8"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Target is what a managed container is to run by the overrides file, and
// how the running container compares.
type Target struct {
	// Spec: the deployed image or command line differs from what the
	// container was created with; the need is rolled in the rollout of the
	// container's team and kind.
	Spec bool
	// Image and Command are what the container is to run.
	Image   string
	Command []string
	// Base is what the replacement's override labels record; nil removes them.
	Base *protocol.OverrideBase
	// InSync: the container runs Image with Command.
	InSync bool
	// Flags: the command line changes on the way to the target.
	Flags bool
	// Restore: there is no entry and the container still runs what one said.
	Restore bool
	// Err says why the entry cannot be applied; nothing is changed then.
	Err string
	// Refused says why the container may not be overridden; it goes back to
	// what was deployed.
	Refused string

	// defaultCommand is the command line without an override.
	defaultCommand []string
}

// targetOf compares a managed container with what it is to run; a nil
// deployed counts as deployed with what the container was created with.
func targetOf(deployed *Deployed, c *protocol.Container, entry *Entry) Target {
	if entry.Empty() {
		entry = nil
	}

	createdImage, createdCommand := created(c)
	image, baseCommand := createdImage, defaultCommand(deployed, c)

	if deployed != nil {
		image = deployed.Image
	}

	t := Target{
		Image: image, Command: slices.Clone(baseCommand), defaultCommand: slices.Clone(baseCommand),
		Spec: !imageref.Same(createdImage, image) || !slices.Equal(createdCommand, baseCommand),
	}

	if entry != nil {
		if err := overridable(c, entry); err != nil {
			t.Refused, entry = err.Error(), nil
		}
	}

	if entry != nil {
		if err := entry.Validate(); err != nil {
			// An invalid entry changes nothing, also when the container runs
			// an earlier form of it.
			t.Err = err.Error()
			t.Image, t.Command, t.InSync, t.Spec = c.Image, slices.Clone(c.Command), true, false

			return t
		}

		if entry.Image != "" {
			t.Image = entry.Image
		}

		if entry.HasCommand {
			t.Command = slices.Clone(entry.Command)
		}
	}

	var base protocol.OverrideBase

	if !imageref.Same(t.Image, image) {
		base.Image = image
	}

	if !slices.Equal(t.Command, baseCommand) {
		// Never nil: the label records an empty command line as [].
		base.Command, base.HasCommand = append([]string{}, baseCommand...), true
	}

	if base.Image != "" || base.HasCommand {
		t.Base = &base
	}

	t.Flags = !slices.Equal(c.Command, t.Command)
	t.InSync = imageref.Same(c.Image, t.Image) && !t.Flags
	t.Spec = t.Spec && !t.InSync
	t.Restore = entry == nil && !t.InSync && !t.Spec

	return t
}

// created is what the container was created as, before an override changed it.
func created(c *protocol.Container) (image string, command []string) {
	image, command = c.Image, c.Command

	if base := c.Base; base != nil {
		if base.Image != "" {
			image = base.Image
		}

		if base.HasCommand {
			command = base.Command
		}
	}

	return image, command
}

// defaultCommand is the command line a container runs without an override:
// the deployed one, else the one it was created with.
func defaultCommand(deployed *Deployed, c *protocol.Container) []string {
	if deployed != nil && deployed.HasCommand {
		return deployed.Command
	}

	_, command := created(c)

	return command
}

// keyBytes is how much of the hash a target key keeps.
const keyBytes = 6

// key identifies the target at the image digest; part of an override need's ID.
func (t *Target) key(digest string) string {
	h := sha256.New()
	h.Write([]byte(imageref.Normalize(t.Image)))
	h.Write([]byte{0})
	h.Write([]byte(digest))

	for _, arg := range t.Command {
		h.Write([]byte{0})
		h.Write([]byte(arg))
	}

	return hex.EncodeToString(h.Sum(nil)[:keyBytes])
}

// specKey identifies a spec need's target across hosts by the image digest
// and the arguments gained and lost, so per-host arguments that stay the same
// do not split it.
func (t *Target) specKey(c *protocol.Container, digest string) string {
	added, removed := argDelta(c.Command, t.Command)

	h := sha256.New()
	h.Write([]byte(imageref.Normalize(t.Image)))
	h.Write([]byte{0})
	h.Write([]byte(digest))

	for _, part := range [][]string{added, removed} {
		h.Write([]byte{1})

		for _, arg := range part {
			h.Write([]byte{0})
			h.Write([]byte(arg))
		}
	}

	return hex.EncodeToString(h.Sum(nil)[:keyBytes])
}

// argDelta returns the arguments only in to and only in from, sorted and unique.
func argDelta(from, to []string) (added, removed []string) {
	missing := func(a, b []string) []string {
		out := make([]string, 0, len(a))

		for _, arg := range a {
			if !slices.Contains(b, arg) {
				out = append(out, arg)
			}
		}

		slices.Sort(out)

		return slices.Compact(out)
	}

	return missing(to, from), missing(from, to)
}

// Bounds of a spec need's command delta, per side: the arguments it names,
// the bytes of one argument and of all of them.
const (
	maxDeltaArgs   = 16
	maxDeltaArgLen = 4 << 10
	maxDeltaBytes  = 8 << 10
)

// specCommand is how a spec need changes the command line: argDelta, which
// the spec key covers, so every host of a key reports the same delta. An
// argument longer than maxDeltaArgLen is cut; one that does not fit the
// side's bytes is left out with every one after it. Nil when nothing changes.
func specCommand(from, to []string) *protocol.CommandDelta {
	added, removed := argDelta(from, to)
	if len(added)+len(removed) == 0 {
		return nil
	}

	d := &protocol.CommandDelta{}
	d.Added = boundArgs(added, &d.Truncated)
	d.Removed = boundArgs(removed, &d.Truncated)

	return d
}

// boundArgs keeps args within the bounds of one side of a delta, setting
// truncated when it cuts or leaves out any.
func boundArgs(args []string, truncated *bool) []string {
	out := make([]string, 0, min(len(args), maxDeltaArgs))
	size := 0

	for i, arg := range args {
		if len(arg) > maxDeltaArgLen {
			arg = cutArg(arg, maxDeltaArgLen)
			*truncated = true
		}

		if i == maxDeltaArgs || size+len(arg) > maxDeltaBytes {
			*truncated = true

			break
		}

		size += len(arg)
		out = append(out, arg)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// cutArg shortens arg to at most limit bytes on a rune boundary, ending in
// protocol.CommandDeltaCut.
func cutArg(arg string, limit int) string {
	n := limit - len(protocol.CommandDeltaCut)
	for n > 0 && !utf8.RuneStart(arg[n]) {
		n--
	}

	return arg[:n] + protocol.CommandDeltaCut
}

func (t *Target) needID(c *protocol.Container, digest string) string {
	if t.Spec {
		return protocol.SpecNeedID(c.Name, t.specKey(c, digest))
	}

	return protocol.OverrideNeedID(c.Name, t.key(digest))
}

// report is the sync's override report; nil when nothing is overridden or restored.
func (t *Target) report(c *protocol.Container, entry *Entry) *protocol.ContainerOverride {
	if entry.Empty() {
		if !t.Restore {
			return nil
		}

		out := &protocol.ContainerOverride{Restoring: true}
		if !imageref.Same(c.Image, t.Image) {
			out.Image = c.Image
		}

		return out
	}

	// The controller sees a command as the arguments it adds to and removes
	// from the default.
	var added, removed []string
	if entry.HasCommand {
		added, removed = commandDelta(t.defaultCommand, entry.Command)
	}

	return &protocol.ContainerOverride{
		Image: entry.Image, AddFlags: added, RemoveFlags: removed,
		By: entry.By, Reason: entry.Reason, Until: entry.Until,
		Applied: t.InSync && t.Err == "" && t.Refused == "", Restoring: t.Restore,
		Error: cmp.Or(t.Err, t.Refused),
	}
}
