package agent

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// origin says who asked for something that is written into the overrides
// file or the audit log.
type origin struct {
	// source is auditlog.SourceController, SourceLocal or SourceAgent.
	source string
	// user is who asked: the user the controller named, or "local:<user>".
	user string
	// teams and feature are what the controller said allowed it.
	teams   []string
	feature string
	// directive is the controller's directive, if any.
	directive string
}

func refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", auditlog.ErrRefused, fmt.Sprintf(format, args...))
}

// refreshOverrides reads the overrides file for a cycle: entries whose end
// has passed are taken out of the file (a comment line stays), what a person
// changed by hand since the last look is written into the audit log, and a
// file that cannot be read is said once.
func (s *service) refreshOverrides() *overrides.Set {
	s.overrideMu.Lock()
	defer s.overrideMu.Unlock()

	now := s.now()
	set := overrides.Read(s.cfg.OverridesFile)

	if !set.Known {
		if s.overridesErr != set.Err {
			s.log.WithField("file", s.cfg.OverridesFile).Error("cannot read the overrides file (" +
				set.Err + "); no override is applied and none is taken back until it reads again")
		}

		s.overridesErr = set.Err

		return set
	}

	if s.overridesErr != "" {
		s.log.WithField("file", s.cfg.OverridesFile).Info("the overrides file reads again")

		s.overridesErr = ""
	}

	s.auditHandEdits(set)

	for _, name := range slices.Sorted(maps.Keys(set.Entries)) {
		entry := set.Entries[name]
		if !entry.Expired(now) {
			continue
		}

		delete(set.Entries, name)

		err := s.overridesFile.Change(func(t *overrides.Text) error {
			return t.Set(name, nil, now, "upgradoor", "expired; was "+entry.Describe()+" by "+entry.By)
		})

		rec := auditlog.Record{
			Action: "override.expire", Source: auditlog.SourceAgent, Container: name,
			Before: entry.Describe(), After: overrides.None, User: entry.By, Result: auditlog.Accepted,
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

	s.seenOverrides = maps.Clone(set.Entries)

	return set
}

// auditHandEdits records what changed in the overrides file without the
// agent having written it: a person's edit, or a reset by Ansible. The first
// look after a start only takes stock.
func (s *service) auditHandEdits(set *overrides.Set) {
	if s.seenOverrides == nil {
		return
	}

	names := slices.Sorted(maps.Keys(set.Entries))

	for name := range s.seenOverrides {
		if _, still := set.Entries[name]; !still {
			names = append(names, name)
		}
	}

	for _, name := range names {
		before, was := s.seenOverrides[name]
		after, is := set.Entries[name]

		if was == is && before.Describe() == after.Describe() && sameEnd(before.Until, after.Until) {
			continue
		}

		rec := auditlog.Record{
			Action: "override.edit", Source: auditlog.SourceAgent, User: "file:" + after.By, Container: name,
			Before: overrides.None, After: overrides.None, Reason: after.Reason, Result: auditlog.Accepted,
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

// setOverride writes (entry) or removes (nil) the override of one container
// in the overrides file, with a comment line that says who changed what, and
// records it in the audit log. A refusal is recorded too. The container is
// brought in line under the next lease; nothing is touched here.
func (s *service) setOverride(who *origin, container string, entry *overrides.Entry, reason string) error {
	return s.changeOverride(who, container, reason,
		func(*overrides.Entry) (*overrides.Entry, *overrides.Entry, error) { return entry, entry, nil })
}

// instructOverride applies an override instruction of the controller: it
// sets or takes away one part of the container's entry (the image, or the
// flags) and leaves the other part as it is. The host's bounds are applied
// to what the instruction asks for, not to what a person wrote into the
// other part by hand.
func (s *service) instructOverride(
	who *origin, container string, inst *protocol.OverrideInstruction, reason string,
) error {
	return s.changeOverride(who, container, reason,
		func(prev *overrides.Entry) (entry, asked *overrides.Entry, err error) {
			next := overrides.Entry{By: who.user, Reason: auditlog.Line(reason)}
			if prev != nil {
				next.Image, next.Add, next.Remove, next.Until = prev.Image, prev.Add, prev.Remove, prev.Until
			}

			asked = &overrides.Entry{Until: inst.Until}

			switch inst.Part {
			case protocol.OverrideImage:
				next.Image, asked.Image = strings.TrimSpace(inst.Image), strings.TrimSpace(inst.Image)
			case protocol.OverrideFlags:
				next.Add, next.Remove = slices.Clone(inst.AddFlags), slices.Clone(inst.RemoveFlags)
				asked.Add, asked.Remove = next.Add, next.Remove
			default:
				return nil, nil, refusedf("unknown part %q of an override", inst.Part)
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

// changeOverride changes the entry of one container under the lock of the
// overrides file. build is given the entry that is there (nil for none) and
// returns the entry to write (nil to remove it) and what of it is being
// asked for now, which is what the host's bounds are applied to.
func (s *service) changeOverride(
	who *origin, container, reason string,
	build func(prev *overrides.Entry) (entry, asked *overrides.Entry, err error),
) error {
	err := s.writeOverride(who, container, reason, build)

	if err != nil && !errors.Is(err, auditlog.ErrRefused) {
		s.log.WithError(err).WithField(logContainer, container).Error("cannot change the overrides file")
	}

	return err
}

func (s *service) writeOverride(
	who *origin, container, reason string,
	build func(prev *overrides.Entry) (entry, asked *overrides.Entry, err error),
) error {
	s.overrideMu.Lock()
	defer s.overrideMu.Unlock()

	now := s.now()
	rec := auditlog.Record{
		Action: "override.set", Source: who.source, User: who.user, Teams: who.teams,
		Feature: who.feature, Directive: who.directive, Container: container, Reason: reason,
		Before: overrides.None, After: overrides.None, Result: auditlog.Accepted,
	}

	refuse := func(err error) error {
		rec.Result = auditlog.Refused
		rec.Error = strings.TrimPrefix(err.Error(), auditlog.ErrRefused.Error()+": ")
		s.audit.Record(rec)

		return err
	}

	m := s.cfg.Managed(container)
	if m == nil {
		return refuse(refusedf("container %s is not managed on this host", container))
	}

	set := overrides.Current(s.cfg.OverridesFile, now)
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
			return refuse(refusedf("%s", invalid.Error()))
		}
	}

	if asked != nil {
		if denied := who.allows(m, asked, now); denied != nil {
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

	err = s.overridesFile.Change(func(t *overrides.Text) error {
		return t.Set(container, entry, now, who.user, note)
	})
	if err != nil {
		rec.Result, rec.Error = "failed", err.Error()
		s.audit.Record(rec)

		return err
	}

	if s.seenOverrides != nil {
		if entry == nil {
			delete(s.seenOverrides, container)
		} else {
			s.seenOverrides[container] = *entry
		}
	}

	s.audit.Record(rec)
	s.log.WithFields(logrus.Fields{
		logContainer: container, "by": who.user, "before": rec.Before, "after": rec.After,
	}).Info("override changed; the container follows under the next lease")
	s.requestSync()

	return nil
}

// allows applies the host's bounds to an override that arrives from the
// controller: the container's opt-in for image overrides with its allowed
// repositories, its opt-in for flag overrides, and an end within
// overrides.MaxDuration. What is asked for on the host itself is not bounded:
// whoever can do that can edit the file.
func (o *origin) allows(m *ManagedContainer, entry *overrides.Entry, now time.Time) error {
	if o.source != auditlog.SourceController {
		return nil
	}

	if err := m.Overrides.Allows(m.Name, entry, now); err != nil {
		return refusedf("%s", err)
	}

	return nil
}

// initOverrides writes the generated block of the overrides file from the
// agent's own configuration and what the containers run, keeping whatever is
// below the block.
func (s *service) initOverrides(containers []overrides.GeneratedContainer) error {
	s.overrideMu.Lock()
	defer s.overrideMu.Unlock()

	return s.overridesFile.Change(func(t *overrides.Text) error {
		t.SetGeneratedBlock(overrides.GeneratedBlock(containers))

		_, err := overrides.Parse(t.Bytes())

		return err
	})
}
