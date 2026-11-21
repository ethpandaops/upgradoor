import { describe, expect, it } from 'vitest';

import type { SlotHistory } from '../api/types';
import type { Lease, Outage } from '../api/types';
import { buildSlotWindow, liveOutages, mergeSlotRows, packOutages, slotWindowEnd } from './slotHistory';

const FIRST = Date.parse('2026-01-01T00:00:00Z');

/** Ten slots of 4 s from slot 100 on; the last two are not settled. */
function perSlot(): SlotHistory {
  return {
    firstSlot: 100,
    firstTime: new Date(FIRST).toISOString(),
    slotMs: 4000,
    slotsPerEpoch: 32,
    bucketSlots: 1,
    minParticipationPercent: 90,
    epochSeconds: 128,
    recorded: [1, 1, 1, 1, 1, 1, 1, 1, 0, 0],
    missed: [0, 0, 1, 0, 0, 0, 0, 0, 0, 0],
    assigned: [8, 8, 8, 8, 8, 8, 8, 8, 0, 0],
    included: [8, 8, 6, 8, 8, 8, 8, 7, 0, 0],
  };
}

describe('slotWindowEnd', () => {
  it('is the point the clock is in, and moves on without a new answer', () => {
    const history = perSlot();
    expect(slotWindowEnd(history, FIRST + 9 * 4000 + 1500)).toBe(9);
    expect(slotWindowEnd(history, FIRST + 10 * 4000)).toBe(10);
    expect(slotWindowEnd({ ...history, bucketSlots: 4 }, FIRST + 9 * 4000)).toBe(2);
  });

  it('is unknown without the chain timing', () => {
    expect(slotWindowEnd(undefined, FIRST)).toBeUndefined();
    expect(slotWindowEnd({ ...perSlot(), slotMs: 0 }, FIRST)).toBeUndefined();
  });
});

describe('buildSlotWindow', () => {
  it('has one column per slot of the window, ending with the slot the clock is in', () => {
    const win = buildSlotWindow(perSlot(), 9, 24_000);

    expect(win.columns.map(c => c.slot)).toEqual([104, 105, 106, 107, 108, 109]);
    expect(win.startMs).toBe(FIRST + 4 * 4000);
    expect(win.endMs).toBe(FIRST + 10 * 4000);
    expect(win.columns[3]).toMatchObject({ recorded: 1, assigned: 8, included: 7, worstAssigned: 8, worstIncluded: 7 });
    expect(win.columns[5]?.recorded).toBe(0);
    expect(win.latest?.slot).toBe(107);
  });

  it('keeps its number of columns when the answer is older than the clock or shorter than the window', () => {
    // Three slots later and no new answer: the same six columns, moved on.
    const stale = buildSlotWindow(perSlot(), 12, 24_000);
    expect(stale.columns.map(c => c.slot)).toEqual([107, 108, 109, 110, 111, 112]);
    expect(stale.columns.map(c => c.recorded)).toEqual([1, 0, 0, 0, 0, 0]);
    expect(stale.endMs - stale.startMs).toBe(24_000);

    // A window longer than the chain: empty columns on the left.
    const young = buildSlotWindow(perSlot(), 9, 60_000);
    expect(young.columns).toHaveLength(15);
    expect(young.columns[0]).toMatchObject({ slot: 95, recorded: 0 });
    expect(young.columns[5]).toMatchObject({ slot: 100, recorded: 1 });
  });

  it('carries the worst slot of a bucket', () => {
    const win = buildSlotWindow(
      {
        ...perSlot(),
        bucketSlots: 4,
        recorded: [4, 4, 2],
        missed: [0, 1, 0],
        assigned: [32, 32, 16],
        included: [32, 29, 16],
        worstAssigned: [8, 8, 8],
        worstIncluded: [8, 5, 8],
      },
      2,
      48_000
    );

    expect(win.columns.map(c => [c.slot, c.slots])).toEqual([
      [100, 4],
      [104, 4],
      [108, 4],
    ]);
    expect(win.columns[1]).toMatchObject({ included: 29, worstAssigned: 8, worstIncluded: 5, missed: 1 });
  });
});

