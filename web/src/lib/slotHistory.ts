// The model behind the participation graph: the controller's slot history
// (api.SlotHistory) cut to a fixed number of columns that ends at the bucket
// the clock is in. The number of columns depends on the window alone, so the
// graph keeps its geometry: a new slot is one more column at the right edge
// and the oldest column leaves on the left.

import { list, type Lease, type Outage, type SlotHistory, type SlotRow } from '../api/types';

/** SlotColumn is one column of the graph: one slot, or a bucket of slots. */
export interface SlotColumn {
  /** The first slot of the column and how many slots it covers. */
  slot: number;
  slots: number;
  /** When the column begins, in milliseconds since the epoch. */
  time: number;
  /** Stored slots; 0 means nothing is known about the column (yet). */
  recorded: number;
  /** Recorded slots without a block. */
  missed: number;
  assigned: number;
  included: number;
  /** The column's slot with the lowest share included; the column itself when it is one slot. */
  worstAssigned: number;
  worstIncluded: number;
}

export interface SlotWindow {
  bucketSlots: number;
  slotMs: number;
  slotsPerEpoch: number;
  /** The columns, oldest first. Always the same number for one window length. */
  columns: SlotColumn[];
  /** The time at the left edge of the first column and the right edge of the last. */
  startMs: number;
  endMs: number;
  /** The newest column with attestations counted. */
  latest: SlotColumn | undefined;
}

/**
 * slotWindowEnd is the index of the point the clock is in, counted in the
 * answer's points. It moves on by one every bucket, whether or not a newer
 * answer has arrived. undefined while the chain's timing is unknown.
 */
export function slotWindowEnd(history: SlotHistory | undefined, now: number): number | undefined {
  if (!history || history.slotMs <= 0 || history.bucketSlots <= 0) return undefined;
  const first = Date.parse(history.firstTime);
  if (!Number.isFinite(first)) return undefined;

  return Math.max(0, Math.floor((now - first) / (history.slotMs * history.bucketSlots)));
}

/**
 * buildSlotWindow cuts the answer to the columns of a window of rangeMs that
 * ends with point `end` (slotWindowEnd). A column the answer does not cover,
 * because the chain is younger than the window or the answer older than the
 * clock, is there with nothing recorded.
 */
export function buildSlotWindow(history: SlotHistory, end: number, rangeMs: number): SlotWindow {
  const bucketMs = history.slotMs * history.bucketSlots;
  const count = Math.max(1, Math.ceil(rangeMs / bucketMs));
  const firstTime = Date.parse(history.firstTime);
  const offset = end - count + 1;

  const recorded = list(history.recorded);
  const missed = list(history.missed);
  const assigned = list(history.assigned);
  const included = list(history.included);
  const worstAssigned = list(history.worstAssigned);
  const worstIncluded = list(history.worstIncluded);

  const columns: SlotColumn[] = [];
  let latest: SlotColumn | undefined;

  for (let i = 0; i < count; i++) {
    const p = offset + i;
    const a = assigned[p] ?? 0;
    const inc = included[p] ?? 0;
    const column: SlotColumn = {
      slot: history.firstSlot + p * history.bucketSlots,
      slots: history.bucketSlots,
      time: firstTime + p * bucketMs,
      recorded: recorded[p] ?? 0,
      missed: missed[p] ?? 0,
      assigned: a,
      included: inc,
      worstAssigned: history.bucketSlots === 1 ? a : (worstAssigned[p] ?? 0),
      worstIncluded: history.bucketSlots === 1 ? inc : (worstIncluded[p] ?? 0),
    };
    columns.push(column);
    if (column.recorded > 0 && column.assigned > 0) latest = column;
  }

  return {
    bucketSlots: history.bucketSlots,
    slotMs: history.slotMs,
    slotsPerEpoch: history.slotsPerEpoch,
    columns,
    startMs: firstTime + offset * bucketMs,
    endMs: firstTime + (end + 1) * bucketMs,
    latest,
  };
}

/**
 * mergeSlotRows brings a slot history up to date with the slots an event
 * stream carries: the newest ones, which are the only ones that still change.
 * A slot that settled is one more column on the right; a late inclusion
 * rewrites a recent one. Nothing is fetched for that.
 *
 * Every point whose slots all lie in the range the rows cover is computed
 * again from the rows, so the result does not depend on how often or in which
 * order rows arrived. Older points keep what they have: the answer of the
 * controller, or what an earlier call made of rows that have left the stream
 * since. In a long window a point is a bucket of slots; the bucket that
 * straddles the oldest row keeps its value too (the part of it that could
 * still change is a few slots, two epochs back).
 */
