// The wire encoding of the event streams, read side; mock/wireStream.ts
// writes it for the mock. pkg/controller/stream/wire.go defines it; the schema of every row is
// read from wire.schema.json, which a Go test writes from the Go types, so
// nothing about a row is written down here a second time.
//
// A stream carries tables of rows. A row is an array of cells in the order of
// its schema; a cell that holds the default is 0 and trailing ones are left
// out; a string that repeats is an index into the stream's string table; a
// time is a number of seconds. After the snapshot, a patch carries new rows,
// the changed cells of changed rows, and the keys of removed rows.
//
// What follows from the clock does not travel: the head slot, the current
// epoch, a host's uptime. chainAt() and the views below compute it.

import {
  list,
  type AuditEntry,
  type AuditHead,
  type ClientView,
  type Gate,
  type HostDetail,
  type HostPlan,
  type HostListing,
  type Inventory,
  type Lease,
  type LiveViews,
  type NetworkHealth,
  type PublicHost,
  type PublicImage,
  type PublicImageRef,
  type QueueEntry,
  type Rollout,
  type RolloutHost,
  type SettingsBody,
  type SlotRow,
} from './types';
import schemaFile from './wire.schema.json';

export type Cell = number | string | Cell[] | { [key: string]: unknown } | null | boolean;

interface Field {
  n: string;
  t: string;
  of?: string;
  omit?: boolean;
}

const SCHEMA = schemaFile as {
  version: string;
  tables: Record<string, string>;
  single: string[];
  schemas: Record<string, Field[]>;
};

/** WIRE_VERSION is the version of the encoding this page reads. */
export const WIRE_VERSION = SCHEMA.version;

const ZERO_TIME = '0001-01-01T00:00:00Z';
/** The public view's text for a beacon node error (beaconErrorPublic in public.go). */
const BEACON_ERROR_PUBLIC = 'beacon node unavailable';

export function fields(schema: string): Field[] {
  const f = SCHEMA.schemas[schema];
  if (!f) throw new Error(`the stream has no row schema "${schema}"`);
  return f;
}

function isoSeconds(seconds: number): string {
  return new Date(seconds * 1000).toISOString().replace('.000Z', 'Z');
}

/** decodeRow turns the cells of a row into the object its schema describes. */
export function decodeRow(schema: string, row: readonly Cell[], strings: readonly string[]): Record<string, unknown> {
  const str = (cell: Cell): string =>
    typeof cell === 'number' ? (strings[cell] ?? '') : typeof cell === 'string' ? cell : '';
  const out: Record<string, unknown> = {};

  fields(schema).forEach((f, i) => {
    const cell = row[i] ?? 0;

    if (cell === 0) {
      if (f.omit) return;
      out[f.n] =
        f.t === 'b' ? false : f.t === 'n' ? 0 : f.t === 's' || f.t === 'r' ? '' : f.t === 't' ? ZERO_TIME : null;
      return;
    }

    switch (f.t) {
      case 'b':
        out[f.n] = true;
        break;
      case 's':
      case 'r':
        out[f.n] = str(cell);
        break;
      case 't':
        out[f.n] = isoSeconds(cell as number);
        break;
      case 'j':
        out[f.n] = (cell as Cell[])[0];
        break;
      case 'l':
        out[f.n] = (cell as Cell[]).map(str);
        break;
      case 'o':
        out[f.n] = decodeRow(f.of ?? '', cell as Cell[], strings);
        break;
      case 'a':
        out[f.n] = (cell as Cell[][]).map(item => decodeRow(f.of ?? '', item, strings));
        break;
      default:
        out[f.n] = cell;
    }
  });

  return out;
}

/** schemaOf names the row schema of a table; kind is "public", "d" (host details) or "". */
export function schemaOf(table: string, kind: string): string {
  const name = SCHEMA.tables[`${kind}.${table}`] ?? SCHEMA.tables[table];
  if (!name) throw new Error(`the stream has no table "${table}"`);
  return name;
}

/** isSingle reports whether a table holds one row without a key. */
export function isSingle(table: string, kind: string): boolean {
  return SCHEMA.single.includes(`${kind}.${table}` in SCHEMA.tables ? `${kind}.${table}` : table);
}

// ------------------------------------------------------------- documents

