package overrides

import (
	"errors"
	"fmt"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Policy is what may be overridden on a container: what its group in
// agent.yaml allows. It bounds the controller, the local commands and a hand
// edit of the file alike; a container whose group allows nothing (tooling)
// runs what Ansible deployed.
type Policy struct {
	// Image allows image overrides.
	Image bool
	// Flags allows flag overrides.
	Flags bool
}

// PolicyOf is what a group of agent.yaml lets anybody override.
func PolicyOf(g *config.Group) Policy {
	return Policy{
		Image: g.MayOverride(protocol.OverrideImage),
		Flags: g.MayOverride(protocol.OverrideFlags),
	}
}

// None reports whether the policy allows nothing.
func (p *Policy) None() bool { return !p.Image && !p.Flags }

// Permits checks the parts of an entry against the policy.
func (p *Policy) Permits(container, group string, entry *Entry) error {
	switch {
	case group == protocol.DefaultGroup && !entry.Empty():
		return fmt.Errorf("container %s cannot be overridden: it is in the default group, "+
			"which allows no overrides", container)
	case p.None() && !entry.Empty():
		return fmt.Errorf("container %s cannot be overridden: its group %q allows no overrides "+
			"(groups.%s.overrides in agent.yaml)", container, group, group)
	case entry.Image != "" && !p.Image:
		return fmt.Errorf("the group %q of container %s does not allow image overrides", group, container)
	case len(entry.Add)+len(entry.Remove) > 0 && !p.Flags:
		return fmt.Errorf("the group %q of container %s does not allow flag overrides", group, container)
	default:
		return nil
	}
}

// Allows applies the policy to an override the controller asks for: the
// parts the group allows and an end within MaxDuration.
func (p *Policy) Allows(container, group string, entry *Entry, now time.Time) error {
	if err := p.Permits(container, group, entry); err != nil {
		return err
	}

	switch {
	case entry.Until == nil:
		return errors.New("an override from the controller needs an end")
	case !entry.Until.After(now):
		return fmt.Errorf("the override's end %s has passed", entry.Until.UTC().Format(time.RFC3339))
	case entry.Until.After(now.Add(MaxDuration + time.Minute)):
		return fmt.Errorf("the override ends more than %s from now", MaxDuration)
	default:
		return nil
	}
}
