// The outages of a slot history, worked out the way the controller does
// (pkg/controller/api/outages.go): the leases of the window are spans, the
// misses a lease does not explain make a span when a host missed in at least
// OFF_SLOTS slots no further than an epoch apart, and more spans than an
// answer carries are merged by kind.

import { list, type Lease, type Outage } from '../api/types';

export const OFF_SLOTS = 3;
const MAX_OUTAGES = 240;
const MAX_NAMES = 8;

/** A slot in which a host missed attestations. */
export interface Deviation {
  slot: number;
  host: string;
  assigned: number;
  included: number;
}

interface OutageWorld {
  first: number;
  last: number;
  nowMs: number;
  slotsPerEpoch: number;
  slotMs: number;
  /** slotTime is when a slot began, in milliseconds. */
  slotTime: (slot: number) => number;
  deviations: readonly Deviation[];
  leases: readonly Lease[];
  validators: (host: string) => number;
}

const iso = (ms: number) => new Date(ms).toISOString();

export function outageSpans(w: OutageWorld): Outage[] {
  const start = w.slotTime(w.first);
  const epochMs = w.slotsPerEpoch * w.slotMs;
  const out: Outage[] = [];
  const byHost = new Map<string, Outage[]>();

  for (const l of w.leases) {
    const ended = ['released', 'failed', 'expired'].includes(l.state);
    const from = Date.parse(l.grantedAt);
    const to = l.endedAt ? Date.parse(l.endedAt) : ended ? from : w.nowMs;
    if (to <= start) continue;

    const kinds = [...new Set(list(l.needs).map(n => n.kind as string))];
    const rollouts = Object.values(l.rollouts ?? {}).sort();
    const span: Outage = {
      hosts: 1,
      names: [l.host],
      from: iso(from),
      to: iso(to),
      ...(from < start ? { fromStart: true } : {}),
      ...(ended ? {} : { open: true }),
      lease: l.id,
      work: kinds.length === 1 ? kinds[0] : kinds.length === 0 ? 'image' : 'mixed',
      ...(rollouts[0] ? { rollout: rollouts[0] } : {}),
      ...(l.state === 'failed' || l.state === 'expired' ? { failed: true } : {}),
      missed: 0,
      assigned: 0,
      validators: w.validators(l.host) || l.weight,
    };
    out.push(span);
    byHost.set(l.host, [...(byHost.get(l.host) ?? []), span]);
  }

  const loose = new Map<string, Deviation[]>();
  for (const d of w.deviations) {
    if (d.slot < w.first || d.slot > w.last || d.included >= d.assigned) continue;
    const at = w.slotTime(d.slot);
    const lease = (byHost.get(d.host) ?? []).find(
      s => at >= Date.parse(s.from) && at < (s.open ? Infinity : Date.parse(s.to) + epochMs)
    );
    if (lease) {
      lease.missed += d.assigned - d.included;
      lease.assigned += d.assigned;
    } else {
      loose.set(d.host, [...(loose.get(d.host) ?? []), d]);
    }
  }

  for (const host of [...loose.keys()].sort()) {
    const rows = loose.get(host) ?? [];
    for (let i = 0; i < rows.length; ) {
      let end = i + 1;
      while (end < rows.length && (rows[end]?.slot ?? 0) - (rows[end - 1]?.slot ?? 0) <= w.slotsPerEpoch) end++;
      const run = rows.slice(i, end);
      const a = run[0];
      const b = run[run.length - 1];
      if (a && b && run.length >= OFF_SLOTS) {
        out.push({
          hosts: 1,
          names: [host],
          from: iso(w.slotTime(a.slot)),
          to: iso(w.slotTime(b.slot + 1)),
          ...(a.slot - w.first < w.slotsPerEpoch ? { fromStart: true } : {}),
          ...(w.last - b.slot <= w.slotsPerEpoch + 2 ? { open: true } : {}),
          missed: run.reduce((n, d) => n + d.assigned - d.included, 0),
          assigned: run.reduce((n, d) => n + d.assigned, 0),
          validators: w.validators(host),
        });
      }
      i = end;
    }
  }

  return sort(out);
}

function sort(spans: Outage[]): Outage[] {
  return spans.sort(
    (a, b) => Date.parse(a.from) - Date.parse(b.from) || (a.names?.[0] ?? '').localeCompare(b.names?.[0] ?? '')
  );
}

const kind = (o: Outage) => `${o.work ?? ''}/${o.rollout ?? ''}/${o.failed ? 1 : 0}/${o.fromStart && o.open ? 1 : 0}`;

/** capOutages merges spans of one kind that overlap, or lie closer together than a gap that grows, until they fit. */
export function capOutages(spans: Outage[], windowMs: number, limit = MAX_OUTAGES): Outage[] {
  for (const gap of [0, windowMs / 200, windowMs / 50, windowMs / 10, windowMs]) {
    if (spans.length <= limit) break;

    const sorted = [...spans].sort((a, b) => kind(a).localeCompare(kind(b)) || Date.parse(a.from) - Date.parse(b.from));
    const merged: Outage[] = [];

    for (const span of sorted) {
      const into = merged[merged.length - 1];
      if (into && kind(into) === kind(span) && Date.parse(span.from) <= Date.parse(into.to) + gap) {
        into.hosts += span.hosts;
        into.names = [...new Set([...list(into.names), ...list(span.names)])].sort().slice(0, MAX_NAMES);
        delete into.lease;
        into.missed += span.missed;
        into.assigned += span.assigned;
        into.validators += span.validators;
        if (span.open) into.open = true;
        if (Date.parse(span.to) > Date.parse(into.to)) into.to = span.to;
      } else {
        merged.push({ ...span });
      }
    }

    spans = sort(merged);
  }

  return spans;
}
