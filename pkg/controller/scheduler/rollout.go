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
	prefixImageKey = "image:"
	prefixPackages = "packages:"
	prefixManual   = "manual:"
	prefixLocal    = "local:"
)

// ImageGroup names the image rollouts of the containers of one client on one
// image reference, whatever the digest: each client's hosts are a rollout of
// their own, under the client's policy. Containers without a client share the
// reference alone. A host's broken mark for the rollouts carries the name.
func ImageGroup(client, image string) string {
	if client == "" {
		return prefixImageKey + image
	}

	return prefixImageKey + image + "#" + client
}

// imageKey is the rollout key of a client's image reference at a digest.
func imageKey(client, image, digest string) string {
	return ImageGroup(client, image) + "@" + digest
}

// imageTarget is the digest a client's image reference is rolled to: the one
// named by the need with the freshest registry check.
type imageTarget struct {
	client   string
	image    string
	digest   string
	revision string
	checked  time.Time
}

// imageTitle says what an image rollout does, for people.
func imageTitle(t *imageTarget) string {
	title := t.image + " to " + shortDigest(t.digest)
	if t.client != "" {
		title = t.client + ": " + title
	}

	return title
}

// reportedNeeds is what the hosts' observed needs (those without a
// directive) ask for.
type reportedNeeds struct {
	// images are the targets by image group (ImageGroup).
	images   map[string]*imageTarget
	packages map[string][]protocol.PackageChange
	reboot   bool
	// specs are the rollouts the spec needs open, by key.
	specs map[string]*fleet.Rollout
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
	case fleet.RolloutImage:
		return protocol.NeedImage
	case fleet.RolloutSpec:
		return protocol.NeedSpec
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
		images:   make(map[string]*imageTarget, 4),
		packages: make(map[string][]protocol.PackageChange, 2),
		specs:    make(map[string]*fleet.Rollout, 2),
		local:    make(map[string][]protocol.Need, 2),
	}

	for _, name := range p.names {
		hv := p.hosts[name]

		for _, n := range hv.needs() {
			switch {
			case n.DirectiveID != "":
			case isLocal(&n):
				rep.local[name] = append(rep.local[name], n)
			case n.Kind == protocol.NeedImage && n.Image != nil:
				rep.image(n.Image, registryChecked(hv.h, n.Image.Container))
			case n.Kind == protocol.NeedSpec && n.Spec != nil:
				if key := needKey(&n); rep.specs[key] == nil {
					rep.specs[key] = specRollout(n.Spec)
				}
			case n.Kind == protocol.NeedPackages && n.Packages != nil:
				rep.packages[protocol.PackagesKey(n.Packages.Packages)] = n.Packages.Packages
			case n.Kind == protocol.NeedReboot:
				rep.reboot = true
			}
		}
	}

	return rep
}

// image offers one image need as the target of its client's reference. The
// freshest registry check wins; on a tie the smaller digest.
func (rep *reportedNeeds) image(n *protocol.ImageNeed, checked time.Time) {
	group := ImageGroup(n.Client, n.Image)
	cur := rep.images[group]

	better := cur == nil
	if !better && n.ToDigest != cur.digest {
		c := checked.Compare(cur.checked)
		better = c > 0 || (c == 0 && n.ToDigest < cur.digest)
	}

	if better {
		rep.images[group] = &imageTarget{
			client: n.Client, image: n.Image, digest: n.ToDigest, revision: n.ToRevision,
			checked: checked,
		}
	}
}

// registryChecked is when the host last looked at the tag of a container.
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

// supersede closes image rollouts whose client's reference now has another
// target
// and packages rollouts whose key nobody reports while another key exists.
func (p *pass) supersede(rep *reportedNeeds) {
	for _, r := range p.rollouts {
		if r.State.Terminal() {
			continue
		}

		switch r.Kind {
		case fleet.RolloutImage:
			if t := rep.images[rolloutGroup(r)]; t != nil && t.digest != r.Digest {
				p.closeRollout(r, fleet.RolloutSuperseded, "the tag moved to "+shortDigest(t.digest))
			}
		case fleet.RolloutPackages:
			if _, ok := rep.packages[strings.TrimPrefix(r.Key, prefixPackages)]; !ok && len(rep.packages) > 0 {
				p.closeRollout(r, fleet.RolloutSuperseded, "newer package versions appeared")
			}
		case fleet.RolloutLocal:
			p.supersedeLocal(r, rep)
		case fleet.RolloutSpec:
			p.supersedeSpec(r, rep)
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

// open creates a rollout for every reported cause that has none. It returns
// the IDs of the new rollouts.
func (p *pass) open(rep *reportedNeeds) map[string]bool {
	created := make(map[string]bool, 2)
	openKeys := p.openKeys()

	add := func(r *fleet.Rollout) {
		if openKeys[r.Key] {
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

	for _, group := range sortedKeys(rep.images) {
		t := rep.images[group]
		add(&fleet.Rollout{
			Kind:     fleet.RolloutImage,
			Key:      imageKey(t.client, t.image, t.digest),
			Title:    imageTitle(t),
			Image:    t.image,
			Digest:   t.digest,
			Revision: t.revision,
			Client:   t.client,
		})
	}

	for _, key := range sortedKeys(rep.specs) {
		add(rep.specs[key])
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

	return created
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
		all := p.hosts[name].needs()
		local := make([]protocol.Need, 0, 2)

		for i := range all {
			if isLocal(&all[i]) {
				local = append(local, all[i])
			}
		}

		for _, n := range all {
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
		case r.Kind == fleet.RolloutImage && expectsImage(r, hv):
			m.State = fleet.HostExpected
		default:
			continue
		}

		joined = append(joined, m)
	}

	// Appended at the end: the index of the members holds while they join.
	r.Hosts = append(r.Hosts, joined...)

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

// recheckExpected queues a recheck directive for every expected host of a
// new image rollout so it reports within one sync.
func (p *pass) recheckExpected(r *fleet.Rollout) {
	for i := range r.Hosts {
		m := &r.Hosts[i]
		if m.State != fleet.HostExpected {
			continue
		}

		if slices.ContainsFunc(p.byHost[m.Host], func(d *fleet.Directive) bool {
			return d.Kind == protocol.DirectiveRecheck && d.State == fleet.DirectivePending
		}) {
			continue
		}

		p.addDirective(&fleet.Directive{
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
	case r.Kind == fleet.RolloutImage && expectsImage(r, hv):
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
	case r.Kind == fleet.RolloutImage && !expectsImage(r, hv) && runsImage(r, hv):
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
// rollout's client on its image reference at another digest.
func expectsImage(r *fleet.Rollout, hv *hostView) bool {
	return hasContainer(r, hv, false)
}

// runsImage reports whether the host runs a managed container of the
// rollout's client on its image reference at its digest.
func runsImage(r *fleet.Rollout, hv *hostView) bool {
	return hasContainer(r, hv, true)
}

func hasContainer(r *fleet.Rollout, hv *hostView, onTarget bool) bool {
	if hv.h.Status == nil {
		return false
	}

	for i := range hv.h.Status.Containers {
		c := &hv.h.Status.Containers[i]
		if c.Managed && c.Client == r.Client && c.Image == r.Image &&
			(c.Digest == r.Digest) == onTarget {
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

	// The host had its stale bound to report, counted from the scheduler's
	// start at the earliest: it could not report to a controller that was down.
	if p.now.Sub(later(r.CreatedAt, p.w.Since)) > hv.h.StaleAfter(p.set.HostStaleAfter.Std()) {
		return false
	}

	if r.Kind == fleet.RolloutImage {
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
