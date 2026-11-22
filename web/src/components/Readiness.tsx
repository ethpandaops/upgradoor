import type { Lease } from '../api/types';
import { formatDuration } from '../lib/format';
import { useSharedNow } from './RelTime';

/**
 * readinessNote is the lease's readiness note at now. While the controller
 * waits for the node to stay at head (readyAt set), the note's duration tail
 * is replaced by the time left; past readyAt the server has not released yet.
 */
export function readinessNote(lease: Pick<Lease, 'readiness' | 'readyAt'>, now: number): string | undefined {
  const { readiness, readyAt } = lease;
  const at = readyAt ? Date.parse(readyAt) : NaN;
  if (!readiness || Number.isNaN(at)) return readiness;

  const base = readiness.replace(/, waiting .*$/, '');
  const left = Math.ceil((at - now) / 1000);
  return left > 0 ? `${base}, ready in ${formatDuration(left)}` : `${base}, ready check pending`;
}

/** useReadinessNote is readinessNote on the shared clock. */
export function useReadinessNote(lease: Pick<Lease, 'readiness' | 'readyAt'>): string | undefined {
  return readinessNote(lease, useSharedNow());
}

/** Readiness renders the lease's readiness note, counting down while readyAt is set. */
export function Readiness({ lease }: { lease: Pick<Lease, 'readiness' | 'readyAt'> }) {
  return <span className="tabular-nums">{useReadinessNote(lease)}</span>;
}
