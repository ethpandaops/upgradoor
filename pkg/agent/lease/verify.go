package lease

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// crashLoopRestarts is the restart count increase that fails the verifying
// phase before its timeout.
const crashLoopRestarts = 3

// verify waits until every touched container has run for the window without
// a restart, or fails the operation at deadline. A container that keeps
// restarting or exited for good fails it at once.
func (o *operation) verify(ctx context.Context, names []string, deadline time.Time) error {
	o.setPhase(protocol.PhaseVerifying)

	var (
		first    = make(map[string]int, len(names))
		baseline map[string]int
		stable   time.Time
		problem  = "containers were never observed"
	)

	for {
		list, err := o.containers(ctx)

		switch {
		case err != nil && ctx.Err() != nil:
			return errInterrupted
		case err != nil:
			baseline, problem = nil, "cannot list containers: "+err.Error()
		default:
			if fatal := crashed(list, names, first); fatal != "" {
				o.fail(fatal)

				return nil
			}

			counts, reason := restartCounts(list, names)

			switch {
			case reason != "":
				baseline, problem = nil, reason
			case baseline == nil:
				baseline, stable = counts, o.now()
			default:
				if name := restarted(baseline, counts); name != "" {
					problem = "container " + name + " restarted"
					baseline, stable = counts, o.now()
				}
			}
		}

		if baseline != nil && o.now().Sub(stable) >= o.r.cfg.Window {
			return nil
		}

		if !o.now().Before(deadline) {
			o.fail(fmt.Sprintf("containers did not stay up for %s within %s: %s",
				o.r.cfg.Window, o.r.cfg.Timeout, problem))

			return nil
		}

		if !sleep(ctx, o.r.poll) {
			return errInterrupted
		}
	}
}

// crashed returns why one of the named containers will not come up: it
// exited without a restart, or restarted crashLoopRestarts times since its
// restart count was first recorded in first.
func crashed(list []protocol.Container, names []string, first map[string]int) string {
	for _, name := range names {
		i := slices.IndexFunc(list, func(c protocol.Container) bool { return c.Name == name })
		if i < 0 {
			continue
		}

		c := &list[i]

		switch c.State {
		case protocol.StateExited, protocol.StateDead:
			return fmt.Sprintf("container %s is %s (exit code %d)", name, c.State, c.ExitCode)
		}

		base, ok := first[name]
		if !ok {
			first[name] = c.RestartCount

			continue
		}

		if n := c.RestartCount - base; n >= crashLoopRestarts {
			return fmt.Sprintf("container %s is restarting: %d restarts", name, n)
		}
	}

	return ""
}

// restartCounts returns the named containers' restart counts, or why one is not up.
func restartCounts(list []protocol.Container, names []string) (counts map[string]int, reason string) {
	counts = make(map[string]int, len(names))

	for _, name := range names {
		i := slices.IndexFunc(list, func(c protocol.Container) bool { return c.Name == name })

		switch {
		case i < 0:
			return nil, "container " + name + " does not exist"
		case list[i].State != protocol.StateRunning:
			return nil, "container " + name + " is " + list[i].State
		}

		counts[name] = list[i].RestartCount
	}

	return counts, ""
}

// restarted returns the first name whose restart count changed.
func restarted(before, after map[string]int) string {
	names := make([]string, 0, len(after))
	for name := range after {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if before[name] != after[name] {
			return name
		}
	}

	return ""
}
