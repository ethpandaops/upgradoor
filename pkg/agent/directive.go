package agent

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// pendingRoll is a roll directive that waits for the registry lookup and
// the pull of its image. It is acknowledged only once that has finished;
// until then the controller keeps repeating the directive. It lives in
// memory only: after a restart the repeated directive starts it again.
type pendingRoll struct {
	container string
	// key is the normalised image reference, ticket the watcher request.
	key    string
	ticket uint64
	// local is true for a roll requested through the local socket: it is
	// not the controller's to withdraw, and its answer goes to the socket.
	local bool
}

// handleDirectives applies every directive of a sync response that has not
// been handled yet, exactly once, and remembers which directives the
// controller is still repeating so that their acks are repeated too.
// It reports whether anything new was handled.
func (s *service) handleDirectives(directives []protocol.Directive, obs *observation) bool {
	s.repeated = s.repeated[:0]
	handledNew := false
	st := s.store.Snapshot()

	listed := make(map[string]struct{}, len(directives))

	for i := range directives {
		d := &directives[i]
		if d.ID == "" {
			s.log.WithField("kind", d.Kind).Warn("ignoring a directive without an ID")

			continue
		}

		if _, dup := listed[d.ID]; dup {
			continue
		}

		listed[d.ID] = struct{}{}
		s.repeated = append(s.repeated, d.ID)

		if st.handled(d.ID) != nil {
			continue
		}

		if _, pending := s.pendingRolls[d.ID]; pending {
			continue
		}

		if s.applyDirective(d, obs) {
			handledNew = true
		}
	}

	// A roll the controller no longer asks for is not carried out.
	for id, p := range s.pendingRolls {
		if _, ok := listed[id]; !ok && !p.local {
			delete(s.pendingRolls, id)
		}
	}

	return handledNew
}

// applyDirective carries out one new directive. It reports whether the
// directive is now handled (a roll that waits for its image is not yet).
func (s *service) applyDirective(d *protocol.Directive, obs *observation) bool {
	log := s.log.WithFields(logrus.Fields{"directive": d.ID, "kind": d.Kind})

	var (
		need   *protocol.Need
		reject string
		err    error
	)

	switch d.Kind {
	case protocol.DirectiveHold:
		err = s.holds.SetController(d.Reason)
		// The directives after this one in the same response are judged
		// against the hold as it is now, not as the cycle first saw it.
		obs.hold = s.holds.Read()
	case protocol.DirectiveReleaseHold:
		// Lifts the controller hold only; the local hold file is not the
		// controller's to remove.
		err = s.holds.ReleaseController()
		obs.hold = s.holds.Read()
	case protocol.DirectiveRecheck:
		s.watcher.RecheckAll()
	case protocol.DirectiveReboot:
		need, reject = s.manualReboot(d, obs)
	case protocol.DirectiveRoll:
		reject = s.rollRefusal(d.Container, obs)
		if reject == "" {
			c := obs.container(d.Container)
			key := NormalizeReference(c.Image)

			s.pendingRolls[d.ID] = pendingRoll{
				container: d.Container,
				key:       key,
				ticket:    s.watcher.Request(key, c.Image),
			}

			log.WithField(logContainer, d.Container).
				Info("manual roll requested: checking the registry and pulling the image")
			s.auditDirective(d, "")

			return false
		}
	case protocol.DirectiveOverride:
		reject = s.overrideDirective(d)
	default:
		reject = fmt.Sprintf("unknown directive kind %q", d.Kind)
	}

	if err != nil {
		// Not applied and not remembered: the repeated directive is tried
		// again.
		log.WithError(err).Error("cannot apply directive")

		return false
	}

	s.auditDirective(d, reject)

	return s.concludeDirective(log, d.ID, need, reject)
}

// actorOf is who the controller says asked for a directive. The answer that
// carried the directive is the controller's (its signature was verified, or
// verification is switched off for development); the user in it is the
// controller's word.
func actorOf(d *protocol.Directive) *origin {
	who := &origin{source: auditController, user: "unknown", directive: d.ID}

	if d.By != nil {
		who.user = cmp.Or(firstLine(d.By.User), "unknown")
		who.teams = protocol.CleanOwners(d.By.Teams)
		who.feature = firstLine(d.By.Feature)
	}

	return who
}

// auditDirective records in the host's audit log what a person asked for
// through the controller and whether the host took it. An override directive
// is recorded where the file is written; a recheck is nobody's request.
func (s *service) auditDirective(d *protocol.Directive, reject string) {
	who := actorOf(d)
	rec := AuditRecord{
		Source: auditController, User: who.user, Teams: who.teams, Feature: who.feature,
		Directive: d.ID, Container: d.Container, Reason: d.Reason, Result: auditAccepted,
	}

	switch d.Kind {
	case protocol.DirectiveRoll:
		rec.Action = "roll.request"
	case protocol.DirectiveReboot:
		rec.Action = "reboot.request"
	case protocol.DirectiveHold:
		rec.Action = "hold.set"
	case protocol.DirectiveReleaseHold:
		rec.Action = "hold.release"
	default:
		return
	}

	if reject != "" {
		rec.Result, rec.Error = auditRefused, reject
	}

	s.audit.record(rec)
}

