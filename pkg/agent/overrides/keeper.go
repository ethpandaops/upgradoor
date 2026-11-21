package overrides

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// logContainer is the log field naming a container.
const logContainer = "container"

// Keeper is the host's overrides file as the agent keeps it: it reads it for
// every cycle, takes out what has expired, records hand edits in the audit
// log, and writes the overrides and holds that the controller and the local
// commands ask for, within the bounds of agent.yaml. It serializes every
// reader and writer of the file (the loop, the operation, the local socket).
type Keeper interface {
	// Refresh reads the overrides file for a cycle.
	Refresh() *Set
	// Err is why the file could not be read when the loop last looked; empty
	// when it could.
	Err() string
	// SetOverride writes (entry) or removes (nil) the override of a container.
	SetOverride(who *Origin, container string, entry *Entry, reason string) error
	// Instruct applies an override instruction of the controller.
	Instruct(who *Origin, container string, inst *protocol.OverrideInstruction, reason string) error
	// Change changes the entry of a container as build says.
	Change(who *Origin, container, reason string, build Builder) error
	// Init writes the generated block from what the containers run.
	Init(containers []GeneratedContainer) error

	// Hold returns the current hold state.
	Hold() protocol.Hold
	// SetControllerHold puts the host on controller hold.
	SetControllerHold(reason string, by *protocol.Actor) error
	// ReleaseControllerHold lifts the controller hold.
	ReleaseControllerHold() error
	// SetLocalHold puts the host on local hold for a person on the host.
	SetLocalHold(user, reason string) error
	// ReleaseLocalHold takes the local hold out.
	ReleaseLocalHold(user string) error
}

// Builder is given the entry of a container that is there (nil for none)
// and returns the entry to write (nil to remove it) and what of it is being
// asked for now, which is what the host's bounds are applied to.
type Builder func(prev *Entry) (entry, asked *Entry, err error)

// keeper is the Keeper.
type keeper struct {
	log   logrus.FieldLogger
	cfg   *config.Config
	audit auditlog.Log
	now   func() time.Time
	// managed is the roll group of a container that the last cycle saw
	// managed; false for one it did not.
	managed func(name string) (string, bool)
	// changed tells the sync loop that an override changed.
	changed func()

	file *File
	// mu serializes the readers and writers of the file. seen and err are
	// what the loop last saw of it.
	mu   sync.Mutex
	seen map[string]Entry
	err  string
}

var _ Keeper = (*keeper)(nil)

// NewKeeper returns the keeper of the overrides file that cfg names.
// managed says of a container whether the last cycle saw it managed, and in
// which group; changed is called when an override was written.
func NewKeeper(
	log logrus.FieldLogger, cfg *config.Config, audit auditlog.Log, now func() time.Time,
	managed func(name string) (string, bool), changed func(),
) Keeper {
	return newKeeper(log, cfg, audit, now, managed, changed)
}

func newKeeper(
	log logrus.FieldLogger, cfg *config.Config, audit auditlog.Log, now func() time.Time,
	managed func(name string) (string, bool), changed func(),
) *keeper {
	return &keeper{
		log: log, cfg: cfg, audit: audit, now: now, managed: managed, changed: changed,
		file: NewFile(cfg.OverridesFile),
	}
}

// Origin says who asked for something that is written into the overrides
// file or the audit log.
type Origin struct {
	// Source is auditlog.SourceController, SourceLocal or SourceAgent.
	Source string
	// User is who asked: the user the controller named, or "local:<user>".
	User string
	// Teams and Feature are what the controller said allowed it.
	Teams   []string
	Feature string
	// Directive is the controller's directive, if any.
	Directive string
}

