import { list, type ActionRun, type Lease } from '../api/types';
import { Disclosure } from './icons';
import { Badge } from './primitives';

/** leaseRun is how the host action of a lease ended, as its step reports it. */
export function leaseRun(lease: Lease | undefined): ActionRun | undefined {
  return list(lease?.operation?.steps).find(s => s.action)?.action;
}

/** RunExit is the outcome of an action run: its exit code, timed out, or unknown after an agent restart. */
export function RunExit({ run }: { run: ActionRun }) {
  return (
    <span className="inline-flex items-center gap-1">
      {run.unknown ? (
        <Badge tone="warning" title="The agent restarted while the action ran: whether it finished is not known">
          outcome unknown
        </Badge>
      ) : (
        <Badge tone={run.exitCode === 0 && !run.timedOut ? 'success' : 'danger'} title="The exit code of the action">
          exit {run.exitCode}
        </Badge>
      )}
      {run.timedOut && <Badge tone="danger">timed out</Badge>}
    </span>
  );
}

/** RunOutput is the tail of an action's output, folded away. */
export function RunOutput({ run, label }: { run: ActionRun; label: string }) {
  if (!run.output) return null;

  const lines = run.output.replace(/\n$/, '').split('\n').length;

  return (
    <Disclosure summary={`output (last ${lines} ${lines === 1 ? 'line' : 'lines'})`}>
      <pre
        aria-label={`Output of ${label}`}
        className="max-h-64 max-w-3xl overflow-auto rounded-sm border border-border bg-background p-2 font-mono text-xs/5 whitespace-pre-wrap"
      >
        {run.output}
      </pre>
    </Disclosure>
  );
}

/** ActionRunView is an action run in full: the outcome, and the output under it. */
export function ActionRunView({ run, label }: { run: ActionRun; label: string }) {
  return (
    <span className="flex flex-col items-start gap-1">
      <RunExit run={run} />
      <RunOutput run={run} label={label} />
    </span>
  );
}
