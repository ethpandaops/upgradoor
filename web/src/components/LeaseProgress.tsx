import { list, type Lease } from '../api/types';
import { cx } from './primitives';

const STEPS = ['Drain', 'Apply', 'Reboot', 'Start', 'Verify', 'Soak'] as const;
type StepName = (typeof STEPS)[number];

/** leaseStep says which step a lease is in, or -1 before the agent picked it up. */
function leaseStep(lease: Lease): { steps: StepName[]; current: number; failed: boolean } {
  const reboots = list(lease.needs).some(n => n.kind === 'reboot');
  const steps = STEPS.filter(s => s !== 'Reboot' || reboots);
  const at = (name: StepName) => steps.indexOf(name);
  const failed = lease.state === 'failed' || lease.operation?.phase === 'failed';

  if (lease.state === 'verifying') return { steps, current: at('Soak'), failed };

  switch (lease.operation?.phase) {
    case 'draining':
      return { steps, current: at('Drain'), failed };
    case 'applying':
      return { steps, current: at('Apply'), failed };
    case 'rebooting':
      return { steps, current: Math.max(at('Reboot'), at('Apply')), failed };
    case 'starting':
      return { steps, current: at('Start'), failed };
    case 'verifying':
      return { steps, current: at('Verify'), failed };
    case 'done':
      return { steps, current: at('Soak'), failed };
    case 'failed':
      // The agent does not say in which phase it failed.
      return { steps, current: -1, failed: true };
    case undefined:
      return { steps, current: -1, failed };
  }
}

/**
 * LeaseProgress shows where a host is in its lease: the agent drains, applies
 * (and reboots), starts and verifies; then the controller waits for the
 * validators to attest again (the soak) before it releases the lease.
 */
export function LeaseProgress({ lease }: { lease: Lease }) {
  const { steps, current, failed } = leaseStep(lease);
  const label =
    current >= 0
      ? `Step ${current + 1} of ${steps.length}: ${steps[current] as string}`
      : failed
        ? 'The agent reported a failure'
        : 'Waiting for the agent to pick up the lease';

  return (
    <ol aria-label={label} className="flex items-center gap-1">
      {steps.map((name, i) => {
        const state = i < current ? 'done' : i === current ? (failed ? 'failed' : 'current') : 'todo';
        return (
          <li key={name} className="flex min-w-0 flex-1 flex-col gap-1">
            <span
              className={cx(
                'h-1 rounded-full',
                state === 'done' && 'bg-accent',
                state === 'current' && 'animate-pulse bg-accent',
                state === 'failed' && 'bg-danger-solid',
                state === 'todo' && (failed ? 'bg-danger-subtle' : 'bg-surface-raised')
              )}
            />
            <span
              className={cx(
                'truncate text-xs/4',
                state === 'current'
                  ? 'font-semibold text-foreground'
                  : state === 'failed'
                    ? 'font-semibold text-danger'
                    : 'text-faint'
              )}
            >
              {name}
            </span>
          </li>
        );
      })}
    </ol>
  );
}
