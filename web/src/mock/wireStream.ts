// The writing side of the stream encoding for the mock controller: what
// pkg/controller/stream/events.go does with the views (streamDocs) and
// wire.go with the tables (diff), so the page reads the mock through the
// same decoder as a real controller.

import {
  list,
  type Clients,
  type Gate,
  type HostDetail,
  type HostListing,
  type Hosts,
  type NetworkHealth,
  type Plan,
  type PublicHosts,
  type PublicImages,
  type Rollouts,
  type SlotRow,
} from '../api/types';
import { fields, isSingle, schemaOf, slotAt, type Cell, type DocBody, type TableBody } from '../api/wire';

const ZERO_TIME = '0001-01-01T00:00:00Z';

/**
 * encodeRow is the writing side, as the controller does it, so the page
 * reads the mock's stream through the same decoder as a real one.
 * ref turns a repeating string into its cell.
 */
export function encodeRow(schema: string, value: object, ref: (s: string) => Cell): Cell[] {
  const obj = value as Record<string, unknown>;
  const cells: Cell[] = [];
  let last = 0;

  fields(schema).forEach((f, i) => {
    const v = obj[f.n];
    let cell: Cell = 0;

    if (v !== undefined && v !== null) {
      switch (f.t) {
        case 'b':
          cell = v === true ? 1 : 0;
          break;
        case 'n':
          cell = v as number;
          break;
        case 's':
          cell = v === '' ? 0 : (v as string);
          break;
        case 'r':
          cell = v === '' ? 0 : ref(v as string);
          break;
        case 't':
          cell = (v as string).startsWith('0001-01-01') ? 0 : Math.floor(Date.parse(v as string) / 1000);
          break;
        case 'j':
          cell = [v] as Cell;
          break;
        case 'l':
          cell = (v as string[]).map(ref);
          break;
        case 'o':
          cell = encodeRow(f.of ?? '', v, ref);
          break;
        case 'a':
          cell = (v as object[]).map(item => encodeRow(f.of ?? '', item, ref));
          break;
      }
    }

    cells.push(cell);
    if (cell !== 0) last = i + 1;
  });

  return cells.slice(0, last);
}

interface TableState {
  rows: Map<string, string[]>;
  order: string | undefined;
}

/** DocState is a document as it was last sent: every cell as JSON. */
export type DocState = Map<string, TableState>;

/** TableInput is what goes into one table: rows with a key in their first cell, or the one row. */
interface TableInput {
  rows?: readonly object[];
  one?: object;
  order?: unknown;
}

type Tables = Record<string, TableInput>;

/** StringTable hands out the index of a repeating string; it only grows. */
export class StringTable {
  readonly list: string[] = [''];
  private readonly index = new Map<string, number>([['', 0]]);

  ref = (s: string): Cell => {
    let i = this.index.get(s);
    if (i === undefined) {
      i = this.list.length;
      this.index.set(s, i);
      this.list.push(s);
    }
    return i;
  };
}

const plain = (s: string): Cell => s;

/** encodeDoc encodes the tables of a document; without a string table its strings stay as they are. */
export function encodeDoc(kind: string, tables: Tables, strings?: StringTable): DocState {
  const doc: DocState = new Map();

  for (const [name, input] of Object.entries(tables)) {
    const schema = schemaOf(name, kind);
    const rows = new Map<string, string[]>();
    const items = isSingle(name, kind) ? (input.one ? [input.one] : []) : list(input.rows);

    for (const item of items) {
      const cells = encodeRow(schema, item, strings?.ref ?? plain).map(c => JSON.stringify(c));
      rows.set(isSingle(name, kind) ? '' : (cells[0] ?? ''), cells);
    }

    doc.set(name, { rows, order: input.order === undefined ? undefined : JSON.stringify(input.order) });
  }

  return doc;
}

const parse = (cell: string): Cell => JSON.parse(cell) as Cell;
const keyCell = (key: string): Cell => (key === '' ? 0 : parse(key));

/** fullBody is a document as a snapshot carries it. */
export function fullBody(doc: DocState): DocBody {
  const out: DocBody = {};

  for (const [name, table] of doc) {
    out[name] = {
      r: [...table.rows.values()].map(row => row.map(parse)),
      ...(table.order === undefined ? {} : { o: parse(table.order) }),
    };
  }

  return out;
}

/** diffBody is what turns the document old into doc: new rows, changed cells, removed keys. */
export function diffBody(doc: DocState, old: DocState): DocBody {
  const out: DocBody = {};

  for (const [name, table] of doc) {
    const before = old.get(name);
    const patch: TableBody = {};

    for (const [key, row] of table.rows) {
      const was = before?.rows.get(key);
      if (!was) {
        (patch.r ??= []).push(row.map(parse));
        continue;
      }

      const update: Cell[] = [keyCell(key)];
      for (let i = 0; i < Math.max(row.length, was.length); i++) {
        if ((row[i] ?? '0') !== (was[i] ?? '0')) update.push(i, parse(row[i] ?? '0'));
      }
      if (update.length > 1) (patch.u ??= []).push(update);
    }

    for (const key of before?.rows.keys() ?? []) {
      if (!table.rows.has(key)) (patch.x ??= []).push(keyCell(key));
    }

    if (table.order !== before?.order) patch.o = table.order === undefined ? 0 : parse(table.order);
    if (Object.keys(patch).length > 0) out[name] = patch;
  }

  return out;
}

