// The model behind the activity timeline: what every host was, is and will be
// busy with. Past and present come from the leases; the future is an estimate
// made here from the queue, the budget and how long recent leases took.

import { list, type Budget, type Lease, type NeedKind, type QueueEntry, type WaitKind } from '../api/types';

/** WorkKind is what a lease does to a host; the timeline colours by it. */
export type WorkKind = 'image' | 'packages' | 'reboot';

export type BarState = 'done' | 'failed' | 'active' | 'planned';

export interface TimelineBar {
  host: string;
  /** Milliseconds since the epoch. An active bar ends at "now". */
  start: number;
  end: number;
  kind: WorkKind;
  state: BarState;
  /** One line for the tooltip: the lease's outcome or the waiting reason. */
  detail: string;
  leaseId?: string;
}

export interface TimelineRow {
  host: string;
  bars: TimelineBar[];
}

export interface TimelineModel {
  rows: TimelineRow[];
  /** Estimated start per queued host that will be served by itself. */
  eta: ReadonlyMap<string, number>;
  /** How long one lease is assumed to take, in milliseconds. */
  typicalMs: number;
  /** Queued hosts that start later than the forecast looks. */
  late: number;
  /** Queued hosts that cannot be placed: other outages use up the budget. */
  unplaced: number;
}

const AUTO_WAITS: ReadonlySet<WaitKind> = new Set(['next', 'budget', 'host-limit', 'order']);

/** clearsByItself: the host is served without anybody acting, as the rollout proceeds. */
export function clearsByItself(wait: WaitKind | undefined): boolean {
  return wait === undefined || AUTO_WAITS.has(wait);
}

const NOBODY_WAITS: ReadonlySet<WaitKind> = new Set(['held', 'silent', 'verb']);

/** holdsBack: hosts behind this one in the order of a rollout wait for it. */
function holdsBack(wait: WaitKind | undefined): boolean {
  return wait === undefined || !NOBODY_WAITS.has(wait);
}

/** needsSomebody: the host waits until a person acts (approve, resume, retry, unhold, fix the agent). */
export function needsSomebody(wait: WaitKind | undefined): boolean {
  return wait !== undefined && wait !== 'gate' && !AUTO_WAITS.has(wait);
}

const KIND_RANK: Record<NeedKind, number> = { image: 0, packages: 1, reboot: 2 };

/** workKind is the most disruptive of the given need kinds; a roll when there is none. */
export function workKind(kinds: readonly NeedKind[]): WorkKind {
  let best: NeedKind = 'image';
  for (const k of kinds) if (KIND_RANK[k] > KIND_RANK[best]) best = k;
  return best;
}

const TERMINAL: ReadonlySet<string> = new Set(['released', 'failed', 'expired']);

export function leaseOpen(lease: Lease): boolean {
  return !TERMINAL.has(lease.state);
}

function median(values: number[]): number | undefined {
  if (values.length === 0) return undefined;
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1 ? sorted[mid] : ((sorted[mid - 1] as number) + (sorted[mid] as number)) / 2;
}

/**
 * typicalLeaseMs is the median duration of the leases that were released; with
 * none to learn from, two epochs plus three minutes (a restart and one epoch
 * of ready check), or 15 minutes without a known epoch length.
 */
export function typicalLeaseMs(leases: readonly Lease[], epochSeconds: number): number {
  const took = leases
    .filter(l => l.state === 'released' && l.endedAt)
    .map(l => Date.parse(l.endedAt as string) - Date.parse(l.grantedAt))
    .filter(ms => Number.isFinite(ms) && ms > 0);

  return median(took) ?? (epochSeconds > 0 ? (2 * epochSeconds + 180) * 1000 : 15 * 60 * 1000);
}

/**
 * StageOf is a host's stage in a rollout (fleet.Stage, in serving order: 1
 * repair, the members whose host is broken; 2 canary; 3 rest), or undefined
 * when unknown. A host waits for its place in the order behind the hosts of
 * an earlier stage.
 */
export type StageOf = (rollout: string, host: string) => number | undefined;

/**
 * splitQueue separates the queued hosts that start without anybody acting
 * from the rest. A host that waits for its place in a rollout's order is only
 * in the first group if nothing ahead of it in that rollout is stuck: behind
 * a broken host or a canary that waits for the gate or an approval it waits
 * as long as that host does. A host that is held, silent or reports a verb
 * its agent does not allow holds nobody back: the scheduler does not make
 * anybody wait for it.
 */
