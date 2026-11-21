package scheduler

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	keyReboot      = "reboot"
	groupPackages  = "packages"
	prefixPackages = "packages:"
	prefixManual   = "manual:"
	prefixLocal    = "local:"
)

// reportedNeeds is what the hosts' observed needs (those without a
// directive) ask for.
type reportedNeeds struct {
	// groups are the targets of the roll groups, by GroupName.
	groups   map[string]*groupTarget
	packages map[string][]protocol.PackageChange
	reboot   bool
	// local are the needs each host asks for itself (overrides, rolls
	// requested on the host), by host.
	local map[string][]protocol.Need
}

// isLocal reports whether a need is one a host asks for itself.
func isLocal(n *protocol.Need) bool {
	return n.DirectiveID == "" && (n.Local || n.Kind == protocol.NeedOverride)
}

// localKey is the rollout key of what a host asks for itself: it changes
// when the host asks for something else.
func localKey(host string, needs []protocol.Need) string {
	ids := make([]string, 0, len(needs))
	for i := range needs {
		ids = append(ids, needs[i].ID)
	}

	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))

	return prefixLocal + host + ":" + hex.EncodeToString(sum[:6])
}

// localTitle says what a host asks for itself, for people.
func localTitle(host string, needs []protocol.Need) string {
	parts := make([]string, 0, len(needs))

	for i := range needs {
		switch n := &needs[i]; {
		case n.Override != nil && n.Override.Restore:
			parts = append(parts, "restore "+n.Override.Container)
		case n.Override != nil:
			parts = append(parts, "override "+n.Override.Container)
		case n.Image != nil:
			parts = append(parts, "roll "+n.Image.Container)
		}
	}

	slices.Sort(parts)

	return host + ": " + strings.Join(parts, ", ")
}

// syncDirectives takes over the agents' acknowledgements and keeps the hold
// directives in line with Host.HoldWanted.
func (p *pass) syncDirectives() {
	for _, d := range p.directives {
		if d.State != fleet.DirectivePending {
			continue
		}

		hv := p.hosts[d.Host]
		if hv == nil {
			d.State = fleet.DirectiveCancelled
			d.Error = reasonHostForgotten

			continue
		}

		if isHoldKind(d.Kind) || hv.h.Status == nil {
			continue
		}

		for _, ack := range hv.h.Status.DirectiveAcks {
			if ack.ID != d.ID {
				continue
			}

			d.State = fleet.DirectiveAccepted

			if ack.Status == protocol.DirectiveRejected {
				d.State = fleet.DirectiveRejected
				d.Error = cmpOr(ack.Error, "rejected by the agent")
			}

			break
		}

		p.settleOverride(d)
	}

	for _, name := range p.names {
		p.syncHold(p.hosts[name])
	}
}

// syncWithdrawals takes back the manual needs that hosts report and nothing
// serves any more: the need of a directive whose rollout is closed or
// unknown, or that the host left (skipped, aborted). A need of the host's
// open lease is left to the lease. A withdraw directive is repeated until the
// agent answers it or the host no longer reports the need.
func (p *pass) syncWithdrawals() {
	for _, name := range p.names {
		hv := p.hosts[name]
		if hv.h.Status == nil || hv.stale {
			continue
		}

		unwanted := make(map[string]bool, 1)

		for _, n := range hv.needs() {
			if n.DirectiveID != "" && !p.manualWanted(hv, n.DirectiveID) &&
				(hv.lease == nil || !slices.ContainsFunc(hv.lease.Needs, func(l protocol.Need) bool {
					return l.ID == n.ID
				})) {
				unwanted[n.DirectiveID] = true
			}
		}

		for _, d := range p.byHost[name] {
			if d.Kind != protocol.DirectiveWithdraw || d.State != fleet.DirectivePending {
				continue
			}

			if unwanted[d.Withdraws] {
				delete(unwanted, d.Withdraws)
			} else {
				d.State = fleet.DirectiveCancelled
			}
		}

		for _, id := range sortedKeys(unwanted) {
			p.addDirective(&fleet.Directive{
				Directive: protocol.Directive{
					ID: p.w.NewID(), Kind: protocol.DirectiveWithdraw, Withdraws: id,
					Reason: "its rollout no longer serves this host",
				},
				Host:      name,
				State:     fleet.DirectivePending,
				CreatedBy: actorScheduler,
				CreatedAt: p.now,
			})
		}
	}
}

