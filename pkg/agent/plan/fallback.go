package plan

import (
	"fmt"
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

// Fallback lets managed containers follow their tags while the controller
// cannot be reached, as kindPolicies and defaultPolicy say: the agent pulls
// their images itself and recreates them without a lease. Nothing staggers
// the hosts then and no network health is checked.
type Fallback struct {
	// After is how long the controller must have been silent.
	After time.Duration `yaml:"after"`
	// Interval is the time between two pulls of a tag; each host starts at
	// a random point of it.
	Interval time.Duration `yaml:"interval"`
}

// FallbackEnabled reports whether the fallback rolls anything.
func (p *Policy) FallbackEnabled() bool { return p.fallbackRolls() }

func (f *Fallback) applyDefaults() {
	if f.After <= 0 {
		f.After = DefaultFallbackAfter
	}

	if f.Interval <= 0 {
		f.Interval = DefaultFallbackInterval
	}
}

func (f *Fallback) validate(enabled bool) error {
	switch {
	case !enabled:
		return nil
	case f.After < minFallbackAfter:
		return fmt.Errorf("fallback.after is below %s", minFallbackAfter)
	case f.Interval < minFallbackInterval:
		return fmt.Errorf("fallback.interval is below %s", minFallbackInterval)
	default:
		return nil
	}
}

// FallbackTags are the tags the fallback pulls, by normalised reference: the
// tag of every container it rolls, and of its pending target.
func FallbackTags(policy *Policy, view *View) map[string]string {
	tags := make(map[string]string, len(view.Containers))

	if !policy.Allows(protocol.VerbRoll) {
		return tags
	}

	for i := range view.Containers {
		c := &view.Containers[i]
		if !c.Managed || c.NotRolled != "" || !policy.fallbackCovers(c) {
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
// controller: the image, spec and override needs of the containers it
// rolls, but for one whose pull has just failed. Packages and reboots always
// wait for the controller.
func FallbackNeeds(policy *Policy, view *View, needs []protocol.Need) []protocol.Need {
	out := make([]protocol.Need, 0, len(needs))

	for i := range needs {
		n := &needs[i]

		c := view.Container(n.Container())
		if c == nil || !c.Managed || n.PullError != "" || !policy.fallbackCovers(c) {
			continue
		}

		out = append(out, cloneNeed(n))
	}

	return out
}
