package scheduler

import (
	"cmp"
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
	prefixImageKey = "image:"
	prefixPackages = "packages:"
	prefixManual   = "manual:"
)

// imageKey is the rollout key of an image reference at a digest.
func imageKey(image, digest string) string {
	return prefixImageKey + image + "@" + digest
}

// imageTarget is the digest an image reference is rolled to: the one named
// by the need with the freshest registry check.
type imageTarget struct {
	digest   string
	revision string
	checked  time.Time
}

// reportedNeeds is what the hosts' observed needs (those without a
// directive) ask for.
type reportedNeeds struct {
	images   map[string]*imageTarget
	packages map[string][]protocol.PackageChange
	reboot   bool
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
	}

	for _, name := range p.names {
		p.syncHold(p.hosts[name])
	}
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

	for _, d := range p.directives {
		if d.Host != hv.h.Name || d.State != fleet.DirectivePending || !isHoldKind(d.Kind) {
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

	p.directives = append(p.directives, &fleet.Directive{
		Directive: protocol.Directive{ID: p.w.NewID(), Kind: kind, Reason: hv.h.HoldReason},
		Host:      hv.h.Name,
		State:     fleet.DirectivePending,
		CreatedBy: actorScheduler,
		CreatedAt: p.now,
	})
}

// syncRollouts supersedes, opens, updates membership of and completes
// rollouts from the needs the hosts report.
func (p *pass) syncRollouts() {
	p.liftSkips()

	rep := p.reported()

	p.supersede(rep)

	created := p.open(rep)

	p.assignNeeds()

	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		p.syncMembers(r, created[r.ID])
		p.complete(r)
	}
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

// skipNote returns the note of the skip that holds for the host in the
// latest closed rollout of the key, or "".
func (p *pass) skipNote(key, host string) string {
	r := p.closedLatest[key]
	if r == nil {
		return ""
	}

	if m := r.Host(host); m != nil && operatorSkipped(m) {
		return m.Error
	}

	return ""
}

// liftSkips ends the skips that outlived their need. A skip holds for the
// key of its rollout for as long as the host keeps reporting the need: once
// a host that is neither held nor silent syncs without it, a later need of
// the same key (the reboot key never changes) is a new one. The sync must be
// newer than the skip. While a rollout of the key is open the skip lives on
// in its member list instead.
func (p *pass) liftSkips() {
	open := p.openKeys()

	for _, key := range sortedKeys(p.closedLatest) {
		r := p.closedLatest[key]
		if open[key] || r.Kind == fleet.RolloutManual {
			continue
		}

		copied := false

		for i := range r.Hosts {
			m := &r.Hosts[i]

			hv := p.hosts[m.Host]
			if !operatorSkipped(m) || hv == nil || hv.h.Status == nil || hv.held || hv.stale ||
				p.hostKeys[m.Host][key] {
				continue
			}

			// Only a sync from after the skip says that the need is gone. After
			// a controller restart a host that has been silent for long is not
			// stale yet, and its old sync may predate the need.
			if !hv.h.LastSeen.After(m.UpdatedAt) {
				continue
			}

			if !copied {
				r = cloneRollout(r)
				m = &r.Hosts[i]
				p.closedLatest[key] = r
				p.closedDirty = append(p.closedDirty, r)
				copied = true
			}

			m.Error = skipLiftedPrefix + reasonNeedGone
			m.UpdatedAt = p.now

			p.record("rollout.skip-lifted", r.ID, map[string]any{
				"host": m.Host, keyKey: key, keyReason: reasonNeedGone,
			})
		}
	}
}

// reported collects the observed needs of all hosts. The need of a host
// that a person skipped for the same key in its latest closed rollout is
// left out: it opens nothing.
func (p *pass) reported() *reportedNeeds {
	rep := &reportedNeeds{
		images:   make(map[string]*imageTarget, 4),
		packages: make(map[string][]protocol.PackageChange, 2),
	}

	incumbent := make(map[string]string, len(p.rollouts))

	for _, r := range p.rollouts {
		if r.Kind == fleet.RolloutImage && !r.State.Terminal() && incumbent[r.Image] == "" {
			incumbent[r.Image] = r.Digest
		}
	}

	open := p.openKeys()

	for _, name := range p.names {
		hv := p.hosts[name]

		for _, n := range hv.needs() {
			key := needKey(&n)
			if key == "" || (!open[key] && p.skipNote(key, name) != "") {
				continue
			}

			switch {
			case n.Kind == protocol.NeedImage && n.Image != nil:
				rep.image(n.Image, registryCheckedAt(hv.h, n.Image.Container), incumbent[n.Image.Image])
			case n.Kind == protocol.NeedPackages && n.Packages != nil:
				rep.packages[protocol.PackagesKey(n.Packages.Packages)] = n.Packages.Packages
			case n.Kind == protocol.NeedReboot:
				rep.reboot = true
			}
		}
	}

	return rep
}

// image offers one image need as the target of its reference. The freshest
// registry check wins; on a tie the digest of the open rollout stays, then
// the smaller digest.
func (rep *reportedNeeds) image(n *protocol.ImageNeed, checked time.Time, incumbent string) {
	cur := rep.images[n.Image]

	better := cur == nil
	if !better && n.ToDigest != cur.digest {
		switch c := checked.Compare(cur.checked); {
		case c != 0:
			better = c > 0
		case cur.digest == incumbent:
			better = false
		case n.ToDigest == incumbent:
			better = true
		default:
			better = n.ToDigest < cur.digest
		}
	}

	if better {
		rep.images[n.Image] = &imageTarget{digest: n.ToDigest, revision: n.ToRevision, checked: checked}
	}
}

// registryCheckedAt is when the host last looked at the tag of a container.
func registryCheckedAt(h *fleet.Host, container string) time.Time {
	if h.Status == nil {
		return time.Time{}
	}

	for i := range h.Status.Containers {
		c := &h.Status.Containers[i]
		if c.Name == container && c.Registry != nil {
			return c.Registry.CheckedAt
		}
	}

	return time.Time{}
}

// supersede closes image rollouts whose reference now has another target
// and packages rollouts whose key nobody reports while another key exists.
func (p *pass) supersede(rep *reportedNeeds) {
	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		switch r.Kind {
		case fleet.RolloutImage:
			if t := rep.images[r.Image]; t != nil && t.digest != r.Digest {
				p.closeRollout(r, fleet.RolloutSuperseded, "the tag moved to "+shortDigest(t.digest))
			}
		case fleet.RolloutPackages:
			if _, ok := rep.packages[strings.TrimPrefix(r.Key, prefixPackages)]; !ok && len(rep.packages) > 0 {
				p.closeRollout(r, fleet.RolloutSuperseded, "newer package versions appeared")
			}
		default:
		}
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

// open creates a rollout for every reported cause that has none, unless the
// latest closed rollout of its key was aborted. It returns the IDs of the
// new rollouts.
func (p *pass) open(rep *reportedNeeds) map[string]bool {
	created := make(map[string]bool, 2)
	openKeys := p.openKeys()

	add := func(r *fleet.Rollout) {
		if openKeys[r.Key] || p.abortedLatest(r.Key) {
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

		created[r.ID] = true

		p.record("rollout.created", r.ID, map[string]any{keyKey: r.Key, "title": r.Title})
	}

	for _, image := range sortedKeys(rep.images) {
		t := rep.images[image]
		add(&fleet.Rollout{
			Kind:     fleet.RolloutImage,
			Key:      imageKey(image, t.digest),
			Title:    image + " to " + shortDigest(t.digest),
			Image:    image,
			Digest:   t.digest,
			Revision: t.revision,
		})
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

	return created
}

// abortedLatest reports whether the latest closed rollout of the key was
// aborted.
func (p *pass) abortedLatest(key string) bool {
	r := p.closedLatest[key]

	return r != nil && r.State == fleet.RolloutAborted
}

// assignNeeds sorts every reported need to the open rollout it belongs to.
func (p *pass) assignNeeds() {
	byKey := make(map[string]*fleet.Rollout, len(p.rollouts))

	for _, r := range p.rollouts {
		if !r.State.Terminal() {
			byKey[r.Key] = r
		}
	}

	manual := make(map[string]string, len(p.directives))

	for _, d := range p.directives {
		if d.RolloutID != "" {
			manual[d.ID] = d.RolloutID
		}
	}

	for _, name := range p.names {
		for _, n := range p.hosts[name].needs() {
			var r *fleet.Rollout

			switch {
			case n.DirectiveID != "":
				r = p.rolloutByID[manual[n.DirectiveID]]
			case n.Kind == protocol.NeedImage && n.Image != nil:
				r = byKey[imageKey(n.Image.Image, n.Image.ToDigest)]
			case n.Kind == protocol.NeedPackages && n.Packages != nil:
				r = byKey[prefixPackages+protocol.PackagesKey(n.Packages.Packages)]
			case n.Kind == protocol.NeedReboot:
				r = byKey[keyReboot]
			}

			if r == nil || r.State.Terminal() {
				continue
			}

			byHost := p.needs[r.ID]
			if byHost == nil {
				byHost = make(map[string][]protocol.Need, len(p.names))
				p.needs[r.ID] = byHost
			}

			byHost[name] = append(byHost[name], n)
		}
	}
}

// syncMembers updates the members of one open rollout. created says that
// the rollout was created in this pass.
func (p *pass) syncMembers(r *fleet.Rollout, created bool) {
	needs := p.needs[r.ID]

	for i := range r.Hosts {
		m := &r.Hosts[i]
		p.syncMember(r, m, needs[m.Host])
	}

	for _, name := range p.names {
		if r.Host(name) != nil {
			continue
		}

		hv := p.hosts[name]
		m := fleet.RolloutHost{Host: name, UpdatedAt: p.now}
		skip := p.skipNote(r.Key, name)

		switch {
		case skip != "":
			// A skip for the key outlives the rollout it was given in.
			m.State = fleet.HostSkipped
			m.Error = skip

			if len(needs[name]) > 0 {
				m.NeedID = needs[name][0].ID
			}
		case len(needs[name]) > 0:
			m.State = fleet.HostPending
			m.NeedID = needs[name][0].ID
		case r.Kind == fleet.RolloutImage && expectsImage(r, hv):
			m.State = fleet.HostExpected
		default:
			continue
		}

		r.Hosts = append(r.Hosts, m)
	}

	p.markBroken(r)
	p.stages(r)

	if created && r.Kind == fleet.RolloutImage {
		p.recheckExpected(r)
	}
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
		case r.Canary == m.Host:
			m.Stage = fleet.StageCanary
		case p.broken(r, m.Host):
			m.Stage = fleet.StageRepair
		case m.Stage == fleet.StageRepair && m.State == fleet.HostDone && m.LeaseID != "":
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
// member, after the first success, while a member whose host is broken is
// still to be served, and while a lease is in flight for the rollout. A host
// that is broken is never chosen.
func (p *pass) pickCanary(r *fleet.Rollout) {
	if c := r.Host(r.Canary); c != nil && !r.Force && p.keepsCanary(r, c) {
		return
	}

	r.Canary = ""

	if r.Force || p.succeeded(r) || p.brokenAwaited(r) != nil || p.inFlight(r) != "" {
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

		if p.broken(r, m.Host) || p.repairLeased(m) ||
			(m.State != fleet.HostLeased && !p.servable(r, m)) {
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

// repairLeased reports whether the member holds a lease that was granted
// through the repair exception of the gate. Such a host is not the canary:
// it was offline before, so its return proves less than a canary must.
func (p *pass) repairLeased(m *fleet.RolloutHost) bool {
	hv := p.hosts[m.Host]

	return m.State == fleet.HostLeased && hv != nil && hv.lease != nil &&
		hv.lease.ID == m.LeaseID && hv.lease.Repair
}

// keepsCanary reports whether the canary was served: it holds a lease, is
// done through one that was released, or failed with one and the rollout is
// still halted by it.
func (p *pass) keepsCanary(r *fleet.Rollout, c *fleet.RolloutHost) bool {
	switch c.State {
	case fleet.HostLeased:
		return true
	case fleet.HostDone:
		return p.doneByRelease(c)
	case fleet.HostFailed:
		return r.State == fleet.RolloutHalted
	default:
		return false
	}
}

// leaseByID returns a lease that was open when the pass began, or a closed
// one the world holds, or nil.
func (p *pass) leaseByID(id string) *fleet.Lease {
	for _, l := range p.leases {
		if l.ID == id {
			return l
		}
	}

	return p.w.ClosedLeases[id]
}

// doneByRelease reports whether the member is done through a lease that was
// released. Where the lease is still known (it ended in this pass, or the
// host still reports its operation) its state is looked at, so a row of an
// older build that kept the ID of a failed lease does not count; a lease
// that is no longer known is taken at the member's word.
func (p *pass) doneByRelease(m *fleet.RolloutHost) bool {
	if m.State != fleet.HostDone || m.LeaseID == "" {
		return false
	}

	l := p.leaseByID(m.LeaseID)

	return l == nil || l.State == fleet.LeaseReleased
}

// succeeded reports whether the rollout has had its first success: a member
// is done through a lease that was released, which a lease is only after the
// host passed the readiness check (a forced lease is released without it, but
// serves forced rollouts only, which have no canary). A member that is done
// because its need went away, or whose step was done in a lease that failed,
// carries no lease ID and is no success (see doneByRelease for rows of older
// builds). From the first success on nobody waits for a canary.
func (p *pass) succeeded(r *fleet.Rollout) bool {
	for i := range r.Hosts {
		if p.doneByRelease(&r.Hosts[i]) {
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

// recheckExpected queues a recheck directive for every expected host of a
// new image rollout so it reports within one sync.
func (p *pass) recheckExpected(r *fleet.Rollout) {
	for i := range r.Hosts {
		m := &r.Hosts[i]
		if m.State != fleet.HostExpected {
			continue
		}

		if slices.ContainsFunc(p.directives, func(d *fleet.Directive) bool {
			return d.Host == m.Host && d.Kind == protocol.DirectiveRecheck &&
				d.State == fleet.DirectivePending
		}) {
			continue
		}

		p.directives = append(p.directives, &fleet.Directive{
			Directive: protocol.Directive{
				ID: p.w.NewID(), Kind: protocol.DirectiveRecheck, Reason: r.Title,
			},
			Host:      m.Host,
			State:     fleet.DirectivePending,
			CreatedBy: actorScheduler,
			CreatedAt: p.now,
		})
	}
}

// syncMember applies the membership rules to one existing member.
func (p *pass) syncMember(r *fleet.Rollout, m *fleet.RolloutHost, needs []protocol.Need) {
	hv := p.hosts[m.Host]
	if hv == nil {
		if !m.State.Settled() && m.State != fleet.HostLeased {
			p.setMember(m, fleet.HostSkipped)
			m.Error = reasonHostForgotten
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
		case r.Kind == fleet.RolloutImage && expectsImage(r, hv):
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
	default:
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

	switch r.Kind {
	case fleet.RolloutImage:
		if !expectsImage(r, hv) && runsImage(r, hv) {
			p.setMember(m, fleet.HostDone)
		}
	case fleet.RolloutManual:
		if !outstanding && len(needs) == 0 && rejected != "" {
			p.setMember(m, fleet.HostSkipped)
			m.Error = rejected
		}
	default:
		if !hv.held && !hv.stale {
			p.setMember(m, fleet.HostDone)
		}
	}
}

// manualDirectives reports whether a directive of the rollout is still
// unanswered by the host, and the reason of the first one it rejected.
func (p *pass) manualDirectives(r *fleet.Rollout, host string) (outstanding bool, rejected string) {
	if r.Kind != fleet.RolloutManual {
		return false, ""
	}

	for _, d := range p.directives {
		if d.RolloutID != r.ID || d.Host != host {
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

// expectsImage reports whether the host runs a managed, non-custom container
// on the rollout's image reference at another digest.
func expectsImage(r *fleet.Rollout, hv *hostView) bool {
	return hasContainer(r, hv, false)
}

// runsImage reports whether the host runs a managed, non-custom container on
// the rollout's image reference at its digest.
func runsImage(r *fleet.Rollout, hv *hostView) bool {
	return hasContainer(r, hv, true)
}

func hasContainer(r *fleet.Rollout, hv *hostView, onTarget bool) bool {
	if hv.h.Status == nil {
		return false
	}

	for i := range hv.h.Status.Containers {
		c := &hv.h.Status.Containers[i]
		if c.Managed && !c.Custom && c.Image == r.Image && (c.Digest == r.Digest) == onTarget {
			return true
		}
	}

	return false
}

// blockingExpected reports whether an expected member is still waited for:
// it is served in its turn (one whose host is broken first, another one can
// be the canary), and it keeps the rollout open.
func (p *pass) blockingExpected(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	hv := p.hosts[m.Host]
	if hv == nil || hv.held || hv.stale {
		return false
	}

	// The host had hostStaleAfter to report, counted from the scheduler's
	// start at the earliest: it could not report to a controller that was down.
	if p.now.Sub(later(r.CreatedAt, p.w.Since)) > p.set.HostStaleAfter.Std() {
		return false
	}

	switch r.Kind {
	case fleet.RolloutImage:
		return expectsImage(r, hv) && hv.allows(protocol.VerbRoll)
	case fleet.RolloutManual:
		outstanding, _ := p.manualDirectives(r, m.Host)

		return outstanding
	default:
		return false
	}
}

// blocks reports whether a member is unsettled in a way that keeps the
// rollout open.
func (p *pass) blocks(r *fleet.Rollout, m *fleet.RolloutHost) bool {
	switch m.State {
	case fleet.HostDone, fleet.HostSkipped:
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
