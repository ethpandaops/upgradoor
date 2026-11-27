// The Hosts table's row model: public data merged with the authenticated
// summary, plus filtering and sorting.

import {
  list,
  type ContainerListing,
  type ContainerKind,
  type HostListing,
  type PublicHost,
  type PublicImageRef,
  type QueueEntry,
} from '../api/types';
import type { SortDirection, Tone } from '../components/primitives';
import { heldBy, QUEUE_PLACE_TITLE, queuePlace } from '../lib/format';
import { matchesUsageWord } from '../lib/usage';

export interface HostRow {
  name: string;
  el?: PublicImageRef;
  cl?: PublicImageRef;
  /** elClient and clClient name the clients: the team of the container, or the image's name without a login. */
  elClient?: string;
  clClient?: string;
  updatePending: boolean;
  /** summary is present for logged-in users only. */
  summary?: HostListing;
  /** queuePosition is the host's place in the plan queue, if it waits. */
  queuePosition?: number;
}

function imageOf(containers: readonly ContainerListing[], kind: ContainerKind): PublicImageRef | undefined {
  const c = containers.find(x => x.managed && x.kind === kind);
  if (!c) return undefined;
  return { image: c.image, digest: c.digest, revision: c.revision };
}

/** IMAGE_CLIENTS are the clients whose image is not named like them. */
const IMAGE_CLIENTS: Record<string, string> = {
  'client-go': 'geth',
  'nimbus-eth2': 'nimbus',
  'prysm-beacon-chain': 'prysm',
  'beacon-chain': 'prysm',
};