// manualWanted reports whether the manual directive of the host belongs to
// an open rollout in which the host still waits or was served.
func (p *pass) manualWanted(hv *hostView, id string) bool {
	for _, d := range p.byHost[hv.h.Name] {
		if d.ID != id {
			continue
		}

		r := p.rolloutByID[d.RolloutID]
		if r == nil || r.State.Terminal() {
			return false
		}

		m := p.member(r, hv.h.Name)

		return m != nil && m.State != fleet.HostSkipped && m.State != fleet.HostAborted
	}

	return false
}

// settleOverride records the agent's answer to an override directive, which
// belongs to no rollout that would show it, and withdraws one that the agent
// has not answered in time.
func (p *pass) settleOverride(d *fleet.Directive) {
	if d.Kind != protocol.DirectiveOverride {
		return
	}

	if d.State == fleet.DirectivePending {
		if p.now.Sub(later(d.CreatedAt, p.w.Since)) <= overrideAnswerTimeout {
			return
		}

		d.State = fleet.DirectiveCancelled
		d.Error = "the agent did not answer in time"
	}

	detail := map[string]any{"directive": d.ID, "kind": d.Kind, "container": d.Container}
	if d.Error != "" {
		detail["error"] = d.Error
	}

	p.record("directive."+string(d.State), d.Host, detail)
}

// holdActor is who set the controller hold of a host, as the hold directive
// tells the agent: the person, and whether through a team feature or as
// staff. The agent writes it into the hold entry of its overrides file.
func holdActor(h *fleet.Host) *protocol.Actor {
	if !h.HoldWanted {
		return nil
	}

	feature := "staff"
	if _, team := fleet.HoldTeams(h.HoldBy); team {
		feature = "team"
	}

	return &protocol.Actor{User: cmpOr(h.HoldActor, actorScheduler), Feature: feature}
}

func isHoldKind(k protocol.DirectiveKind) bool {
	return k == protocol.DirectiveHold || k == protocol.DirectiveReleaseHold
}

// syncHold issues hold / release-hold directives until the agent's reported
// controller hold matches Host.HoldWanted. They are settled by the reported
// state, not by acknowledgements.
func (p *pass) syncHold(hv *hostView) {
	if hv.h.Status == nil {
		return
	}

	kind := protocol.DirectiveReleaseHold
	if hv.h.HoldWanted {
		kind = protocol.DirectiveHold
	}

	agreed := hv.h.Status.Hold.Controller == hv.h.HoldWanted
	outstanding := false

	for _, d := range p.byHost[hv.h.Name] {
		if d.State != fleet.DirectivePending || !isHoldKind(d.Kind) {
			continue
		}

		switch {
		case agreed && d.Kind == kind:
			d.State = fleet.DirectiveAccepted
		case agreed || d.Kind != kind:
			d.State = fleet.DirectiveCancelled
		default:
			outstanding = true
		}
	}

	if agreed || outstanding {
		return
	}

	p.addDirective(&fleet.Directive{
		Directive: protocol.Directive{ID: p.w.NewID(), Kind: kind, Reason: hv.h.HoldReason, By: holdActor(hv.h)},
		Host:      hv.h.Name,
		State:     fleet.DirectivePending,
		CreatedBy: actorScheduler,
		CreatedAt: p.now,
	})
}

// syncRollouts supersedes, opens, updates membership of and completes
// rollouts from the needs the hosts report.
func (p *pass) syncRollouts() {
	rep := p.reported()

	p.supersede(rep)

	p.open(rep)

	p.assignNeeds()

	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		p.syncMembers(r)
		p.complete(r)
		p.pauseAfterCanary(r)
	}
}

// pauseAfterCanary pauses a rollout that had its first success in this pass
// and is still open, when the setting of the kind it serves asks for it. It
// is the pause a person sets (Rollout.Paused), with a reason of its own, and a
// person's resume lifts it. A rollout has one first success, so it is paused
// once; a forced rollout has no canary, and one that a person paused already
// stays as it is.
func (p *pass) pauseAfterCanary(r *fleet.Rollout) {
	l := p.first[r.ID]
	if l == nil || r.Force || r.Paused || r.State.Terminal() || !p.set.PauseAfterCanaryFor(p.servedKind(r)) {
		return
	}

	r.Paused = true
	r.PauseReason = reasonCanaryPause

	p.record("rollout.paused", r.ID, map[string]any{
		keyKey: r.Key, keyReason: reasonCanaryPause, keyLease: l.ID,
	})
}