describe('mergeSlotRows', () => {
  const row = (slot: number, included: number, proposed = true) => ({
    slot,
    proposed,
    assigned: 8,
    included,
    hosts: [
      { host: 'a', assigned: 4, included: Math.min(4, included) },
      { host: 'b', assigned: 4, included: Math.max(0, included - 4) },
    ],
  });

  it('adds a slot that settled as one more column and rewrites a late inclusion', () => {
    // The stream holds slots 106 to 109: 107 got the attestation it lacked, 108 and 109 settled.
    const merged = mergeSlotRows(perSlot(), [row(106, 8), row(107, 8), row(108, 8), row(109, 5, false)]);

    expect(merged.recorded).toEqual([1, 1, 1, 1, 1, 1, 1, 1, 1, 1]);
    expect(merged.included).toEqual([8, 8, 6, 8, 8, 8, 8, 8, 8, 5]);
    expect(merged.missed).toEqual([0, 0, 1, 0, 0, 0, 0, 0, 0, 1]);

    // The clock moved on: the rows reach past what the window was asked for.
    const later = mergeSlotRows(merged, [row(109, 8), row(110, 8), row(111, 8)]);
    expect(later.recorded).toHaveLength(12);
    expect(later.included?.slice(-3)).toEqual([8, 8, 8]);
    expect(later.included?.slice(0, 9)).toEqual(merged.included?.slice(0, 9));
    expect(mergeSlotRows(later, [row(109, 8), row(110, 8), row(111, 8)])).toEqual(later);
  });

  it('computes a bucket again once all its slots are in the rows, and leaves the others', () => {
    const history: SlotHistory = {
      ...perSlot(),
      firstSlot: 96,
      bucketSlots: 4,
      recorded: [4, 4, 1],
      missed: [0, 1, 0],
      assigned: [32, 32, 8],
      included: [32, 30, 8],
      worstAssigned: [8, 8, 8],
      worstIncluded: [8, 6, 8],
    };

    // Rows from slot 102 on: bucket 1 (100..103) is not covered as a whole and keeps its value.
    const rows = [row(102, 8), row(103, 8), row(104, 8), row(105, 3), row(106, 8), row(107, 8), row(108, 8)];
    const merged = mergeSlotRows(
      history,
      rows.map(r => ({ ...r, hosts: undefined }))
    );

    expect(merged.recorded).toEqual([4, 4, 4, 1]);
    expect(merged.included).toEqual([32, 30, 27, 8]);
    expect(merged.worstIncluded).toEqual([8, 6, 3, 8]);
    expect(mergeSlotRows(history, [])).toBe(history);
  });
});

describe('liveOutages', () => {
  const at = (slot: number) => new Date(FIRST + (slot - 100) * 4000).toISOString();
  const base = (outages: Outage[]) => ({ ...perSlot(), slotsPerEpoch: 8, offSlots: 3, outages });
  const miss = (slot: number, ...hosts: string[]) => ({
    slot,
    proposed: true,
    assigned: 8,
    included: 8 - 2 * hosts.length,
    hosts: hosts.map(host => ({ host, assigned: 2, included: 0 })),
  });
  const lease = (id: string, host: string, state: Lease['state'], from: number, to?: number): Lease => ({
    id,
    host,
    state,
    needs: [{ id: 'reboot', kind: 'reboot' }],
    rollouts: {},
    weight: 16,
    charged: 16,
    force: false,
    grantedBy: 'scheduler',
    grantedAt: at(from),
    expiresAt: at(from + 100),
    ...(to === undefined ? {} : { endedAt: at(to) }),
  });
  const input = { validators: () => 32, nowMs: FIRST + 12 * 4000 };

  it('brings the leases of the answer up to the stream and adds the ones it does not know', () => {
    const known: Outage = {
      hosts: 1,
      names: ['a'],
      from: at(101),
      to: at(103),
      open: true,
      lease: 'l1',
      work: 'reboot',
      missed: 4,
      assigned: 4,
      validators: 32,
    };
    const { outages, stale } = liveOutages({
      ...input,
      history: base([known]),
      rows: [],
      leases: [lease('l1', 'a', 'failed', 101, 106), lease('l2', 'b', 'running', 108)],
    });

    expect(stale).toBe(false);
    expect(outages).toEqual(
      [
        { ...known, to: at(106), open: undefined, failed: true },
        {
          hosts: 1,
          names: ['b'],
          from: at(108),
          to: new Date(input.nowMs).toISOString(),
          open: true,
          lease: 'l2',
          work: 'reboot',
          missed: 0,
          assigned: 0,
          validators: 32,
        },
      ].map(o => JSON.parse(JSON.stringify(o)) as Outage)
    );
  });

  it('opens a span for a host that keeps missing, extends the one it has, and leaves a host that is being worked on alone', () => {
    const off: Outage = {
      hosts: 1,
      names: ['c'],
      from: at(90),
      to: at(104),
      fromStart: true,
      open: true,
      missed: 10,
      assigned: 10,
      validators: 32,
    };
    const { outages } = liveOutages({
      ...input,
      history: base([off]),
      // a misses three slots in a row: off. b misses twice: not yet. c goes on missing. d misses under its lease.
      rows: [miss(105, 'a', 'c', 'd'), miss(106, 'a', 'b', 'c', 'd'), miss(107, 'a', 'b', 'd'), miss(108)],
      leases: [lease('l9', 'd', 'running', 104)],
    });

    expect(outages.map(o => [o.names?.[0], o.work ?? 'off', o.from, o.open ?? false])).toEqual([
      ['c', 'off', at(90), true],
      ['d', 'reboot', at(104), true],
      ['a', 'off', at(105), true],
    ]);
    expect(outages[0]).toMatchObject({ missed: 14, assigned: 14 });
    expect(outages[2]).toMatchObject({ missed: 6, assigned: 6, validators: 32 });
  });

  it('asks for the window again when a lease may belong to a span of several hosts', () => {
    const wave: Outage = {
      hosts: 30,
      names: ['n1', 'n2'],
      from: at(101),
      to: at(105),
      open: true,
      work: 'image',
      missed: 0,
      assigned: 0,
      validators: 960,
    };
    expect(
      liveOutages({ ...input, history: base([wave]), rows: [], leases: [lease('l5', 'n7', 'released', 101, 106)] })
        .stale
    ).toBe(true);
    expect(liveOutages({ ...input, history: base([wave]), rows: [], leases: [] }).stale).toBe(false);
  });
});

