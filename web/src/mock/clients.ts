// The client views of the mock controller, with the rules of
// pkg/controller/views/clients.go: the fleet grouped by the client software of
// its managed containers, one state per host and client, and one page of a
// client's hosts at a time.

import {
  CLIENT_HOST_STATES,
  list,
  type ClientHost,
  type ClientHostState,
  type ClientHosts,
  type ClientImage,
  type ClientView,
  type Container,
  type ContainerKind,
  type HostSummary,
  type Need,
  type Settings,
} from '../api/types';
import { isHeld } from '../lib/format';
import { hostUsage } from '../lib/usage';
import { hex } from './fixture';

/** The page size of a client's host list, and its bounds (clients.go). */
export const CLIENT_HOST_LIMIT = 50;
export const CLIENT_HOST_LIMIT_MAX = 200;
const CLIENT_HOST_QUERY_MAX = 128;
/** The most names a names=true answer carries (maxActionHosts). */
const CLIENT_HOST_NAMES_MAX = 2000;

/** ClientIndex is every client with its hosts, built from the host table. */
export interface ClientIndex {
  views: ClientView[];
  hosts: Map<string, ClientHost[]>;
}

/** ClientQueryError is a query the hosts route refuses: 400 for a bad filter or page, 404 for an unknown client. */
export class ClientQueryError extends Error {
  constructor(
    readonly status: 400 | 404,
    message: string
  ) {
    super(message);
  }
}

const compare = (a: string, b: string): number => (a < b ? -1 : a > b ? 1 : 0);

/** imagePolicy is the policy the image rollouts of a client answer to (fleet.Settings.ImagePolicy). */
export function imagePolicy(settings: Settings, client: string | undefined): Settings['policyImage'] {
  return (client ? settings.policyClients[client] : undefined) ?? settings.policyImage;
}

/** shouldRun mirrors protocol.Container.ShouldRun: a container that is down and was not stopped by a person. */
function shouldRun(c: Container): boolean {
  if (c.state === 'running' || c.state === 'restarting') return true;
  if (c.state === 'exited' || c.state === 'dead') {
    const code = c.exitCode ?? 0;
    return code !== 0 && code !== 137 && code !== 143;
  }
  return false;
}

function needContainer(n: Need): string {
  return n.image?.container ?? n.override?.container ?? n.spec?.container ?? '';
}

/** scheduler.UnitName: the group rollouts of one kind of one team, or of the kind alone. */
export function unitName(kind: string | undefined, team: string | undefined): string {
  return team ? `unit:${kind ?? ''}#${team}` : `unit:${kind ?? ''}`;
}

function failure(sum: HostSummary, containers: readonly Container[]): string {
  for (const c of containers) {
    if (sum.broken && unitName(c.kind, c.team) in sum.broken) return `the last lease for ${c.image} failed`;
    if (c.state !== 'running' && shouldRun(c)) return `container ${c.name} is ${c.state}`;
  }

  return sum.plan.slot === 'failed' ? sum.plan.reason || 'the last lease failed' : '';
}

function pending(sum: HostSummary, containers: readonly Container[]): boolean {
  return containers.some(
    c =>
      (c.registry?.digest !== undefined && c.registry.digest !== '' && c.registry.digest !== (c.digest ?? '')) ||
      list(sum.needs).some(n => needContainer(n) === c.name)
  );
}

/** hostState is the state of a host on a client's page, and what there is to say about it (clientHostState). */
function hostState(sum: HostSummary, containers: readonly Container[]): { state: ClientHostState; reason: string } {
  const failed = failure(sum, containers);
  if (failed !== '') return { state: 'failed', reason: failed };

  if (sum.plan.slot === 'leased' || sum.plan.slot === 'verifying') return { state: 'rolling', reason: sum.plan.slot };

  if (sum.hold.local) return { state: 'held', reason: sum.hold.localReason || 'held on the host' };
  if (isHeld(sum))
    return {
      state: 'held',
      reason: list(sum.hold.holds)
        .map(h => (h.reason ? `${h.holder}: ${h.reason}` : h.holder))
        .join('; '),
    };

  if (containers.some(c => c.override)) return { state: 'overridden', reason: sum.plan.reason ?? '' };
  if (pending(sum, containers)) return { state: 'pending', reason: sum.plan.reason ?? '' };

  return { state: 'current', reason: '' };
}

