package agentapi

import (
	"math/rand/v2"
	"sync"
	"time"
)

// Sync pacing: every answer to a sync tells the agent when to sync next. An
// idle host is asked to come back at a pace that keeps the whole fleet near
// idleRate syncs per second; a busy one (a lease, a directive, near the front
// of a queue) at a pace that keeps all busy hosts near busyRate. Both slow
// down while the controller is loaded: sync requests that take longer than
// targetLatency, or sync slots more than half taken. Each interval is spread
// by ±pacingJitter so that hosts that came up together do not stay in step.
const (
	idleMinInterval = 30 * time.Second
	idleMaxInterval = 5 * time.Minute
	busyMinInterval = 5 * time.Second
	busyMaxInterval = 15 * time.Second

	idleRate = 20.0
	busyRate = 25.0

	targetLatency = 100 * time.Millisecond
	maxLoadFactor = 4.0
	// busyLoadFactor caps how much load slows busy hosts.
	busyLoadFactor = 2.0
	pacingJitter   = 0.1

	// queueBusy is the queue position up to which a waiting host counts as
	// busy: it is about to be granted a lease.
	queueBusy = 5

	// pacedForget is how long a host that stopped syncing is still counted.
	pacedForget = 3 * idleMaxInterval
	// latencyWeight is the weight of one sample in the latency average.
	latencyWeight = 0.05
)

// pace is the pair of intervals the controller asks for right now, before
// jitter.
type pace struct {
	idle, busy time.Duration
}

// bound is the longest interval an answer under this pace asks for.
func (p pace) bound() time.Duration {
	return time.Duration(float64(p.idle) * (1 + pacingJitter))
}

// pacer decides the sync interval each answer asks of its agent. It is safe
// for concurrent use.
type pacer struct {
	mu sync.Mutex
	// seen is when each host last synced and whether it was busy then.
	seen      map[string]pacedHost
	busy      int
	latency   float64
	lastPrune time.Time
}

// pacedHost is one host as the pacer counts it.
type pacedHost struct {
	at   time.Time
	busy bool
}

func newPacer() *pacer {
	return &pacer{seen: make(map[string]pacedHost, 64)}
}

// observe takes the handling time of one sync.
func (p *pacer) observe(took time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.latency == 0 {
		p.latency = took.Seconds()

		return
	}

	p.latency += latencyWeight * (took.Seconds() - p.latency)
}

// current is the pace for the fleet as counted at now, with slotsTaken the
// share of sync slots in use.
func (p *pacer) current(now time.Time, slotsTaken float64) pace {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.prune(now)

	load := min(max(1, p.latency/targetLatency.Seconds(), slotsTaken/0.5), maxLoadFactor)
	hosts := float64(max(1, len(p.seen)))
	busy := float64(max(1, p.busy))

	return pace{
		idle: clampDuration(scaled(max(idleMinInterval, perRate(hosts, idleRate)), load),
			idleMinInterval, idleMaxInterval),
		busy: clampDuration(scaled(max(busyMinInterval, perRate(busy, busyRate)),
			min(load, busyLoadFactor)), busyMinInterval, busyMaxInterval),
	}
}

// ask records the host's sync at now and returns the interval its answer
// asks for: the pace's busy or idle one, jittered.
func (p *pacer) ask(host string, now time.Time, busy bool, pc pace) time.Duration {
	p.mu.Lock()

	if prev, ok := p.seen[host]; ok && prev.busy {
		p.busy--
	}

	if busy {
		p.busy++
	}

	p.seen[host] = pacedHost{at: now, busy: busy}
	p.mu.Unlock()

	base := pc.idle
	if busy {
		base = pc.busy
	}

	return scaled(base, 1+pacingJitter*(2*rand.Float64()-1)) //nolint:gosec // jitter only
}

// prune forgets hosts that stopped syncing, at most once a minute.
func (p *pacer) prune(now time.Time) {
	if now.Sub(p.lastPrune) < time.Minute {
		return
	}

	p.lastPrune = now

	for host, h := range p.seen {
		if now.Sub(h.at) > pacedForget {
			if h.busy {
				p.busy--
			}

			delete(p.seen, host)
		}
	}
}

// perRate is the interval at which n hosts make rate requests per second.
func perRate(n, rate float64) time.Duration {
	return time.Duration(n / rate * float64(time.Second))
}

func scaled(d time.Duration, f float64) time.Duration {
	return time.Duration(float64(d) * f).Round(time.Millisecond)
}

func clampDuration(d, lo, hi time.Duration) time.Duration {
	return min(max(d, lo), hi)
}
