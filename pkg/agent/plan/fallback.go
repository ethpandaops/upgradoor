package plan

import (
	"fmt"
	"path"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Fallback defaults and bounds.
const (
	DefaultFallbackAfter    = 24 * time.Hour
	DefaultFallbackInterval = 6 * time.Hour
	minFallbackAfter        = 5 * time.Minute
	minFallbackInterval     = time.Minute
)

// Fallback lets the listed containers follow their tags while the
// controller cannot be reached: the agent pulls their images itself and
// recreates them without a lease. Nothing staggers the hosts then and no
// network health is checked.
type Fallback struct {
	// Containers are the containers it applies to: names or path.Match
	// patterns. Empty turns the fallback off.
	Containers []string `yaml:"containers"`
	// After is how long the controller must have been silent.
	After time.Duration `yaml:"after"`
	// Interval is the time between two pulls of a tag; each host starts at
	// a random point of it.
	Interval time.Duration `yaml:"interval"`
}

// Enabled reports whether any container is listed.
func (f *Fallback) Enabled() bool { return len(f.Containers) > 0 }

func (f *Fallback) applyDefaults() {
	if f.After <= 0 {
		f.After = DefaultFallbackAfter
	}

	if f.Interval <= 0 {
		f.Interval = DefaultFallbackInterval
	}
}

func (f *Fallback) validate() error {
	for _, pattern := range f.Containers {
		if _, err := path.Match(pattern, ""); err != nil || pattern == "" {
			return fmt.Errorf("fallback.containers: %q is not a name or pattern", pattern)
		}
	}

	switch {
	case !f.Enabled():
		return nil
	case f.After < minFallbackAfter:
		return fmt.Errorf("fallback.after is below %s", minFallbackAfter)
	case f.Interval < minFallbackInterval:
		return fmt.Errorf("fallback.interval is below %s", minFallbackInterval)
	default:
		return nil
	}
}

func (f *Fallback) covers(name string) bool {
	return slices.ContainsFunc(f.Containers, func(pattern string) bool {
		ok, _ := path.Match(pattern, name)

		return ok
	})
}

// FallbackTags are the tags the fallback pulls, by normalised reference: the
// tag of every covered container that is rolled, and of its pending target.
func FallbackTags(policy *Policy, view *View) map[string]string {
	tags := make(map[string]string, len(view.Containers))

	if !policy.Allows(protocol.VerbRoll) {
		return tags
	}

	for i := range view.Containers {
		c := &view.Containers[i]
		if !c.Managed || c.NotRolled != "" || !policy.Fallback.covers(c.Name) {
			continue
		}

		follow(tags, imageref.Normalize(c.Image), c.Image)

		if t, ok := view.Targets[c.Name]; ok && !t.InSync && t.Err == "" && imageref.IsTag(t.Image) {
			follow(tags, imageref.Normalize(t.Image), t.Image)
		}
	}

	return tags
}

// FallbackNeeds are the needs the fallback serves without a lease of the
// controller: the image, spec and override needs of covered containers,
// but for one whose pull has just failed. Packages and reboots always wait
// for the controller.
func FallbackNeeds(policy *Policy, needs []protocol.Need) []protocol.Need {
	out := make([]protocol.Need, 0, len(needs))

	for i := range needs {
		n := &needs[i]

		name := ""

		switch {
		case n.Image != nil:
			name = n.Image.Container
		case n.Spec != nil:
			name = n.Spec.Container
		case n.Override != nil:
			name = n.Override.Container
		}

		if name == "" || n.PullError != "" || !policy.Fallback.covers(name) {
			continue
		}

		out = append(out, cloneNeed(n))
	}

	return out
}