// servedKind is the kind of need a rollout serves. A manual rollout serves
// what its directives ask for: a roll is about images, a reboot about reboots.
func (p *pass) servedKind(r *fleet.Rollout) protocol.NeedKind {
	switch r.Kind {
	case fleet.RolloutGroup:
		return protocol.NeedImage
	case fleet.RolloutPackages:
		return protocol.NeedPackages
	case fleet.RolloutReboot:
		return protocol.NeedReboot
	default:
	}

	for _, d := range p.directives {
		switch {
		case d.RolloutID != r.ID:
		case d.Kind == protocol.DirectiveRoll:
			return protocol.NeedImage
		case d.Kind == protocol.DirectiveReboot:
			return protocol.NeedReboot
		}
	}

	return ""
}

// openKeys are the keys of the non-terminal rollouts.
func (p *pass) openKeys() map[string]bool {
	keys := make(map[string]bool, len(p.rollouts))

	for _, r := range p.rollouts {
		if !r.State.Terminal() {
			keys[r.Key] = true
		}
	}

	return keys
}

// reported collects the observed needs of all hosts.
func (p *pass) reported() *reportedNeeds {
	rep := &reportedNeeds{
		groups:   make(map[string]*groupTarget, 4),
		packages: make(map[string][]protocol.PackageChange, 2),
		local:    make(map[string][]protocol.Need, 2),
	}

	for _, name := range p.names {
		hv := p.hosts[name]

		for _, n := range hv.needs() {
			switch {
			case n.DirectiveID != "":
			case isLocal(&n):
				rep.local[name] = append(rep.local[name], n)
			case isGroupNeed(&n):
				rep.group(&n, registryChecked(hv.h, n.Container()))
			case n.Kind == protocol.NeedPackages && n.Packages != nil:
				rep.packages[protocol.PackagesKey(n.Packages.Packages)] = n.Packages.Packages
			case n.Kind == protocol.NeedReboot:
				rep.reboot = true
			}
		}
	}

	return rep
}

// group offers one image or spec need as a part of its group's target.
func (rep *reportedNeeds) group(n *protocol.Need, checked time.Time) {
	name := needGroupName(n)

	g := rep.groups[name]
	if g == nil {
		g = &groupTarget{
			parts: make(map[string]fleet.RolloutTarget, 2), checked: make(map[string]time.Time, 2),
		}

		if n.Image != nil {
			g.group, g.client = n.Image.Group, n.Image.Client
		} else {
			g.group, g.client = n.Spec.Group, n.Spec.Client
		}

		rep.groups[name] = g
	}

	g.add(n, checked)
}

// registryChecked is when the tag of a container was looked up, as the host has it.
func registryChecked(h *fleet.Host, container string) time.Time {
	if h.Status == nil {
		return time.Time{}
	}

	for i := range h.Status.Containers {
		if c := &h.Status.Containers[i]; c.Name == container && c.Registry != nil {
			return c.Registry.CheckedAt
		}
	}

	return time.Time{}
}

// supersede closes group rollouts whose group the hosts ask something else
// of, and packages rollouts whose key nobody reports while another key
// exists.
func (p *pass) supersede(rep *reportedNeeds) {
	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		switch r.Kind {
		case fleet.RolloutGroup:
			p.supersedeGroup(r, rep)
		case fleet.RolloutPackages:
			if _, ok := rep.packages[strings.TrimPrefix(r.Key, prefixPackages)]; !ok && len(rep.packages) > 0 {
				p.closeRollout(r, fleet.RolloutSuperseded, "newer package versions appeared")
			}
		case fleet.RolloutLocal:
			p.supersedeLocal(r, rep)
		default:
		}
	}
}

// supersedeLocal closes the rollout of what a host asked for itself when the
// host asks for something else now, or for nothing: the overrides file
// changed again, the container was put right by other means, or the work
// failed and left nothing to ask for. Only a host that reports says so (one
// that is held, silent or forgotten says nothing about its needs), and only
// of a member that still waits or failed: one that holds a lease or is done
// is settled by the lease.
func (p *pass) supersedeLocal(r *fleet.Rollout, rep *reportedNeeds) {
	if len(r.Hosts) != 1 {
		return
	}

	m := &r.Hosts[0]

	hv := p.hosts[m.Host]
	if hv == nil || hv.h.Status == nil || hv.held || hv.stale ||
		m.State == fleet.HostLeased || m.State == fleet.HostDone {
		return
	}

	needs := rep.local[m.Host]

	switch {
	case len(needs) == 0:
		p.closeRollout(r, fleet.RolloutSuperseded, "the host no longer asks for it")
	case localKey(m.Host, needs) != r.Key:
		p.closeRollout(r, fleet.RolloutSuperseded, "the host asks for something else now")
	}
}

