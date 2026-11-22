package agent

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// directives is the loop's memory of the controller's directives: the IDs
// the last answer repeated, and the rolls waiting for their tag's digest (in
// memory only; the controller repeats a roll directive after a restart).
type directives struct {
	repeated []string
	pending  map[string]pendingRoll
}

// pendingRoll is a roll waiting for the digest of its tag.
type pendingRoll struct {
	container string
	// ref is the tag the container ran when the roll was asked.
	ref string
	// local: requested on the host; the controller cannot drop it.
	local bool
}

// handleDirectives applies every directive of an answer that has no answer
// yet, in order. It reports whether anything new was answered.
func (a *agent) handleDirectives(list []protocol.Directive, view *plan.View) bool {
	d := &a.directives
	d.repeated = d.repeated[:0]
	listed := make(map[string]struct{}, len(list))
	answered := false

	for i := range list {
		dir := &list[i]

		if dir.ID == "" {
			a.log.WithField("kind", dir.Kind).Warn("ignoring a directive without an ID")

			continue
		}

		if _, dup := listed[dir.ID]; dup {
			continue
		}

		listed[dir.ID] = struct{}{}
		d.repeated = append(d.repeated, dir.ID)

		if _, waiting := d.pending[dir.ID]; waiting || a.state.Answered(dir.ID) {
			continue
		}

		if a.applyDirective(dir, view) {
			answered = true
		}
	}

	// A roll the controller no longer repeats is dropped.
	for id, p := range d.pending {
		if _, ok := listed[id]; !ok && !p.local {
			delete(d.pending, id)
		}
	}

	return answered
}

// applyDirective carries out one new directive and reports whether it is
// answered now. A roll is answered by resolveRolls; a hold whose write failed is
// tried again with the next answer.
func (a *agent) applyDirective(d *protocol.Directive, view *plan.View) bool {
	log := a.log.WithFields(logrus.Fields{"directive": d.ID, "kind": d.Kind})

	var (
		need   *protocol.Need
		reject string
		err    error
	)

	switch d.Kind {
	case protocol.DirectiveHold:
		hold := plan.ControllerHold(d.Reason, d.By)
		err = a.state.SetHold(plan.HoldSource(plan.FromController), &hold)
		a.refreshHold(view)
	case protocol.DirectiveReleaseHold:
		err = a.state.SetHold(plan.HoldSource(plan.FromController), nil)
		a.refreshHold(view)
	case protocol.DirectiveReboot:
		n, why := plan.ManualReboot(&a.cfg.Policy, view, d.ID)
		if reject = why; why == "" {
			need = &n
		}
	case protocol.DirectiveRoll:
		if reject = a.startRoll(d.ID, d.Container, view, false); reject == "" {
			log.WithField("container", d.Container).Info("manual roll requested")
			a.auditDirective(d, "")

			return false
		}
	case protocol.DirectiveOverride:
		reject = a.overrideDirective(d, view)
	case protocol.DirectiveWithdraw:
		if ids := plan.Withdrawn(a.state.Manual(), d.Withdraws); len(ids) > 0 {
			if err = a.state.ReconcileManual(nil, ids); err == nil {
				log.WithFields(logrus.Fields{"needs": ids, "withdraws": d.Withdraws}).
					Info("manual need withdrawn by the controller")
			}
		}
	default:
		reject = fmt.Sprintf("unknown directive kind %q", d.Kind)
	}

	if err != nil {
		log.WithError(err).Error("cannot apply directive")

		return false
	}

	a.auditDirective(d, reject)

	return a.answer(log, d.ID, need, reject)
}

// refreshHold sets the view's hold as it is now, for the directives that
// follow in the same answer.
func (a *agent) refreshHold(view *plan.View) {
	files := a.inputs()
	view.Hold = plan.HoldState(&files, a.now())
}

// overrideDirective writes the entry plan.Instruct decides. It returns ""
// or the reason of a refusal or of a failed write.
func (a *agent) overrideDirective(d *protocol.Directive, view *plan.View) string {
	who := actorOf(d)
	rec := local.Record{
		Source: local.SourceController, User: who.User, Teams: who.Teams,
		Feature: who.Feature, Directive: d.ID, Container: d.Container, Reason: d.Reason,
	}

	var built overrideBuild

	deployed := a.deployed(d.Container)

	prev, next, err := a.files.SetEntry(d.Container, who.User, built.wrap(
		func(prev *plan.Entry) (*plan.Entry, error) {
			return plan.Instruct(&a.cfg.Policy, view, d.Container, d.Override, prev, deployed,
				who.User, d.Reason, a.now())
		}))

	a.auditOverride(&rec, &built, prev, next, err)

	if err != nil {
		return refusalText(err)
	}

	a.wake()

	return ""
}

// startRoll refuses a roll (plan.RollRefusal) or keeps it pending.
func (a *agent) startRoll(id, container string, view *plan.View, isLocal bool) string {
	if reason := plan.RollRefusal(&a.cfg.Policy, view, container); reason != "" {
		return reason
	}

	a.directives.pending[id] = pendingRoll{
		container: container, ref: view.Container(container).Image, local: isLocal,
	}

	return ""
}

