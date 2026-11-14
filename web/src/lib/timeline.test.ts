import { describe, expect, it } from 'vitest';

import type { Budget, Lease, QueueEntry } from '../api/types';
import {
  buildTimeline,
  clearsByItself,
  forecast,
  needsSomebody,
  splitQueue,
  typicalLeaseMs,
  workKind,
  type StageOf,
} from './timeline';

const MIN = 60_000;
const NOW = Date.parse('2026-01-01T12:00:00Z');
const iso = (ms: number) => new Date(ms).toISOString();

function budget(over: Partial<Budget> = {}): Budget {
  return { totalWeight: 1000, allowedWeight: 100, unavailableWeight: 0, leasedHosts: 0, maxLeasedHosts: 2, ...over };
}

function entry(host: string, position: number, wait: QueueEntry['wait'], reason = String(wait)): QueueEntry {
  return { host, position, reason, wait, rollouts: ['r1'] };
}

function lease(over: Partial<Lease>): Lease {
  return {
    id: 'l1',
    host: 'a',
    state: 'released',
    needs: [],
    rollouts: {},
    weight: 100,
    charged: 100,
    force: false,
    grantedBy: 'scheduler',
    grantedAt: iso(NOW - 30 * MIN),
    expiresAt: iso(NOW),
    ...over,
  };
}

const weights = (w: number, ...hosts: string[]) => new Map(hosts.map(h => [h, w]));

describe('wait kinds', () => {
  it('splits the waits into self-clearing, network and person', () => {
    expect(clearsByItself('budget')).toBe(true);
    expect(clearsByItself(undefined)).toBe(true);
    expect(clearsByItself('gate')).toBe(false);
    expect(needsSomebody('gate')).toBe(false);
    expect(needsSomebody('approval')).toBe(true);
    expect(needsSomebody('order')).toBe(false);
  });
});

describe('workKind', () => {
  it('names the most disruptive need', () => {
    expect(workKind([])).toBe('image');
    expect(workKind(['image', 'packages'])).toBe('packages');
    expect(workKind(['packages', 'reboot', 'image'])).toBe('reboot');
  });
});