// closeRollout moves a rollout to a terminal state and cancels the manual
// directives nobody answered.
func (p *pass) closeRollout(r *fleet.Rollout, state fleet.RolloutState, reason string) {
	ended := p.now
	r.State = state
	r.Reason = reason
	r.EndedAt = &ended

	for _, d := range p.directives {
		if d.RolloutID == r.ID && d.State == fleet.DirectivePending {
			d.State = fleet.DirectiveCancelled
		}
	}

	p.record("rollout."+string(state), r.ID, map[string]any{keyKey: r.Key, keyReason: reason})
}

// open creates a rollout for every reported cause that has none.
func (p *pass) open(rep *reportedNeeds) {
	openKeys := p.openKeys()

	add := func(r *fleet.Rollout) {
		if openKeys[r.Key] || p.w.AbortedKeys[r.Key] {
			return
		}

		r.ID = p.w.NewID()
		r.State = fleet.RolloutActive
		r.CreatedBy = actorScheduler
		r.CreatedAt = p.now
		r.UpdatedAt = p.now

		p.rollouts = append(p.rollouts, r)
		p.rolloutByID[r.ID] = r
		openKeys[r.Key] = true

		p.record("rollout.created", r.ID, map[string]any{keyKey: r.Key, "title": r.Title})
	}

	// A group with an open rollout that holds everything the hosts ask for
	// keeps it (supersede closed the others).
	openGroups := make(map[string]bool, len(rep.groups))

	for _, r := range p.rollouts {
		if r.Kind == fleet.RolloutGroup && !r.State.Terminal() {
			openGroups[rolloutGroupName(r)] = true
		}
	}

	for _, name := range sortedKeys(rep.groups) {
		if !openGroups[name] {
			add(groupRollout(rep.groups[name]))
		}
	}

	for _, key := range sortedKeys(rep.packages) {
		add(&fleet.Rollout{
			Kind:     fleet.RolloutPackages,
			Key:      prefixPackages + key,
			Title:    packagesTitle(rep.packages[key]),
			Packages: slices.Clone(rep.packages[key]),
		})
	}

	if rep.reboot {
		add(&fleet.Rollout{Kind: fleet.RolloutReboot, Key: keyReboot, Title: "reboot"})
	}

	for _, host := range sortedKeys(rep.local) {
		add(&fleet.Rollout{
			Kind:  fleet.RolloutLocal,
			Key:   localKey(host, rep.local[host]),
			Title: localTitle(host, rep.local[host]),
		})
	}
}

// assignNeeds sorts every reported need to the open rollout it belongs to.
func (p *pass) assignNeeds() {
	byKey := make(map[string]*fleet.Rollout, len(p.rollouts))
	byGroup := make(map[string]*fleet.Rollout, len(p.rollouts))

	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		byKey[r.Key] = r

		if r.Kind == fleet.RolloutGroup {
			byGroup[rolloutGroupName(r)] = r
		}
	}

	manual := make(map[string]string, len(p.directives))

	for _, d := range p.directives {
		if d.RolloutID != "" {
			manual[d.ID] = d.RolloutID
		}
	}

	for _, name := range p.names {
		all := p.hosts[name].needs()
		local := make([]protocol.Need, 0, 2)

		for i := range all {
			if isLocal(&all[i]) {
				local = append(local, all[i])
			}
		}

		p.assignGroupNeeds(name, all, byGroup)

		for _, n := range all {
			if isGroupNeed(&n) {
				continue
			}

			r := byKey[needKey(&n)]

			switch {
			case n.DirectiveID != "":
				r = p.rolloutByID[manual[n.DirectiveID]]
			case isLocal(&n):
				r = byKey[localKey(name, local)]
			}

			if r == nil || r.State.Terminal() {
				continue
			}

			p.addNeeds(r, name, []protocol.Need{n})
		}
	}
}

