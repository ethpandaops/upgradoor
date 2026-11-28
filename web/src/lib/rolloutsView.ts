// Which list the Rollouts tab shows, kept in the URL hash after #rollouts, and
// the counts of its tabs.

import { list, type Lease, type Rollout, type Rollouts } from '../api/types';
import { waitsForApproval } from './approvals';

export type RolloutsView = 'open' | 'leases' | 'finished';
export type LeaseView = 'active' | 'recent';

/** ROLLOUTS_PLACE is the hash part after #rollouts that opens each list. */
export const ROLLOUTS_PLACE = {
  open: '',
  leases: 'leases',
  recentLeases: 'leases/recent',
  finished: 'finished',
} as const;

/** rolloutsPlace reads the hash part after #rollouts; anything unknown is the open rollouts. */
export function rolloutsPlace(sub: string): { view: RolloutsView; leases: LeaseView } {
  const [view, leases] = sub.split('/');
  if (view === 'leases') return { view, leases: leases === 'recent' ? 'recent' : 'active' };
  if (view === 'finished') return { view, leases: 'active' };
  return { view: 'open', leases: 'active' };
}

/** placeOf is the hash part after #rollouts of a list. */
export function placeOf(view: RolloutsView, leases: LeaseView = 'active'): string {
  if (view === 'leases') return leases === 'recent' ? ROLLOUTS_PLACE.recentLeases : ROLLOUTS_PLACE.leases;
  return ROLLOUTS_PLACE[view];
}

/**
 * leasePlace is where the Rollouts tab shows the first of these leases that
 * the stream knows: the active leases, the recent ones, or the open rollouts
 * when it knows none of them.
 */
export function leasePlace(ids: readonly string[], rollouts: Rollouts | undefined): string {
  const has = (leases: readonly Lease[]) => leases.some(l => ids.includes(l.id));
  if (has(list(rollouts?.leases))) return ROLLOUTS_PLACE.leases;
  if (has(list(rollouts?.recentLeases))) return ROLLOUTS_PLACE.recentLeases;
  return ROLLOUTS_PLACE.open;
}

/** OpenSummary counts the open rollouts by what they do; a rollout counts once, by its first match. */
export interface OpenSummary {
  total: number;
  halted: number;
  paused: number;
  approval: number;
  active: number;
}

export function openSummary(rollouts: readonly Rollout[]): OpenSummary {
  const out: OpenSummary = { total: rollouts.length, halted: 0, paused: 0, approval: 0, active: 0 };
  for (const r of rollouts) {
    if (r.state === 'halted') out.halted++;
    else if (r.paused) out.paused++;
    else if (waitsForApproval(r)) out.approval++;
    else out.active++;
  }
  return out;
}

export interface LeaseSummary {
  active: number;
  running: number;
  verifying: number;
  recent: number;
  released: number;
  failed: number;
  expired: number;
  cancelled: number;
}

/** leaseSummary counts the active leases by phase and the recently ended ones by how they ended. */
export function leaseSummary(active: readonly Lease[], recent: readonly Lease[]): LeaseSummary {
  const count = (leases: readonly Lease[], ...states: Lease['state'][]) =>
    leases.filter(l => states.includes(l.state)).length;
  return {
    active: active.length,
    running: count(active, 'granted', 'running'),
    verifying: count(active, 'verifying'),
    recent: recent.length,
    released: count(recent, 'released'),
    failed: count(recent, 'failed'),
    expired: count(recent, 'expired'),
    cancelled: count(recent, 'cancelled'),
  };
}

export interface FinishedSummary {
  total: number;
  completed: number;
  superseded: number;
  aborted: number;
}

/** finishedSummary counts the recently finished rollouts by how they ended. */
export function finishedSummary(rollouts: readonly Rollout[]): FinishedSummary {
  const count = (state: Rollout['state']) => rollouts.filter(r => r.state === state).length;
  return {
    total: rollouts.length,
    completed: count('completed'),
    superseded: count('superseded'),
    aborted: count('aborted'),
  };
}