describe('packOutages', () => {
  const span = (from: number, to: number, more: Partial<Outage> = {}): Outage => ({
    hosts: 1,
    names: [`h${from}`],
    from: new Date(from).toISOString(),
    to: new Date(to).toISOString(),
    work: 'image',
    missed: 0,
    assigned: 0,
    validators: 8,
    ...more,
  });
  const opts = { startMs: 0, endMs: 1000, width: 1000, lanes: { work: 2, off: 1 } };

  it('packs spans into lanes by time, whoever the host is', () => {
    const { blocks } = packOutages(
      [span(0, 100), span(50, 150), span(120, 200), span(300, 400, { work: undefined })],
      opts
    );

    expect(blocks.map(b => [b.group, b.lane, b.x, b.width])).toEqual([
      ['work', 0, 0, 100],
      ['work', 1, 50, 100],
      ['work', 0, 120, 80],
      ['off', 0, 300, 100],
    ]);
  });

  it('merges spans of one kind when they do not fit, and keeps a failed one apart while it can', () => {
    const many = Array.from({ length: 40 }, (_, i) => span(100 + i, 300 + i, { rollout: 'r1' }));
    const { blocks } = packOutages([...many, span(120, 180, { failed: true, rollout: 'r1' })], opts);

    expect(blocks).toHaveLength(2);
    expect(blocks.map(b => [b.hosts, b.failed])).toEqual([
      [40, false],
      [1, true],
    ]);
    expect(blocks.every(b => b.lane < 2)).toBe(true);
  });

  it('always fits: at the worst one block per stretch of time', () => {
    const kinds = ['image', 'packages', 'reboot', 'mixed'].map((work, i) => span(100, 500, { work, names: [`k${i}`] }));
    const { blocks } = packOutages(kinds, opts);

    expect(blocks.length).toBeLessThanOrEqual(2);
    expect(blocks.reduce((n, b) => n + b.hosts, 0)).toBe(4);
    expect(blocks.some(b => b.work === 'mixed')).toBe(true);
  });

  it('shows hosts that are off for the whole window as one block that reaches the right edge', () => {
    const down = Array.from({ length: 12 }, (_, i) =>
      span(-500, 900, { work: undefined, fromStart: true, open: true, names: [`d${i}`] })
    );
    const { blocks, wholeWindow } = packOutages(down, opts);

    expect(wholeWindow).toBe(12);
    expect(blocks).toMatchObject([{ group: 'off', lane: 0, x: 0, width: 1000, hosts: 12, open: true }]);
  });
});