// overrideDirective writes what an override directive asks for into the
// overrides file, within what this host's configuration lets the controller
// override. It returns the reason of a refusal, or "".
func (s *service) overrideDirective(d *protocol.Directive) string {
	who := actorOf(d)

	var err error

	switch {
	case d.Override == nil || d.Container == "":
		err = refusedf("the override directive names no container or no override")
	case !s.cfg.Allows(protocol.VerbRoll):
		// Recorded like every refusal.
		err = s.changeOverride(who, d.Container, d.Reason,
			func(*Override) (*Override, *Override, error) {
				return nil, nil, refusedf("verb roll is not allowed on this host")
			})
	default:
		err = s.instructOverride(who, d.Container, d.Override, d.Reason)
	}

	if err == nil {
		return ""
	}

	return strings.TrimPrefix(err.Error(), errRefused.Error()+": ")
}

// concludeDirective persists the answer to a directive together with the
// manual need it created, if any.
func (s *service) concludeDirective(
	log logrus.FieldLogger, id string, need *protocol.Need, reject string,
) bool {
	h := handledDirective{ID: id, Status: protocol.DirectiveAccepted}
	if reject != "" {
		h.Status, h.Error = protocol.DirectiveRejected, reject
	}

	err := s.store.Update(func(st *persistedState) {
		if st.handled(id) != nil {
			return
		}

		st.rememberDirective(h)

		if need != nil {
			st.ManualNeeds = append(st.ManualNeeds, *need)
		}
	})
	if err != nil {
		log.WithError(err).Error("cannot persist the answer to a directive; it is handled again")

		return false
	}

	if reject != "" {
		log.WithField("reason", reject).Warn("directive rejected")
	} else {
		log.Info("directive accepted")
	}

	return true
}

// manualReboot answers a reboot directive: a manual reboot need, or the
// reason why not. A directive creates a need; it never reboots by itself.
func (s *service) manualReboot(
	d *protocol.Directive, obs *observation,
) (need *protocol.Need, reject string) {
	switch {
	case !s.cfg.Allows(protocol.VerbReboot):
		return nil, "verb reboot is not allowed on this host"
	case obs.hold.Active():
		return nil, holdReason(obs.hold)
	}

	return &protocol.Need{
		ID:          protocol.ManualNeedID(d.ID),
		Kind:        protocol.NeedReboot,
		DirectiveID: d.ID,
		Reboot:      &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}, ""
}

// rollRefusal returns why a manual roll of a container is refused, or "".
func (s *service) rollRefusal(container string, obs *observation) string {
	switch {
	case !s.cfg.Allows(protocol.VerbRoll):
		return "verb roll is not allowed on this host"
	case obs.hold.Active():
		return holdReason(obs.hold)
	case container == "":
		return "roll directive names no container"
	}

	reason, _ := rollable(s.cfg, obs.container(container), container)

	return reason
}

// resolvePendingRolls turns roll directives whose image is now resolved
// and local into manual image needs, or rejects them.
func (s *service) resolvePendingRolls(obs *observation) {
	for id, p := range s.pendingRolls {
		log := s.log.WithFields(logrus.Fields{
			"directive": id, "kind": protocol.DirectiveRoll, logContainer: p.container,
		})

		if reason := s.rollRefusal(p.container, obs); reason != "" {
			if s.concludeRoll(log, id, &p, nil, reason) {
				delete(s.pendingRolls, id)
			}

			continue
		}

		c := obs.container(p.container)

		check, done, gone := s.watcher.Result(p.key, p.ticket)

		var (
			need   *protocol.Need
			reject string
		)

		switch {
		case gone || NormalizeReference(c.Image) != p.key:
			reject = "container " + p.container + " moved to another image"
		case !done:
			continue
		case check.Digest == "" || !check.Pulled:
			reject = "image " + c.Image + " is not available locally: " +
				cmp.Or(check.Error, "the pull did not bring the registry digest")
		default:
			need = &protocol.Need{
				ID:          protocol.ManualNeedID(id),
				Kind:        protocol.NeedImage,
				DirectiveID: id,
				Image: &protocol.ImageNeed{
					Container:  c.Name,
					Image:      c.Image,
					FromDigest: c.Digest,
					ToDigest:   check.Digest,
					ToRevision: check.Revision,
				},
			}
		}

		if s.concludeRoll(log, id, &p, need, reject) {
			delete(s.pendingRolls, id)
		}
	}
}

// concludeRoll answers a roll whose image is resolved: the directive of the
// controller, or the roll requested on the host.
func (s *service) concludeRoll(
	log logrus.FieldLogger, id string, p *pendingRoll, need *protocol.Need, reject string,
) bool {
	if p.local {
		return s.concludeLocalRoll(id, need, reject)
	}

	return s.concludeDirective(log, id, need, reject)
}

// directiveAcks returns the acks for the directives the controller repeated
// in its last response and that are handled.
func (s *service) directiveAcks(st *persistedState) []protocol.DirectiveAck {
	if len(s.repeated) == 0 {
		return nil
	}

	acks := make([]protocol.DirectiveAck, 0, len(s.repeated))

	for _, id := range s.repeated {
		if h := st.handled(id); h != nil {
			acks = append(acks, protocol.DirectiveAck{ID: h.ID, Status: h.Status, Error: h.Error})
		}
	}

	return acks
}