// takeLocalRolls starts the rolls requested on the host since the last cycle.
func (a *agent) takeLocalRolls(view *plan.View) {
	rolls := a.rolls.Take()

	for i := range rolls {
		if reason := a.startRoll(rolls[i].ID, rolls[i].Container, view, true); reason != "" {
			a.rolls.Set(rolls[i].ID, local.RollRejected, reason)
		}
	}
}

// resolveRolls answers every pending roll plan.RollNeed decides: a manual
// need or a rejection.
func (a *agent) resolveRolls(view *plan.View) {
	for id, p := range a.directives.pending {
		needID := protocol.ManualNeedID(id)
		if p.local {
			needID = protocol.LocalNeedID(id)
		}

		need, reject := plan.RollNeed(&a.cfg.Policy, view, p.container, p.ref, needID)
		if need == nil && reject == "" {
			continue
		}

		if a.concludeRoll(id, &p, need, reject) {
			delete(a.directives.pending, id)
		}
	}
}

// concludeRoll answers the roll directive, or the roll requested on the
// host. It reports whether the roll is answered.
func (a *agent) concludeRoll(id string, p *pendingRoll, need *protocol.Need, reject string) bool {
	if !p.local {
		log := a.log.WithFields(logrus.Fields{
			"directive": id, "kind": protocol.DirectiveRoll, "container": p.container,
		})

		if need != nil {
			need.DirectiveID = id
		}

		return a.answer(log, id, need, reject)
	}

	if reject != "" {
		a.rolls.Set(id, local.RollRejected, reject)
		a.audit.Record(&local.Record{
			Action: "roll.result", Source: local.SourceAgent, Result: local.Refused, Error: reject,
			Reason: "requested on the host as " + id,
		})

		return true
	}

	need.Local = true

	if err := a.state.AddManual(need); err != nil {
		a.log.WithError(err).WithField("roll", id).
			Error("cannot persist the need of a local roll; it is tried again")

		return false
	}

	a.rolls.Set(id, local.RollQueued, "waiting for the controller")

	return true
}

// answer stores the answer to a directive with the manual need it created,
// in one write. It reports whether that write succeeded.
func (a *agent) answer(log logrus.FieldLogger, id string, need *protocol.Need, reject string) bool {
	ack := protocol.DirectiveAck{ID: id, Status: protocol.DirectiveAccepted}
	if reject != "" {
		ack.Status, ack.Error = protocol.DirectiveRejected, reject
	}

	if err := a.state.Answer(ack, need); err != nil {
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

// actor is who the controller says asked for a directive, cut to one line.
type actor struct {
	User    string
	Teams   []string
	Feature string
}

func actorOf(d *protocol.Directive) actor {
	if d.By == nil {
		return actor{User: "unknown"}
	}

	return actor{
		User:    cmp.Or(plan.Line(d.By.User), "unknown"),
		Teams:   protocol.CleanOwners(d.By.Teams),
		Feature: plan.Line(d.By.Feature),
	}
}

// auditDirective records a roll, reboot, hold or release directive and
// whether the host took it.
func (a *agent) auditDirective(d *protocol.Directive, reject string) {
	actions := map[protocol.DirectiveKind]string{
		protocol.DirectiveRoll: "roll.request", protocol.DirectiveReboot: "reboot.request",
		protocol.DirectiveHold: "hold.set", protocol.DirectiveReleaseHold: "hold.release",
	}

	action, ok := actions[d.Kind]
	if !ok {
		return
	}

	who := actorOf(d)
	rec := &local.Record{
		Action: action, Source: local.SourceController, User: who.User, Teams: who.Teams,
		Feature: who.Feature, Directive: d.ID, Container: d.Container, Reason: d.Reason,
		Result: local.Accepted,
	}

	if reject != "" {
		rec.Result, rec.Error = local.Refused, reject
	}

	a.audit.Record(rec)
}

// overrideBuild records what the build passed to local.Files.SetEntry did.
type overrideBuild struct {
	ran     bool
	built   *plan.Entry
	cleared bool
}

// wrap returns build, recording into b; a refused build writes nothing.
func (b *overrideBuild) wrap(
	build func(prev *plan.Entry) (*plan.Entry, error),
) func(prev *plan.Entry) (*plan.Entry, error) {
	return func(prev *plan.Entry) (*plan.Entry, error) {
		next, err := build(prev)
		b.ran, b.built, b.cleared = true, next, err == nil && next == nil

		if err != nil {
			return nil, err
		}

		return next, nil
	}
}

// auditOverride records an override change; After is what was built, also
// when it was refused.
func (a *agent) auditOverride(rec *local.Record, b *overrideBuild, prev, next *plan.Entry, err error) {
	if b.ran {
		next = b.built
	}

	rec.Action, rec.Result = "override.set", local.Accepted
	rec.Before, rec.After = prev.Describe(), next.Describe()

	if b.ran && b.cleared {
		rec.Action = "override.clear"
	}

	switch {
	case errors.Is(err, plan.ErrRefused):
		rec.Result, rec.Error = local.Refused, refusalText(err)
	case err != nil:
		rec.Result, rec.Error = local.Failed, err.Error()
	case b.cleared && prev == nil:
		rec.Error = "there was no override"
	}

	a.audit.Record(rec)
}

// refusalText is an error's text without the "refused: " prefix.
func refusalText(err error) string {
	return strings.TrimPrefix(err.Error(), plan.ErrRefused.Error()+": ")
}
