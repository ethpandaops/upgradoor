package overrides

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Target is what a managed container is to run, by the agent
// configuration and the overrides file, and how the container that is there
// compares.
type Target struct {
	// Spec: agent.yaml names another image or command line than the one the
	// container was created with (before an override changed it).
	Spec bool
	// Image and Command are what the container is to run.
	Image   string
	Command []string
	// Labels are the override labels the container is to carry: a nil value
	// removes the label.
	Labels map[string]*string
	// InSync: the container runs Image with Command.
	InSync bool
	// Flags: the command line changes on the way to the target.
	Flags bool
	// Restore: there is no entry (any more) and the container still runs
	// what one said.
	Restore bool
	// Err says why the entry cannot be applied.
	Err string
}

// TargetOf compares a managed container with what it is to run. image and
// command are what agent.yaml says the container runs; a nil command keeps
// the one the container was created with.
func TargetOf(image string, command []string, c *protocol.Container, entry *Entry) Target {
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

	// What agent.yaml says it is: its image, and its command line where it
	// names one.
	baseCommand := createdCommand
	if command != nil {
		baseCommand = command
	}

	t := Target{
		Image: image, Command: slices.Clone(baseCommand),
		Spec: !imageref.Same(createdImage, image) || !slices.Equal(createdCommand, baseCommand),
	}

	if entry != nil {
		if err := entry.Validate(); err != nil {
			t.Err = err.Error()
			// What cannot be applied changes nothing: the container stays
			// as it is, also when it runs an earlier form of the entry.
			t.Image, t.Command, t.InSync, t.Spec = c.Image, slices.Clone(c.Command), true, false

			return t
		}

		if entry.Image != "" {
			t.Image = entry.Image
		}

		t.Command = applyFlags(baseCommand, entry.Add, entry.Remove)
	}

	t.Labels = map[string]*string{LabelImage: nil, LabelCommand: nil}

	if !imageref.Same(t.Image, image) {
		configured := image
		t.Labels[LabelImage] = &configured
	}

	if !slices.Equal(t.Command, baseCommand) {
		// Encoding a list of strings cannot fail.
		raw, _ := json.Marshal(nonNilStrings(baseCommand))
		encoded := string(raw)
		t.Labels[LabelCommand] = &encoded
	}

	t.Flags = !slices.Equal(c.Command, t.Command)
	t.InSync = imageref.Same(c.Image, t.Image) && !t.Flags
	t.Spec = t.Spec && !t.InSync
	t.Restore = entry == nil && !t.InSync && !t.Spec

	return t
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}

	return in
}

// Key identifies the target together with the digest its image is at: the
// part of an override need's ID that changes when the file asks for
// something else.
func (t *Target) Key(digest string) string {
	h := sha256.New()
	h.Write([]byte(imageref.Normalize(t.Image)))
	h.Write([]byte{0})
	h.Write([]byte(digest))

	for _, arg := range t.Command {
		h.Write([]byte{0})
		h.Write([]byte(arg))
	}

	return hex.EncodeToString(h.Sum(nil)[:6])
}

// SpecKey identifies the target of a spec need across hosts: the image at
// its digest and the arguments the command line gains and loses. Two hosts
// whose rendered command lines differ in per-host values that stay the same
// share it.
func (t *Target) SpecKey(c *protocol.Container, digest string) string {
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

	return hex.EncodeToString(h.Sum(nil)[:6])
}

// argDelta returns the arguments that are in to and not in from, and those
// in from and not in to, each sorted and without repeats.
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

// NeedID is the ID of the need that brings the container to the target with
// its image at digest.
func (t *Target) NeedID(c *protocol.Container, digest string) string {
	if t.Spec {
		return protocol.SpecNeedID(c.Name, t.SpecKey(c, digest))
	}

	return protocol.OverrideNeedID(c.Name, t.Key(digest))
}

// Report is what the sync says about a container's override; nil when the
// container runs what the configuration says and no entry asks otherwise.
func (t *Target) Report(c *protocol.Container, entry *Entry) *protocol.ContainerOverride {
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
		Applied: t.InSync && t.Err == "", Error: t.Err,
	}
}
