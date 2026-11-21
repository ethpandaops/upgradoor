// Package overrides is the host's overrides file and what it asks of a
// container.
//
// The overrides file is the only image and flag state on the host: what
// Ansible deployed every managed container as, and the overrides of them:
// another image, added and removed flags, for a while. It is a file in the
// home directory of the entry user so that whoever has SSH can read and edit
// it; the controller stores no override and learns of them from the agent's
// report. What a container is to run is the deployed entry with the override
// applied; a managed container without a deployed entry counts as deployed
// with what it was created with.
//
// Three writers share the file, so it has two parts:
//
//	# >>> upgradoor generated block ... >>>      written by Ansible on every
//	# (how-to)                                     run (or by 'upgradoor host
//	deployed:                                      init-overrides'); the agent
//	  geth:                                        keeps it byte for byte
//	    image: "ethpandaops/geth:master"
//	    command: ["--http"]
//	# <<< upgradoor generated block <<<
//	overrides:                                    written by people and by
//	  # [geth] 2026-11-20T16:00:00Z sam: ...       the agent
//	  geth:
//	    image: ethpandaops/geth:fix-sync
//	    flags:
//	      add: ["--verbosity=4"]
//	      remove: ["--maxpeers"]
//	    by: sam@example.org
//	    reason: debugging the sync
//	    until: 2026-11-21T16:00:00Z
//
// The agent changes the entry of one container at a time and leaves every
// other byte as it is: other entries, comments, blank lines, the order.
// Each change it makes adds one comment line "# [<container>] <time> <who>:
// <what>" above the entry; the last maxHistory of them are kept per
// container.
package overrides

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// overridesBegin and overridesEnd are the first and the last line of the
	// generated block. Only the part before the colon is compared.
	overridesBegin = "# >>> upgradoor generated block"
	overridesEnd   = "# <<< upgradoor generated block"

	overridesBeginLine = overridesBegin + ": rewritten by Ansible on every run, do not edit >>>"
	overridesEndLine   = overridesEnd + " <<<"

	keyOverrides = "overrides"
	keyDeployed  = "deployed"

	// maxHistory bounds the comment lines the agent keeps per
	// container.
	maxHistory = 5
	// maxFileSize bounds the file the agent is willing to read.
	maxFileSize = 1 << 20

	// DefaultDuration and MaxDuration bound an override that
	// is not set by hand: it always has an end.
	DefaultDuration = 24 * time.Hour
	MaxDuration     = 7 * 24 * time.Hour

	maxText = 500

	// None is how the absence of an override is written where one is
	// described.
	None = "nothing"
)

// Labels the agent puts on a container it recreates with an override, so
// that the container itself says what it was before. A container without
// them is in the form the inventory gave it.
const (
	// LabelImage holds the deployed image reference of a container that
	// runs an override image: what the deployed block said when the override
	// was applied. A container whose label (or, without one, image) differs
	// from what the block says now is moved there with a spec need.
	LabelImage = "io.ethpandaops.upgradoor.override.image"
	// LabelCommand holds the command line the container had before a
	// flag override changed it, as a JSON array of strings.
	LabelCommand = "io.ethpandaops.upgradoor.override.command"
)

// Entry is the entry of one container in the overrides file.
type Entry struct {
	// Image replaces the configured image reference; empty keeps it.
	Image string
	// Add and Remove change the command line (applyFlags).
	Add    []string
	Remove []string
	// By and Reason say who set it and why.
	By     string
	Reason string
	// Until is when the override ends; nil for one without an end, which
	// only a hand edit can make.
	Until *time.Time
}

// entryYAML is an entry as it is written in the file.
type entryYAML struct {
	Image string `yaml:"image"`
	Flags struct {
		Add    []string `yaml:"add"`
		Remove []string `yaml:"remove"`
	} `yaml:"flags"`
	By     string     `yaml:"by"`
	Reason string     `yaml:"reason"`
	Until  *time.Time `yaml:"until"`
}

// Deployed is what Ansible deployed one container as: its entry in the
// deployed block.
type Deployed struct {
	Image string
	// Command is the command line (the arguments after the entrypoint) when
	// HasCommand; without it the container keeps the one it was created with.
	Command    []string
	HasCommand bool
}

// deployedYAML is a deployed entry as it is written in the block.
type deployedYAML struct {
	Image   string    `yaml:"image"`
	Command *[]string `yaml:"command"`
}

// Set is the overrides file as last read.
type Set struct {
	// Known is false when the file exists and cannot be read or understood.
	// Nothing is then applied and nothing restored: a file that is broken
	// for a moment must not put every container back.
	Known bool
	// Err says what is wrong with the file.
	Err string
	// Entries are the entries by container name.
	Entries map[string]Entry
	// Deployed are the entries of the deployed block by container name.
	Deployed map[string]Deployed
	// Holds are the holds by source (HoldLocal, HoldController).
	Holds map[string]Hold
}

// DeployedOf returns the deployed entry of a container, or nil.
func (s *Set) DeployedOf(name string) *Deployed {
	if s == nil {
		return nil
	}

	if d, ok := s.Deployed[name]; ok {
		return &d
	}

	return nil
}