// syncMembers updates the members of one open rollout.
func (p *pass) syncMembers(r *fleet.Rollout) {
	needs := p.needs[r.ID]

	for i := range r.Hosts {
		m := &r.Hosts[i]
		p.syncMember(r, m, needs[m.Host])
	}

	joined := make([]fleet.RolloutHost, 0, 4)

	for _, name := range p.names {
		if p.member(r, name) != nil {
			continue
		}

		hv := p.hosts[name]
		m := fleet.RolloutHost{Host: name, UpdatedAt: p.now}

		switch {
		case len(needs[name]) > 0:
			m.State = fleet.HostPending
			m.NeedID = needs[name][0].ID
		case r.Kind == fleet.RolloutGroup && expectsImage(r, hv):
			m.State = fleet.HostExpected
		default:
			continue
		}

		joined = append(joined, m)
	}

	// Appended at the end: the index of the members holds while they join.
	r.Hosts = append(r.Hosts, joined...)

	p.stages(r)
}

// stages settles the canary and gives every member its stage: the canary is
// in its own, a member whose host is broken for the rollout's group in the
// repair stage, everybody else in the rest. A member that is done through the
// lease that repaired it keeps the repair stage: its host is in order now,
// and the stage says how it was served.
func (p *pass) stages(r *fleet.Rollout) {
	p.pickCanary(r)

	for i := range r.Hosts {
		m := &r.Hosts[i]

		switch {
		case m.Stage == fleet.StageRepair && m.State == fleet.HostDone && m.LeaseID != "":
		case r.Canary == m.Host:
			m.Stage = fleet.StageCanary
		case p.broken(r, m.Host):
			m.Stage = fleet.StageRepair
		default:
			m.Stage = fleet.StageRest
		}
	}
}

// pickCanary settles which member is the canary of the rollout: the one host
// that goes alone until the rollout has its first success. A canary that
// holds a lease, is done by one that was released, or failed and halted the
// rollout stays. Until then the choice is made anew in every pass, so a
// canary that left without having been served (skipped, held, silent,
// forgotten, its need gone by itself) is replaced by the next candidate.
// There is no new canary in a forced rollout, in a rollout with a single
// member, after the first success, and while a member whose host is broken is
// still to be served. A host that is broken is never chosen.
func (p *pass) pickCanary(r *fleet.Rollout) {
	if c := p.member(r, r.Canary); c != nil && !r.Force && p.keepsCanary(r, c) {
		return
	}

	r.Canary = ""

	if r.Force || p.succeeded(r) || p.brokenAwaited(r) != nil {
		return
	}

	var (
		best    *fleet.RolloutHost
		members int
	)

	for i := range r.Hosts {
		m := &r.Hosts[i]

		if p.blocks(r, m) {
			members++
		}

		if p.broken(r, m.Host) || (m.State != fleet.HostLeased && !p.servable(r, m)) {
			continue
		}

		if best == nil || p.canaryBefore(m, best) {
			best = m
		}
	}

	if best != nil && members > 1 {
		r.Canary = best.Host
	}
}

// keepsCanary reports whether the canary was served: it holds a lease, is
// done through one that was released, or failed with one and the rollout is
// still halted by it.
func (p *pass) keepsCanary(r *fleet.Rollout, c *fleet.RolloutHost) bool {
	switch c.State {
	case fleet.HostLeased:
		return true
	case fleet.HostDone:
		return doneByRelease(c)
	case fleet.HostFailed:
		return r.State == fleet.RolloutHalted
	default:
		return false
	}
}

// doneByRelease reports whether the member is done through a lease that was
// released: only such a member keeps the ID of its lease.
func doneByRelease(m *fleet.RolloutHost) bool {
	return m.State == fleet.HostDone && m.LeaseID != ""
}

// succeeded reports whether the rollout has had its first success: a member
// is done through a lease that was released, which a lease is only after the
// host passed the readiness check (a forced lease is released without it, but
// serves forced rollouts only, which have no canary). A member that is done
// because its need went away, or whose step was done in a lease that failed,
// carries no lease ID and is no success. From the first success on nobody
// waits for a canary.
func (p *pass) succeeded(r *fleet.Rollout) bool {
	for i := range r.Hosts {
		if doneByRelease(&r.Hosts[i]) {
			return true
		}
	}

	return false
}

