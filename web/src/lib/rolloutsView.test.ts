import { describe, expect, it } from 'vitest';

import type { Lease, Rollout, Rollouts } from '../api/types';
import { finishedSummary, leasePlace, leaseSummary, openSummary, placeOf, rolloutsPlace } from './rolloutsView';

const lease = (id: string, state: Lease['state']) => ({ id, state }) as Lease;
const rollout = (p: Partial<Rollout>) =>
  ({ state: 'active', paused: false, policy: 'auto', approved: false, ...p }) as Rollout;

describe('rolloutsPlace and placeOf', () => {
  it('reads the list from the hash and writes it back', () => {
    expect(rolloutsPlace('')).toEqual({ view: 'open', leases: 'active' });
    expect(rolloutsPlace('leases')).toEqual({ view: 'leases', leases: 'active' });
    expect(rolloutsPlace('leases/recent')).toEqual({ view: 'leases', leases: 'recent' });
    expect(rolloutsPlace('finished')).toEqual({ view: 'finished', leases: 'active' });
    expect(rolloutsPlace('nonsense/recent')).toEqual({ view: 'open', leases: 'active' });

    expect(placeOf('open')).toBe('');
    expect(placeOf('leases')).toBe('leases');
    expect(placeOf('leases', 'recent')).toBe('leases/recent');
    expect(placeOf('finished', 'recent')).toBe('finished');
  });
});

describe('leasePlace', () => {
  const data = { leases: [lease('a', 'running')], recentLeases: [lease('b', 'released')] } as unknown as Rollouts;

  it('opens the list that holds the lease', () => {
    expect(leasePlace(['a'], data)).toBe('leases');
    expect(leasePlace(['b'], data)).toBe('leases/recent');
    expect(leasePlace(['b', 'a'], data)).toBe('leases');
    expect(leasePlace(['c'], data)).toBe('');
    expect(leasePlace([], undefined)).toBe('');
  });
});

describe('summaries', () => {
  it('counts an open rollout once, by what keeps it most', () => {
    expect(
      openSummary([
        rollout({}),
        rollout({ state: 'halted', paused: true }),
        rollout({ paused: true, policy: 'manual' }),
        rollout({ policy: 'manual' }),
        rollout({ policy: 'manual', approved: true }),
      ])
    ).toEqual({ total: 5, active: 2, halted: 1, paused: 1, approval: 1 });
  });

  it('counts the leases by phase and how they ended', () => {
    expect(
      leaseSummary(
        [lease('1', 'granted'), lease('2', 'running'), lease('3', 'verifying')],
        [lease('4', 'released'), lease('5', 'failed'), lease('6', 'expired'), lease('7', 'cancelled')]
      )
    ).toEqual({
      active: 3,
      running: 2,
      verifying: 1,
      recent: 4,
      released: 1,
      failed: 1,
      expired: 1,
      cancelled: 1,
    });
  });

  it('counts the finished rollouts by how they ended', () => {
    expect(
      finishedSummary([rollout({ state: 'completed' }), rollout({ state: 'completed' }), rollout({ state: 'aborted' })])
    ).toEqual({ total: 3, completed: 2, superseded: 0, aborted: 1 });
  });
});