/** TableBody is a table in a snapshot or a patch (wireTableJSON in wire.go). */
export interface TableBody {
  r?: Cell[][];
  u?: Cell[][];
  x?: Cell[];
  o?: Cell;
}

export type DocBody = Record<string, TableBody | undefined>;

interface Table {
  rows: Map<string, Cell[]>;
  order: Cell | undefined;
}

const keyOf = (cell: Cell | undefined): string => (cell === 0 || cell === undefined ? '' : JSON.stringify(cell));

/**
 * WireDoc is a set of tables as a stream last described it: a snapshot
 * replaces it, a patch changes rows and cells of it.
 */
export class WireDoc {
  private readonly tables = new Map<string, Table>();
  private readonly decoded = new WeakMap<Cell[], Record<string, unknown>>();

  /** kind says which tables these are: "public", "d" (one host's details) or "". */
  constructor(private readonly kind: string) {}

  /** apply takes a snapshot (full) or a patch in. */
  apply(body: DocBody, full: boolean): void {
    if (full) this.tables.clear();

    for (const [name, patch] of Object.entries(body)) {
      if (!patch || typeof patch !== 'object' || Array.isArray(patch)) continue;

      let table = this.tables.get(name);
      if (!table) {
        table = { rows: new Map(), order: undefined };
        this.tables.set(name, table);
      }

      const keyed = !isSingle(name, this.kind);

      for (const key of patch.x ?? []) table.rows.delete(keyOf(key));
      for (const row of patch.r ?? []) table.rows.set(keyed ? keyOf(row[0]) : '', row);

      for (const update of patch.u ?? []) {
        const key = keyOf(update[0]);
        // A new array: the decoded row is remembered by the old one.
        const row = [...(table.rows.get(key) ?? [])];

        for (let i = 1; i + 1 < update.length; i += 2) {
          const at = update[i] as number;
          while (row.length < at) row.push(0);
          row[at] = update[i + 1] ?? 0;
        }

        table.rows.set(key, row);
      }

      if (patch.o !== undefined) table.order = patch.o === 0 ? undefined : patch.o;
    }
  }

  /** rows decodes the rows of a table, in the order they arrived. T is what the table's schema describes. */

  rows<T>(name: string, strings: readonly string[]): T[] {
    const table = this.tables.get(name);
    if (!table) return [];

    const schema = schemaOf(name, this.kind);

    return [...table.rows.values()].map(row => {
      let obj = this.decoded.get(row);
      if (!obj) {
        obj = decodeRow(schema, row, strings);
        this.decoded.set(row, obj);
      }
      return obj as T;
    });
  }

  /** one decodes the row of a table of one row. */
  // eslint-disable-next-line @typescript-eslint/no-unnecessary-type-parameters -- as rows
  one<T>(name: string, strings: readonly string[]): T | undefined {
    return this.rows<T>(name, strings)[0];
  }

  /** order is the listing order of a table, when it has one. */
  // eslint-disable-next-line @typescript-eslint/no-unnecessary-type-parameters -- as rows
  order<T>(name: string): T | undefined {
    return this.tables.get(name)?.order as T | undefined;
  }
}

// ------------------------------------------------------------------ clock

/** WireNetwork is the network's row: a NetworkHealth without what the clock says (wireNetwork in wire.go). */
type WireNetwork = NetworkHealth & { clocked?: boolean; headBehind?: number };

/** slotAt is the slot the chain's timing puts an instant in, or undefined without timing. */
export function slotAt(n: NetworkHealth, ms: number): number | undefined {
  const phases = list(n.phases);

  for (let i = phases.length - 1; i >= 0; i--) {
    const p = phases[i];
    if (!p || p.slotMs <= 0) continue;

    const start = Date.parse(p.start);
    if (ms >= start) return p.epoch * n.slotsPerEpoch + Math.floor((ms - start) / p.slotMs);
  }

  return undefined;
}

/**
 * chainAt completes a network row for an instant: the current epoch from the
 * chain's timing, the head as the clock's slot less what the head is behind,
 * the finalized epoch from the lag. A row whose numbers the clock does not
 * explain already has them. updatedAt stays as the row has it: see
 * beaconSeen.
 */