function clientHost(sum: HostSummary, client: string): ClientHost {
  const containers = list(sum.containers).filter(c => c.managed && c.team === client);
  const { state, reason } = hostState(sum, containers);

  return {
    host: sum.name,
    state,
    ...(reason ? { reason } : {}),
    ...(sum.stale ? { stale: true } : {}),
    slot: sum.plan.slot,
    containers,
    ...(sum.resources ? { usage: hostUsage(sum.resources) } : {}),
  };
}

const KIND_ORDER: readonly ContainerKind[] = ['el', 'cl', 'vc'];

/** images lists the configured image references of a client's containers with what their tags point at (clientImages). */
function images(rows: readonly ClientHost[]): ClientImage[] {
  const groups = new Map<
    string,
    {
      image: ClientImage;
      checked: number;
      digests: string[];
      commands: Map<string, number>;
      args: Map<string, number>;
      voted: number;
    }
  >();

  for (const row of rows) {
    const seen = new Set<string>();

    for (const c of list(row.containers)) {
      const configured = c.configuredImage || c.image;
      const key = `${c.kind ?? ''} ${configured}`;

      let g = groups.get(key);
      if (!g) {
        g = {
          image: { kind: c.kind ?? 'other', image: configured, hosts: 0, behind: 0, commandHosts: 0 },
          checked: 0,
          digests: [],
          commands: new Map(),
          args: new Map(),
          voted: 0,
        };
        groups.set(key, g);
      }

      if (!seen.has(key)) {
        seen.add(key);
        g.image.hosts++;
        const o = c.override;
        if (!o || list(o.addFlags).length + list(o.removeFlags).length === 0) {
          const joined = list(c.command).join('\u0000');
          g.commands.set(joined, (g.commands.get(joined) ?? 0) + 1);
          g.voted++;
          for (const arg of new Set(list(c.command))) g.args.set(arg, (g.args.get(arg) ?? 0) + 1);
        }
        // A container on another reference (overridden, or rolling onto agent.yaml's) says nothing about this tag.
        if (c.image === configured) g.digests.push(c.digest ?? '');
      }

      const reg = c.registry;
      if (c.image !== configured || !reg?.digest) continue;

      const checked = Date.parse(reg.checkedAt);
      if (!g.image.digest || checked > g.checked || (checked === g.checked && reg.digest < g.image.digest)) {
        g.image.digest = reg.digest;
        g.image.revision = reg.revision;
        g.checked = checked;
      }
    }
  }

  return [...groups.values()]
    .map(g => ({
      ...g.image,
      ...commonCommand(g.commands),
      commonArgs: commonArgs(g.args, g.voted),
      behind: g.image.digest ? g.digests.filter(d => d !== g.image.digest).length : 0,
    }))
    .sort((a, b) => KIND_ORDER.indexOf(a.kind) - KIND_ORDER.indexOf(b.kind) || compare(a.image, b.image));
}

/** commonCommand is the command line most hosts report; on a tie the first by order (commonCommand). */
function commonCommand(counts: ReadonlyMap<string, number>): { command?: string[]; commandHosts: number } {
  let best = '';
  let hosts = 0;
  for (const [joined, n] of counts) {
    if (n > hosts || (n === hosts && joined < best)) [best, hosts] = [joined, n];
  }
  return best === '' ? { commandHosts: hosts } : { command: best.split('\u0000'), commandHosts: hosts };
}

/** commonArgs are the arguments more than half of the hosts report, in first-seen order (argVotes.common). */
function commonArgs(counts: ReadonlyMap<string, number>, hosts: number): string[] | undefined {
  const out = [...counts].filter(([, n]) => n * 2 > hosts).map(([arg]) => arg);
  return out.length > 0 ? out : undefined;
}

/** overrideParts reports whether a container of the row has an image override, a flag override (overrideParts). */
function overrideParts(row: ClientHost): { image: boolean; flags: boolean } {
  const all = list(row.containers).flatMap(c => (c.override ? [c.override] : []));
  return {
    image: all.some(o => Boolean(o.image)),
    flags: all.some(o => list(o.addFlags).length + list(o.removeFlags).length > 0),
  };
}