// servable reports whether a member that waits can be served as far as the
// host itself goes: it reports a need its agent allows and is neither held
// nor silent, or it is expected to report within hostStaleAfter. A member
// that is not servable waits for a person and holds nobody back.
func (p *pass) servable(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	hv := p.hosts[m.Host]
	if hv == nil {
		return false
	}

	switch m.State {
	case fleet.HostPending:
		return !hv.held && !hv.stale && slices.ContainsFunc(p.needs[r.ID][m.Host],
			func(n protocol.Need) bool { return hv.allows(n.Kind.Verb()) })
	case fleet.HostExpected:
		return p.blockingExpected(r, m)
	default:
		return false
	}
}

// awaited reports whether a member is unsettled in a way that holds back the
// members behind it: it holds a lease or is servable.
func (p *pass) awaited(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	return m.State == fleet.HostLeased || p.servable(r, m)
}

// canaryBefore orders the members that can be the canary: a member that
// holds a lease first, then hosts that fit the budget as it is before this
// pass grants anything, hosts with live validators (only they prove that
// attesting still works after the change; a host whose validators are all
// offline proves as little as a host without any), hosts that report the
// need before hosts that are only expected to, ascending weight and name.
func (p *pass) canaryBefore(a, b *fleet.RolloutHost) bool {
	type rank struct {
		waiting, overBudget, notLive, expected int
		weight                                 uint64
	}

	of := func(m *fleet.RolloutHost) rank {
		k := rank{notLive: 1}

		if m.State != fleet.HostLeased {
			k.waiting = 1
		}

		if m.State == fleet.HostExpected {
			k.expected = 1
		}

		hv := p.hosts[m.Host]
		if hv == nil {
			return k
		}

		if !p.budgetBefore().fits(hv.live) {
			k.overBudget = 1
		}

		k.weight = hv.weight

		if hv.live > 0 {
			k.notLive = 0
		}

		return k
	}

	ka, kb := of(a), of(b)

	return cmp.Or(
		cmp.Compare(ka.waiting, kb.waiting),
		cmp.Compare(ka.overBudget, kb.overBudget),
		cmp.Compare(ka.notLive, kb.notLive),
		cmp.Compare(ka.expected, kb.expected),
		cmp.Compare(ka.weight, kb.weight),
		cmp.Compare(a.Host, b.Host),
	) < 0
}

// syncMember applies the membership rules to one existing member.
func (p *pass) syncMember(r *fleet.Rollout, m *fleet.RolloutHost, needs []protocol.Need) {
	hv := p.hosts[m.Host]
	if hv == nil {
		// A member that a person skipped is left out for the forgotten host
		// from here on: it keeps no rollout open.
		if operatorSkipped(m) || (!m.State.Settled() && m.State != fleet.HostLeased) {
			m.State = fleet.HostSkipped
			m.SkippedBy = ""
			m.Error = reasonHostForgotten
			m.UpdatedAt = p.now
		}

		return
	}

	switch m.State {
	case fleet.HostExpected:
		p.syncExpected(r, m, hv, needs)
	case fleet.HostPending:
		switch {
		case len(needs) > 0:
			m.NeedID = needs[0].ID
		case hv.h.Status == nil:
		case r.Kind == fleet.RolloutGroup && expectsImage(r, hv):
			// The host no longer reports the need (it is held, or it looks
			// at an older digest) but still runs another digest.
			m.NeedID = ""
			p.setMember(m, fleet.HostExpected)
		default:
			// The need disappeared without a lease.
			m.LeaseID = ""
			p.setMember(m, fleet.HostDone)
		}
	case fleet.HostDone:
		// A host that was done without a lease and reports the need again
		// (a hold was lifted) rejoins.
		if m.LeaseID == "" && len(needs) > 0 {
			m.NeedID = needs[0].ID
			p.setMember(m, fleet.HostPending)
		}
	case fleet.HostSkipped:
		p.syncSkipped(r, m, hv, needs)
	default:
	}
}

// syncSkipped looks whether a member that a person skipped in a rollout of an
// observed cause still has the need. The skip holds, and keeps the rollout
// open (blocks), for as long as it has: a rollout that completed would be
// opened again for the host at once. Once a host that is neither held nor
// silent syncs without the need, the member is done like a waiting one whose
// need went away; a held or silent host says nothing about its needs. In a
// manual rollout a skip is final: nothing follows the rollout.
func (p *pass) syncSkipped(r *fleet.Rollout, m *fleet.RolloutHost, hv *hostView, needs []protocol.Need) {
	if !operatorSkipped(m) || r.Kind == fleet.RolloutManual {
		return
	}

	switch {
	case len(needs) > 0:
		m.NeedID = needs[0].ID
	case hv.h.Status == nil || hv.held || hv.stale:
	case r.Kind == fleet.RolloutGroup && expectsImage(r, hv):
		// It still runs another digest: the need is not reported, not gone.
	default:
		m.LeaseID = ""
		p.setMember(m, fleet.HostDone)
	}
}