/** clientOf names the client of a container: its team, or what its image is called. */
export function clientOf(image: PublicImageRef | undefined, team?: string): string | undefined {
  if (team) return team;
  if (!image) return undefined;
  const name = image.image.replace(/[@:][^/]*$/, '').replace(/^.*\//, '');
  return IMAGE_CLIENTS[name] ?? name;
}

function teamOf(containers: readonly ContainerListing[], kind: ContainerKind): string | undefined {
  return containers.find(x => x.managed && x.kind === kind)?.team;
}

/**
 * mergeHostRows joins the public host list with the authenticated summaries
 * by name. A host known to only one side still gets a row.
 */
export function mergeHostRows(
  publicHosts: readonly PublicHost[],
  summaries: readonly HostListing[],
  queue: readonly QueueEntry[]
): HostRow[] {
  const rows = new Map<string, HostRow>();

  for (const p of publicHosts) {
    rows.set(p.name, {
      name: p.name,
      el: p.el,
      cl: p.cl,
      elClient: clientOf(p.el),
      clClient: clientOf(p.cl),
      updatePending: p.updatePending,
    });
  }

  for (const s of summaries) {
    const containers = list(s.containers);
    const row = rows.get(s.name) ?? {
      name: s.name,
      el: imageOf(containers, 'el'),
      cl: imageOf(containers, 'cl'),
      updatePending: list(s.needs).length > 0,
    };
    row.summary = s;
    row.elClient = clientOf(row.el, teamOf(containers, 'el'));
    row.clClient = clientOf(row.cl, teamOf(containers, 'cl'));
    rows.set(s.name, row);
  }

  for (const q of queue) {
    const row = rows.get(q.host);
    if (row) row.queuePosition = q.position;
  }

  return [...rows.values()];
}

/** hostState is the one state a host is in, as the State column shows it. */
export function hostState(row: HostRow): { tone: Tone; label: string; title?: string; detail?: string } | undefined {
  const s = row.summary;
  if (!s) return undefined;

  switch (s.plan.slot) {
    case 'leased':
      return { tone: 'accent', label: 'updating' };
    case 'verifying':
      return { tone: 'accent', label: 'verifying' };
    case 'failed':
      return { tone: 'danger', label: 'failed', detail: s.plan.reason };
    case 'held': {
      // One hold per holder, the host's own among them: the count says how many there are.
      const holds = list(s.holders).length;
      return {
        tone: 'warning',
        label: holds > 1 ? `held ×${holds}` : 'held',
        detail: heldBy(s),
      };
    }
    case 'offline':
      return { tone: 'warning', label: 'offline', detail: s.offline?.reason };
    case 'stale':
      // The plan serves an unsupported agent nothing, as a silent one.
      if (s.incompatible) return { tone: 'danger', label: 'incompatible', detail: s.incompatible };
      return { tone: 'warning', label: 'silent', detail: 'the agent stopped syncing' };
    case 'queued':
      return {
        tone: 'info',
        label: row.queuePosition === undefined ? 'queued' : queuePlace(row.queuePosition),
        ...(row.queuePosition === undefined ? {} : { title: QUEUE_PLACE_TITLE }),
        detail: s.plan.reason,
      };
    case 'idle':
      return list(s.needs).length > 0 ? { tone: 'neutral', label: 'pending' } : undefined;
  }
}

/**
 * matchesFilter reports whether every word of the filter occurs in the row's
 * name, images, roles or state: the state as the table shows it ("updating",
 * "silent", "up to date") and the waiting reason. A word such as "memory>=90"
 * keeps the hosts at that use or more.
 */
export function matchesFilter(row: HostRow, filter: string): boolean {
  const all = filter.toLowerCase().split(/\s+/).filter(Boolean);
  if (all.length === 0) return true;

  const words: string[] = [];
  for (const w of all) {
    const usage = matchesUsageWord(w, row.summary?.usage);
    if (usage === false) return false;
    if (usage === undefined) words.push(w);
  }
  if (words.length === 0) return true;

  const haystack = [
    row.name,
    row.el?.image,
    row.el?.digest,
    row.cl?.image,
    row.cl?.digest,
    ...list(row.summary?.roles),
    ...(row.summary ? hostOwners(row.summary) : []),
    row.summary ? (hostState(row)?.label ?? 'up to date') : undefined,
    row.summary?.plan.reason,
    row.summary?.plan.notReady ? `not ready ${row.summary.plan.notReady}` : undefined,
    row.summary?.hold.local ? 'local hold' : undefined,
    row.updatePending ? 'pending' : undefined,
  ]
    .filter(Boolean)
    .join(' ')
    .toLowerCase();

  return words.every(w => haystack.includes(w));
}

export type SortKey =
  | 'name'
  | 'el'
  | 'cl'
  | 'pending'
  | 'kernel'
  | 'uptime'
  | 'security'
  | 'attesting'
  | 'lastSeen'
  | 'slot'
  | 'cpu'
  | 'memory';

/** attestingShare is live / weight, or undefined without validators or data. */
export function attestingShare(s: HostListing | undefined): number | undefined {
  if (!s || s.plan.weight === 0 || !s.plan.liveKnown) return undefined;
  return s.plan.live / s.plan.weight;
}

function sortValue(row: HostRow, key: SortKey): string | number | undefined {
  const s = row.summary;
  switch (key) {
    case 'name':
      return row.name;
    case 'el':
      return row.el?.image;
    case 'cl':
      return row.cl?.image;
    case 'pending':
      return row.updatePending ? 1 : 0;
    case 'kernel':
      return s?.os.runningKernel;
    case 'uptime':
      return s?.uptimeSeconds;
    case 'security':
      return s?.pending.security;
    case 'attesting':
      return attestingShare(s);
    case 'lastSeen':
      return s ? Date.parse(s.lastSeen) : undefined;
    case 'slot':
      return s?.plan.slot;
    case 'cpu':
    case 'memory':
      return s?.usage?.[key];
  }
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

/** sortHostRows sorts by a column; rows without a value sort last, ties by name. */
export function sortHostRows(rows: readonly HostRow[], key: SortKey, direction: SortDirection): HostRow[] {
  const sign = direction === 'asc' ? 1 : -1;

  return [...rows].sort((a, b) => {
    const va = sortValue(a, key);
    const vb = sortValue(b, key);

    if (va === undefined && vb !== undefined) return 1;
    if (va !== undefined && vb === undefined) return -1;

    let cmp = 0;
    if (typeof va === 'number' && typeof vb === 'number') cmp = va - vb;
    else if (va !== undefined && vb !== undefined) cmp = collator.compare(String(va), String(vb));

    return cmp !== 0 ? sign * cmp : collator.compare(a.name, b.name);
  });
}

/** hostOwners lists the teams of the managed containers of the host, sorted. */
export function hostOwners(summary: Pick<HostListing, 'containers'>): string[] {
  const owners = new Set<string>();
  for (const c of list(summary.containers)) {
    if (c.managed && c.team) owners.add(c.team);
  }
  return [...owners].sort();
}

/** rollableContainers lists the managed container names of the given hosts. */
export function rollableContainers(summaries: readonly HostListing[]): string[] {
  const names = new Set<string>();
  for (const s of summaries) {
    for (const c of list(s.containers)) {
      if (c.managed) names.add(c.name);
    }
  }
  return [...names].sort();
}