/** version is a digest of a client's host list: it differs whenever a page of the list would (clientVersion). */
function version(rows: readonly ClientHost[]): string {
  // The time of the registry check and the resource use move with every sync without saying anything new.
  return hex(
    JSON.stringify(rows, (key, value: unknown) =>
      key === 'checkedAt' || key === 'usage' || key === 'stats' ? undefined : value
    ),
    16
  );
}

/**
 * clientIndex groups the hosts by the clients of their managed containers.
 * The hosts are the summaries with their containers whole (team, override).
 */
export function clientIndex(hosts: readonly HostSummary[], settings: Settings): ClientIndex {
  const byClient = new Map<string, ClientHost[]>();

  for (const sum of hosts) {
    const names = new Set(list(sum.containers).flatMap(c => (c.managed && c.team ? [c.team] : [])));
    for (const name of names) {
      const rows = byClient.get(name) ?? [];
      rows.push(clientHost(sum, name));
      byClient.set(name, rows);
    }
  }

  const views: ClientView[] = [];

  for (const [name, rows] of byClient) {
    rows.sort((a, b) => compare(a.host, b.host));

    const count = (state: ClientHostState) => rows.filter(r => r.state === state).length;
    const consensus = rows.some(r => list(r.containers).some(c => c.kind === 'cl' || c.kind === 'vc'));

    views.push({
      name,
      layer: consensus ? 'consensus' : 'execution',
      policy: imagePolicy(settings, name),
      policySet: name in settings.policyClients,
      defaultPolicy: settings.policyImage,
      images: images(rows),
      hosts: rows.length,
      current: count('current'),
      pending: count('pending'),
      rolling: count('rolling'),
      overridden: count('overridden'),
      held: count('held'),
      failed: count('failed'),
      imageOverrides: rows.filter(r => overrideParts(r).image).length,
      flagOverrides: rows.filter(r => overrideParts(r).flags).length,
      version: version(rows),
    });
  }

  views.sort((a, b) => compare(a.layer, b.layer) || compare(a.name, b.name));

  return { views, hosts: byClient };
}

/**
 * clientHostsPage answers GET /api/v1/clients/{client}/hosts: one page of
 * the hosts that run the client, ordered by name. state is a list of states
 * separated by commas, q a part of the host name.
 */
export function clientHostsPage(index: ClientIndex, client: string, params: URLSearchParams): ClientHosts {
  const needle = (params.get('q') ?? '').trim().toLowerCase();
  if (needle.length > CLIENT_HOST_QUERY_MAX) throw new ClientQueryError(400, 'q is too long');

  const states: ClientHostState[] = [];
  for (const part of (params.get('state') ?? '').split(',')) {
    if (part === '') continue;
    if (!(CLIENT_HOST_STATES as readonly string[]).includes(part)) {
      throw new ClientQueryError(400, 'unknown state in the state filter');
    }
    states.push(part as ClientHostState);
  }

  const whole = (name: string, fallback: number, min: number, problem: string): number => {
    const raw = params.get(name);
    if (raw === null || raw === '') return fallback;
    if (!/^\d+$/.test(raw) || Number(raw) < min) throw new ClientQueryError(400, problem);
    return Number(raw);
  };

  const override = params.get('override') ?? '';
  if (override !== '' && override !== 'image' && override !== 'flags') {
    throw new ClientQueryError(400, 'override must be image or flags');
  }

  const limit = Math.min(
    whole('limit', CLIENT_HOST_LIMIT, 1, 'limit must be a positive integer'),
    CLIENT_HOST_LIMIT_MAX
  );
  const offset = whole('offset', 0, 0, 'offset must not be negative');

  const view = index.views.find(v => v.name === client);
  const rows = index.hosts.get(client);
  if (!view || !rows) throw new ClientQueryError(404, 'unknown client');

  const matching = rows.filter(
    row =>
      (states.length === 0 || states.includes(row.state)) &&
      (needle === '' || row.host.toLowerCase().includes(needle)) &&
      (override === '' || overrideParts(row)[override])
  );

  if (params.get('names') === 'true') {
    return {
      client,
      version: view.version,
      total: rows.length,
      matched: matching.length,
      offset: 0,
      limit,
      hosts: [],
      names: matching.slice(0, CLIENT_HOST_NAMES_MAX).map(r => r.host),
    };
  }

  return {
    client,
    version: view.version,
    total: rows.length,
    matched: matching.length,
    offset,
    limit,
    hosts: matching.slice(offset, offset + limit),
  };
}
