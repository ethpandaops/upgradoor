// Package directives applies the controller's directives, each exactly once:
// holds, rechecks, manual reboots and rolls, and overrides. A roll waits for
// the registry lookup of its image first, like a roll requested on the host.
package directives

import (
	"cmp"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/agent/imagewatch"
	"github.com/ethpandaops/upgradoor/pkg/agent/journal"
	"github.com/ethpandaops/upgradoor/pkg/agent/needs"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/imageref"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// logContainer is the log field naming a container.
const logContainer = "container"

// Handler applies the directives of the controller's answers and follows the
// rolls that wait for their image. It belongs to the sync loop.
type Handler interface {
	// Handle applies every directive of a sync response that has not been
	// handled yet, exactly once. It reports whether anything new was handled.
	Handle(directives []protocol.Directive, obs *needs.Observation) bool
	// AwaitLocalRoll starts a roll requested on the host: it waits for the
	// registry lookup and the pull of its image like a roll directive. It
	// returns why the roll is refused, or "".
	AwaitLocalRoll(id, container string, obs *needs.Observation) string
	// Resolve turns the rolls whose image is now resolved and local into
	// manual image needs, or rejects them.
	Resolve(obs *needs.Observation)
	// Pending reports whether a roll waits for its image.
	Pending() bool
	// Acks returns the acks for the directives the controller repeated in its
	// last response and that are handled.
	Acks(st *journal.State) []protocol.DirectiveAck
}

// Deps are what the handler works with.
type Deps struct {
	Log       logrus.FieldLogger
	Config    *config.Config
	Store     journal.Store
	Overrides overrides.Keeper
	Watcher   imagewatch.Watcher
	Audit     auditlog.Log
	// ConcludeLocalRoll answers a roll requested on the host whose image is
	// resolved: it persists its need, or rejects it with the reason. It
	// reports whether the roll is answered.
	ConcludeLocalRoll func(id string, need *protocol.Need, reject string) bool
}

// handler is the Handler.
type handler struct {
	log       logrus.FieldLogger
	cfg       *config.Config
	store     journal.Store
	overrides overrides.Keeper
	watcher   imagewatch.Watcher
	audit     auditlog.Log
	// concludeLocalRoll is Deps.ConcludeLocalRoll.
	concludeLocalRoll func(id string, need *protocol.Need, reject string) bool

	// repeated are the directive IDs of the last response.
	repeated     []string
	pendingRolls map[string]pendingRoll
}

var _ Handler = (*handler)(nil)

// New returns the handler.
func New(d *Deps) Handler {
	return &handler{
		log:               d.Log,
		cfg:               d.Config,
		store:             d.Store,
		overrides:         d.Overrides,
		watcher:           d.Watcher,
		audit:             d.Audit,
		concludeLocalRoll: d.ConcludeLocalRoll,
		pendingRolls:      make(map[string]pendingRoll, 4),
	}
}

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

// Handle applies every directive of a sync response that has not been
// handled yet, exactly once, and remembers which directives the controller
// is still repeating so that their acks are repeated too. It reports whether
// anything new was handled.
func (s *handler) Handle(directives []protocol.Directive, obs *needs.Observation) bool {
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

		if st.Directive(d.ID) != nil {
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
func (s *handler) applyDirective(d *protocol.Directive, obs *needs.Observation) bool {
	log := s.log.WithFields(logrus.Fields{"directive": d.ID, "kind": d.Kind})

	var (
		need   *protocol.Need
		reject string
		err    error
	)

	switch d.Kind {
	case protocol.DirectiveHold:
		err = s.overrides.SetControllerHold(d.Reason, d.By)
		// The directives after this one in the same response are judged
		// against the hold as it is now, not as the cycle first saw it.
		obs.Hold = s.overrides.Hold()
	case protocol.DirectiveReleaseHold:
		// Lifts the controller hold only; the local hold is not the
		// controller's to remove.
		err = s.overrides.ReleaseControllerHold()
		obs.Hold = s.overrides.Hold()
	case protocol.DirectiveRecheck:
		s.watcher.RecheckAll()
	case protocol.DirectiveReboot:
		need, reject = s.manualReboot(d, obs)
	case protocol.DirectiveRoll:
		reject = s.rollRefusal(d.Container, obs)
		if reject == "" {
			c := obs.Container(d.Container)
			key := imageref.Normalize(c.Image)

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
func actorOf(d *protocol.Directive) *overrides.Origin {
	who := &overrides.Origin{Source: auditlog.SourceController, User: "unknown", Directive: d.ID}

	if d.By != nil {
		who.User = cmp.Or(auditlog.Line(d.By.User), "unknown")
		who.Teams = protocol.CleanOwners(d.By.Teams)
		who.Feature = auditlog.Line(d.By.Feature)
	}

	return who
}

// auditDirective records in the host's audit log what a person asked for
// through the controller and whether the host took it. An override directive
// is recorded where the file is written; a recheck is nobody's request.
func (s *handler) auditDirective(d *protocol.Directive, reject string) {
	who := actorOf(d)
	rec := auditlog.Record{
		Source: auditlog.SourceController, User: who.User, Teams: who.Teams, Feature: who.Feature,
		Directive: d.ID, Container: d.Container, Reason: d.Reason, Result: auditlog.Accepted,
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
		rec.Result, rec.Error = auditlog.Refused, reject
	}

	s.audit.Record(rec)
}

// overrideDirective writes what an override directive asks for into the
// overrides file, within what this host's configuration lets the controller
// override. It returns the reason of a refusal, or "".
func (s *handler) overrideDirective(d *protocol.Directive) string {
	who := actorOf(d)

	var err error

	switch {
	case d.Override == nil || d.Container == "":
		err = auditlog.Refusal("the override directive names no container or no override")
	case !s.cfg.Allows(protocol.VerbRoll):
		// Recorded like every refusal.
		err = s.overrides.Change(who, d.Container, d.Reason,
			func(*overrides.Entry) (*overrides.Entry, *overrides.Entry, error) {
				return nil, nil, auditlog.Refusal("verb roll is not allowed on this host")
			})
	default:
		err = s.overrides.Instruct(who, d.Container, d.Override, d.Reason)
	}

	if err == nil {
		return ""
	}

	return strings.TrimPrefix(err.Error(), auditlog.ErrRefused.Error()+": ")
}

// concludeDirective persists the answer to a directive together with the
// manual need it created, if any.
func (s *handler) concludeDirective(
	log logrus.FieldLogger, id string, need *protocol.Need, reject string,
) bool {
	h := journal.Directive{ID: id, Status: protocol.DirectiveAccepted}
	if reject != "" {
		h.Status, h.Error = protocol.DirectiveRejected, reject
	}

	err := s.store.Update(func(st *journal.State) {
		if st.Directive(id) != nil {
			return
		}

		st.Remember(h)

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
func (s *handler) manualReboot(
	d *protocol.Directive, obs *needs.Observation,
) (need *protocol.Need, reject string) {
	switch {
	case !s.cfg.Allows(protocol.VerbReboot):
		return nil, "verb reboot is not allowed on this host"
	case obs.Hold.Active():
		return nil, overrides.HoldReason(obs.Hold)
	}

	return &protocol.Need{
		ID:          protocol.ManualNeedID(d.ID),
		Kind:        protocol.NeedReboot,
		DirectiveID: d.ID,
		Reboot:      &protocol.RebootNeed{Reasons: []string{protocol.RebootReasonManual}},
	}, ""
}

// rollRefusal returns why a manual roll of a container is refused, or "".
func (s *handler) rollRefusal(container string, obs *needs.Observation) string {
	switch {
	case !s.cfg.Allows(protocol.VerbRoll):
		return "verb roll is not allowed on this host"
	case obs.Hold.Active():
		return overrides.HoldReason(obs.Hold)
	case container == "":
		return "roll directive names no container"
	}

	reason, _ := needs.Rollable(obs.Container(container), container)

	return reason
}

// AwaitLocalRoll starts a roll requested on the host; it waits for the
// registry lookup and the pull of its image like a roll directive.
func (s *handler) AwaitLocalRoll(id, container string, obs *needs.Observation) string {
	if reason := s.rollRefusal(container, obs); reason != "" {
		return reason
	}

	c := obs.Container(container)
	key := imageref.Normalize(c.Image)

	s.pendingRolls[id] = pendingRoll{
		container: container, key: key, ticket: s.watcher.Request(key, c.Image), local: true,
	}

	return ""
}

// Pending reports whether a roll waits for its image.
func (s *handler) Pending() bool { return len(s.pendingRolls) > 0 }

// Resolve turns roll directives whose image is now resolved and local into
// manual image needs, or rejects them.
func (s *handler) Resolve(obs *needs.Observation) {
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

		c := obs.Container(p.container)

		check, done, gone := s.watcher.Result(p.key, p.ticket)

		var (
			need   *protocol.Need
			reject string
		)

		switch {
		case gone || imageref.Normalize(c.Image) != p.key:
			reject = "container " + p.container + " moved to another image"
		case !done:
			continue
		case check.Digest == "":
			// The image itself is pulled in a pull window, once the need is
			// reported.
			reject = "the registry did not say where " + c.Image + " points: " +
				cmp.Or(check.Error, "no digest")
		default:
			need = &protocol.Need{
				ID:          protocol.ManualNeedID(id),
				Kind:        protocol.NeedImage,
				DirectiveID: id,
				Image: &protocol.ImageNeed{
					Container:  c.Name,
					Group:      c.Group,
					Client:     c.Client,
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
func (s *handler) concludeRoll(
	log logrus.FieldLogger, id string, p *pendingRoll, need *protocol.Need, reject string,
) bool {
	if p.local {
		return s.concludeLocalRoll(id, need, reject)
	}

	return s.concludeDirective(log, id, need, reject)
}

// Acks returns the acks for the directives the controller repeated in its
// last response and that are handled.
func (s *handler) Acks(st *journal.State) []protocol.DirectiveAck {
	if len(s.repeated) == 0 {
		return nil
	}

	acks := make([]protocol.DirectiveAck, 0, len(s.repeated))

	for _, id := range s.repeated {
		if h := st.Directive(id); h != nil {
			acks = append(acks, protocol.DirectiveAck{ID: h.ID, Status: h.Status, Error: h.Error})
		}
	}

	return acks
}
