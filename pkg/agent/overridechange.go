package agent

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// origin says who asked for something that is written into the overrides
// file or the audit log.
type origin struct {
	// source is auditController, auditLocal or auditAgent.
	source string
	// user is who asked: the user the controller named, or "local:<user>".
	user string
	// teams and feature are what the controller said allowed it.
	teams   []string
	feature string
	// directive is the controller's directive, if any.
	directive string
}

// errRefused marks an instruction the host's own rules refuse.
var errRefused = errors.New("refused")

func refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRefused, fmt.Sprintf(format, args...))
}

// currentOverrides reads the overrides file. An entry whose end has passed
// counts as gone from that moment on, whether or not it has been taken out
// of the file yet.
func currentOverrides(file string, now time.Time) *overrideSet {
	set := readOverrides(file)

	for name, entry := range set.entries {
		if entry.expired(now) {
			delete(set.entries, name)
		}
	}

	return set
}

// refreshOverrides reads the overrides file for a cycle: entries whose end
// has passed are taken out of the file (a comment line stays), what a person
// changed by hand since the last look is written into the audit log, and a
// file that cannot be read is said once.
func (s *service) refreshOverrides() *overrideSet {
	s.overrideMu.Lock()
	defer s.overrideMu.Unlock()

	now := s.now()
	set := readOverrides(s.cfg.OverridesFile)

	if !set.known {
		if s.overridesErr != set.err {
			s.log.WithField("file", s.cfg.OverridesFile).Error("cannot read the overrides file (" +
				set.err + "); no override is applied and none is taken back until it reads again")
		}

		s.overridesErr = set.err

		return set
	}

	if s.overridesErr != "" {
		s.log.WithField("file", s.cfg.OverridesFile).Info("the overrides file reads again")

		s.overridesErr = ""
	}

	s.auditHandEdits(set)

	for _, name := range slices.Sorted(maps.Keys(set.entries)) {
		entry := set.entries[name]
		if !entry.expired(now) {
			continue
		}

		delete(set.entries, name)

		err := s.overridesFile.change(func(t *overridesText) error {
			return t.set(name, nil, now, "upgradoor", "expired; was "+entry.describe()+" by "+entry.By)
		})

		rec := AuditRecord{
			Action: "override.expire", Source: auditAgent, Container: name,
			Before: entry.describe(), After: noOverride, User: entry.By, Result: auditAccepted,
		}

		if err != nil {
			// It is gone for the agent all the same: its end has passed.
			rec.Error = "the entry could not be taken out of the file: " + err.Error()

			s.log.WithError(err).WithField(logContainer, name).
				Error("an override has expired and cannot be taken out of the overrides file")
		} else {
			s.log.WithFields(logrus.Fields{logContainer: name, "was": entry.describe()}).
				Info("override expired; the container is put back under the next lease")
		}

		s.audit.record(rec)
	}

	s.seenOverrides = maps.Clone(set.entries)

	return set
}

// auditHandEdits records what changed in the overrides file without the
// agent having written it: a person's edit, or a reset by Ansible. The first
// look after a start only takes stock.
func (s *service) auditHandEdits(set *overrideSet) {
	if s.seenOverrides == nil {
		return
	}

	names := slices.Sorted(maps.Keys(set.entries))

	for name := range s.seenOverrides {
		if _, still := set.entries[name]; !still {
			names = append(names, name)
		}
	}

	for _, name := range names {
		before, was := s.seenOverrides[name]
		after, is := set.entries[name]

		if was == is && before.describe() == after.describe() && sameEnd(before.Until, after.Until) {
			continue
		}

		rec := AuditRecord{
			Action: "override.edit", Source: auditAgent, User: "file:" + after.By, Container: name,
			Before: noOverride, After: noOverride, Reason: after.Reason, Result: auditAccepted,
		}

		if was {
			rec.Before = before.describe()
		}

		if is {
			rec.After = after.describe()
		}

		s.log.WithFields(logrus.Fields{
			logContainer: name, "before": rec.Before, "after": rec.After,
		}).Info("the overrides file was changed by hand")

		s.audit.record(rec)
	}
}

func sameEnd(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}

