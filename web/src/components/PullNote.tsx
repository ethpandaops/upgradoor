import type { Lease } from '../api/types';
import { formatBytes, formatDuration, isZeroTime } from '../lib/format';
import { cx } from './primitives';
import { useSharedNow } from './RelTime';

/**
 * QUIET_MS is how long a pull may go without reported progress before the
 * note says so. The agent writes progress every 30s and reports it with its
 * next sync, so a shorter quiet is no sign of a stall.
 */
const QUIET_MS = 90_000;

function since(iso: string | undefined, now: number): number | null {
  if (isZeroTime(iso)) return null;
  const t = Date.parse(iso as string);
  return Number.isNaN(t) ? null : Math.max(0, (now - t) / 1000);
}

/**
 * pullNote describes the pull of an operation in the pulling phase: "pulling
 * <image> — 412 MiB of 933 MiB, 3/7 layers, attempt 2, 7m 2s", and how long
 * the daemon has reported nothing once that is a while. An agent that does
 * not report its pull gets the time since the operation started.
 */
export function pullNote(operation: Lease['operation'], now: number): { text: string; quiet?: string } | undefined {
  if (operation?.phase !== 'pulling') return undefined;

  const pull = operation.pull;
  const elapsed = since(pull?.startedAt ?? operation.startedAt, now);
  if (!pull) return { text: elapsed === null ? 'pulling' : `pulling for ${formatDuration(elapsed)}` };

  const facts: string[] = [];
  if (pull.bytesTotal) facts.push(`${formatBytes(pull.bytesDone ?? 0)} of ${formatBytes(pull.bytesTotal)}`);
  if (pull.layersTotal) facts.push(`${pull.layersDone ?? 0}/${pull.layersTotal} layers`);
  if ((pull.attempt ?? 1) > 1) facts.push(`attempt ${pull.attempt}`);
  if (elapsed !== null) facts.push(formatDuration(elapsed));

  const head = pull.image ? `pulling ${pull.image}` : 'pulling';
  const text = facts.length > 0 ? `${head} — ${facts.join(', ')}` : head;
  const silent = since(pull.lastProgressAt, now);

  return silent !== null && silent * 1000 >= QUIET_MS
    ? { text, quiet: `no progress for ${formatDuration(silent)}` }
    : { text };
}

/** PullNote shows pullNote for an operation, ticking with the shared clock; nothing outside the pulling phase. */
export function PullNote({ operation, className }: { operation: Lease['operation']; className?: string }) {
  const note = pullNote(operation, useSharedNow());
  if (!note) return null;

  return (
    <span className={cx('text-muted', className)}>
      {note.text}
      {note.quiet && <span className="text-warning"> · {note.quiet}</span>}
    </span>
  );
}
