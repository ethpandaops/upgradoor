import { describe, expect, it } from 'vitest';

import type { Gate, HostSummary, QueueEntry, Rollout } from '../api/types';
import { attentionItems } from './activity';

function entry(
  host: string,
  position: number,
  wait: QueueEntry['wait'],
  rollouts: string[] = [],
  rollout?: string
): QueueEntry {
  return { host, position, reason: `reason ${host}`, wait, rollouts, rollout };
}

const rollout = (id: string, over: Partial<Rollout> = {}) =>
  ({ id, title: `Rollout ${id}`, state: 'active', ...over }) as Rollout;
const gate = (ok: boolean, reasons: string[] = []) => ({ ok, reasons }) as Gate;

describe('attentionItems', () => {
  it('is empty when everything flows', () => {
    expect(
      attentionItems({ gate: gate(true), paused: false, queue: [entry('a', 1, 'budget')], rollouts: [], hosts: [] })
    ).toEqual([]);
  });

  it('groups the waiting hosts by what blocks them', () => {
    const items = attentionItems({
      gate: gate(false, ['participation 80% < 90%']),
      paused: true,
      queue: [
        entry('g', 1, 'gate'),
        entry('a1', 2, 'approval', ['r1']),
        entry('a2', 3, 'approval', ['r1']),
        entry('h1', 4, 'halted', ['r1', 'r2'], 'r2'),
        entry('held', 5, 'held'),
        entry('quiet', 6, 'silent'),
      ],
      rollouts: [rollout('r1'), rollout('r2', { state: 'halted', reason: 'x failed' })],
      hosts: [{ name: 'quiet', stale: true } as HostSummary, { name: 'gone', stale: true } as HostSummary],
    });

    expect(items.map(i => [i.kind, i.title, i.hosts])).toEqual([
      ['gate', 'The network gate is closed', ['g']],
      ['halted', 'Rollout r2 is halted', ['h1']],
      ['paused', 'The scheduler is paused', []],
      ['approval', 'Rollout r1 waits for approval', ['a1', 'a2']],
      ['held', 'held is held', ['held']],
      ['silent', '2 hosts are silent', ['quiet', 'gone']],
    ]);
    expect(items[0]?.detail).toBe('participation 80% < 90%. 1 host waits for it to open.');
    expect(items[3]?.detail).toBe('2 hosts wait.');
    expect(items[1]?.detail).toBe('x failed. 1 host waits.');
    expect(items[1]?.rollout?.id).toBe('r2');
  });

  it('acts on the rollout the plan names, not on the first one of the host', () => {
    // h is in a rollout somebody paused (A) and in one that waits for
    // approval (B). Approving must not resume A.
    const [item] = attentionItems({
      gate: gate(true),
      paused: false,
      queue: [entry('h', 1, 'approval', ['A', 'B'], 'B')],
      rollouts: [rollout('A', { paused: true }), rollout('B')],
      hosts: [],
    });

    expect(item?.title).toBe('Rollout B waits for approval');
    expect(item?.rollout?.id).toBe('B');
  });

  it('says of a rollout that the scheduler paused after its canary that it is so', () => {
    const items = attentionItems({
      gate: gate(true),
      paused: false,
      queue: [entry('h1', 1, 'rollout-paused', ['A'], 'A'), entry('h2', 2, 'rollout-paused', ['B'], 'B')],
      rollouts: [
        rollout('A', { paused: true, pauseReason: 'paused after the canary; resume to continue' }),
        rollout('B', { paused: true }),
      ],
      hosts: [],
    });

    expect(items.map(i => [i.kind, i.title, i.rollout?.id])).toEqual([
      ['rollout-paused', 'Rollout A is paused after its canary', 'A'],
      ['rollout-paused', 'Rollout B is paused', 'B'],
    ]);
  });

  it('offers no rollout when an old controller leaves the choice open', () => {
    const [item] = attentionItems({
      gate: gate(true),
      paused: false,
      queue: [entry('h', 1, 'approval', ['A', 'B'])],
      rollouts: [rollout('A'), rollout('B')],
      hosts: [],
    });

    expect(item?.title).toBe('A rollout waits for approval');
    expect(item?.rollout).toBeUndefined();
  });
});