export function mergeSlotRows(history: SlotHistory, rows: readonly SlotRow[]): SlotHistory {
  const first = rows[0];
  const last = rows[rows.length - 1];
  const bucket = history.bucketSlots;
  if (!first || !last || bucket <= 0 || history.slotMs <= 0 || last.slot < history.firstSlot) return history;

  // The first point that lies in the rows' range as a whole, and the point of the newest row.
  const from = Math.max(0, Math.ceil((first.slot - history.firstSlot) / bucket));
  const to = Math.floor((last.slot - history.firstSlot) / bucket);
  if (to < from) return history;

  const grow = (column: readonly number[] | null | undefined): number[] => {
    const out = [...list(column)];
    while (out.length <= to) out.push(0);
    for (let p = from; p <= to; p++) out[p] = 0;
    return out;
  };

  const recorded = grow(history.recorded);
  const missed = grow(history.missed);
  const assigned = grow(history.assigned);
  const included = grow(history.included);
  const worstAssigned = bucket > 1 ? grow(history.worstAssigned) : undefined;
  const worstIncluded = bucket > 1 ? grow(history.worstIncluded) : undefined;

  for (const row of rows) {
    const p = Math.floor((row.slot - history.firstSlot) / bucket);
    if (p < from || p > to) continue;

    recorded[p] = (recorded[p] ?? 0) + 1;
    if (!row.proposed) missed[p] = (missed[p] ?? 0) + 1;
    assigned[p] = (assigned[p] ?? 0) + row.assigned;
    included[p] = (included[p] ?? 0) + row.included;

    // The slot of the point with the lowest share included.
    if (worstAssigned && worstIncluded && row.assigned > 0) {
      const wa = worstAssigned[p] ?? 0;
      if (wa === 0 || row.included * wa < (worstIncluded[p] ?? 0) * row.assigned) {
        worstAssigned[p] = row.assigned;
        worstIncluded[p] = row.included;
      }
    }
  }

  return {
    ...history,
    recorded,
    missed,
    assigned,
    included,
    ...(worstAssigned && worstIncluded ? { worstAssigned, worstIncluded } : {}),
  };
}

// ---------------------------------------------------------------- outages

const WORKS = ['image', 'packages', 'action', 'reboot'];

/** leaseOutage is the span of a lease, as the controller makes it (outageSpans in outages.go). */
function leaseOutage(lease: Lease, validators: number, nowMs: number): Outage {
  const kinds = [...new Set(list(lease.needs).map(n => n.kind as string))];
  const ended = ['released', 'failed', 'expired'].includes(lease.state);
  const rollouts = Object.values(lease.rollouts ?? {}).sort();

  return {
    hosts: 1,
    names: [lease.host],
    from: lease.grantedAt,
    to: lease.endedAt ?? (ended ? lease.grantedAt : new Date(nowMs).toISOString()),
    ...(ended ? {} : { open: true }),
    lease: lease.id,
    work: kinds.length === 1 && WORKS.includes(kinds[0] ?? '') ? kinds[0] : kinds.length === 0 ? 'image' : 'mixed',
    ...(rollouts[0] ? { rollout: rollouts[0] } : {}),
    ...(lease.state === 'failed' || lease.state === 'expired' ? { failed: true } : {}),
    missed: 0,
    assigned: 0,
    validators: validators || lease.weight,
  };
}

interface LiveOutageInput {
  /** The window as the controller answered it (and mergeSlotRows extended it). */
  history: SlotHistory;
  /** The newest slots, from the event stream: each names the hosts that missed attestations in it. */
  rows: readonly SlotRow[];
  /** The open and the recent leases, from the event stream. */
  leases: readonly Lease[];
  /** The number of validators of a host. */
  validators: (host: string) => number;
  nowMs: number;
}

interface LiveOutages {
  outages: Outage[];
  /**
   * stale: something happened to a span that stands for several hosts, which
   * the page cannot take apart. The window should be asked for again (once,
   * not on a timer).
   */
  stale: boolean;
}

/**
 * liveOutages keeps the outages of a window current from the event stream,
 * without asking for the window again:
 *
 *   - a lease of the stream that the answer has as a span of its own is
 *     brought up to its state (still running, ended, failed); a lease the
 *     answer does not know is a new span;
 *   - a host that missed attestations in the stream's newest slots, in at
 *     least offSlots of them no further than an epoch apart, is off: the span
 *     the answer has for it is extended, or a new one begins;
 *   - a span that is still open ends now.
 *
 * The rules are the controller's (outages.go), applied to the few slots and
 * leases the stream carries.
 */