export function splitQueue(
  queue: readonly QueueEntry[],
  stageOf: StageOf
): { next: QueueEntry[]; behind: QueueEntry[] } {
  const next: QueueEntry[] = [];
  const behind: QueueEntry[] = [];
  const stuck: QueueEntry[] = [];

  for (const q of [...queue].sort((a, b) => a.position - b.position)) {
    if (!clearsByItself(q.wait)) {
      stuck.push(q);
      continue;
    }

    const rollouts = q.rollout ? [q.rollout] : list(q.rollouts);
    const blocked =
      q.wait === 'order' &&
      stuck.some(
        s =>
          holdsBack(s.wait) &&
          rollouts.some(r => {
            const mine = stageOf(r, q.host);
            const theirs = list(s.rollouts).includes(r) ? stageOf(r, s.host) : undefined;
            return mine !== undefined && theirs !== undefined && theirs < mine;
          })
      );

    if (blocked) {
      behind.push(q);
      stuck.push(q);
    } else {
      next.push(q);
    }
  }

  return { next, behind };
}

export interface ForecastInput {
  now: number;
  typicalMs: number;
  /** Give up scheduling past now + horizonMs. */
  horizonMs: number;
  /** The hosts that start by themselves (splitQueue's next). */
  queue: readonly QueueEntry[];
  active: readonly { host: string; start: number; weight: number; rollouts?: readonly string[] }[];
  budget: Budget;
  /**
   * What granting a host costs the budget: its live validators. The dead
   * ones are in the budget's unavailable weight already.
   */
  costs: ReadonlyMap<string, number>;
  stageOf?: StageOf;
}

export interface ForecastSlot {
  host: string;
  start: number;
  end: number;
}

export interface Forecast {
  slots: ForecastSlot[];
  /** Hosts that would start after the horizon (or beyond the first MAX_CANDIDATES of the queue). */
  late: number;
  /** Hosts that never fit: what is unavailable for other reasons uses up the budget. */
  unplaced: number;
}

const EPSILON = 1e-6;
/** The forecast looks at this many queued hosts; a longer queue is "late" beyond it. */
const MAX_CANDIDATES = 250;

interface Item {
  host: string;
  end: number;
  cost: number;
  rollouts: readonly string[];
  reason: string;
  active: boolean;
}

/**
 * forecast replays the scheduler's grant rules over the queue with one
 * assumption: every lease takes typicalMs. Like the scheduler it walks the
 * queue in order at every step and starts each host that fits the host limit
 * and the weight budget, skipping the ones that do not. A host that waits for
 * its place in the order ("after x", "after canary x") starts once the hosts
 * of the earlier stages of its rollout (the broken hosts, the canary) are
 * done. When the host it waits for is neither in the queue nor holds a lease
 * (it is expected to report its need, and nobody knows when it will), the
 * waiting host gets no slot and is counted as late.
 */
export function forecast(input: ForecastInput): Forecast {
  const { now, typicalMs, budget } = input;
  const stageOf: StageOf = input.stageOf ?? (() => undefined);
  const queued = [...input.queue].filter(q => clearsByItself(q.wait)).sort((a, b) => a.position - b.position);
  // A host that waits for one that is not here yet starts nobody knows when.
  const here = new Set([...input.active.map(a => a.host), ...queued.map(q => q.host)]);
  const ordered = queued.filter(q => q.wait !== 'order' || q.after === undefined || here.has(q.after));
  let pending = ordered.slice(0, MAX_CANDIDATES);
  let late = queued.length - pending.length;

  const items: Item[] = input.active.map(a => ({
    host: a.host,
    end: Math.max(a.start + typicalMs, now + typicalMs / 4),
    cost: a.weight,
    rollouts: a.rollouts ?? [],
    reason: '',
    active: true,
  }));
  // What is unavailable for other reasons than a lease is assumed to stay so.
  const base = Math.max(0, budget.unavailableWeight - input.active.reduce((sum, a) => sum + a.weight, 0));
  const maxLeased = Math.max(1, budget.maxLeasedHosts);

  /** ahead: a must be done before the order-waiting c may start. */
  const ahead = (
    a: { host: string; rollouts: readonly string[]; reason: string; active?: boolean; position?: number },
    c: QueueEntry
  ) => {
    // The rollout the wait is about, as in splitQueue; all of the host's
    // rollouts only when the plan does not name one.
    const about = c.rollout ? [c.rollout] : list(c.rollouts);
    const shared = about.filter(r => a.rollouts.includes(r));
    let known = false;
    for (const r of shared) {
      const mine = stageOf(r, c.host);
      const theirs = stageOf(r, a.host);
      if (mine === undefined || theirs === undefined) continue;
      known = true;
      if (theirs < mine) return true;
    }
    if (known) return false;
    // Without stages: a lease that serves the same rollout is ahead, and so
    // is a host of the same rollout that is served earlier and waits for
    // something else. A lease of another rollout holds nobody back here.
    if (shared.length === 0) return false;
    if (a.active === true) return true;
    return a.reason !== c.reason && (a.position === undefined || a.position < c.position);
  };

  const slots: ForecastSlot[] = [];
  let t = now;

  for (;;) {
    const running = items.filter(i => i.end > t + EPSILON);
    let unavailable = base + running.reduce((sum, i) => sum + i.cost, 0);
    let leased = running.length;
    const waiting: QueueEntry[] = [];

    for (const c of pending) {
      const cost = input.costs.get(c.host) ?? 0;
      const inOrder =
        c.wait !== 'order' ||
        (!running.some(i => ahead(i, c)) &&
          !pending.some(
            p =>
              p !== c && ahead({ host: p.host, rollouts: list(p.rollouts), reason: p.reason, position: p.position }, c)
          ));
      const fits =
        cost === 0 ||
        unavailable + cost <= budget.allowedWeight + EPSILON ||
        (unavailable === 0 && budget.allowedWeight > 0 && leased === 0);

      if (!inOrder || !fits || leased >= maxLeased) {
        waiting.push(c);
        continue;
      }

      const item = {
        host: c.host,
        end: t + typicalMs,
        cost,
        rollouts: list(c.rollouts),
        reason: c.reason,
        active: false,
      };
      items.push(item);
      running.push(item);
      slots.push({ host: c.host, start: t, end: item.end });
      unavailable += cost;
      leased++;
    }

    pending = waiting;
    if (pending.length === 0) return { slots, late, unplaced: 0 };
    // Nothing is running that could free the budget or the order.
    if (running.length === 0) return { slots, late, unplaced: pending.length };

    t = Math.min(...running.map(i => i.end));
    if (t > now + input.horizonMs) {
      late += pending.length;
      return { slots, late, unplaced: 0 };
    }
  }
}

