package plan

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Target is what a managed container is to run by the overrides file, and
// how the running container compares.
type Target struct {
	// Spec: the deployed image or command line differs from what the
	// container was created with; the need is rolled in the group's rollout.
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
	// Refused says why the group's policy does not permit the entry; the
	// container goes back to what was deployed.
	Refused string
}

// targetOf compares a managed container with what it is to run; a nil
// deployed counts as deployed with what the container was created with.
func targetOf(deployed *Deployed, c *protocol.Container, entry *Entry, g *Group, group string) Target {
	if entry.Empty() {
		entry = nil
	}

	// What the container was created as, before an override changed it.
	createdImage, createdCommand := c.Image, c.Command

	if base := c.Base; base != nil {
		if base.Image != "" {
			createdImage = base.Image
		}

		if base.HasCommand {
			createdCommand = base.Command
		}
	}

	image, baseCommand := createdImage, createdCommand

	if deployed != nil {
		image = deployed.Image

		if deployed.HasCommand {
			baseCommand = deployed.Command
		}
	}

	t := Target{
		Image: image, Command: slices.Clone(baseCommand),
		Spec: !imageref.Same(createdImage, image) || !slices.Equal(createdCommand, baseCommand),
	}

	if entry != nil {
		if err := partsPermitted(g, group, c.Name, entry); err != nil {
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

		t.Command = applyFlags(baseCommand, entry.Add, entry.Remove)
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

	return &protocol.ContainerOverride{
		Image: entry.Image, AddFlags: slices.Clone(entry.Add), RemoveFlags: slices.Clone(entry.Remove),
		By: entry.By, Reason: entry.Reason, Until: entry.Until,
		Applied: t.InSync && t.Err == "" && t.Refused == "", Restoring: t.Restore,
		Error: cmp.Or(t.Err, t.Refused),
	}
}