export function liveOutages(input: LiveOutageInput): LiveOutages {
  const { history, rows, leases, nowMs } = input;
  const outages = list(history.outages).map(o => ({ ...o }));
  const slotMs = history.slotMs;
  const firstTime = Date.parse(history.firstTime);
  const slotTime = (slot: number) => new Date(firstTime + (slot - history.firstSlot) * slotMs).toISOString();
  const merged = outages.some(o => o.hosts > 1 && o.work);
  let stale = false;

  const byLease = new Map(outages.flatMap(o => (o.lease ? [[o.lease, o] as const] : [])));

  for (const lease of leases) {
    const fresh = leaseOutage(lease, input.validators(lease.host), nowMs);
    if (Date.parse(fresh.to) <= firstTime) continue;

    const known = byLease.get(lease.id);
    if (known) {
      // The counts are the controller's; the state and the end are the stream's.
      Object.assign(known, { to: fresh.to, open: fresh.open, failed: fresh.failed });
      if (!fresh.open) delete known.open;
      if (!fresh.failed) delete known.failed;
    } else if (merged) {
      // It may be one of the hosts of a merged span, or a new one.
      stale = true;
    } else {
      outages.push(fresh);
      byLease.set(lease.id, fresh);
    }
  }

  // The hosts that missed attestations in the newest slots, and are not being worked on.
  const off = history.offSlots ?? 0;
  if (off > 0 && slotMs > 0 && rows.length > 0) {
    const epoch = Math.max(1, history.slotsPerEpoch);
    const lastSlot = rows[rows.length - 1]?.slot ?? 0;
    const misses = new Map<string, { slot: number; assigned: number; missed: number }[]>();

    for (const row of rows) {
      for (const h of row.hosts ?? []) {
        if (h.included >= h.assigned) continue;
        const at = Date.parse(slotTime(row.slot));
        const worked = outages.some(
          o =>
            o.work &&
            list(o.names).includes(h.host) &&
            at >= Date.parse(o.from) &&
            at < (o.open ? Infinity : Date.parse(o.to) + epoch * slotMs)
        );
        if (worked) continue;
        const of = misses.get(h.host) ?? [];
        of.push({ slot: row.slot, assigned: h.assigned, missed: h.assigned - h.included });
        misses.set(h.host, of);
      }
    }

    for (const [host, slots] of misses) {
      // The run that ends with the newest miss.
      let start = slots.length - 1;
      while (start > 0 && (slots[start]?.slot ?? 0) - (slots[start - 1]?.slot ?? 0) <= epoch) start--;
      const run = slots.slice(start);
      const first = run[0];
      const last = run[run.length - 1];
      if (!first || !last) continue;

      const from = slotTime(first.slot);
      const to = slotTime(last.slot + 1);
      const open = lastSlot - last.slot <= epoch + 2;
      // The span the answer has for the host, when this run continues it.
      const known = outages.find(
        o => !o.work && o.hosts === 1 && o.names?.[0] === host && Date.parse(o.to) >= Date.parse(from) - epoch * slotMs
      );

      if (known) {
        if (Date.parse(to) > Date.parse(known.to)) {
          const after = run.filter(s => Date.parse(slotTime(s.slot)) >= Date.parse(known.to));
          known.missed += after.reduce((n, s) => n + s.missed, 0);
          known.assigned += after.reduce((n, s) => n + s.assigned, 0);
          known.to = to;
        }
        if (open) known.open = true;
        else delete known.open;
      } else if (run.length >= off) {
        if (outages.some(o => !o.work && o.hosts > 1 && o.open)) {
          // Hosts that are off stand as one span: this may be one of them.
          stale = stale || !outages.some(o => !o.work && list(o.names).includes(host));
          continue;
        }
        outages.push({
          hosts: 1,
          names: [host],
          from,
          to,
          ...(open ? { open: true } : {}),
          missed: run.reduce((n, s) => n + s.missed, 0),
          assigned: run.reduce((n, s) => n + s.assigned, 0),
          validators: input.validators(host),
        });
      }
    }
  }

  // What is still off is off until now.
  const now = new Date(nowMs).toISOString();
  for (const o of outages) if (o.open && Date.parse(o.to) < nowMs) o.to = now;

  outages.sort(
    (a, b) => Date.parse(a.from) - Date.parse(b.from) || (a.names?.[0] ?? '').localeCompare(b.names?.[0] ?? '')
  );

  return { outages, stale };
}

// ------------------------------------------------------------------ lanes

/** OutageGroup is which of the two kinds a span is: work of upgradoor, or a host that is off without it. */
export type OutageGroup = 'work' | 'off';