func (p *pass) syncExpected(
	r *fleet.Rollout, m *fleet.RolloutHost, hv *hostView, needs []protocol.Need,
) {
	outstanding, rejected := p.manualDirectives(r, m.Host)

	if len(needs) > 0 && !outstanding {
		m.NeedID = needs[0].ID
		p.setMember(m, fleet.HostPending)

		return
	}

	if hv.h.Status == nil {
		return
	}

	// Only image and manual rollouts have expected members.
	switch {
	case r.Kind == fleet.RolloutGroup && !expectsImage(r, hv) && runsImage(r, hv):
		p.setMember(m, fleet.HostDone)
	case r.Kind == fleet.RolloutManual && !outstanding && len(needs) == 0 && rejected != "":
		p.setMember(m, fleet.HostSkipped)
		m.Error = rejected
	}
}

// manualDirectives reports whether a directive of the rollout is still
// unanswered by the host, and the reason of the first one it rejected.
func (p *pass) manualDirectives(r *fleet.Rollout, host string) (outstanding bool, rejected string) {
	if r.Kind != fleet.RolloutManual {
		return false, ""
	}

	for _, d := range p.byHost[host] {
		if d.RolloutID != r.ID {
			continue
		}

		switch d.State {
		case fleet.DirectivePending:
			outstanding = true
		case fleet.DirectiveRejected:
			if rejected == "" {
				rejected = cmpOr(d.Error, "rejected by the agent")
			}
		default:
		}
	}

	return outstanding, rejected
}

// expectsImage reports whether the host runs a managed container of the
// rollout's group and client on one of its tags at another digest.
func expectsImage(r *fleet.Rollout, hv *hostView) bool {
	return groupContainer(r, hv, false)
}

// runsImage reports whether the host runs a managed container of the
// rollout's group and client on one of its tags at the target digest.
func runsImage(r *fleet.Rollout, hv *hostView) bool {
	return groupContainer(r, hv, true)
}

// blockingExpected reports whether an expected member is still waited for:
// it is served in its turn (one whose host is broken first, another one can
// be the canary), and it keeps the rollout open.
func (p *pass) blockingExpected(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	hv := p.hosts[m.Host]
	if hv == nil || hv.held || hv.stale {
		return false
	}

	// The host had its stale bound to report, counted from the scheduler's
	// start at the earliest: it could not report to a controller that was down.
	if p.now.Sub(later(r.CreatedAt, p.w.Since)) > hv.h.StaleAfter(p.set.HostStaleAfter.Std()) {
		return false
	}

	if r.Kind == fleet.RolloutGroup {
		return expectsImage(r, hv) && hv.allows(protocol.VerbRoll)
	}

	outstanding, _ := p.manualDirectives(r, m.Host)

	return outstanding
}

// blocks reports whether a member is unsettled in a way that keeps the
// rollout open. A member that a person skipped in a rollout of an observed
// cause is: see syncSkipped.
func (p *pass) blocks(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	switch m.State {
	case fleet.HostDone:
		return false
	case fleet.HostSkipped:
		return operatorSkipped(m) && r.Kind != fleet.RolloutManual
	case fleet.HostAborted:
		return false
	case fleet.HostExpected:
		return p.blockingExpected(r, m)
	default:
		return true
	}
}

// complete closes an active rollout nobody waits in any more.
func (p *pass) complete(r *fleet.Rollout) {
	if r.State != fleet.RolloutActive {
		return
	}

	for i := range r.Hosts {
		if p.blocks(r, &r.Hosts[i]) {
			return
		}
	}

	p.closeRollout(r, fleet.RolloutCompleted, "")
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}

	return d
}

func packagesTitle(pkgs []protocol.PackageChange) string {
	parts := make([]string, 0, len(pkgs))
	for _, c := range pkgs {
		parts = append(parts, fmt.Sprintf("%s %s", c.Name, c.To))
	}

	slices.Sort(parts)

	return "packages: " + strings.Join(parts, ", ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}