// Entry returns the override of a container, or nil.
func (s *Set) Entry(name string) *Entry {
	if s == nil {
		return nil
	}

	if e, ok := s.Entries[name]; ok {
		return &e
	}

	return nil
}

// NewSet is the set of a host without an overrides file.
func NewSet() *Set {
	return &Set{Known: true, Entries: map[string]Entry{}, Deployed: map[string]Deployed{}, Holds: map[string]Hold{}}
}

// Read reads the overrides file. A file that does not exist, is
// empty or holds nothing but the generated block is no overrides.
func Read(file string) *Set {
	raw, err := readFile(file)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return NewSet()
	case err != nil:
		return &Set{Err: err.Error()}
	}

	set, err := Parse(raw)
	if err != nil {
		return &Set{Err: err.Error()}
	}

	return set
}

// Current reads the overrides file as it stands at now: an entry whose end
// has passed counts as gone from that moment on, whether or not it has been
// taken out of the file yet.
func Current(file string, now time.Time) *Set {
	set := Read(file)

	for name, entry := range set.Entries {
		if entry.Expired(now) {
			delete(set.Entries, name)
		}
	}

	return set
}

func readFile(file string) ([]byte, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("overrides file: %w", err)
	}

	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("overrides file %s is larger than %d bytes", file, maxFileSize)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("overrides file: %w", err)
	}

	return raw, nil
}

// Parse reads the content of an overrides file.
func Parse(raw []byte) (*Set, error) {
	var doc struct {
		Deployed  map[string]*deployedYAML `yaml:"deployed"`
		Overrides map[string]*entryYAML    `yaml:"overrides"`
		Hold      map[string]*holdYAML     `yaml:"hold"`
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("overrides file: %w", err)
	}

	set := NewSet()

	if err := parseHolds(set, doc.Hold); err != nil {
		return nil, err
	}

	for name, y := range doc.Deployed {
		if y == nil || strings.TrimSpace(y.Image) == "" {
			return nil, fmt.Errorf("overrides file: deployed entry %q names no image", name)
		}

		d := Deployed{Image: strings.TrimSpace(y.Image)}
		if y.Command != nil {
			d.Command, d.HasCommand = nonNilStrings(*y.Command), true
		}

		set.Deployed[name] = d
	}

	for name, y := range doc.Overrides {
		if y == nil {
			// "geth:" with nothing under it overrides nothing.
			continue
		}

		o := Entry{
			Image: strings.TrimSpace(y.Image), Add: y.Flags.Add, Remove: y.Flags.Remove,
			By: auditlog.Line(y.By), Reason: auditlog.Line(y.Reason), Until: y.Until,
		}

		set.Entries[name] = o
	}

	return set, nil
}

// Empty reports whether the entry changes nothing.
func (o *Entry) Empty() bool {
	return o == nil || (o.Image == "" && len(o.Add) == 0 && len(o.Remove) == 0)
}

// Expired reports whether the entry's end has passed.
func (o *Entry) Expired(now time.Time) bool {
	return o != nil && o.Until != nil && !o.Until.After(now)
}

// Describe says what the entry changes, for a history line and the audit
// log: "image ethpandaops/geth:fix, flags +--verbosity=4 ---maxpeers".
func (o *Entry) Describe() string {
	if o.Empty() {
		return None
	}

	parts := make([]string, 0, 2)

	if o.Image != "" {
		parts = append(parts, "image "+o.Image)
	}

	if len(o.Add)+len(o.Remove) > 0 {
		flags := make([]string, 0, len(o.Add)+len(o.Remove))
		for _, f := range o.Add {
			flags = append(flags, "+"+f)
		}

		for _, f := range o.Remove {
			flags = append(flags, "-"+f)
		}

		parts = append(parts, "flags "+strings.Join(flags, " "))
	}

	return strings.Join(parts, ", ")
}

// Validate checks an entry that is about to be written.
func (o *Entry) Validate() error {
	if o.Empty() {
		return errors.New("an override names an image, flags or both")
	}

	if o.Image != "" {
		if _, err := imageref.Parse(o.Image); err != nil {
			return err
		}

		if !imageref.IsTag(o.Image) {
			return fmt.Errorf("image %s is pinned to a digest; an override follows a tag", o.Image)
		}
	}

	if err := validateFlags(o.Add, o.Remove); err != nil {
		return err
	}

	if len(o.By) > maxText || len(o.Reason) > maxText {
		return errors.New("by or reason is too long")
	}

	return nil
}

// BaseFromLabels reads what a container was before an override from its
// labels; nil when it carries none of them.
func BaseFromLabels(labels map[string]string) *protocol.OverrideBase {
	image := labels[LabelImage]
	raw, hasCommand := labels[LabelCommand]

	if image == "" && !hasCommand {
		return nil
	}

	base := &protocol.OverrideBase{Image: image}

	if hasCommand {
		// A label that does not read as a list of strings is no record.
		if err := json.Unmarshal([]byte(raw), &base.Command); err == nil {
			base.HasCommand = true
		}
	}

	if base.Image == "" && !base.HasCommand {
		return nil
	}

	return base
}
