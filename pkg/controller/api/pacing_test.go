package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPacer(t *testing.T) {
	now := time.Unix(10_000, 0)

	within := func(t *testing.T, got, want time.Duration) {
		t.Helper()
		assert.InDelta(t, float64(want), float64(got), float64(want)*pacingJitter+1e6, "%s", got)
	}

	t.Run("small fleet keeps the floors", func(t *testing.T) {
		p := newPacer()
		pc := p.current(now, 0)
		assert.Equal(t, pace{idle: idleMinInterval, busy: busyMinInterval}, pc)
		within(t, p.ask("a", now, false, pc), idleMinInterval)
		within(t, p.ask("a", now, true, pc), busyMinInterval)
	})

	t.Run("a large fleet spreads out, its busy hosts stay quick", func(t *testing.T) {
		p := newPacer()
		pc := p.current(now, 0)

		for i := range 2000 {
			p.ask(fmt.Sprintf("h%d", i), now, i < 100, pc)
		}

		pc = p.current(now, 0)
		assert.Equal(t, 100*time.Second, pc.idle, "2000 hosts at 20/s")
		assert.Equal(t, busyMinInterval, pc.busy, "100 busy hosts at 25/s")
		assert.Equal(t, 110*time.Second, pc.bound())
	})

	t.Run("load slows everyone, busy hosts less", func(t *testing.T) {
		p := newPacer()
		p.observe(time.Second)
		pc := p.current(now, 0)
		assert.Equal(t, time.Duration(maxLoadFactor)*idleMinInterval, pc.idle)
		assert.Equal(t, time.Duration(busyLoadFactor)*busyMinInterval, pc.busy)

		p = newPacer()
		assert.Equal(t, 2*idleMinInterval, p.current(now, 1).idle, "all slots taken")
	})

	t.Run("silent hosts are forgotten", func(t *testing.T) {
		p := newPacer()
		pc := p.current(now, 0)
		p.ask("a", now, true, pc)
		p.ask("b", now, false, pc)
		p.current(now.Add(pacedForget+2*time.Minute), 0)
		assert.Empty(t, p.seen)
		assert.Zero(t, p.busy)
	})
}