export interface OutageBlock {
  group: OutageGroup;
  /** The lane of its group, from 0. */
  lane: number;
  /** Left edge and width, in the pixels of the plot. */
  x: number;
  width: number;
  /** The spans the block stands for: one, or several that were merged to fit. */
  outages: Outage[];
  hosts: number;
  failed: boolean;
  /** The work of the spans when they share one, else "mixed"; undefined for hosts that are off. */
  work: string | undefined;
  open: boolean;
}

interface OutageLayout {
  blocks: OutageBlock[];
  /** Hosts that are off for the whole window: one block, in the first lane of "off". */
  wholeWindow: number;
}

/**
 * packOutages lays the spans out as blocks in a fixed number of lanes per
 * group, on the time axis of the plot. It is interval packing: a span takes
 * the first lane that is free where it begins, so the lanes are not hosts.
 * When the spans of a group do not fit its lanes, spans of the same kind
 * (the same work and rollout, failed or not) that overlap or lie close
 * together are merged into one block that counts its hosts; the distance
 * grows until everything fits, so the height of the graph never depends on
 * the size of the fleet.
 */
export function packOutages(
  outages: readonly Outage[],
  opts: { startMs: number; endMs: number; width: number; lanes: Record<OutageGroup, number> }
): OutageLayout {
  const scale = opts.width / Math.max(1, opts.endMs - opts.startMs);
  const MIN = 4;
  const GAP = 2;

  interface Draft {
    from: number;
    to: number;
    key: string;
    outages: Outage[];
  }

  const drafts: Record<OutageGroup, Draft[]> = { work: [], off: [] };
  let wholeWindow = 0;

  for (const o of outages) {
    const from = Math.max(opts.startMs, Date.parse(o.from));
    // What is still off is off up to the right edge.
    const to = o.open ? opts.endMs : Math.min(opts.endMs, Date.parse(o.to));
    if (to <= opts.startMs || from >= opts.endMs) continue;

    // Off before the window began and still off: off for all of it.
    const whole = !o.work && (o.fromStart ?? false) && (o.open ?? false);
    if (whole) wholeWindow += o.hosts;

    const group: OutageGroup = o.work ? 'work' : 'off';
    const key = o.work ? `${o.work}/${o.rollout ?? ''}/${o.failed ? 'failed' : ''}` : whole ? 'whole' : 'off';
    drafts[group].push({ from, to: Math.max(to, from), key, outages: [o] });
  }

  const blocks: OutageBlock[] = [];

  for (const group of ['work', 'off'] as const) {
    const lanes = opts.lanes[group];
    let items = drafts[group];
    let placed: OutageBlock[] | undefined;

    // Hosts that are off for the whole window are one block from the start.
    items = mergeDrafts(items, 0, d => d.key === 'whole');

    for (const gapPx of [-1, 0, 8, 24, 80, 240, Infinity]) {
      if (gapPx >= 0) items = mergeDrafts(items, gapPx / scale, () => true);
      placed = place(items, group, lanes);
      if (placed) break;
      // The last resort: whatever the kind, one block per stretch of time.
      if (gapPx === Infinity)
        placed = place(
          mergeDrafts(
            items.map(d => ({ ...d, key: '' })),
            0,
            () => true
          ),
          group,
          lanes
        );
    }

    blocks.push(...(placed ?? []));
  }

  return { blocks, wholeWindow };

  function mergeDrafts(items: Draft[], gapMs: number, which: (d: Draft) => boolean): Draft[] {
    const sorted = [...items].sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : a.from - b.from));
    const out: Draft[] = [];

    for (const d of sorted) {
      const last = out[out.length - 1];
      if (last && which(d) && last.key === d.key && d.from <= last.to + gapMs) {
        last.to = Math.max(last.to, d.to);
        last.outages = [...last.outages, ...d.outages];
      } else {
        out.push({ ...d });
      }
    }

    return out;
  }

  function place(items: Draft[], group: OutageGroup, lanes: number): OutageBlock[] | undefined {
    const ends = new Array<number>(lanes).fill(-Infinity);
    const out: OutageBlock[] = [];

    for (const d of [...items].sort((a, b) => a.from - b.from || b.to - a.to)) {
      const x = (d.from - opts.startMs) * scale;
      const width = Math.max(MIN, (d.to - d.from) * scale);
      const lane = ends.findIndex(end => end + GAP <= x);
      if (lane < 0) return undefined;

      ends[lane] = x + width;
      const works = new Set(d.outages.map(o => o.work));
      out.push({
        group,
        lane,
        x,
        width: Math.min(width, opts.width - x),
        outages: d.outages,
        hosts: d.outages.reduce((n, o) => n + o.hosts, 0),
        failed: d.outages.some(o => o.failed),
        work: group === 'off' ? undefined : works.size === 1 ? [...works][0] : 'mixed',
        open: d.outages.some(o => o.open),
      });
    }

    return out;
  }
}