// setOverride writes (entry) or removes (nil) the override of one container
// in the overrides file, with a comment line that says who changed what, and
// records it in the audit log. A refusal is recorded too. The container is
// brought in line under the next lease; nothing is touched here.
func (s *service) setOverride(who *origin, container string, entry *Override, reason string) error {
	return s.changeOverride(who, container, reason,
		func(*Override) (*Override, *Override, error) { return entry, entry, nil })
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
		func(prev *Override) (entry, asked *Override, err error) {
			next := Override{By: who.user, Reason: firstLine(reason)}
			if prev != nil {
				next.Image, next.Add, next.Remove, next.Until = prev.Image, prev.Add, prev.Remove, prev.Until
			}

			asked = &Override{Until: inst.Until}

			switch inst.Part {
			case protocol.OverrideImage:
				next.Image, asked.Image = strings.TrimSpace(inst.Image), strings.TrimSpace(inst.Image)
			case protocol.OverrideFlags:
				next.Add, next.Remove = slices.Clone(inst.AddFlags), slices.Clone(inst.RemoveFlags)
				asked.Add, asked.Remove = next.Add, next.Remove
			default:
				return nil, nil, refusedf("unknown part %q of an override", inst.Part)
			}

			if asked.empty() {
				// Taking a part away needs no opt-in, and no end.
				asked = nil
			} else {
				next.Until = inst.Until
			}

			if next.empty() {
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
	build func(prev *Override) (entry, asked *Override, err error),
) error {
	err := s.writeOverride(who, container, reason, build)

	if err != nil && !errors.Is(err, errRefused) {
		s.log.WithError(err).WithField(logContainer, container).Error("cannot change the overrides file")
	}

	return err
}

func (s *service) writeOverride(
	who *origin, container, reason string,
	build func(prev *Override) (entry, asked *Override, err error),
) error {
	s.overrideMu.Lock()
	defer s.overrideMu.Unlock()

	now := s.now()
	rec := AuditRecord{
		Action: "override.set", Source: who.source, User: who.user, Teams: who.teams,
		Feature: who.feature, Directive: who.directive, Container: container, Reason: reason,
		Before: noOverride, After: noOverride, Result: auditAccepted,
	}

	refuse := func(err error) error {
		rec.Result = auditRefused
		rec.Error = strings.TrimPrefix(err.Error(), errRefused.Error()+": ")
		s.audit.record(rec)

		return err
	}

	m := s.cfg.Managed(container)
	if m == nil {
		return refuse(refusedf("container %s is not managed on this host", container))
	}

	set := currentOverrides(s.cfg.OverridesFile, now)
	if !set.known {
		return refuse(fmt.Errorf("the overrides file cannot be read: %s", set.err))
	}

	previous := set.entry(container)
	if previous != nil {
		rec.Before = previous.describe()
	}

	entry, asked, err := build(previous)
	if err != nil {
		return refuse(err)
	}

	if entry == nil {
		rec.Action = "override.clear"
	} else {
		rec.After = entry.describe()

		if invalid := entry.validate(); invalid != nil {
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
		s.audit.record(rec)

		return nil
	}

	note := "cleared; was " + rec.Before
	if entry != nil {
		note = "set " + rec.After + "; was " + rec.Before
	}

	err = s.overridesFile.change(func(t *overridesText) error {
		return t.set(container, entry, now, who.user, note)
	})
	if err != nil {
		rec.Result, rec.Error = "failed", err.Error()
		s.audit.record(rec)

		return err
	}

	if s.seenOverrides != nil {
		if entry == nil {
			delete(s.seenOverrides, container)
		} else {
			s.seenOverrides[container] = *entry
		}
	}

	s.audit.record(rec)
	s.log.WithFields(logrus.Fields{
		logContainer: container, "by": who.user, "before": rec.Before, "after": rec.After,
	}).Info("override changed; the container follows under the next lease")
	s.requestSync()

	return nil
}

// allows applies the host's bounds to an override that arrives from the
// controller: the container's opt-in for image overrides with its allowed
// repositories, its opt-in for flag overrides, and an end within
// MaxOverrideDuration. What is asked for on the host itself is not bounded:
// whoever can do that can edit the file.
func (o *origin) allows(m *ManagedContainer, entry *Override, now time.Time) error {
	if o.source != auditController {
		return nil
	}

	switch {
	case entry.Image != "" && len(m.Overrides.Images) == 0:
		return refusedf("this host does not allow image overrides of container %s "+
			"(no overrides.images in its agent.yaml)", m.Name)
	case entry.Image != "" && !imageAllowed(m.Overrides.Images, entry.Image):
		return refusedf("image %s is not in a repository this host allows for container %s (%v)",
			entry.Image, m.Name, m.Overrides.Images)
	case len(entry.Add)+len(entry.Remove) > 0 && !m.Overrides.Flags:
		return refusedf("this host does not allow flag overrides of container %s "+
			"(no overrides.flags in its agent.yaml)", m.Name)
	case entry.Until == nil:
		return refusedf("an override from the controller needs an end")
	case !entry.Until.After(now):
		return refusedf("the override's end %s has passed", entry.Until.UTC().Format(time.RFC3339))
	case entry.Until.After(now.Add(MaxOverrideDuration + time.Minute)):
		return refusedf("the override ends more than %s from now", MaxOverrideDuration)
	default:
		return nil
	}
}

// initOverrides writes the generated block of the overrides file from the
// agent's own configuration and what the containers run, keeping whatever is
// below the block.
func (s *service) initOverrides(containers []GeneratedContainer) error {
	s.overrideMu.Lock()
	defer s.overrideMu.Unlock()

	return s.overridesFile.change(func(t *overridesText) error {
		t.setGeneratedBlock(GeneratedBlock(containers))

		_, err := parseOverrides(t.bytes())

		return err
	})
}