// Refresh reads the overrides file for a cycle: entries whose end has
// passed are taken out of the file (a comment line stays), what a person
// changed by hand since the last look is written into the audit log, and a
// file that cannot be read is said once.
func (s *keeper) Refresh() *Set {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	set := Read(s.cfg.OverridesFile)

	if !set.Known {
		if s.err != set.Err {
			s.log.WithField("file", s.cfg.OverridesFile).Error("cannot read the overrides file (" +
				set.Err + "); no override is applied and none is taken back until it reads again")
		}

		s.err = set.Err

		return set
	}

	if s.err != "" {
		s.log.WithField("file", s.cfg.OverridesFile).Info("the overrides file reads again")

		s.err = ""
	}

	s.auditHandEdits(set)

	for _, name := range slices.Sorted(maps.Keys(set.Entries)) {
		entry := set.Entries[name]
		if !entry.Expired(now) {
			continue
		}

		delete(set.Entries, name)

		err := s.file.Change(func(t *Text) error {
			return t.Set(name, nil, now, "upgradoor", "expired; was "+entry.Describe()+" by "+entry.By)
		})

		rec := auditlog.Record{
			Action: "override.expire", Source: auditlog.SourceAgent, Container: name,
			Before: entry.Describe(), After: None, User: entry.By, Result: auditlog.Accepted,
		}

		if err != nil {
			// It is gone for the agent all the same: its end has passed.
			rec.Error = "the entry could not be taken out of the file: " + err.Error()

			s.log.WithError(err).WithField(logContainer, name).
				Error("an override has expired and cannot be taken out of the overrides file")
		} else {
			s.log.WithFields(logrus.Fields{logContainer: name, "was": entry.Describe()}).
				Info("override expired; the container is put back under the next lease")
		}

		s.audit.Record(rec)
	}

	s.seen = maps.Clone(set.Entries)

	return set
}

// Err is why the file could not be read when the loop last looked.
func (s *keeper) Err() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.err
}

// auditHandEdits records what changed in the overrides file without the
// agent having written it: a person's edit, or a reset by Ansible. The first
// look after a start only takes stock.
func (s *keeper) auditHandEdits(set *Set) {
	if s.seen == nil {
		return
	}

	names := slices.Sorted(maps.Keys(set.Entries))

	for name := range s.seen {
		if _, still := set.Entries[name]; !still {
			names = append(names, name)
		}
	}

	for _, name := range names {
		before, was := s.seen[name]
		after, is := set.Entries[name]

		if was == is && before.Describe() == after.Describe() && sameEnd(before.Until, after.Until) {
			continue
		}

		rec := auditlog.Record{
			Action: "override.edit", Source: auditlog.SourceAgent, User: "file:" + after.By, Container: name,
			Before: None, After: None, Reason: after.Reason, Result: auditlog.Accepted,
		}

		if was {
			rec.Before = before.Describe()
		}

		if is {
			rec.After = after.Describe()
		}

		s.log.WithFields(logrus.Fields{
			logContainer: name, "before": rec.Before, "after": rec.After,
		}).Info("the overrides file was changed by hand")

		s.audit.Record(rec)
	}
}

