package lease

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// verify waits until every touched container has run for the window without
// a restart, or fails the operation at deadline.
func (o *operation) verify(ctx context.Context, names []string, deadline time.Time) error {
	o.setPhase(protocol.PhaseVerifying)

	var (
		baseline map[string]int
		stable   time.Time
		problem  = "containers were never observed"
	)

	for {
		counts, reason, err := o.restartCounts(ctx, names)

		switch {
		case err != nil && ctx.Err() != nil:
			return errInterrupted
		case err != nil:
			baseline, problem = nil, "cannot list containers: "+err.Error()
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

// restartCounts returns the named containers' restart counts, or why one is not up.
func (o *operation) restartCounts(
	ctx context.Context, names []string,
) (counts map[string]int, reason string, err error) {
	list, err := o.containers(ctx)
	if err != nil {
		return nil, "", err
	}

	counts = make(map[string]int, len(names))

	for _, name := range names {
		i := slices.IndexFunc(list, func(c protocol.Container) bool { return c.Name == name })

		switch {
		case i < 0:
			return nil, "container " + name + " does not exist", nil
		case list[i].State != protocol.StateRunning:
			return nil, "container " + name + " is " + list[i].State, nil
		}

		counts[name] = list[i].RestartCount
	}

	return counts, "", nil
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
