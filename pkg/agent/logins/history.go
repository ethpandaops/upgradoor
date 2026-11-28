package logins

import (
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// Window is how long after a session ends the next one of the same
	// identity, login user and address still joins its group; a group with
	// no open session closes after it.
	Window = 10 * time.Minute
	// reapGrace is how long a session's process may be gone before the
	// session ends unknown: its logout may still be on the way to the
	// journal.
	reapGrace = time.Minute
	// MaxClosed bounds the closed groups waiting for an acknowledgement;
	// the oldest are dropped.
	MaxClosed = 2000
	// maxSessions bounds the open sessions; the oldest end unknown.
	maxSessions = 4096
)

// Lookup returns who the key with a fingerprint belongs to: its identity
// and source, or "" and SourceUnknown.
type Lookup func(fingerprint string) (identity, source string)

// History is the host's SSH login history, persisted by the agent as it is.
type History struct {
	// Cursor is the journal position after the last entry read; ReadTo is
	// the time the journal is read up to.
	Cursor string    `json:"cursor,omitempty"`
	ReadTo time.Time `json:"readTo"`
	// NextSeq is the Seq of the next group.
	NextSeq  int64     `json:"nextSeq"`
	Sessions []Session `json:"sessions,omitempty"`
	// Groups are the groups that are not closed.
	Groups []protocol.SSHLoginGroup `json:"groups,omitempty"`
	// Closed are the closed groups not acknowledged yet, oldest first.
	Closed []protocol.SSHLoginGroup `json:"closed,omitempty"`
	// Dropped counts the closed groups dropped unsent.
	Dropped int64 `json:"dropped,omitempty"`
}

// Session is one open SSH session.
type Session struct {
	Boot string `json:"boot"`
	PID  int    `json:"pid"`
	From string `json:"from"`
	Port int    `json:"port"`
	// Group is the Seq of the session's group.
	Group int64     `json:"group"`
	Start time.Time `json:"start"`
	// LastSeen is when its process was last seen running.
	LastSeen time.Time `json:"lastSeen"`
}

// NewHistory starts a history that reads the journal from since. Its
// sequence starts at the time in milliseconds, above that of any history
// this host started before.
func NewHistory(since time.Time) *History {
	return &History{ReadTo: since, NextSeq: since.UnixMilli()}
}

// Apply reads journal entries in order: a login opens a session (and joins
// or opens a group), a logout or a closed connection ends it. The cursor
// moves to the last entry.
func (h *History) Apply(entries []Entry, lookup Lookup) {
	for i := range entries {
		e := &entries[i]
		h.Cursor = e.Cursor

		if e.Time.After(h.ReadTo) {
			h.ReadTo = e.Time
		}

		ev := parseMessage(e.Message)

		switch ev.kind {
		case eventLogin:
			h.login(e, &ev, lookup)
		case eventEndAddr:
			if i := h.find(func(s *Session) bool {
				return s.Boot == e.Boot && s.From == ev.from && s.Port == ev.port
			}); i >= 0 {
				h.end(i, e.Time, false)
			}
		case eventEndProcess:
			if i := h.find(func(s *Session) bool { return s.Boot == e.Boot && s.PID == e.PID }); i >= 0 {
				h.end(i, e.Time, false)
			}
		case eventNone:
		}
	}
}

// Reap ends unknown the sessions whose process is gone: those of another
// boot at once, at their last sighting; those whose process does not run
// after reapGrace.
func (h *History) Reap(boot string, alive func(pid int) bool, now time.Time) {
	for i := 0; i < len(h.Sessions); {
		s := &h.Sessions[i]

		switch {
		case s.Boot == boot && alive(s.PID):
			s.LastSeen = now
		case s.Boot != boot || now.Sub(s.LastSeen) >= reapGrace:
			h.end(i, s.LastSeen, true)

			continue
		}

		i++
	}
}

// CloseIdle closes the groups without an open session whose last session
// ended Window ago or longer, oldest first; beyond MaxClosed the oldest
// closed groups are dropped.
func (h *History) CloseIdle(now time.Time) {
	h.Groups = slices.DeleteFunc(h.Groups, func(g protocol.SSHLoginGroup) bool {
		if g.Open > 0 || now.Sub(lastActivity(&g)) < Window {
			return false
		}

		h.Closed = append(h.Closed, g)

		return true
	})

	if over := len(h.Closed) - MaxClosed; over > 0 {
		h.Closed = slices.Delete(h.Closed, 0, over)
		h.Dropped += int64(over)
	}
}