describe('typicalLeaseMs', () => {
  it('is the median of the released leases', () => {
    const leases = [10, 20, 60].map(took =>
      lease({ grantedAt: iso(NOW - 100 * MIN), endedAt: iso(NOW - 100 * MIN + took * MIN) })
    );
    leases.push(lease({ state: 'failed', grantedAt: iso(NOW - 500 * MIN), endedAt: iso(NOW) }));
    expect(typicalLeaseMs(leases, 384)).toBe(20 * MIN);
  });

  it('falls back to two epochs plus three minutes', () => {
    expect(typicalLeaseMs([], 384)).toBe((2 * 384 + 180) * 1000);
    expect(typicalLeaseMs([], 0)).toBe(15 * MIN);
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
      { host: 'stuck', position: 1, reason: 'policy paused', wait: 'policy-paused', rollouts: ['r2'] },
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

describe('forecast', () => {
  const base = { now: NOW, typicalMs: 10 * MIN, horizonMs: 600 * MIN, active: [] };
  const starts = (f: { slots: { host: string; start: number }[] }) => f.slots.map(s => [s.host, (s.start - NOW) / MIN]);

  it('judges an order wait by the rollout it is about, and lets no lease of another rollout hold a host back', () => {
    // "b" waits for its place in r1; it is also in r2, where "x" holds a lease.
    const queue: QueueEntry[] = [
      { host: 'b', position: 1, reason: 'after canary c', wait: 'order', rollout: 'r1', rollouts: ['r1', 'r2'] },
    ];
    const active = [{ host: 'x', start: NOW, weight: 0, rollouts: ['r2'] }];
    const costs = weights(0, 'b');

    // With stages: x is in an earlier stage of r2, which the wait is not about.
    const stageOf: StageOf = (rollout, host) => (rollout === 'r2' ? (host === 'x' ? 2 : 3) : undefined);
    expect(starts(forecast({ ...base, active, queue, budget: budget(), costs, stageOf }))).toEqual([['b', 0]]);

    // Without stages: a lease that shares no rollout with the wait is not ahead.
    const elsewhere = [{ host: 'x', start: NOW, weight: 0, rollouts: ['r9'] }];
    expect(starts(forecast({ ...base, active: elsewhere, queue, budget: budget(), costs }))).toEqual([['b', 0]]);

    // A lease of the rollout the wait is about is.
    const same = [{ host: 'x', start: NOW, weight: 0, rollouts: ['r1'] }];
    expect(starts(forecast({ ...base, active: same, queue, budget: budget(), costs }))).toEqual([['b', 10]]);
  });

  it('gives no slot to a host that waits for one that is not in the queue and holds no lease', () => {
    // "x" is expected to report its need: nobody knows when it will go.
    const queue = [
      { ...entry('a', 1, 'order', 'after x'), after: 'x' },
      { ...entry('b', 2, 'order', 'after canary x'), after: 'x' },
      entry('c', 3, 'budget'),
    ];
    const costs = weights(0, 'a', 'b', 'c');
    const stageOf = stages({ x: 1, a: 3, b: 3, c: 3 });

    const f = forecast({ ...base, queue, budget: budget(), costs, stageOf });
    expect(starts(f)).toEqual([['c', 0]]);
    expect(f.late).toBe(2);
    expect(f.unplaced).toBe(0);

    // Once it holds a lease, the others follow it.
    const active = [{ host: 'x', start: NOW, weight: 0, rollouts: ['r1'] }];
    const g = forecast({ ...base, active, queue, budget: budget({ maxLeasedHosts: 5 }), costs, stageOf });
    expect(starts(g)).toEqual([
      ['c', 0],
      ['a', 10],
      ['b', 10],
    ]);
    expect(g.late).toBe(0);
  });

  it('serves in order, one after the other when the weight budget fits one host', () => {
    const f = forecast({
      ...base,
      queue: [entry('b', 2, 'budget'), entry('a', 1, 'next'), entry('c', 3, 'budget')],
      budget: budget(),
      costs: weights(100, 'a', 'b', 'c'),
    });

    expect(f).toEqual({
      slots: [
        { host: 'a', start: NOW, end: NOW + 10 * MIN },
        { host: 'b', start: NOW + 10 * MIN, end: NOW + 20 * MIN },
        { host: 'c', start: NOW + 20 * MIN, end: NOW + 30 * MIN },
      ],
      late: 0,
      unplaced: 0,
    });
  });

  it('runs as many at once as the host limit allows', () => {
    const f = forecast({
      ...base,
      queue: [entry('a', 1, 'next'), entry('b', 2, 'next'), entry('c', 3, 'host-limit')],
      budget: budget({ allowedWeight: 1000 }),
      costs: weights(100, 'a', 'b', 'c'),
    });

    expect(starts(f)).toEqual([
      ['a', 0],
      ['b', 0],
      ['c', 10],
    ]);
  });

  it('waits for the active lease, which has at least a quarter of a typical lease left', () => {
    const f = forecast({
      ...base,
      queue: [entry('b', 1, 'budget')],
      active: [{ host: 'a', start: NOW - 60 * MIN, weight: 100 }],
      budget: budget({ unavailableWeight: 100, leasedHosts: 1 }),
      costs: weights(100, 'b'),
    });

    expect(f.slots).toEqual([{ host: 'b', start: NOW + 2.5 * MIN, end: NOW + 12.5 * MIN }]);
  });

  it('leaves out hosts that wait for a person or the network', () => {
    const f = forecast({
      ...base,
      queue: [entry('a', 1, 'approval'), entry('b', 2, 'gate'), entry('c', 3, 'next')],
      budget: budget(),
      costs: weights(100, 'a', 'b', 'c'),
    });

    expect(f.slots.map(s => s.host)).toEqual(['c']);
  });

  const staged = [
    entry('canary', 1, 'next'),
    entry('b1', 2, 'order', 'after canary canary'),
    entry('b2', 3, 'order', 'after canary canary'),
    entry('boot', 4, 'order', 'after canary canary'),
  ];

  it.each([
    ['with stages', stages({ canary: 2, b1: 3, b2: 3, boot: 3 })],
    ['without stages', undefined],
  ])('starts the rest after the canary, the bootnode with it (%s)', (_name, stageOf) => {
    const f = forecast({
      ...base,
      queue: staged,
      budget: budget({ allowedWeight: 1000, maxLeasedHosts: 3 }),
      costs: weights(100, 'canary', 'b1', 'b2', 'boot'),
      stageOf,
    });

    expect(starts(f)).toEqual([
      ['canary', 0],
      ['b1', 10],
      ['b2', 10],
      ['boot', 10],
    ]);
  });

  it('waits for an active lease of an earlier stage only', () => {
    const f = forecast({
      ...base,
      queue: [entry('b1', 1, 'order', 'after canary canary')],
      active: [{ host: 'same-stage', start: NOW, weight: 0, rollouts: ['r1'] }],
      budget: budget({ allowedWeight: 1000, maxLeasedHosts: 3 }),
      costs: weights(100, 'b1'),
      stageOf: stages({ 'same-stage': 3, b1: 3 }),
    });

    expect(starts(f)).toEqual([['b1', 0]]);
  });

  it('charges a host its live validators: a dead host costs nothing', () => {
    // 64 of 640 allowed, all of it used by the host that is down. The
    // scheduler grants that host at no cost; so does the forecast.
    const f = forecast({
      ...base,
      queue: [entry('down', 1, 'host-limit'), entry('up', 2, 'budget')],
      active: [{ host: 'busy', start: NOW, weight: 0 }],
      budget: budget({ totalWeight: 640, allowedWeight: 64, unavailableWeight: 64, maxLeasedHosts: 1 }),
      costs: new Map([
        ['down', 0],
        ['up', 64],
      ]),
    });

    expect(starts(f)).toEqual([['down', 10]]);
    expect(f.unplaced).toBe(1);
  });

  it('skips a host that does not fit and starts a later one that does', () => {
    const f = forecast({
      ...base,
      queue: [entry('big', 1, 'budget'), entry('small', 2, 'budget')],
      budget: budget({ allowedWeight: 64, unavailableWeight: 32 }),
      costs: new Map([
        ['big', 64],
        ['small', 16],
      ]),
    });

    expect(starts(f)).toEqual([['small', 0]]);
    expect(f).toMatchObject({ late: 0, unplaced: 1 });
  });

  it('reports hosts it cannot place when other outages exhaust the budget', () => {
    const f = forecast({
      ...base,
      queue: [entry('a', 1, 'budget'), entry('b', 2, 'budget')],
      budget: budget({ unavailableWeight: 150 }),
      costs: weights(100, 'a', 'b'),
    });

    expect(f).toEqual({ slots: [], late: 0, unplaced: 2 });
  });

  it('lets hosts without validators through a full weight budget', () => {
    const f = forecast({
      ...base,
      queue: [entry('rpc', 1, 'next')],
      budget: budget({ unavailableWeight: 150 }),
      costs: weights(0, 'rpc'),
    });

    expect(f.slots.map(s => s.host)).toEqual(['rpc']);
  });

  it('counts what lies past the horizon as late, not as unplaceable', () => {
    const f = forecast({
      ...base,
      horizonMs: 15 * MIN,
      queue: [entry('a', 1, 'next'), entry('b', 2, 'budget'), entry('c', 3, 'budget')],
      budget: budget(),
      costs: weights(100, 'a', 'b', 'c'),
    });

    expect(f.slots.map(s => s.host)).toEqual(['a', 'b']);
    expect(f).toMatchObject({ late: 1, unplaced: 0 });
  });

  it('ends on degenerate input', () => {
    const queue = Array.from({ length: 2000 }, (_, i) => entry(`h${i}`, i + 1, i % 3 === 0 ? 'order' : 'budget'));
    const f = forecast({
      ...base,
      queue,
      active: [{ host: 'x', start: Number.NaN, weight: 0 }],
      budget: budget({ maxLeasedHosts: 0, allowedWeight: 0 }),
      costs: new Map(),
    });

    expect(f.slots.length + f.late + f.unplaced).toBe(2000);
    expect(forecast({ ...base, queue: [], budget: budget(), costs: new Map() })).toEqual({
      slots: [],
      late: 0,
      unplaced: 0,
    });
  });
});

describe('buildTimeline', () => {
  it('draws past, active and planned work per host', () => {
    const model = buildTimeline({
      now: NOW,
      from: NOW - 120 * MIN,
      horizonMs: 120 * MIN,
      epochSeconds: 384,
      leases: [
        lease({ id: 'old', host: 'x', grantedAt: iso(NOW - 600 * MIN), endedAt: iso(NOW - 590 * MIN) }),
        lease({
          id: 'done',
          host: 'a',
          grantedAt: iso(NOW - 60 * MIN),
          endedAt: iso(NOW - 40 * MIN),
          readiness: 'ready',
          needs: [{ id: 'n', kind: 'reboot' }],
        }),
        lease({
          id: 'bad',
          host: 'b',
          state: 'failed',
          grantedAt: iso(NOW - 50 * MIN),
          endedAt: iso(NOW - 45 * MIN),
          error: 'boom',
        }),
        lease({ id: 'run', host: 'c', state: 'running', grantedAt: iso(NOW - 5 * MIN) }),
      ],
      queue: [entry('d', 1, 'budget', 'budget 10% of 10%')],
      budget: budget({ unavailableWeight: 100, leasedHosts: 1 }),
      costs: weights(100, 'd'),
      needKinds: new Map([['d', ['packages'] as const]]),
    });

    expect(model.typicalMs).toBe(15 * MIN);
    expect(model.rows.map(r => r.host)).toEqual(['a', 'b', 'c', 'd']);
    expect(model.rows.map(r => r.bars.map(b => [b.state, b.kind, b.detail]))).toEqual([
      [['done', 'reboot', 'ready']],
      [['failed', 'image', 'boom']],
      [['active', 'image', 'lease running']],
      [['planned', 'packages', 'budget 10% of 10%']],
    ]);
    expect(model.rows[2]?.bars[0]?.end).toBe(NOW);
    expect(model.eta.get('d')).toBe(NOW + 10 * MIN);
    expect(model).toMatchObject({ late: 0, unplaced: 0 });
  });

  it('counts hosts it cannot place', () => {
    const model = buildTimeline({
      now: NOW,
      from: NOW - 60 * MIN,
      horizonMs: 60 * MIN,
      epochSeconds: 384,
      leases: [],
      queue: [entry('a', 1, 'budget')],
      budget: budget({ unavailableWeight: 500 }),
      costs: weights(100, 'a'),
      needKinds: new Map(),
    });

    expect(model.rows).toEqual([]);
    expect(model).toMatchObject({ late: 0, unplaced: 1 });
  });
});
