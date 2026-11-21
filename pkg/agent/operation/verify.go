package operation

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/agent/needs"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// verify waits until every touched container has been running, without a
// restart, for verify.window in one piece. It gives up at deadline, the
// verify.timeout that began with the starting phase.
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

		if baseline != nil && o.now().Sub(stable) >= o.cfg.Verify.Window {
			return nil
		}

		if !o.now().Before(deadline) {
			o.fail(fmt.Sprintf("containers did not stay up for %s within %s: %s",
				o.cfg.Verify.Window, o.cfg.Verify.Timeout, problem))

			return nil
		}

		if !sleep(ctx, o.poll) {
			return errInterrupted
		}
	}
}

// restartCounts returns the restart count of every named container, or the
// reason why one of them does not count as up.
func (o *operation) restartCounts(
	ctx context.Context, names []string,
) (counts map[string]int, reason string, err error) {
	containers, err := o.observe(ctx)
	if err != nil {
		return nil, "", err
	}

	counts = make(map[string]int, len(names))

	for _, name := range names {
		c := needs.FindContainer(containers, name)

		switch {
		case c == nil:
			return nil, "container " + name + " does not exist", nil
		case c.State != protocol.StateRunning:
			return nil, "container " + name + " is " + c.State, nil
		}

		counts[name] = c.RestartCount
	}

	return counts, "", nil
}

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
