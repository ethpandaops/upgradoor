package agent

import (
	"cmp"
	"fmt"

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
	for id := range s.pendingRolls {
		if _, ok := listed[id]; !ok {
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

			return false
		}
	default:
		reject = fmt.Sprintf("unknown directive kind %q", d.Kind)
	}

	if err != nil {
		// Not applied and not remembered: the repeated directive is tried
		// again.
		log.WithError(err).Error("cannot apply directive")

		return false
	}

	return s.concludeDirective(log, d.ID, need, reject)
}

// concludeDirective persists the answer to a directive together with the
// manual need it created, if any.
func (s *service) concludeDirective(
	log logrus.FieldLogger, id string, need *protocol.Need, reject string,
) bool {
	h := handledDirective{ID: id, Status: protocol.DirectiveAccepted, At: s.now()}
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
			if s.concludeDirective(log, id, nil, reason) {
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

		if s.concludeDirective(log, id, need, reject) {
			delete(s.pendingRolls, id)
		}
	}
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
