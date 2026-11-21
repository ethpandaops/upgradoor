package plan

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// Overrides is the host's overrides file: what Ansible deployed each managed
// container as, their overrides, and the holds.
type Overrides struct {
	// Known is false when the file exists but cannot be read. Nothing is
	// then applied or restored and the host is held, so a briefly broken
	// file does not put every container back.
	Known bool
	// Err says what is wrong with the file; one line.
	Err string
	// Deployed is the generated block, by container name.
	Deployed map[string]Deployed
	// Entries are the unexpired overrides, by container name.
	Entries map[string]Entry
	// Holds are the holds by source (HoldLocal, HoldController).
	Holds map[string]Hold
}

// Deployed is what Ansible deployed one container as.
type Deployed struct {
	Image string
	// Command is the arguments after the entrypoint, when HasCommand;
	// otherwise the container keeps the one it was created with.
	Command    []string
	HasCommand bool
}

// Entry is the override of one container.
type Entry struct {
	// Image replaces the deployed image reference; empty keeps it.
	Image string
	// Add and Remove change the command line (applyFlags).
	Add    []string
	Remove []string
	// By and Reason say who set it and why; one line each.
	By     string
	Reason string
	// Until is when the override ends; nil (no end) only from the host.
	Until *time.Time
}

// Hold is one hold entry of the overrides file.
type Hold struct {
	// By, Reason, Teams and Feature say who set it and why.
	By      string
	Reason  string
	Teams   []string
	Feature string
	// Until is when the hold ends by itself; nil for one without an end.
	Until *time.Time
}

// Hold sources: the keys of the hold mapping in the overrides file.
const (
	HoldLocal      = "local"
	HoldController = "controller"
)

// Bounds of an override that is not written by hand.
const (
	// DefaultOverride is how long a local override without --until lasts.
	DefaultOverride = 24 * time.Hour
	// MaxOverride is the longest an override from the controller may last.
	MaxOverride = 7 * 24 * time.Hour
)

// NoOverride describes the absence of an override (history, audit log).
const NoOverride = "nothing"

// ErrRefused marks a request the host's own rules refuse: a rejected
// directive, a socket 409, an audit record "refused".
var ErrRefused = errors.New("refused")

// Refusal is an ErrRefused with the reason the format says.
func Refusal(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Empty reports whether the entry changes nothing (nil included).
func (e *Entry) Empty() bool {
	return e == nil || (e.Image == "" && len(e.Add) == 0 && len(e.Remove) == 0)
}

// Expired reports whether the entry's end has passed at now.
func (e *Entry) Expired(now time.Time) bool {
	return e != nil && e.Until != nil && !e.Until.After(now)
}

// Describe says what the entry changes, e.g. "image ethpandaops/geth:fix,
// flags +--verbosity=4 ---maxpeers"; NoOverride for an empty entry.
func (e *Entry) Describe() string {
	if e.Empty() {
		return NoOverride
	}

	parts := make([]string, 0, 2)

	if e.Image != "" {
		parts = append(parts, "image "+e.Image)
	}

	if len(e.Add)+len(e.Remove) > 0 {
		flags := make([]string, 0, len(e.Add)+len(e.Remove))

		for _, f := range e.Add {
			flags = append(flags, "+"+f)
		}

		for _, f := range e.Remove {
			flags = append(flags, "-"+f)
		}

		parts = append(parts, "flags "+strings.Join(flags, " "))
	}

	return strings.Join(parts, ", ")
}

// maxEntryText bounds By and Reason of an entry that is written.
const maxEntryText = 500

// Validate checks an entry before it is written or applied: it changes
// something, the image is a tag, the flags are well formed and the texts
// are bounded.
func (e *Entry) Validate() error {
	if e.Empty() {
		return errors.New("an override names an image, flags or both")
	}

	if e.Image != "" {
		if _, err := imageref.Parse(e.Image); err != nil {
			return err
		}

		if !imageref.IsTag(e.Image) {
			return fmt.Errorf("image %s is pinned to a digest; an override follows a tag", e.Image)
		}
	}

	if err := validateFlags(e.Add, e.Remove); err != nil {
		return err
	}

	if len(e.By) > maxEntryText || len(e.Reason) > maxEntryText {
		return errors.New("by or reason is too long")
	}

	return nil
}

// Expired reports whether the hold's end has passed at now.
func (h *Hold) Expired(now time.Time) bool {
	return h != nil && h.Until != nil && !h.Until.After(now)
}

// Line cuts free text to one trimmed, valid UTF-8 line of at most 512 bytes.
// Control characters and the Unicode line and paragraph separators (which
// YAML reads as line breaks) become blanks.
func Line(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(s, "?"))
	s = strings.TrimSpace(s)

	if len(s) > maxLine {
		// Cut on a byte boundary; a rune split in two is dropped.
		s = strings.ToValidUTF8(s[:maxLine], "")
	}

	return s
}

const maxLine = 512
