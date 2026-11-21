import { describe, expect, it } from 'vitest';

import type { QueueEntry } from '../api/types';
import { clearsByItself, splitQueue, workKind, type StageOf } from './timeline';

function entry(host: string, position: number, wait: QueueEntry['wait'], reason: string = wait): QueueEntry {
  return { host, position, reason, wait, rollouts: ['r1'] };
}

describe('wait kinds', () => {
  it('tells the waits that clear by themselves from the others', () => {
    expect(clearsByItself('budget')).toBe(true);
    expect(clearsByItself('order')).toBe(true);
    // The agent lets a failed pull be tried again by itself.
    expect(clearsByItself('pull')).toBe(true);
    expect(clearsByItself('gate')).toBe(false);
    expect(clearsByItself('approval')).toBe(false);
  });
});

describe('workKind', () => {
  it('names the most disruptive need', () => {
    expect(workKind([])).toBe('image');
    expect(workKind(['image', 'packages'])).toBe('packages');
    expect(workKind(['packages', 'reboot', 'image'])).toBe('reboot');
  });
});

/** stages builds a StageOf for one rollout "r1". */
function stages(byHost: Record<string, number>): StageOf {
  return (rollout, host) => (rollout === 'r1' ? byHost[host] : undefined);
}

describe('splitQueue', () => {
  it('keeps a host out of "next" while the canary of its rollout is stuck', () => {
    const queue = [
      entry('rpc', 1, 'gate'),
      entry('val-1', 2, 'order', 'after canary rpc'),
      entry('val-2', 3, 'order', 'after canary rpc'),
      entry('boot', 4, 'order', 'after canary rpc'),
      entry('other', 5, 'budget'),
    ];
    const { next, behind } = splitQueue(queue, stages({ rpc: 2, 'val-1': 3, 'val-2': 3, boot: 3 }));

    expect(next.map(q => q.host)).toEqual(['other']);
    // All three wait behind the canary, which waits for the gate.
    expect(behind.map(q => q.host)).toEqual(['val-1', 'val-2', 'boot']);
  });

  it('keeps the canary and the rest out of "next" while a broken host of the rollout is stuck', () => {
    const queue = [
      entry('broken', 1, 'gate'),
      entry('canary', 2, 'order', 'after broken'),
      entry('rest', 3, 'order', 'after broken'),
    ];
    const { next, behind } = splitQueue(queue, stages({ broken: 1, canary: 2, rest: 3 }));

    expect(next).toEqual([]);
    expect(behind.map(q => q.host)).toEqual(['canary', 'rest']);
  });

  it('lets nobody wait for a host that is held, silent or reports a verb it does not allow', () => {
    // The scheduler serves such a host when a person has acted; meanwhile the
    // hosts behind it wait for somebody else, who is on the way.
    for (const wait of ['held', 'silent', 'verb'] as const) {
      const queue = [entry('broken', 1, wait), entry('rest', 2, 'order', 'after other')];
      const { next, behind } = splitQueue(queue, stages({ broken: 1, rest: 3 }));

      expect(next.map(q => q.host)).toEqual(['rest']);
      expect(behind).toEqual([]);
    }
  });

  it('does not hold a host back for a stuck host of its own stage', () => {
    const queue = [entry('held', 1, 'held'), entry('b', 2, 'order', 'after canary c')];
    const { next, behind } = splitQueue(queue, stages({ held: 3, b: 3 }));

    expect(next.map(q => q.host)).toEqual(['b']);
    expect(behind).toEqual([]);
  });

  it('judges by the rollout the reason is about', () => {
    const queue: QueueEntry[] = [
      { host: 'stuck', position: 1, reason: 'rollout paused', wait: 'rollout-paused', rollouts: ['r2'] },
      { host: 'b', position: 2, reason: 'after canary c', wait: 'order', rollout: 'r1', rollouts: ['r1', 'r2'] },
    ];
    const stageOf: StageOf = (_rollout, host) => (host === 'stuck' ? 2 : 3);

    expect(splitQueue(queue, stageOf).next.map(q => q.host)).toEqual(['b']);
  });

  it('treats every self-clearing host as next without stage data', () => {
    const queue = [entry('a', 1, 'held'), entry('b', 2, 'order')];
    expect(splitQueue(queue, () => undefined).next.map(q => q.host)).toEqual(['b']);
  });
});
