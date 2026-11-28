package scheduler

import (
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// tagMoveWaitMax bounds the wait of a tag move: the lookup interval, at most
// this long.
const tagMoveWaitMax = 10 * time.Minute

// TagMove is an image need that a lease of its host ended with superseded:
// the agent found the tag at another digest than the need's. The host is not
// granted the need again until the tag was looked up after the end, or the
// wait (tagMoveWait) is over: its agent would supersede it again.
type TagMove struct {
	Host    string
	NeedID  string
	Ref     string
	Digest  string
	EndedAt time.Time
	// Repeat: the need was superseded again within the wait of an earlier
	// move. The agent and the registry disagree: it waits out the wait,
	// whatever a lookup says.
	Repeat bool
}

// tagMoveWait is how long a tag move holds its need back at most. The
// scheduled lookup comes within the lookup interval, so a host that waits
// that long waits no longer than without the move; the bound keeps a long
// interval from parking hosts when the tracker cannot answer.
func tagMoveWait(p *pass) time.Duration {
	return min(p.set.LookupInterval.Std(), tagMoveWaitMax)
}

// lookupAnswer is a lookup as one pass sees it.
type lookupAnswer struct {
	discovery.Lookup

	followed bool
}

// lookedUp returns the newest lookup of a tag, asked once per pass.
func (p *pass) lookedUp(ref string) lookupAnswer {
	if a, ok := p.lookups[ref]; ok {
		return a
	}

	lk, ok := p.w.LookedUp(ref)
	a := lookupAnswer{Lookup: lk, followed: ok}
	p.lookups[ref] = a

	return a
}

// initTagMoves keeps the tag moves of the world that still hold: within
// their wait and of a need the host still reports.
func (p *pass) initTagMoves() {
	if p.w.LookedUp == nil {
		return
	}

	for i := range p.w.TagMoves {
		m := p.w.TagMoves[i]

		hv := p.hosts[m.Host]
		if hv == nil || !p.now.Before(m.EndedAt.Add(tagMoveWait(p))) ||
			!slices.ContainsFunc(hv.needs(), func(n protocol.Need) bool { return n.ID == m.NeedID }) {
			continue
		}

		p.moves[m.Host] = append(p.moves[m.Host], m)
	}
}

// noteTagMoves notes every image need the ended lease's agent superseded
// and asks for a lookup of its tag, unless the newest lookup already found
// another digest than the need's or the move repeats.
func (p *pass) noteTagMoves(l *fleet.Lease) {
	if p.w.LookedUp == nil || l.Operation == nil || p.hosts[l.Host] == nil {
		return
	}

	for i := range l.Needs {
		n := &l.Needs[i]
		if n.Kind != protocol.NeedImage || n.Image == nil || n.Image.Image == "" || n.DirectiveID != "" ||
			stepStatus(l.Operation, n.ID) != protocol.StepSuperseded {
			continue
		}

		moves := p.moves[l.Host]
		before := len(moves)
		moves = slices.DeleteFunc(moves, func(m TagMove) bool { return m.NeedID == n.ID })
		repeat := len(moves) < before

		p.moves[l.Host] = append(moves, TagMove{
			Host: l.Host, NeedID: n.ID, Ref: n.Image.Image, Digest: n.Image.ToDigest, EndedAt: p.now, Repeat: repeat,
		})

		lk := p.lookedUp(n.Image.Image)
		if repeat || !lk.followed || (lk.Digest != "" && lk.Digest != n.Image.ToDigest) ||
			slices.Contains(p.refresh, n.Image.Image) {
			continue
		}

		p.refresh = append(p.refresh, n.Image.Image)
	}
}

// tagMoveVerdict holds the needs back while one of them waits for its tag
// move; a group waits for all of its needs.
func (p *pass) tagMoveVerdict(host string, needs []protocol.Need) verdict {
	for _, m := range p.moves[host] {
		if !slices.ContainsFunc(needs, func(n protocol.Need) bool { return n.ID == m.NeedID }) {
			continue
		}

		if reason := p.tagMoveWaits(&m); reason != "" {
			return verdict{rank: rankPull, reason: reason}
		}
	}

	return verdict{}
}

// tagMoveWaits says what the move's need waits for; empty when it does not.
func (p *pass) tagMoveWaits(m *TagMove) string {
	if m.Repeat {
		return "the host saw " + m.Ref + " move on again, which the registry does not confirm; waiting until " +
			m.EndedAt.Add(tagMoveWait(p)).UTC().Format(time.TimeOnly)
	}

	lk := p.lookedUp(m.Ref)

	switch {
	case lk.Digest != "" && lk.Digest != m.Digest:
		return "waiting for the host to take up the new digest of " + m.Ref
	case lk.followed && lk.Error == "" && lk.Started.After(m.EndedAt):
		// The registry still says the need's digest.
		return ""
	default:
		return "waiting for a new lookup of " + m.Ref + ": the host saw it move on"
	}
}

// tagMoves lists the moves that hold after the pass, by host.
func (p *pass) tagMoves() []TagMove {
	var out []TagMove

	for _, name := range p.names {
		out = append(out, p.moves[name]...)
	}

	return out
}