// Report is what the next sync carries: the open groups (the newest
// protocol.MaxSSHOpen), the oldest closed ones (at most
// protocol.MaxSSHClosed) and the dropped count.
func (h *History) Report() *protocol.SSHReport {
	open := h.Groups[max(0, len(h.Groups)-protocol.MaxSSHOpen):]

	return &protocol.SSHReport{
		Open:    cloneGroups(open),
		Closed:  cloneGroups(h.Closed[:min(len(h.Closed), protocol.MaxSSHClosed)]),
		Dropped: h.Dropped,
	}
}

// Ack drops what a verified answer acknowledged of a sent report: its
// closed groups up to ack, and its dropped count.
func (h *History) Ack(sent *protocol.SSHReport, ack int64) {
	acked := make(map[int64]struct{}, len(sent.Closed))

	for i := range sent.Closed {
		if sent.Closed[i].Seq <= ack {
			acked[sent.Closed[i].Seq] = struct{}{}
		}
	}

	h.Closed = slices.DeleteFunc(h.Closed, func(g protocol.SSHLoginGroup) bool {
		_, ok := acked[g.Seq]

		return ok
	})

	h.Dropped = max(0, h.Dropped-sent.Dropped)
}

// login opens a session and puts it into the open group of its identity,
// user and address, or a new one.
func (h *History) login(e *Entry, ev *event, lookup Lookup) {
	if h.find(func(s *Session) bool { return s.Boot == e.Boot && s.PID == e.PID && s.Port == ev.port }) >= 0 {
		return
	}

	identity, source := "", SourceUnknown
	if ev.fingerprint != "" && lookup != nil {
		identity, source = lookup(ev.fingerprint)
	}

	keyType := ev.keyType
	if ev.fingerprint == "" {
		keyType = ev.method
	}

	g := h.join(identity, ev.fingerprint, ev.user, ev.from, e.Time)
	if g == nil {
		h.Groups = append(h.Groups, protocol.SSHLoginGroup{
			Seq: h.NextSeq, Identity: identity, Source: source, Fingerprint: ev.fingerprint,
			KeyType: keyType, User: ev.user, From: ev.from, FirstLogin: e.Time,
		})
		h.NextSeq++
		g = &h.Groups[len(h.Groups)-1]
	}

	g.Sessions++
	g.Open++

	h.Sessions = append(h.Sessions, Session{
		Boot: e.Boot, PID: e.PID, From: ev.from, Port: ev.port, Group: g.Seq, Start: e.Time, LastSeen: e.Time,
	})

	if over := len(h.Sessions) - maxSessions; over > 0 {
		for range over {
			h.end(0, h.Sessions[0].LastSeen, true)
		}
	}
}

// join returns the open group a session of a key, user and address that
// starts at start joins, or nil. The key is its identity, or its
// fingerprint when it has none.
func (h *History) join(identity, fingerprint, user, from string, start time.Time) *protocol.SSHLoginGroup {
	for i := len(h.Groups) - 1; i >= 0; i-- {
		g := &h.Groups[i]

		sameKey := g.Identity == identity && (identity != "" || g.Fingerprint == fingerprint)
		if !sameKey || g.User != user || g.From != from {
			continue
		}

		if g.Open > 0 || start.Sub(lastActivity(g)) <= Window {
			return g
		}
	}

	return nil
}

// end ends session i at at; unknown marks an end not in the journal.
func (h *History) end(i int, at time.Time, unknown bool) {
	s := h.Sessions[i]
	h.Sessions = slices.Delete(h.Sessions, i, i+1)

	gi := slices.IndexFunc(h.Groups, func(g protocol.SSHLoginGroup) bool { return g.Seq == s.Group })
	if gi < 0 {
		return
	}

	g := &h.Groups[gi]
	g.Open = max(0, g.Open-1)

	at = maxTime(at, s.Start)
	d := at.Sub(s.Start).Milliseconds()
	g.TotalMillis += d
	g.LongestMillis = max(g.LongestMillis, d)
	g.EndUnknown = g.EndUnknown || unknown

	if g.LastLogout == nil || at.After(*g.LastLogout) {
		t := at
		g.LastLogout = &t
	}
}

// find returns the index of the first session match accepts, or -1.
func (h *History) find(match func(s *Session) bool) int {
	for i := range h.Sessions {
		if match(&h.Sessions[i]) {
			return i
		}
	}

	return -1
}

// lastActivity is a group's newest logout, or its first login.
func lastActivity(g *protocol.SSHLoginGroup) time.Time {
	if g.LastLogout != nil {
		return maxTime(*g.LastLogout, g.FirstLogin)
	}

	return g.FirstLogin
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// cloneGroups copies groups with their logout times.
func cloneGroups(in []protocol.SSHLoginGroup) []protocol.SSHLoginGroup {
	if len(in) == 0 {
		return nil
	}

	out := slices.Clone(in)

	for i := range out {
		if out[i].LastLogout != nil {
			t := *out[i].LastLogout
			out[i].LastLogout = &t
		}
	}

	return out
}
