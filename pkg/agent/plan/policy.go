package plan

import (
	"fmt"
	"maps"
	"slices"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Policy is what agent.yaml says the host manages and allows. It is set on
// the host only, never by the controller. Which containers are managed, and
// how, the containers' own labels say (protocol.LabelKind, LabelTeam).
type Policy struct {
	// Verbs are the allowed verbs: any of roll, packages, reboot.
	Verbs []protocol.Verb `yaml:"verbs"`
	// DefaultPolicy is what happens to containers without a kind label.
	DefaultPolicy DefaultPolicy `yaml:"defaultPolicy"`
	// KindPolicies say, by kind, which labelled containers the fallback
	// rolls. They have no effect while the controller answers.
	KindPolicies map[protocol.ContainerKind]KindPolicy `yaml:"kindPolicies"`
	// Packages bounds the packages verb.
	Packages Packages `yaml:"packages"`
	// Fallback is what the host does while the controller cannot be reached.
	Fallback Fallback `yaml:"fallback"`
}

// DefaultPolicy is the policy of unlabelled containers.
type DefaultPolicy string

// Default policies.
const (
	// DefaultIgnore leaves unlabelled containers alone (the default).
	DefaultIgnore DefaultPolicy = "ignore"
	// DefaultRoll lets unlabelled containers follow their tag, rolled with
	// impact independent.
	DefaultRoll DefaultPolicy = "roll"
)

// KindPolicy is the fallback's policy of one kind.
type KindPolicy struct {
	// Roll lets the fallback roll the containers of the kind.
	Roll bool `yaml:"roll"`
}

// Packages bounds the packages verb.
type Packages struct {
	// Blocked are the packages unattended-upgrades must not touch and the
	// agent upgrades under a lease; nothing outside this set is upgraded.
	Blocked []string `yaml:"blocked"`
}

// Allows reports whether the policy allows a verb.
func (p *Policy) Allows(v protocol.Verb) bool { return slices.Contains(p.Verbs, v) }

// ApplyDefaults fills unset fields: the default policy and the fallback's
// times.
func (p *Policy) ApplyDefaults() {
	if p.DefaultPolicy == "" {
		p.DefaultPolicy = DefaultIgnore
	}

	p.Fallback.applyDefaults()
}

// Validate checks verbs, the default policy, the kind policies and the
// fallback. packages.blocked is checked by host.
func (p *Policy) Validate() error {
	for _, v := range p.Verbs {
		if v != protocol.VerbRoll && v != protocol.VerbPackages && v != protocol.VerbReboot {
			return fmt.Errorf("unknown verb %q", v)
		}
	}

	if p.DefaultPolicy != DefaultIgnore && p.DefaultPolicy != DefaultRoll {
		return fmt.Errorf("defaultPolicy: unknown policy %q (ignore or roll)", p.DefaultPolicy)
	}

	// Sorted, so a file with several mistakes always names the same one.
	for _, kind := range slices.Sorted(maps.Keys(p.KindPolicies)) {
		if !protocol.ValidKind(kind) || kind == protocol.KindNone {
			return fmt.Errorf("kindPolicies: %q is not a kind ([a-z0-9][a-z0-9._-]{0,62}, not none)", kind)
		}
	}

	return p.Fallback.validate(p.fallbackRolls())
}

// rollsUnlabelled reports whether unlabelled containers are managed.
func (p *Policy) rollsUnlabelled() bool { return p.DefaultPolicy == DefaultRoll }

// fallbackRolls reports whether the fallback rolls anything: a kind policy
// with roll, or unlabelled containers by defaultPolicy.
func (p *Policy) fallbackRolls() bool {
	return p.rollsUnlabelled() || slices.ContainsFunc(slices.Collect(maps.Values(p.KindPolicies)),
		func(k KindPolicy) bool { return k.Roll })
}

// fallbackCovers reports whether the fallback rolls a managed container: a
// labelled one when its kind's policy says roll, an unlabelled one by
// defaultPolicy.
func (p *Policy) fallbackCovers(c *protocol.Container) bool {
	if c.Kind == "" {
		return p.rollsUnlabelled()
	}

	return p.KindPolicies[c.Kind].Roll
}