func sameEnd(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// SetOverride writes (entry) or removes (nil) the override of one container
// in the overrides file, with a comment line that says who changed what, and
// records it in the audit log. A refusal is recorded too. The container is
// brought in line under the next lease; nothing is touched here.
func (s *keeper) SetOverride(who *Origin, container string, entry *Entry, reason string) error {
	return s.Change(who, container, reason,
		func(*Entry) (*Entry, *Entry, error) { return entry, entry, nil })
}

// Instruct applies an override instruction of the controller: it
// sets or takes away one part of the container's entry (the image, or the
// flags) and leaves the other part as it is. The host's bounds are applied
// to what the instruction asks for, not to what a person wrote into the
// other part by hand.
func (s *keeper) Instruct(
	who *Origin, container string, inst *protocol.OverrideInstruction, reason string,
) error {
	return s.Change(who, container, reason,
		func(prev *Entry) (entry, asked *Entry, err error) {
			next := Entry{By: who.User, Reason: auditlog.Line(reason)}
			if prev != nil {
				next.Image, next.Add, next.Remove, next.Until = prev.Image, prev.Add, prev.Remove, prev.Until
			}

			asked = &Entry{Until: inst.Until}

			switch inst.Part {
			case protocol.OverrideImage:
				next.Image, asked.Image = strings.TrimSpace(inst.Image), strings.TrimSpace(inst.Image)
			case protocol.OverrideFlags:
				next.Add, next.Remove = slices.Clone(inst.AddFlags), slices.Clone(inst.RemoveFlags)
				asked.Add, asked.Remove = next.Add, next.Remove
			default:
				return nil, nil, auditlog.Refusal("unknown part %q of an override", inst.Part)
			}

			if asked.Empty() {
				// Taking a part away needs no opt-in, and no end.
				asked = nil
			} else {
				next.Until = inst.Until
			}

			if next.Empty() {
				return nil, asked, nil
			}

			return &next, asked, nil
		})
}

// Change changes the entry of one container under the lock of the overrides
// file, as build says.
func (s *keeper) Change(who *Origin, container, reason string, build Builder) error {
	err := s.writeOverride(who, container, reason, build)

	if err != nil && !errors.Is(err, auditlog.ErrRefused) {
		s.log.WithError(err).WithField(logContainer, container).Error("cannot change the overrides file")
	}

	return err
}

func (s *keeper) writeOverride(who *Origin, container, reason string, build Builder) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	rec := auditlog.Record{
		Action: "override.set", Source: who.Source, User: who.User, Teams: who.Teams,
		Feature: who.Feature, Directive: who.Directive, Container: container, Reason: reason,
		Before: None, After: None, Result: auditlog.Accepted,
	}

	refuse := func(err error) error {
		rec.Result = auditlog.Refused
		rec.Error = strings.TrimPrefix(err.Error(), auditlog.ErrRefused.Error()+": ")
		s.audit.Record(rec)

		return err
	}

	group, managed := s.managed(container)
	if !managed {
		return refuse(auditlog.Refusal("container %s is not managed on this host", container))
	}

	set := Current(s.cfg.OverridesFile, now)
	if !set.Known {
		return refuse(fmt.Errorf("the overrides file cannot be read: %s", set.Err))
	}

	previous := set.Entry(container)
	if previous != nil {
		rec.Before = previous.Describe()
	}

	entry, asked, err := build(previous)
	if err != nil {
		return refuse(err)
	}

	if entry == nil {
		rec.Action = "override.clear"
	} else {
		rec.After = entry.Describe()

		if invalid := entry.Validate(); invalid != nil {
			return refuse(auditlog.Refusal("%s", invalid.Error()))
		}
	}

	if asked != nil {
		g := s.cfg.Group(group)
		if denied := who.allows(container, group, &g, asked, now); denied != nil {
			return refuse(denied)
		}
	}

	if entry == nil && previous == nil {
		rec.Error = "there was no override"
		s.audit.Record(rec)

		return nil
	}

	note := "cleared; was " + rec.Before
	if entry != nil {
		note = "set " + rec.After + "; was " + rec.Before
	}

	err = s.file.Change(func(t *Text) error {
		return t.Set(container, entry, now, who.User, note)
	})
	if err != nil {
		rec.Result, rec.Error = "failed", err.Error()
		s.audit.Record(rec)

		return err
	}

	if s.seen != nil {
		if entry == nil {
			delete(s.seen, container)
		} else {
			s.seen[container] = *entry
		}
	}

	s.audit.Record(rec)
	s.log.WithFields(logrus.Fields{
		logContainer: container, "by": who.User, "before": rec.Before, "after": rec.After,
	}).Info("override changed; the container follows under the next lease")
	s.changed()

	return nil
}

// allows applies the host's bounds to an override: what the container's
// group lets anybody override (a tooling group nothing) and, for one that
// arrives from the controller, an end within MaxDuration. What is
// asked for on the host itself may run without an end.
func (o *Origin) allows(container, group string, g *config.Group, entry *Entry, now time.Time) error {
	policy := PolicyOf(g)

	check := policy.Permits(container, group, entry)
	if o.Source == auditlog.SourceController {
		check = policy.Allows(container, group, entry, now)
	}

	if check != nil {
		return auditlog.Refusal("%s", check)
	}

	return nil
}

// Init writes the generated block of the overrides file from the agent's own
// configuration and what the containers run, keeping whatever is below the
// block.
func (s *keeper) Init(containers []GeneratedContainer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.file.Change(func(t *Text) error {
		t.SetGeneratedBlock(GeneratedBlock(containers))

		_, err := Parse(t.Bytes())

		return err
	})
}
