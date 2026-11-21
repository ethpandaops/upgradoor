package plan

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Policy is what agent.yaml says the host manages and allows. It is set on
// the host only, never by the controller.
type Policy struct {
	// Verbs are the allowed verbs: any of roll, packages, reboot.
	Verbs []protocol.Verb `yaml:"verbs"`
	// Groups configure the roll groups; a group without an entry is
	// admin-only and nothing in it may be overridden.
	Groups map[string]Group `yaml:"groups"`
	// Containers puts containers without a group label in a group by name;
	// a label wins.
	Containers map[string]MappedContainer `yaml:"containers"`
	// DefaultGroup handles containers no label or Containers entry groups.
	DefaultGroup DefaultGroup `yaml:"defaultGroup"`
	// Packages bounds the packages verb.
	Packages Packages `yaml:"packages"`
}

// Group is the agent.yaml entry of one roll group.
type Group struct {
	// Client is the client software the group's containers run ("geth").
	Client string `yaml:"client"`
	// Owners are the teams that own the group's containers.
	Owners []string `yaml:"owners"`
	// Overrides lists the parts that may be overridden (image, flags); empty
	// means the group always runs what Ansible deployed.
	Overrides []string `yaml:"overrides"`
	// Impact is node, slot or independent; empty derives it from the kinds.
	Impact protocol.Impact `yaml:"impact"`
	// StopTimeout is the stop grace period; zero uses the container's own.
	StopTimeout time.Duration `yaml:"stopTimeout"`
}

// MappedContainer is the group and kind of an unlabelled container.
type MappedContainer struct {
	Group string `yaml:"group"`
	// Kind is el, cl, vc or other (default other).
	Kind protocol.ContainerKind `yaml:"kind"`
}

// DefaultGroup says what happens to the containers of protocol.DefaultGroup.
type DefaultGroup struct {
	// Roll lets them follow new digests of their tag; without it updates
	// are only reported.
	Roll bool `yaml:"roll"`
	// Exclude leaves containers out of everything: names or path.Match patterns.
	Exclude []string `yaml:"exclude"`
}

// Packages bounds the packages verb.
type Packages struct {
	// Blocked are the packages unattended-upgrades must not touch and the
	// agent upgrades under a lease; nothing outside this set is upgraded.
	Blocked []string `yaml:"blocked"`
}

// Allows reports whether the policy allows a verb.
func (p *Policy) Allows(v protocol.Verb) bool { return slices.Contains(p.Verbs, v) }

// ApplyDefaults fills unset fields: the kind of a mapped container is other.
func (p *Policy) ApplyDefaults() {
	for name, m := range p.Containers {
		if m.Kind == "" {
			m.Kind = protocol.KindOther
			p.Containers[name] = m
		}
	}
}

// Validate checks verbs, groups, mapped containers and exclude patterns.
// packages.blocked is checked by host.
func (p *Policy) Validate() error {
	for _, v := range p.Verbs {
		if v != protocol.VerbRoll && v != protocol.VerbPackages && v != protocol.VerbReboot {
			return fmt.Errorf("unknown verb %q", v)
		}
	}

	// Sorted, so a file with several mistakes always names the same one.
	for _, name := range slices.Sorted(maps.Keys(p.Groups)) {
		g := p.Groups[name]
		if err := g.validate(name); err != nil {
			return fmt.Errorf("group %q: %w", name, err)
		}
	}

	for _, name := range slices.Sorted(maps.Keys(p.Containers)) {
		m := p.Containers[name]
		if err := m.validate(name); err != nil {
			return fmt.Errorf("containers: %q: %w", name, err)
		}
	}

	for _, pattern := range p.DefaultGroup.Exclude {
		if _, err := path.Match(pattern, ""); err != nil || pattern == "" {
			return fmt.Errorf("defaultGroup.exclude: %q is not a name or pattern", pattern)
		}
	}

	return nil
}

// group returns a roll group's entry; the zero Group when it is not listed.
func (p *Policy) group(name string) Group { return p.Groups[name] }

func (d *DefaultGroup) excludes(name string) bool {
	return slices.ContainsFunc(d.Exclude, func(pattern string) bool {
		ok, _ := path.Match(pattern, name)

		return ok
	})
}

func (g *Group) mayOverride(part string) bool { return slices.Contains(g.Overrides, part) }

// overridable is what the group lets anybody override; nil when nothing.
func (g *Group) overridable() *protocol.Overridable {
	image, flags := g.mayOverride(protocol.OverrideImage), g.mayOverride(protocol.OverrideFlags)
	if !image && !flags {
		return nil
	}

	return &protocol.Overridable{Image: image, Flags: flags}
}

func (g *Group) validate(name string) error {
	switch {
	case !protocol.ValidGroup(name):
		return errors.New("not a group name ([a-z0-9][a-z0-9._-]{0,62})")
	case protocol.ReservedGroup(name):
		return errors.New("the name is reserved: the default group cannot be configured")
	case g.Impact != "" && !g.Impact.Valid():
		return fmt.Errorf("unknown impact %q (node, slot or independent)", g.Impact)
	case g.Client != "" && !protocol.ValidClient(g.Client):
		return fmt.Errorf("client %q is not a client name ([a-z0-9][a-z0-9._-]{0,62})", g.Client)
	}

	for i, part := range g.Overrides {
		if part != protocol.OverrideImage && part != protocol.OverrideFlags {
			return fmt.Errorf("overrides: unknown part %q (image or flags)", part)
		}

		if slices.Contains(g.Overrides[:i], part) {
			return fmt.Errorf("overrides: %q listed twice", part)
		}
	}

	if g.StopTimeout < 0 {
		return errors.New("stopTimeout is negative")
	}

	if len(g.Owners) > protocol.MaxOwners {
		return fmt.Errorf("more than %d owners", protocol.MaxOwners)
	}

	for i, owner := range g.Owners {
		if !protocol.ValidOwner(owner) {
			return fmt.Errorf("owner %q is not a team name ([a-z0-9][a-z0-9._-]{0,62})", owner)
		}

		if slices.Contains(g.Owners[:i], owner) {
			return fmt.Errorf("owner %q listed twice", owner)
		}
	}

	return nil
}

// containerName is the form Docker gives a container name.
var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (m *MappedContainer) validate(name string) error {
	switch {
	case !containerName.MatchString(name):
		return errors.New("not a container name")
	case !protocol.ValidGroup(m.Group):
		return fmt.Errorf("group %q is not a group name ([a-z0-9][a-z0-9._-]{0,62})", m.Group)
	case protocol.ReservedGroup(m.Group):
		return fmt.Errorf("group %q is reserved", m.Group)
	case protocol.ParseKind(string(m.Kind)) != m.Kind:
		return fmt.Errorf("unknown kind %q (el, cl, vc or other)", m.Kind)
	default:
		return nil
	}
}
