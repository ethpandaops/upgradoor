package overrides

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The holds of a host live in the overrides file too, below the generated
// block, one entry per source:
//
//	hold:
//	  # [hold:local] 2026-11-20T16:00:00Z local:sam: set: debugging teku
//	  local:
//	    by: "sam"
//	    reason: "debugging teku"
//	    until: 2026-11-21T16:00:00Z      # optional
//	  controller:
//	    by: "pk@example.org"
//	    teams: ["lighthouse"]
//	    feature: "team_hold"
//	    reason: "network split"
//
// A local hold is written by hand or by 'upgradoor host hold'; nothing the
// controller sends lifts it. A controller hold is written by the agent when
// the controller's hold directive arrives and taken out on its release. While
// either is in effect the agent reports no needs and refuses all work.
const (
	keyHold = "hold"

	// HoldLocal and HoldController are the sources of a hold.
	HoldLocal      = "local"
	HoldController = "controller"
)

// Hold is one hold entry.
type Hold struct {
	// By and Reason say who set it and why. For a controller hold By, Teams
	// and Feature are what the controller said about the person.
	By      string
	Reason  string
	Teams   []string
	Feature string
	// Until is when the hold ends by itself; nil for one without an end.
	Until *time.Time
}

// holdYAML is a hold entry as it is written in the file.
type holdYAML struct {
	By      string     `yaml:"by"`
	Reason  string     `yaml:"reason"`
	Teams   []string   `yaml:"teams"`
	Feature string     `yaml:"feature"`
	Until   *time.Time `yaml:"until"`
}

// parseHolds reads the hold mapping of the file.
func parseHolds(set *Set, holds map[string]*holdYAML) error {
	for source, y := range holds {
		if source != HoldLocal && source != HoldController {
			return fmt.Errorf("overrides file: unknown hold %q (local or controller)", source)
		}

		h := Hold{}
		if y != nil {
			h = Hold{
				By: auditlog.Line(y.By), Reason: auditlog.Line(y.Reason), Teams: y.Teams,
				Feature: auditlog.Line(y.Feature), Until: y.Until,
			}
		}

		set.Holds[source] = h
	}

	return nil
}

// HoldOf returns the hold of a source, or nil.
func (s *Set) HoldOf(source string) *Hold {
	if s == nil {
		return nil
	}

	if h, ok := s.Holds[source]; ok {
		return &h
	}

	return nil
}

// Expired reports whether the hold's end has passed.
func (h *Hold) Expired(now time.Time) bool {
	return h != nil && h.Until != nil && !h.Until.After(now)
}

// HoldState is the hold state the file says at now. A file that cannot be
// read holds the host: when in doubt the agent does nothing.
func HoldState(set *Set, now time.Time) protocol.Hold {
	var out protocol.Hold

	if !set.Known {
		return protocol.Hold{Local: true, LocalReason: "the overrides file cannot be read: " + auditlog.Line(set.Err)}
	}

	if h := set.HoldOf(HoldLocal); h != nil && !h.Expired(now) {
		out.Local = true
		out.LocalReason = h.Reason
	}

	if h := set.HoldOf(HoldController); h != nil && !h.Expired(now) {
		out.Controller = true
	}

	return out
}

// SetHold writes the hold of a source (or takes it out, with h nil) and adds
// a history line saying note. Everything else stays as it is.
func (t *Text) SetHold(source string, h *Hold, at time.Time, who, note string) error {
	if source != HoldLocal && source != HoldController {
		return fmt.Errorf("unknown hold %q", source)
	}

	err := t.setEntry(keyHold, source, keyHold+":"+source, at, who, note, func(pad string) []string {
		if h == nil {
			return nil
		}

		return renderHold(source, h, pad)
	})
	if err != nil {
		return err
	}

	set, err := Parse(t.Bytes())
	if err != nil {
		return fmt.Errorf("the change would break the overrides file: %w", err)
	}

	got := set.HoldOf(source)

	switch {
	case (got == nil) != (h == nil):
		return errors.New("the hold did not take in the overrides file")
	case h != nil && (got.Reason != auditlog.Line(h.Reason) || got.By != auditlog.Line(h.By) ||
		!slices.Equal(got.Teams, h.Teams)):
		return errors.New("the hold reads back differently from what was written")
	default:
		return nil
	}
}

// renderHold writes one hold entry.
func renderHold(source string, h *Hold, pad string) []string {
	in := pad + "  "
	out := []string{
		pad + source + ":",
		in + "by: " + jsonString(auditlog.Line(h.By)),
		in + "reason: " + jsonString(auditlog.Line(h.Reason)),
	}

	if len(h.Teams) > 0 {
		out = append(out, in+"teams: "+jsonList(h.Teams))
	}

	if h.Feature != "" {
		out = append(out, in+"feature: "+jsonString(auditlog.Line(h.Feature)))
	}

	if h.Until != nil {
		out = append(out, in+"until: "+h.Until.UTC().Format(time.RFC3339))
	}

	return out
}

// HoldReason is why a host on hold does nothing, as a step or a refusal says
// it.
func HoldReason(h protocol.Hold) string {
	switch {
	case h.Local && h.LocalReason != "":
		return "host is on local hold: " + h.LocalReason
	case h.Local:
		return "host is on local hold"
	default:
		return "host is on controller hold"
	}
}

// The two holds of a host are entries of the overrides file (HoldLocal,
// HoldController), so that whoever logs in sees them where the overrides are.
//
// The local hold is written by a person, by hand or with 'upgradoor host
// hold'. The controller hold is written by the agent on the controller's
// directives. Nothing the controller sends can touch the local hold.

// Hold returns the current hold state. A file that cannot be read counts as
// a hold: when in doubt the agent does nothing.
func (s *keeper) Hold() protocol.Hold {
	return HoldState(Read(s.cfg.OverridesFile), s.now())
}

// SetControllerHold puts the host on controller hold, with who asked as the
// controller states it.
func (s *keeper) SetControllerHold(reason string, by *protocol.Actor) error {
	hold := &Hold{By: "controller", Reason: auditlog.Line(reason)}
	if by != nil {
		hold.By, hold.Teams, hold.Feature = by.User, protocol.CleanOwners(by.Teams), by.Feature
	}

	return s.setHold(HoldController, hold, "controller:"+hold.By, "set: "+hold.Reason)
}

// ReleaseControllerHold lifts the controller hold. It never touches the local
// hold.
func (s *keeper) ReleaseControllerHold() error {
	return s.setHold(HoldController, nil, "controller", "released")
}

// SetLocalHold puts the host on local hold for a person who asked for it on
// the host (the 'upgradoor host hold' command): the same entry a person
// writes by hand.
func (s *keeper) SetLocalHold(user, reason string) error {
	return s.setHold(HoldLocal, &Hold{By: user, Reason: auditlog.Line(reason)},
		user, "set: "+auditlog.Line(reason))
}

// ReleaseLocalHold takes the local hold out.
func (s *keeper) ReleaseLocalHold(user string) error {
	return s.setHold(HoldLocal, nil, user, "released")
}

func (s *keeper) setHold(source string, hold *Hold, who, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if set := Read(s.cfg.OverridesFile); hold == nil && set.Known && set.HoldOf(source) == nil {
		// Nothing to take out.
		return nil
	}

	return s.file.Change(func(t *Text) error {
		return t.SetHold(source, hold, s.now(), who, note)
	})
}