export interface TimelineInput {
  now: number;
  /** Leases older than this are left out. */
  from: number;
  horizonMs: number;
  epochSeconds: number;
  leases: readonly Lease[];
  queue: readonly QueueEntry[];
  budget: Budget | undefined;
  /** Live validators per host: what granting it costs the budget. */
  costs: ReadonlyMap<string, number>;
  stageOf?: StageOf;
  /** The open need kinds per host, to colour the planned bars. */
  needKinds: ReadonlyMap<string, readonly NeedKind[]>;
}

function leaseDetail(lease: Lease): string {
  if (lease.error) return lease.error;
  if (lease.operation?.error) return lease.operation.error;
  if (leaseOpen(lease)) {
    const phase = lease.operation?.phase;
    return [`lease ${lease.state}`, phase && `agent ${phase}`, lease.readiness].filter(Boolean).join(' · ');
  }
  return lease.readiness || lease.state;
}

/** buildTimeline turns leases and the queue into one row of bars per host, oldest activity first. */
export function buildTimeline(input: TimelineInput): TimelineModel {
  const { now } = input;
  const byHost = new Map<string, TimelineBar[]>();
  const add = (bar: TimelineBar) => {
    const bars = byHost.get(bar.host);
    if (bars) bars.push(bar);
    else byHost.set(bar.host, [bar]);
  };

  const active: ForecastInput['active'][number][] = [];

  for (const lease of input.leases) {
    const start = Date.parse(lease.grantedAt);
    if (!Number.isFinite(start)) continue;

    const open = leaseOpen(lease);
    const end = open ? now : Date.parse(lease.endedAt ?? '');
    if (!Number.isFinite(end) || end < input.from) continue;

    if (open) {
      active.push({ host: lease.host, start, weight: lease.weight, rollouts: Object.values(lease.rollouts ?? {}) });
    }

    add({
      host: lease.host,
      start,
      end: Math.max(end, start),
      kind: workKind(list(lease.needs).map(n => n.kind)),
      state: open ? 'active' : lease.state === 'released' ? 'done' : 'failed',
      detail: leaseDetail(lease),
      leaseId: lease.id,
    });
  }

  const typicalMs = typicalLeaseMs(input.leases, input.epochSeconds);
  const result: Forecast = input.budget
    ? forecast({
        now,
        typicalMs,
        horizonMs: input.horizonMs,
        queue: input.queue,
        active,
        budget: input.budget,
        costs: input.costs,
        stageOf: input.stageOf,
      })
    : { slots: [], late: 0, unplaced: 0 };
  const slots = result.slots;

  const reasons = new Map(input.queue.map(q => [q.host, q.reason]));
  const eta = new Map<string, number>();

  for (const slot of slots) {
    eta.set(slot.host, slot.start);
    add({
      host: slot.host,
      start: slot.start,
      end: slot.end,
      kind: workKind(input.needKinds.get(slot.host) ?? []),
      state: 'planned',
      detail: reasons.get(slot.host) ?? 'queued',
    });
  }

  const rows = [...byHost.entries()]
    .map(([host, bars]) => ({ host, bars: bars.sort((a, b) => a.start - b.start) }))
    .sort(
      (a, b) => (a.bars[0] as TimelineBar).start - (b.bars[0] as TimelineBar).start || a.host.localeCompare(b.host)
    );

  return {
    rows,
    eta,
    typicalMs,
    late: result.late,
    unplaced: result.unplaced,
  };
}