// ------------------------------------------------------- views to tables

const seconds = (iso: string): string => new Date(Math.floor(Date.parse(iso) / 1000) * 1000).toISOString();

/** wireNetwork takes out of a network view what the clock says (newWireNetwork in wire.go). */
function wireNetwork(n: NetworkHealth, nowMs: number): object {
  const slot = slotAt(n, Date.parse(n.updatedAt));
  const explained =
    slot !== undefined &&
    !n.timingError &&
    Math.floor(slot / n.slotsPerEpoch) === n.currentEpoch &&
    n.headSlot <= slot &&
    n.finalizedEpoch + n.finalityLagEpochs === n.currentEpoch;

  if (!explained) return n;

  const behind = slot - n.headSlot;

  return {
    ...n,
    updatedAt: (slotAt(n, nowMs) ?? 0) <= slot + 4 ? ZERO_TIME : n.updatedAt,
    headSlot: 0,
    currentEpoch: 0,
    finalizedEpoch: 0,
    clocked: true,
    headBehind: behind > 1 ? behind : 0,
  };
}

function wireGate(gate: Gate, nowMs: number): object {
  return { ...gate, network: wireNetwork(gate.network, nowMs) };
}

/** wireHost takes out of a host row what moves with every sync (newWireHost in wire.go). */
function wireHost(h: HostListing, planned: boolean): object {
  return {
    ...h,
    lastSeen: h.stale ? h.lastSeen : ZERO_TIME,
    uptimeSeconds: 0,
    bootTime:
      h.uptimeSeconds > 0
        ? seconds(new Date(Date.parse(h.lastSeen) - h.uptimeSeconds * 1000).toISOString())
        : ZERO_TIME,
    unplanned: !planned,
  };
}

function sharedTables(images: PublicImages, slots: readonly SlotRow[]): Tables {
  return {
    im: {
      rows: list(images.images).map(i => ({ key: `${i.kind} ${i.image}`, digest: i.digest, revision: i.revision })),
    },
    sl: { rows: slots },
  };
}

/** publicTables are the tables of the public stream: built from the public views and nothing else. */
export function publicTables(v: {
  now: number;
  gate: Gate;
  hosts: PublicHosts;
  images: PublicImages;
  /** slots are the newest slots of the slot history. */
  slots: readonly SlotRow[];
}): Tables {
  // The public rows are built field by field: the split per host cannot ride along.
  const slots = v.slots.map(({ slot, proposed, assigned, included }) => ({ slot, proposed, assigned, included }));

  return {
    n: { one: wireGate(v.gate, v.now) },
    h: { rows: list(v.hosts.hosts) },
    ...sharedTables(v.images, slots),
  };
}

/** privateTables are the tables of the authenticated stream. */
export function privateTables(v: {
  now: number;
  plan: Plan;
  hosts: Hosts;
  clients: Clients;
  images: PublicImages;
  slots: readonly SlotRow[];
  rollouts: Rollouts;
  latestAuditId: number;
}): Tables {
  const all = [...list(v.rollouts.open), ...list(v.rollouts.recent)];
  const planned = v.plan.hosts ?? {};

  return {
    n: { one: wireGate(v.plan.gate, v.now) },
    h: { rows: list(v.hosts.hosts).map(h => wireHost(h, h.name in planned)) },
    c: { rows: list(v.clients.clients) },
    ...sharedTables(v.images, v.slots),
    pl: { one: { paused: v.plan.paused, budget: v.plan.budget } },
    q: { rows: list(v.plan.queue) },
    ro: {
      rows: all.map(r => ({ ...r, hosts: null })),
      order: [list(v.rollouts.open).map(r => r.id), list(v.rollouts.recent).map(r => r.id)],
    },
    m: {
      rows: all.flatMap(r =>
        list(r.hosts).map((m, index) => ({ key: `${r.id}/${m.host}`, rolloutId: r.id, index, ...m }))
      ),
    },
    le: {
      rows: [...list(v.rollouts.leases), ...list(v.rollouts.recentLeases)],
      order: [list(v.rollouts.leases).map(l => l.id), list(v.rollouts.recentLeases).map(l => l.id)],
    },
    a: { one: { latestId: v.latestAuditId } },
  };
}

/** detailTables are the tables of one host's details. */
export function detailTables(d: HostDetail): Tables {
  const { inventory, leases, history, ...rest } = d;

  return {
    x: { one: { ...rest, hasInventory: inventory !== undefined } },
    ...(inventory ? { v: { one: inventory } } : {}),
    l: { rows: list(leases), order: list(leases).map(l => l.id) },
    a: { rows: list(history) },
  };
}