export function chainAt(network: NetworkHealth, ms: number): NetworkHealth {
  const n = network as WireNetwork;
  const slot = n.clocked ? slotAt(n, ms) : undefined;
  if (slot === undefined || n.slotsPerEpoch <= 0) return network;

  const epoch = Math.floor(slot / n.slotsPerEpoch);

  return {
    ...n,
    headSlot: Math.max(0, slot - (n.headBehind ?? 0)),
    currentEpoch: epoch,
    finalizedEpoch: Math.max(0, epoch - n.finalityLagEpochs),
  };
}

/**
 * beaconSeen says what a network row knows about the beacon node's last
 * answer: "now" while it answers (the time moves with every poll and is not
 * sent), the time it went silent once that stands still, or, for a row the
 * clock does not explain, the time as it is.
 */
export function beaconSeen(network: NetworkHealth): { at: string | undefined; silent: boolean } {
  const n = network as WireNetwork;
  const zero = n.updatedAt.startsWith('0001-01-01');

  if (!n.clocked) return { at: zero ? undefined : n.updatedAt, silent: false };
  return zero ? { at: undefined, silent: false } : { at: n.updatedAt, silent: true };
}

// ------------------------------------------------------------------ views

type WireGate = Gate & { publicReasons?: string[] };
type WireHost = HostListing & { bootTime: string; unplanned?: boolean };
type WireMember = RolloutHost & { key?: string; rolloutId?: string; index?: number };
type WireDetail = HostDetail & { hasInventory?: boolean };

/** without returns a copy of an object that lacks the named keys. */
export function without<T extends object, K extends keyof T>(obj: T, ...keys: K[]): Omit<T, K> {
  const out = { ...obj };
  for (const key of keys) Reflect.deleteProperty(out, key);
  return out;
}

