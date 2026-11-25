package lease

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Bounds of the verifying phase.
const (
	// stableUp is how long every touched container must have run without a
	// restart when verifying ends; at most the window.
	stableUp = 30 * time.Second
	// maxVerify bounds verifying past the window while a container is not up
	// for stableUp; at least the window.
	maxVerify = 3 * time.Minute
)

// watched is what verifying saw of one touched container.
type watched struct {
	// count and started identify the current run.
	count   int
	started time.Time
	// restarts counts the runs that ended while verifying.
	restarts int
	// state is empty while the container is missing.
	state    string
	exitCode int
	// upSince is when the current run was first seen running; zero while it
	// is not running.
	upSince time.Time
	longest time.Duration
}

// verify watches the touched containers for the window and passes when each
// has then run for stableUp without a restart. One that restarted late
// extends the window up to maxVerify; one that is missing, exited or dead
// at the end of the window fails it, as does one not up for stableUp at the
// bound. The bound never passes deadline.
func (o *operation) verify(ctx context.Context, names []string, deadline time.Time) error {
	o.setPhase(protocol.PhaseVerifying)

	var (
		start = o.now()
		need  = min(stableUp, o.r.cfg.Window)
		end   = start.Add(o.r.cfg.Window)
		bound = start.Add(max(maxVerify, o.r.cfg.Window))
		seen  = make(map[string]*watched, len(names))
	)

	if deadline.Before(bound) {
		bound = deadline
	}

	for {
		list, err := o.containers(ctx)
		now := o.now()
		listErr := ""

		switch {
		case err != nil && ctx.Err() != nil:
			return errInterrupted
		case err != nil:
			listErr = "cannot list containers: " + err.Error()
		default:
			observe(seen, list, names, now)
		}

		if listErr == "" && !now.Before(end) {
			if reason := gone(seen, names); reason != "" {
				o.fail(reason)

				return nil
			}

			if unstable(seen, names, now, need) == "" {
				return nil
			}
		}

		if !now.Before(bound) {
			if reason := cmp.Or(listErr, unstable(seen, names, now, need)); reason != "" {
				o.fail(reason)
			}

			return nil
		}

		if !sleep(ctx, o.r.poll) {
			return errInterrupted
		}
	}
}

// observe takes in one listing of the containers.
func observe(seen map[string]*watched, list []protocol.Container, names []string, now time.Time) {
	for _, name := range names {
		w := seen[name]

		var c *protocol.Container

		for i := range list {
			if list[i].Name == name {
				c = &list[i]

				break
			}
		}

		if c == nil {
			if w != nil {
				w.state, w.upSince = "", time.Time{}
			}

			continue
		}

		if w == nil {
			w = &watched{count: c.RestartCount, started: c.StartedAt}
			seen[name] = w
		}

		restarted := c.RestartCount != w.count || !c.StartedAt.Equal(w.started)
		if restarted {
			w.restarts += max(c.RestartCount-w.count, 1)
			w.count, w.started = c.RestartCount, c.StartedAt
		}

		w.state, w.exitCode = c.State, c.ExitCode

		switch {
		case c.State != protocol.StateRunning:
			w.upSince = time.Time{}
		case restarted || w.upSince.IsZero():
			w.upSince = now
		default:
			w.longest = max(w.longest, now.Sub(w.upSince))
		}
	}
}

// gone returns why a container will not come back: it is missing, exited
// or dead.
func gone(seen map[string]*watched, names []string) string {
	for _, name := range names {
		w := seen[name]

		switch {
		case w == nil || w.state == "":
			return "container " + name + " does not exist"
		case w.state == protocol.StateExited || w.state == protocol.StateDead:
			return fmt.Sprintf("container %s is %s (exit code %d)", name, w.state, w.exitCode)
		}
	}

	return ""
}

// unstable returns why a container has not run for need without a restart,
// or "" when every one has.
func unstable(seen map[string]*watched, names []string, now time.Time, need time.Duration) string {
	for _, name := range names {
		w := seen[name]
		if w == nil || w.state == "" {
			return "container " + name + " does not exist"
		}

		running := w.state == protocol.StateRunning
		up := time.Duration(0)

		if running {
			up = now.Sub(w.upSince)
		}

		switch {
		case running && up >= need:
			continue
		case w.restarts > 0 && max(w.longest, up) < need:
			return fmt.Sprintf("container %s kept restarting: %d restarts, never up %s", name, w.restarts, need)
		case w.restarts > 0 && running:
			return fmt.Sprintf("container %s kept restarting: %d restarts, up %s at the end",
				name, w.restarts, up.Round(time.Second))
		case w.restarts > 0:
			return fmt.Sprintf("container %s kept restarting: %d restarts, %s at the end", name, w.restarts, w.state)
		case running:
			return fmt.Sprintf("container %s up only %s", name, up.Round(time.Second))
		default:
			return "container " + name + " is " + w.state
		}
	}

	return ""
}