function byName(a: { name: string }, b: { name: string }): number {
  return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

/** imageRef is the public reference of a host's client container (publicImageRef in public.go). */
function imageRef(host: HostListing, kind: 'el' | 'cl'): PublicImageRef | undefined {
  const c = list(host.containers).find(c => c.managed && c.kind === kind);
  if (!c) return undefined;

  return {
    image: c.image,
    ...(c.digest ? { digest: c.digest } : {}),
    ...(c.revision ? { revision: c.revision } : {}),
  };
}

function publicHost(host: HostListing): PublicHost {
  const el = imageRef(host, 'el');
  const cl = imageRef(host, 'cl');

  return { name: host.name, ...(el ? { el } : {}), ...(cl ? { cl } : {}), updatePending: list(host.needs).length > 0 };
}

/** images lists the image references with the hosts on and behind each digest. */
function images(
  rows: readonly { key: string; digest?: string; revision?: string }[],
  hosts: readonly PublicHost[]
): PublicImage[] {
  const out = rows.map(row => {
    const space = row.key.indexOf(' ');
    const kind = row.key.slice(0, space);
    const image = row.key.slice(space + 1);
    const hostsOn: string[] = [];
    const hostsBehind: string[] = [];

    for (const host of hosts) {
      const ref = kind === 'el' ? host.el : kind === 'cl' ? host.cl : undefined;
      if (ref?.image !== image) continue;
      ((ref.digest ?? '') === (row.digest ?? '') ? hostsOn : hostsBehind).push(host.name);
    }

    return {
      image,
      kind,
      ...(row.digest ? { digest: row.digest } : {}),
      ...(row.revision ? { revision: row.revision } : {}),
      hostsOn,
      hostsBehind,
    };
  });

  // EL before CL, then by reference.
  return out.sort((a, b) =>
    a.kind !== b.kind ? (a.kind === 'el' ? -1 : 1) : a.image < b.image ? -1 : a.image > b.image ? 1 : 0
  );
}

function ordered<T extends { id: string }>(rows: readonly T[], ids: readonly string[] | undefined): T[] {
  const byId = new Map(rows.map(r => [r.id, r]));
  return list(ids).flatMap(id => byId.get(id) ?? []);
}

/**
 * buildViews assembles the views the page works with from the tables of a
 * stream. A fact travels once: on the authenticated stream the public host
 * list, the image summary and the plan's host map are read off the host
 * table. ms is the page's clock, for what the clock says.
 */
export function buildViews(doc: WireDoc, strings: readonly string[], priv: boolean, ms: number): Partial<LiveViews> {
  const out: Partial<LiveViews> = {};

  const gate = doc.one<WireGate>('n', strings);
  if (gate) {
    const { publicReasons } = gate;
    const network = chainAt(gate.network, ms);
    const full: Gate = { ...without(gate, 'publicReasons'), network };

    out['public-network'] = priv
      ? {
          ...full,
          reasons: publicReasons ?? full.reasons,
          network: network.error ? { ...network, error: BEACON_ERROR_PUBLIC } : network,
        }
      : full;

    if (priv) {
      const hostRows = doc.rows<WireHost>('h', strings);
      const plan = doc.one<{ paused: boolean; budget: LiveViews['plan']['budget'] }>('pl', strings);

      if (plan) {
        const hosts: Record<string, HostPlan> = {};
        for (const row of hostRows) if (!row.unplanned) hosts[row.name] = row.plan;

        out.plan = {
          computedAt: new Date(ms).toISOString(),
          paused: plan.paused,
          gate: full,
          budget: plan.budget,
          queue: [...doc.rows<QueueEntry>('q', strings)].sort((a, b) => a.position - b.position),
          hosts,
        };
      }
    }
  }

  let publicHosts: PublicHost[];

  if (priv) {
    const hosts = doc
      .rows<WireHost>('h', strings)
      .map(row => ({
        ...without(row, 'unplanned'),
        uptimeSeconds: row.bootTime === ZERO_TIME ? 0 : Math.max(0, Math.floor((ms - Date.parse(row.bootTime)) / 1000)),
      }))
      .sort(byName);

    out.hosts = { hosts };
    publicHosts = hosts.map(publicHost);
  } else {
    publicHosts = [...doc.rows<PublicHost>('h', strings)].sort(byName);
  }

  out['public-hosts'] = { hosts: publicHosts };
  out['public-images'] = { images: images(doc.rows('im', strings), publicHosts) };

  // A slot that settled is a row; who of the hosts attested in it is on the authenticated stream only.
  out.slots = { rows: [...doc.rows<SlotRow>('sl', strings)].sort((a, b) => a.slot - b.slot) };

  if (!priv) return out;

  const members = new Map<string, WireMember[]>();
  for (const m of doc.rows<WireMember>('m', strings)) {
    const of = members.get(m.rolloutId ?? '') ?? [];
    of.push(m);
    members.set(m.rolloutId ?? '', of);
  }

  const rollouts = doc.rows<Rollout>('ro', strings).map(r => ({
    ...r,
    hosts: (members.get(r.id) ?? [])
      .sort((a, b) => (a.index ?? 0) - (b.index ?? 0))
      .map(m => without(m, 'key', 'rolloutId', 'index')),
  }));
  const rolloutOrder = doc.order<[string[], string[]]>('ro');
  const leases = doc.rows<Lease>('le', strings);
  const leaseOrder = doc.order<[string[], string[]]>('le');

  out.rollouts = {
    open: ordered(rollouts, rolloutOrder?.[0]),
    recent: ordered(rollouts, rolloutOrder?.[1]),
    leases: ordered(leases, leaseOrder?.[0]),
    recentLeases: ordered(leases, leaseOrder?.[1]),
  };

  // Consensus before execution, then by name: the order of the controller's list.
  out.clients = {
    clients: [...doc.rows<ClientView>('c', strings)].sort((a, b) =>
      a.layer !== b.layer ? (a.layer === 'consensus' ? -1 : 1) : byName(a, b)
    ),
  };

  const settings = doc.one<SettingsBody>('se', strings);
  if (settings) out.settings = settings;

  const audit = doc.one<AuditHead>('a', strings);
  if (audit) out.audit = audit;

  return out;
}

/** buildHostDetail assembles one host's details from its tables. Its strings travel as they are. */
export function buildHostDetail(doc: WireDoc): HostDetail | undefined {
  const base = doc.one<WireDetail>('x', []);
  if (!base) return undefined;

  const detail = without(base, 'hasInventory');
  const inventory = base.hasInventory ? doc.one<Inventory>('v', []) : undefined;

  return {
    ...detail,
    ...(inventory ? { inventory } : {}),
    leases: ordered(doc.rows<Lease>('l', []), doc.order<string[]>('l')),
    history: [...doc.rows<AuditEntry>('a', [])].sort((a, b) => b.id - a.id),
  };
}
