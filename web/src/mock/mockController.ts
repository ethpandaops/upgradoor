// An in-browser stand-in for the controller API, serving the fixture through
// the same routes and JSON shapes (pkg/controller/views/schema.go). Actions
// change the in-memory state, so the UI can be exercised end to end.

import type { FetchFn } from '../api/client';
import {
  list,
  type ActionResult,
  type Audit,
  type AuditEntry,
  type Budget,
  type HostHold,
  type ApproveLeaseRequest,
  type ClientPolicyRequest,
  type Clients,
  type Container,
  type ClientOverrideRequest,
  type FlagOverrideRequest,
  type ImageOverrideRequest,
  type Config,
  type EndLeaseRequest,
  type Gate,
  type HoldRequest,
  type OfflineRequest,
  type HostDetail,
  type HostPlan,
  type HostListing,
  type HostSummary,
  type Hosts,
  type Lease,
  type Need,
  type ImageMatch,
  type Images,
  type ImagesRefresh,
  type PackageMatch,
  type Packages,
  type Plan,
  type SlotHistory,
  type SlotRow,
  type PublicHost,
  type PublicHosts,
  type PublicImage,
  type PublicImageRef,
  type PublicImages,
  type QueueEntry,
  type RebootRequest,
  type RollRequest,
  type Rollout,
  type RolloutHost,
  type RolloutHostsRequest,
  type Rollouts,
  type ScopeRequest,
  type Settings,
  type SettingsBody,
  type WaitKind,
} from '../api/types';
import { WIRE_VERSION, without } from '../api/wire';
import { applyFlags, validateFlags } from '../lib/flags';
import { formatPercent, heldBy, isHeld } from '../lib/format';
import { hostUsage } from '../lib/usage';
import { validateImageRef } from '../lib/imageRef';
import { parseGoDuration } from '../lib/goDuration';
import { rolloutActionOf } from '../lib/hostActions';
import { FEATURE, TEAM_PREFIX, TWINNED, can, type Login, type PermissionCall } from '../lib/permissions';
import { FIELDS, toForm, validateField } from '../lib/settingsForm';
import {
  EPOCH_MS,
  SECONDS_PER_SLOT,
  SLOTS_PER_EPOCH,
  buildMockData,
  currentDigest,
  hash,
  hex,
  type MockData,
  type MockFleet,
  type MockRollout,
} from './fixture';
import { ClientQueryError, clientHostsPage, clientIndex, imagePolicy, type ClientIndex } from './clients';
import { MockHostActionError, seedHostActions, startHostAction } from './hostActions';
import { OFF_SLOTS, capOutages, outageSpans, type Deviation } from './outages';
import { MockSources } from './sources';
import {
  StringTable,
  detailTables,
  diffBody,
  encodeDoc,
  fullBody,
  privateTables,
  publicTables,
  type DocState,
} from './wireStream';

/** The bearer token the mock auth client hands out to a staff login. */
const MOCK_TOKEN = 'mock-token';
/** The auth provider URL the mock reports in auth mode. */
export const MOCK_AUTH_URL = 'https://auth.mock.invalid';
/** The service name the mock reports. */
export const MOCK_AUTH_SERVICE = 'upgradoor';

/** Who the fake auth client can be logged in as. */
export type MockProfile = 'staff' | 'team' | 'reader';

interface MockLogin {
  token: string;
  /** features the token grants; "all" is a grant like upgradoor:all. */
  features: 'all' | readonly string[];
  teams: readonly string[];
}

/**
 * The mock's logins: staff may do everything, the team member has the team
 * features (a grant like upgradoor:team_*) and is in the lighthouse team, the
 * reader has basic access only.
 */
export const MOCK_LOGINS: Record<MockProfile, MockLogin> = {
  staff: { token: MOCK_TOKEN, features: 'all', teams: ['panda'] },
  team: {
    token: 'mock-token-team',
    features: [...TWINNED, FEATURE.details].map(f => TEAM_PREFIX + f),
    teams: ['lighthouse'],
  },
  reader: { token: 'mock-token-reader', features: [], teams: [] },
};

/** withoutCommands takes the command lines out of the clients the teams do not own, as the controller's reduced views do. */
function withoutCommands(clients: Clients, teams: readonly string[]): Clients {
  return {
    clients: list(clients.clients).map(c =>
      teams.includes(c.name) ? c : { ...c, images: list(c.images).map(i => ({ ...i, command: undefined })) }
    ),
  };
}

/** mockHasFeature is the feature check of a mock login. */
export function mockHasFeature(login: MockLogin, feature: string): boolean {
  return login.features === 'all' || login.features.includes(feature);
}

/** The actions the mock authorizes; any other name is no route. */
const ACTIONS: readonly string[] = [
  'roll',
  'reboot',
  'hold',
  'unhold',
  'forget',
  'end-lease',
  'ignore-gate',
  'approve-lease',
  'pause',
  'resume',
  'abort',
  'skip',
  'retry',
  'client-policy',
  'image-override',
  'flag-override',
  'client-override',
];

/** How long the mock's agent takes to recreate a container under the lease of an override. */
const OVERRIDE_APPLY_MS = 10_000;
/** The longest an override may last, and how long it lasts when the request names no end. */
const OVERRIDE_MAX_MS = 7 * 86_400_000;
const OVERRIDE_DEFAULT_MS = 86_400_000;

/** What a container ran before the mock applied an override to it: what it is put back to. */
interface OverrideBase {
  image: string;
  imageId: string;
  digest: string | undefined;
  revision: string | undefined;
  command: string[];
}

/** A client name in normal form (protocol.ValidClient): the form of a team name. */
const CLIENT_NAME = /^[a-z0-9][a-z0-9._-]{0,62}$/;

/** The controller's slot history: its default, shortest and longest window in seconds, and its points per answer. */
const SLOTS_WINDOW_DEFAULT = 3 * 3600;
const SLOTS_WINDOW_MIN = 60;
const SLOTS_WINDOW_MAX = 12 * 3600;
const SLOTS_MAX_POINTS = 400;
const SLOT_MS = SECONDS_PER_SLOT * 1000;

/** mix returns a deterministic 32-bit number for two integers: cheap enough for every host and slot of 12 hours. */
function mix(a: number, b: number): number {
  let h = (a ^ Math.imul(b, 0x9e3779b1)) >>> 0;
  h = Math.imul(h ^ (h >>> 16), 0x85ebca6b);
  h = Math.imul(h ^ (h >>> 13), 0xc2b2ae35);
  return (h ^ (h >>> 16)) >>> 0;
}

/**
 * RESUME ends the pause reasons that say why a rollout is paused: after its
 * canary, or because a person aborted a rollout of an observed cause.
 */
const RESUME = '; resume to continue';

/** waitKind classifies a waiting reason the way the scheduler does. */
function waitKind(reason: string): WaitKind {
  if (reason === 'held' || reason.startsWith('held by ')) return 'held';
  if (reason === 'agent silent' || reason.startsWith('agent protocol ')) return 'silent';
  if (reason === 'paused') return 'paused';
  if (reason === 'rollout paused' || reason.endsWith(RESUME)) return 'rollout-paused';
  if (reason.startsWith('waiting for approval')) return 'approval';
  if (reason.startsWith('rollout halted')) return 'halted';
  if (reason.startsWith('network gate')) return 'gate';
  if (reason.startsWith('verb not allowed')) return 'verb';
  if (reason === REASON_PULL) return 'pull';
  if (reason.startsWith('after ')) return 'order';
  if (reason.startsWith('validators down: ')) return 'budget';
  return 'host-limit';
}

/** The waiting reason of a host whose last lease could not pull an image (reasonPull). */
const REASON_PULL = 'waiting to pull its image again after a failed pull';

/** The stage of fleet.Stage that the mock gives every member it adds. */
const STAGE_REST = 3;

/** The failure reason of a lease that a person ended. */
const ENDED_BY_OPERATOR = 'ended by operator';

/** The mock scheduler grants the lease of a host's own request this long after the request. */
const RECONCILE_MS = 5_000;

const isOpen = (l: Lease) => !['released', 'failed', 'expired'].includes(l.state);

/** succeeded: the rollout has served a member successfully (one is done through a lease). */
function succeeded(r: MockRollout): boolean {
  return list(r.hosts).some(m => m.state === 'done' && Boolean(m.leaseId));
}

/** fits: the three grant conditions of the availability budget for a host that costs cost. */
function fits(cost: number, budget: Budget): boolean {
  return (
    cost === 0 ||
    budget.unavailableWeight + cost <= budget.allowedWeight + 1e-9 ||
    (budget.unavailableWeight === 0 && budget.allowedWeight > 0 && budget.leasedHosts === 0)
  );
}

class HttpError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

interface MockOptions {
  /** auth true makes the user routes require the mock bearer token. */
  auth?: boolean;
  /** latencyMs delays every answer; 0 answers on the next tick. */
  latencyMs?: number;
  /** now is the clock; tests pin it. */
  now?: () => number;
  /** fleet: the two dozen hosts of the fixture, or three hundred with a rolling update. */
  fleet?: MockFleet;
}

/** One open event stream of the mock: who reads it and what it was sent last. */
interface MockStream {
  user: boolean;
  /** full: the login reads the hosts in full (details); the others get no command lines of the clients. */
  full: boolean;
  hosts: string[];
  controller: ReadableStreamDefaultController<Uint8Array>;
  /** What the stream was sent last: its tables, its string table, their version, and the details of its hosts. */
  doc: DocState;
  strings: StringTable;
  version: number;
  details: Map<string, DocState | null>;
}

const PUBLIC_EVENTS_PATH = '/api/v1/public/events';
const USER_EVENTS_PATH = '/api/v1/events';
/** The windows of the history and rollout views on a stream (events.go). */
const STREAM_ROLLOUT_LIMIT = 50;

/**
 * MockController holds the fixture and answers API requests. It also serves
 * the two event streams, the way the controller does and in its encoding: a
 * snapshot, then the rows and cells that changed, whenever publish() is called
 * (after every action, and by whoever lets time pass).
 */
export class MockController {
  readonly data: MockData;
  private readonly auth: boolean;
  private readonly latencyMs: number;
  private readonly now: () => number;
  private readonly actor: () => string;
  /** authorization is what allowed the request being served; its audit entries record it. */
  private authorization: { authorizedBy: string; teams: string[] } | undefined;
  private readonly streams = new Set<MockStream>();
  /** The overrides that were set or cleared through the mock, by "host/container"; the fixture's own stay as they are. */
  private readonly overrides = new Map<string, { host: string; container: string; base?: OverrideBase }>();
  private readonly sources: MockSources;

  constructor(opts: MockOptions = {}) {
    this.now = opts.now ?? (() => Date.now());
    this.data = buildMockData(this.now(), opts.fleet ?? 'small');
    seedHostActions(this.data);
    this.auth = opts.auth ?? false;
    this.latencyMs = opts.latencyMs ?? 0;
    this.actor = () => (this.auth ? 'mock-user@example.org' : 'anonymous');
    this.sources = new MockSources(this.now, () => [...this.data.hosts.values()]);
  }

  /** gateFailing reports whether the network gate is forced to fail. */
  get gateFailing(): boolean {
    return this.data.gateFailing;
  }

  /** setGateFailing toggles a failing network gate (low participation, finality lag). */
  setGateFailing(failing: boolean): void {
    this.data.gateFailing = failing;
    this.publish();
  }

  /** fetch is a drop-in for the browser's fetch, limited to the API routes. */
  fetch: FetchFn = async (input, init) => {
    if (this.latencyMs > 0) await new Promise(resolve => setTimeout(resolve, this.latencyMs));
    if (init?.signal?.aborted) throw new DOMException('aborted', 'AbortError');

    const changes = (init?.method ?? 'GET').toUpperCase() !== 'GET';

    try {
      const path = new URL(input, 'http://mock.invalid').pathname;
      if (path === PUBLIC_EVENTS_PATH || path === USER_EVENTS_PATH) return this.openStream(input, init);

      const body = this.route(input, init);
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
    } catch (err) {
      const status =
        err instanceof HttpError || err instanceof ClientQueryError || err instanceof MockHostActionError
          ? err.status
          : 500;
      const message = err instanceof Error ? err.message : String(err);
      return new Response(JSON.stringify({ error: message }), {
        status,
        headers: { 'Content-Type': 'application/json' },
      });
    } finally {
      // What an action changed reaches every open stream.
      if (changes) this.publish();
    }
  };

  // ------------------------------------------------------- event streams

  /** openStreams is the number of event streams that are open. */
  get openStreams(): number {
    return this.streams.size;
  }

  /**
   * publish sends every open stream the views that changed since it was last
   * sent them. The mock's views move with the clock (last seen, the chain's
   * head), so letting time pass and publishing stands for the agents'
   * syncs.
   */
  publish(): void {
    for (const stream of this.streams) this.sendTo(stream, false);
  }

  /** closeStreams ends every open stream, like a controller that restarts. */
  closeStreams(): void {
    for (const stream of this.streams) stream.controller.close();
    this.streams.clear();
  }

  private openStream(input: string, init?: RequestInit): Response {
    const url = new URL(input, 'http://mock.invalid');
    const user = url.pathname === USER_EVENTS_PATH;
    const full = user && this.login(init).hasFeature(FEATURE.details);

    let stream: MockStream | undefined;
    const close = () => {
      if (!stream || !this.streams.delete(stream)) return;
      try {
        stream.controller.close();
      } catch {
        // The reader cancelled first.
      }
    };

    const body = new ReadableStream<Uint8Array>({
      start: controller => {
        stream = {
          user,
          full,
          hosts: user ? url.searchParams.getAll('host') : [],
          controller,
          doc: new Map(),
          strings: new StringTable(),
          version: 0,
          details: new Map(),
        };
        this.streams.add(stream);
        this.sendTo(stream, true);
      },
      cancel: close,
    });

    init?.signal?.addEventListener('abort', close);

    return new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
  }

  private sendTo(stream: MockStream, snapshot: boolean): void {
    const emit = (event: string, data: unknown) => {
      stream.controller.enqueue(new TextEncoder().encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`));
    };

    this.settleOverrides();

    const images = this.publicImages();
    const slots = this.slotRows();
    const tables = stream.user
      ? privateTables({
          now: this.now(),
          plan: this.plan(),
          hosts: this.hosts(),
          clients: stream.full ? this.clients() : withoutCommands(this.clients(), []),
          images,
          slots,
          rollouts: this.rollouts(STREAM_ROLLOUT_LIMIT),
          latestAuditId: this.data.audit.reduce((max, e) => Math.max(max, e.id), 0),
        })
      : publicTables({ now: this.now(), gate: this.gate(), hosts: this.publicHosts(), images, slots });

    const sent = stream.strings.list.length;
    const doc = encodeDoc(stream.user ? '' : 'public', tables, stream.strings);
    const body: Record<string, unknown> = snapshot ? fullBody(doc) : diffBody(doc, stream.doc);
    const changed = snapshot || Object.keys(body).length > 0;
    stream.doc = doc;

    const details: Record<string, unknown> = {};
    for (const name of stream.hosts) {
      const before = stream.details.get(name);
      const now = this.data.hosts.has(name) ? encodeDoc('d', detailTables(this.host(name))) : null;
      stream.details.set(name, now);

      if (now === null) {
        if (before !== null) details[name] = 0;
      } else if (!before) {
        details[name] = fullBody(now);
      } else {
        const patch = diffBody(now, before);
        if (Object.keys(patch).length > 0) details[name] = patch;
      }
    }

    if (changed) {
      stream.version += 1;
      body.i = stream.version;
      const fresh = snapshot ? stream.strings.list : stream.strings.list.slice(sent);
      if (fresh.length > 0) body.s = [...fresh];
    }
    if (Object.keys(details).length > 0) body.d = details;

    if (snapshot) emit('snap', { ...body, v: WIRE_VERSION, t: Math.floor(this.now() / 1000) });
    else if (Object.keys(body).length > 0) emit('patch', body);
  }

  // ------------------------------------------------------------ routing

  private route(input: string, init?: RequestInit): unknown {
    const url = new URL(input, 'http://mock.invalid');
    const method = (init?.method ?? 'GET').toUpperCase();
    const path = url.pathname;
    const q = url.searchParams;

    if (!path.startsWith('/api/v1/')) throw new HttpError(404, `no such route: ${path}`);
    const route = path.slice('/api/v1/'.length);

    this.settleOverrides();

    if (method === 'GET') {
      switch (route) {
        case 'config':
          return this.config();
        case 'public/slots':
          return this.slots(q.get('window'), false);
      }
    }

    // Everything below is a user route.
    this.authorization = undefined;
    const login = this.login(init);

    if (method === 'GET') {
      if (route === 'clients')
        return login.hasFeature(FEATURE.details) ? this.clients() : withoutCommands(this.clients(), login.teams);
      const clientHosts = /^clients\/([^/]+)\/hosts$/.exec(route);
      if (clientHosts) {
        const page = clientHostsPage(this.clientIndex(), decodeURIComponent(clientHosts[1] ?? ''), q);
        // The resource use of a host only for a login that reads it (Access.ClientHost).
        if (login.hasFeature(FEATURE.details) || login.teams.includes(page.client)) return page;
        return { ...page, hosts: list(page.hosts).map(h => without(h, 'usage')) };
      }
      if (route === 'packages')
        return this.packages(q.get('q') ?? '', Number(q.get('limit')) || 50, q.get('after') ?? '');
      if (route === 'images') return this.images(q.get('q') ?? '', Number(q.get('limit')) || 50, q.get('after') ?? '');
      if (route === 'settings') {
        this.authorizeAdmin(login);
        return this.settingsBody();
      }
      if (route === 'settings/sources') {
        this.authorizeAdmin(login);
        return this.sources.view();
      }
      if (route === 'slots') return this.slots(q.get('window'), true);
      if (route === 'audit') {
        return this.auditPage(
          q.get('target') ?? '',
          q.get('action') ?? '',
          Number(q.get('before')) || 0,
          Number(q.get('limit')) || 100
        );
      }
    }

    const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;

    if (method === 'PUT' && route === 'settings') {
      this.authorize(login, { action: 'settings' });
      return this.putSettings(body);
    }

    if (method === 'POST' && route === 'settings/sources/validator-ranges/refresh') {
      this.authorizeAdmin(login);
      const done = this.sources.refresh();
      if (done.fetched) this.log('sources.validator-ranges.refresh', 'validator-ranges', { changed: false });
      return done;
    }

    if (method === 'POST' && route === 'images/refresh') {
      if (!login.open && !login.hasFeature(FEATURE.manage)) {
        throw new HttpError(403, `forbidden: this login lacks the feature ${login.service}:${FEATURE.manage}`);
      }

      return this.refreshImages();
    }

    if (method === 'POST' && route === 'actions/host-action') {
      // Permitted host by host: see hostActions.ts.
      this.authorization = { authorizedBy: 'action', teams: [...login.teams] };
      return startHostAction(
        {
          data: this.data,
          now: this.now(),
          actor: this.actor(),
          newId: prefix => this.newId(prefix),
          log: (action, target, detail) => {
            this.log(action, target, detail);
          },
        },
        login,
        body
      );
    }

    if (method === 'POST' && route.startsWith('actions/')) {
      const name = route.slice('actions/'.length);
      if (ACTIONS.includes(name)) this.authorize(login, { action: name, body } as PermissionCall);

      return this.action(name, body);
    }

    throw new HttpError(404, `no such route: ${method} ${path}`);
  }

  /** login is who makes a request to a user route; without a known token it is refused. */
  private login(init?: RequestInit): Login {
    if (!this.auth) return { open: true, service: MOCK_AUTH_SERVICE, hasFeature: () => true, teams: [] };

    const header = new Headers(init?.headers).get('Authorization');
    const known = Object.values(MOCK_LOGINS).find(l => header === `Bearer ${l.token}`);
    if (!known) throw new HttpError(401, 'missing or invalid bearer token');

    return {
      open: false,
      service: MOCK_AUTH_SERVICE,
      hasFeature: feature => mockHasFeature(known, feature),
      teams: known.teams,
    };
  }

  /**
   * authorize refuses a request the login may not make, with the rule the
   * controller uses, and remembers what allowed it for the audit entries.
   */
  /** authorizeAdmin refuses a login without the feature admin, and notes what allowed the request. */
  private authorizeAdmin(login: Login): void {
    if (!login.open && !login.hasFeature(FEATURE.admin)) {
      throw new HttpError(403, `forbidden: this login lacks the feature ${login.service}:${FEATURE.admin}`);
    }
    this.authorization = { authorizedBy: login.open ? 'open' : FEATURE.admin, teams: [...login.teams] };
  }

  private authorize(login: Login, call: PermissionCall): void {
    const verdict = can(login, call, {
      hosts: [...this.data.hosts.values()],
      // The client table is only read for the policy of a client and its rollouts.
      clients: ['client-policy', 'pause', 'resume'].includes(call.action) ? this.clientIndex().views : [],
      leaseWork: id => {
        const l = this.data.leases.find(x => x.id === id);
        return l ? { host: l.host, needs: list(l.needs) } : undefined;
      },
      rolloutKind: id => this.data.rollouts.find(x => x.id === id)?.kind,
      rolloutClient: id => {
        const r = this.data.rollouts.find(x => x.id === id);
        return r ? (r.team ?? '') : undefined;
      },
      rolloutAction: id => {
        const r = this.data.rollouts.find(x => x.id === id);
        return r ? rolloutActionOf(r) : undefined;
      },
    });
    if (!verdict.allowed) throw new HttpError(403, `forbidden: ${verdict.reason}`);

    this.authorization = { authorizedBy: verdict.by, teams: [...login.teams] };
  }

  // --------------------------------------------------------- chain view

  private iso(ms: number): string {
    return new Date(ms).toISOString();
  }

  private currentEpoch(): number {
    return Math.floor((this.now() - this.data.genesis) / EPOCH_MS);
  }

  private openLease(host: string): Lease | undefined {
    return this.data.leases.find(l => l.host === host && !['released', 'failed', 'expired'].includes(l.state));
  }

  /** liveAt is the number of the host's validators that attested, `back` epochs before the newest one. */
  private liveAt(host: HostDetail, back: number, epoch: number): number {
    const weight = host.validators;
    if (weight === 0) return 0;

    if (this.data.quiet) return hash(`live/${host.name}/${epoch}`) % 11 === 0 ? weight - 1 : weight;
    if (list(host.containers).some(c => c.kind === 'el' && c.state !== 'running')) return back < 27 ? 0 : weight;
    if (host.name === 'lighthouse-geth-2' && this.openLease(host.name))
      return back === 1 || back === 2 ? 0 : weight - 2;
    if (this.data.gateFailing && back < 2 && hash(`gate/${host.name}`) % 4 === 0) return 0;

    return hash(`live/${host.name}/${epoch}`) % 11 === 0 ? weight - 1 : weight;
  }

  /**
   * darkSpans are the times a host's validators do not attest: from shortly
   * after a lease takes it down until its agent is done (or until now, while
   * the lease has not got that far), and for as long as its execution client
   * is not running.
   */
  private darkSpans(host: HostDetail): [number, number][] {
    const now = this.now();
    const spans: [number, number][] = [];
    if (this.data.quiet) return spans;

    for (const l of this.data.leases) {
      if (l.host !== host.name) continue;
      const from = Date.parse(l.grantedAt) + 2 * SLOT_MS;
      const to = Date.parse(l.agentDoneAt ?? l.endedAt ?? '');
      spans.push([from, Number.isFinite(to) ? to : now]);
    }

    if (list(host.containers).some(c => c.kind === 'el' && c.state !== 'running')) {
      spans.push([now - 27 * EPOCH_MS, now]);
    }
    if (this.data.gateFailing && hash(`gate/${host.name}`) % 4 === 0) spans.push([now - 2 * EPOCH_MS, now]);
    // Hosts that are off without anybody having touched them.
    spans.push(...(this.data.offline.get(host.name) ?? []));

    return spans;
  }

  /**
   * slots is the slot history of the last `window` seconds, made the way the
   * controller makes it: one point per slot, or per bucket of a power of two
   * slots when the window has more slots than an answer carries points. A
   * host attests with all the validators it has a duty for in a slot, except
   * while it is dark (darkSpans) and for a rare single miss; a slot now and
   * then has no block.
   */
  private slots(raw: string | null, perHost: boolean): SlotHistory {
    const seconds = raw === null ? SLOTS_WINDOW_DEFAULT : Number(raw);
    if (!Number.isInteger(seconds) || seconds < SLOTS_WINDOW_MIN) {
      throw new HttpError(400, `window must be a number of seconds, at least ${SLOTS_WINDOW_MIN}`);
    }

    const from = Math.floor((this.now() - Math.min(seconds, SLOTS_WINDOW_MAX) * 1000 - this.data.genesis) / SLOT_MS);

    const scan = this.slotsFrom(Math.max(0, from));
    if (!perHost) return scan.history;

    // The outages are worked out from the leases and the slots in which a
    // host missed, as the controller does; there is no row per host.
    const validators = (host: string) => this.data.hosts.get(host)?.validators ?? 0;
    const spans = outageSpans({
      first: scan.history.firstSlot,
      last: Math.floor((this.now() - this.data.genesis) / SLOT_MS),
      nowMs: this.now(),
      slotsPerEpoch: SLOTS_PER_EPOCH,
      slotMs: SLOT_MS,
      slotTime: slot => this.data.genesis + slot * SLOT_MS,
      deviations: scan.deviations,
      leases: this.data.leases,
      validators,
    });

    return {
      ...scan.history,
      outages: capOutages(spans, Math.min(seconds, SLOTS_WINDOW_MAX) * 1000),
      outageHosts: new Set(spans.map(o => o.names?.[0])).size,
      offSlots: OFF_SLOTS,
    };
  }

  /**
   * slotRows are the slots an event stream carries: the settled slots of the
   * current epoch and the two before it, each with the hosts that missed
   * attestations in it. That is normally none: a row is four numbers,
   * however many hosts there are.
   */
  private slotRows(): SlotRow[] {
    const last = Math.floor((this.now() - this.data.genesis) / SLOT_MS);
    const first = Math.max(0, (Math.floor(last / SLOTS_PER_EPOCH) - 2) * SLOTS_PER_EPOCH);
    const scan = this.slotsFrom(first);
    const history = scan.history;
    const hosts = new Map<number, { host: string; assigned: number; included: number }[]>();
    for (const d of scan.deviations) {
      hosts.set(d.slot, [...(hosts.get(d.slot) ?? []), { host: d.host, assigned: d.assigned, included: d.included }]);
    }
    const rows: SlotRow[] = [];

    list(history.recorded).forEach((recorded, i) => {
      if (recorded === 0) return;
      const slot = history.firstSlot + i;
      const deviating = hosts.get(slot);
      rows.push({
        slot,
        proposed: list(history.missed)[i] === 0,
        assigned: list(history.assigned)[i] ?? 0,
        included: list(history.included)[i] ?? 0,
        ...(deviating ? { hosts: deviating } : {}),
      });
    });

    return rows;
  }

  /** slotsFrom is the slot history from a slot on to now, and the slots in which a host missed attestations. */
  private slotsFrom(from: number): { history: SlotHistory; deviations: Deviation[] } {
    const now = this.now();
    const genesis = this.data.genesis;
    const last = Math.floor((now - genesis) / SLOT_MS);

    let bucket = 1;
    while ((last - from) / bucket >= SLOTS_MAX_POINTS) bucket *= 2;
    const first = from - (from % bucket);
    const points = Math.floor((last - first) / bucket) + 1;
    const zeros = () => new Array<number>(points).fill(0);

    const recorded = zeros();
    const missed = zeros();
    const assigned = zeros();
    const included = zeros();
    const worstAssigned = zeros();
    const worstIncluded = zeros();

    const hosts = [...this.data.hosts.values()]
      .filter(h => h.validators > 0)
      .sort((a, b) => a.name.localeCompare(b.name))
      .map(h => ({
        name: h.name,
        seed: hash(`slots/${h.name}`),
        duty: Math.round(h.validators / SLOTS_PER_EPOCH),
        dark: this.darkSpans(h),
      }));
    const deviations: Deviation[] = [];

    // A slot is stored once the clock is two slots past it.
    for (let slot = first; slot <= last - 2; slot++) {
      const point = Math.floor((slot - first) / bucket);
      const start = genesis + slot * SLOT_MS;
      let slotAssigned = 0;
      let slotIncluded = 0;

      for (const h of hosts) {
        // The shuffle gives a host a few more or fewer duties from slot to slot.
        const a = Math.max(0, h.duty + (mix(h.seed, slot) % 3) - 1);
        let inc = a;
        if (h.dark.some(([darkFrom, darkTo]) => start >= darkFrom && start < darkTo)) inc = 0;
        else if (a > 0 && mix(h.seed ^ 0x5bd1e995, slot) % 500 === 0) inc = a - 1;

        if (inc < a) deviations.push({ slot, host: h.name, assigned: a, included: inc });
        slotAssigned += a;
        slotIncluded += inc;
      }

      recorded[point] = (recorded[point] ?? 0) + 1;
      if (mix(0xb10c, slot) % 90 === 0) missed[point] = (missed[point] ?? 0) + 1;
      assigned[point] = (assigned[point] ?? 0) + slotAssigned;
      included[point] = (included[point] ?? 0) + slotIncluded;

      const wa = worstAssigned[point] ?? 0;
      if (slotAssigned > 0 && (wa === 0 || slotIncluded * wa < (worstIncluded[point] ?? 0) * slotAssigned)) {
        worstAssigned[point] = slotAssigned;
        worstIncluded[point] = slotIncluded;
      }
    }

    const history: SlotHistory = {
      firstSlot: first,
      firstTime: this.iso(genesis + first * SLOT_MS),
      slotMs: SLOT_MS,
      slotsPerEpoch: SLOTS_PER_EPOCH,
      bucketSlots: bucket,
      minParticipationPercent: this.data.settings.minParticipationPercent,
      epochSeconds: EPOCH_MS / 1000,
      recorded,
      missed,
      assigned,
      included,
      ...(bucket > 1 ? { worstAssigned, worstIncluded } : {}),
    };

    return { history, deviations };
  }

  private gate(): Gate {
    const now = this.now();
    const epoch = this.currentEpoch();
    const lag = this.data.gateFailing ? 5 : 2;

    let tracked = 0;
    let live = 0;
    let gateTracked = 0;
    let gateLive = 0;
    for (const host of this.data.hosts.values()) {
      const l = this.liveAt(host, 0, epoch - 1);
      tracked += host.validators;
      live += l;
      // Participation is judged among the hosts that hold no lease: a host
      // that holds one is down because of its operation.
      if (!this.openLease(host.name)) {
        gateTracked += host.validators;
        gateLive += l;
      }
    }

    const s = this.data.settings;
    const participation = gateTracked > 0 ? (gateLive / gateTracked) * 100 : 100;
    const reasons: string[] = [];
    if (lag > s.maxFinalityLagEpochs) reasons.push(`finality lag ${lag} epochs > ${s.maxFinalityLagEpochs}`);
    if (participation < s.minParticipationPercent) {
      reasons.push(`participation ${formatPercent(participation)} < ${formatPercent(s.minParticipationPercent)}`);
    }

    return {
      ok: reasons.length === 0,
      reasons,
      network: {
        updatedAt: this.iso(now - ((now / 1000) % 6) * 1000),
        genesisTime: this.iso(this.data.genesis),
        secondsPerSlot: SECONDS_PER_SLOT,
        slotsPerEpoch: SLOTS_PER_EPOCH,
        phases: [{ epoch: 0, start: this.iso(this.data.genesis), slotMs: SECONDS_PER_SLOT * 1000 }],
        headSlot: Math.floor((now - this.data.genesis) / (SECONDS_PER_SLOT * 1000)),
        currentEpoch: epoch,
        finalizedEpoch: epoch - lag,
        finalityLagEpochs: lag,
        livenessKnown: true,
        livenessEpoch: epoch - 1,
        tracked,
        live,
      },
    };
  }

  // ---------------------------------------------------------- scheduler

  private openRollouts(): MockRollout[] {
    return this.data.rollouts
      .filter(r => r.state === 'active' || r.state === 'halted')
      .sort((a, b) => a.createdAt.localeCompare(b.createdAt));
  }

  private isStale(name: string): boolean {
    const staleAfter = (parseGoDuration(this.data.settings.hostStaleAfter) ?? 120_000) / 1000;
    return this.seenAgo(name) > staleAfter;
  }

  private seenAgo(name: string): number {
    const base = this.data.seenAgo.get(name) ?? 0;
    const elapsed = Math.floor((this.now() - this.data.createdAt) / 1000);
    // Healthy agents sync every 30 s; a silent one only gets older.
    return base > 60 ? base + elapsed : (base + elapsed) % 30;
  }

  private policy(r: MockRollout): Settings['policyImage'] {
    const s = this.data.settings;
    switch (r.kind) {
      case 'group':
        // A group rollout answers to the image policy of its team, else of its kind, else to policyImage.
        return imagePolicy(s, r.team, r.containerKind)[0];
      case 'packages':
        return s.policyPackages;
      case 'reboot':
        return s.policyReboot;
      case 'manual':
      case 'local':
        // Asked for by a person, or on the host itself: no policy.
        return 'auto';
      case 'action':
        // A person's start has no policy; a due action answers to its own, manual when it has none.
        return r.key.startsWith('manual:') ? 'auto' : (s.actionPolicies[r.action ?? ''] ?? 'manual');
    }
  }

  private budget(): Budget {
    const epoch = this.currentEpoch() - 1;
    let total = 0;
    let unavailable = 0;
    let leased = 0;

    for (const host of this.data.hosts.values()) {
      total += host.validators;
      if (this.openLease(host.name)) {
        leased += 1;
        unavailable += host.validators;
      } else {
        unavailable += host.validators - this.liveAt(host, 0, epoch);
      }
    }

    const share = this.data.settings.maxUnavailablePercent / 100;
    return {
      totalWeight: total,
      allowedWeight: share * total,
      unavailableWeight: unavailable,
      leasedHosts: leased,
      maxLeasedHosts: Math.max(1, Math.floor((this.data.settings.maxLeasedPercent / 100) * this.data.hosts.size)),
      independentHosts: 0,
      maxIndependent: Math.max(1, Math.floor((this.data.settings.maxIndependentPercent / 100) * this.data.hosts.size)),
    };
  }

  /** live is the number of the host's validators that attested in the newest epoch. */
  private live(name: string): number {
    const host = this.data.hosts.get(name);
    return host ? this.liveAt(host, 0, this.currentEpoch() - 1) : 0;
  }

  /** hasVerb: the agent allows the verb of a need it reports for the rollout. */
  private hasVerb(r: MockRollout, host: HostDetail): boolean {
    const kinds = this.rolloutNeeds(r, host).map(n => n.kind);
    return kinds.some(kind =>
      list(host.verbs).includes(kind === 'image' || kind === 'override' || kind === 'spec' ? 'roll' : kind)
    );
  }

  /**
   * rolloutNeeds are the needs of a host that a rollout serves: for a group
   * rollout those of its kind and team, for another the need its member
   * names, for a manual or local one every need of the host.
   */
  private rolloutNeeds(r: MockRollout, host: HostDetail): readonly Need[] {
    const needs = list(host.needs);
    switch (r.kind) {
      case 'group':
        return needs.filter(n => {
          const of = n.image ?? n.spec;
          return (
            !n.directiveId &&
            !n.local &&
            (of?.kind ?? '') === (r.containerKind ?? '') &&
            (of?.team ?? '') === (r.team ?? '')
          );
        });
      case 'manual':
      case 'local':
        return needs;
      default: {
        const id = list(r.hosts).find(m => m.host === host.name)?.needId;
        return needs.filter(n => n.id === id || (r.kind === n.kind && !n.directiveId));
      }
    }
  }

  /** pullFailed: the last lease could not pull the image of one of the host's needs in the rollout. */
  private pullFailed(r: MockRollout, host: HostDetail): boolean {
    return this.rolloutNeeds(r, host).some(n => n.pullError);
  }

  /**
   * orderWait is the canary the fixture names, while the rollout has no
   * success yet. The mock does not choose canaries or stages: the fixture's
   * stay as they are.
   */
  private orderWait(r: MockRollout, host: string): { reason: string; after: string } | undefined {
    if (r.force || !r.canary || r.canary === host || succeeded(r)) return undefined;
    return { reason: `after canary ${r.canary}`, after: r.canary };
  }

  /**
   * waitingReason is a short form of the scheduler's waiting reasons, enough
   * for every kind the page shows. A host that waits for nothing has none.
   */
  private waitingReason(host: HostDetail, memberOf: MockRollout[], gate: Gate, budget: Budget): string | undefined {
    if (isHeld(host)) return heldBy(host) ?? 'held';
    if (host.incompatible) return host.incompatible;
    if (this.isStale(host.name)) return 'agent silent';
    if (this.data.settings.paused) return 'paused';

    const active = memberOf.filter(r => r.state === 'active');
    const usable = active.filter(r => !r.paused);
    if (active.length > 0 && usable.length === 0) return active[0]?.pauseReason ?? 'rollout paused';

    const approved = usable.filter(r => this.policy(r) !== 'manual' || r.approved);
    if (usable.length > 0 && approved.length === 0) return 'waiting for approval (policy manual)';

    // Every member rollout is halted.
    if (approved.length === 0) return `rollout halted: ${memberOf[0]?.reason ?? 'a host failed'}`;

    const allowed = approved.filter(r => this.hasVerb(r, host));
    if (allowed.length === 0) return 'verb not allowed by the agent';

    // A failed pull is tried again when the agent clears the need's error.
    const able = allowed.filter(r => !this.pullFailed(r, host));
    if (able.length === 0) return REASON_PULL;

    const ignoresBudget = able.some(r => r.ignoreBudget);
    const throughGate = !gate.ok && !able.some(r => r.ignoreBudget || r.ignoreGate);
    if (throughGate) return `network gate: ${list(gate.reasons)[0] ?? 'failing'}`;

    const waits = able.map(r => this.orderWait(r, host.name));
    if (waits[0] !== undefined && waits.every(w => w !== undefined)) return waits[0].reason;

    if (!ignoresBudget) {
      const cost = this.live(host.name);
      if (!fits(cost, budget)) {
        const pct = (w: number) => (budget.totalWeight > 0 ? (w / budget.totalWeight) * 100 : 0);
        const now = formatPercent(pct(budget.unavailableWeight));
        const withHost = formatPercent(pct(budget.unavailableWeight + cost));
        return `validators down: ${now} now, ${withHost} with this host, limit ${formatPercent(pct(budget.allowedWeight))}`;
      }
      if (budget.leasedHosts >= budget.maxLeasedHosts) {
        return `hosts at once: ${budget.leasedHosts} of ${budget.maxLeasedHosts} busy`;
      }
    }

    return undefined;
  }

  /** orderAfter names the host that a host waiting for its place in the order waits for. */
  private orderAfter(host: string, memberOf: MockRollout[]): string | undefined {
    for (const r of memberOf) {
      const wait = r.state === 'active' ? this.orderWait(r, host) : undefined;
      if (wait) return wait.after;
    }

    return undefined;
  }

  /** aboutRollout is the rollout a waiting reason is about, as the scheduler names it in the plan. */
  private aboutRollout(host: string, memberOf: MockRollout[], wait: WaitKind): string | undefined {
    const active = memberOf.filter(r => r.state === 'active');

    switch (wait) {
      case 'rollout-paused':
        return active.find(r => r.paused)?.id;
      case 'approval':
        return active.find(r => !r.paused && this.policy(r) === 'manual' && !r.approved)?.id;
      case 'halted':
        return memberOf.find(r => r.state === 'halted')?.id;
      case 'order':
        return active.find(r => this.orderWait(r, host) !== undefined)?.id;
      case 'gate':
      case 'verb':
        return active[0]?.id;
      default:
        return undefined;
    }
  }

  private plan(): Plan {
    const gate = this.gate();
    const budget = this.budget();
    const open = this.openRollouts();
    const epoch = this.currentEpoch() - 1;

    const hosts: Record<string, HostPlan> = {};
    const queue: QueueEntry[] = [];

    // Queue order: oldest rollout first, inside a rollout by the fixture's
    // stage, then ascending weight and name.
    const weight = (m: RolloutHost) => this.data.hosts.get(m.host)?.validators ?? 0;
    const order: string[] = [];
    for (const r of open) {
      const members = [...list(r.hosts)].sort(
        (a, b) => a.stage - b.stage || weight(a) - weight(b) || a.host.localeCompare(b.host)
      );
      for (const m of members) {
        if (m.state === 'pending' && !order.includes(m.host)) order.push(m.host);
      }
    }

    for (const host of this.data.hosts.values()) {
      const lease = this.openLease(host.name);
      const memberOf = open.filter(r => list(r.hosts).some(m => m.host === host.name && m.state === 'pending'));
      const failed = open.some(r => list(r.hosts).some(m => m.host === host.name && m.state === 'failed'));
      const held = isHeld(host);

      const plan: HostPlan = {
        slot: 'idle',
        weight: host.validators,
        live: this.liveAt(host, 0, epoch),
        liveKnown: host.validators > 0,
      };

      if (lease) {
        plan.slot = lease.state === 'verifying' ? 'verifying' : 'leased';
        plan.leaseId = lease.id;
      } else if (host.offline) {
        plan.slot = 'offline';
      } else if (held) {
        plan.slot = 'held';
      } else if (host.incompatible || this.isStale(host.name)) {
        plan.slot = 'stale';
      } else if (failed) {
        plan.slot = 'failed';
      } else if (memberOf.length > 0) {
        plan.slot = 'queued';
      }

      if (!lease && memberOf.length > 0) {
        plan.reason = this.waitingReason(host, memberOf, gate, budget);
      }

      hosts[host.name] = plan;
    }

    // The queue is the hosts that wait, with what they wait for.
    for (const name of order) {
      const plan = hosts[name];
      if (!plan || plan.leaseId || plan.reason === undefined) continue;
      const memberOf = open.filter(r => list(r.hosts).some(m => m.host === name && m.state === 'pending'));
      const wait = waitKind(plan.reason);
      queue.push({
        host: name,
        position: queue.length + 1,
        reason: plan.reason,
        wait,
        after: wait === 'order' ? this.orderAfter(name, memberOf) : undefined,
        rollout: this.aboutRollout(name, memberOf, wait),
        rollouts: memberOf.map(r => r.id),
      });
    }

    return { computedAt: this.iso(this.now() - 1000), paused: this.data.settings.paused, gate, budget, queue, hosts };
  }

  // -------------------------------------------------------------- views

  private config(): Config {
    return {
      network: this.data.network,
      version: this.data.version,
      authProviderUrl: this.auth ? MOCK_AUTH_URL : '',
      authService: MOCK_AUTH_SERVICE,
    };
  }

  private imageRef(host: HostDetail, kind: 'el' | 'cl'): PublicImageRef | undefined {
    const c = list(host.containers).find(x => x.managed && x.kind === kind);
    if (!c) return undefined;
    return { image: c.image, digest: c.digest, revision: c.revision };
  }

  private publicHosts(): PublicHosts {
    const hosts: PublicHost[] = [...this.data.hosts.values()].map(h => ({
      name: h.name,
      el: this.imageRef(h, 'el'),
      cl: this.imageRef(h, 'cl'),
      updatePending: list(h.needs).length > 0,
    }));
    return { hosts };
  }

  private publicImages(): PublicImages {
    const byKey = new Map<string, PublicImage>();

    for (const host of this.data.hosts.values()) {
      for (const c of list(host.containers)) {
        if (!c.managed || (c.kind !== 'el' && c.kind !== 'cl')) continue;

        const key = `${c.kind}/${c.image}`;
        const target = c.registry?.digest ?? c.digest;
        const entry = byKey.get(key) ?? {
          image: c.image,
          kind: c.kind,
          digest: target,
          revision: c.registry?.revision,
          hostsOn: [],
          hostsBehind: [],
        };

        (c.digest === target ? entry.hostsOn : entry.hostsBehind)?.push(host.name);
        byKey.set(key, entry);
      }
    }

    const images = [...byKey.values()].sort((a, b) => a.kind.localeCompare(b.kind) || a.image.localeCompare(b.image));
    return { images };
  }

  /**
   * summary is a host as the host table has it. Its containers come without
   * what only the details and the client views carry (the command line, an
   * override and what may be overridden), unless whole is set.
   */
  private summary(host: HostDetail, plan: Plan, whole = false): HostSummary {
    return {
      name: host.name,
      roles: host.roles,
      validators: host.validators,
      lastSeen: this.iso(this.now() - this.seenAgo(host.name) * 1000),
      stale: this.isStale(host.name),
      agentVersion: host.agentVersion,
      agentProtocol: host.agentProtocol,
      ...(host.incompatible ? { incompatible: host.incompatible } : {}),
      containers: whole
        ? host.containers
        : list(host.containers).map(c => without(c, 'command', 'override', 'overridable')),
      node: host.node,
      os: host.os,
      pending: host.pending,
      uptimeSeconds: host.uptimeSeconds + Math.floor((this.now() - this.data.createdAt) / 1000),
      resources: host.resources,
      hold: host.hold,
      holders: list(host.hold.holds).map(h => h.holder),
      offline: host.offline,
      broken: host.broken && Object.keys(host.broken).length > 0 ? host.broken : undefined,
      verbs: host.verbs,
      actions: host.actions,
      needs: host.needs,
      plan: plan.hosts?.[host.name] ?? host.plan,
    };
  }

  private hosts(): Hosts {
    const plan = this.plan();
    return { hosts: [...this.data.hosts.values()].map(h => hostListing(this.summary(h, plan))) };
  }

  /** clientIndex is every client with its hosts, from the hosts as the plan has them now. */
  private clientIndex(): ClientIndex {
    const plan = this.plan();
    return clientIndex(
      [...this.data.hosts.values()].map(h => this.summary(h, plan, true)),
      this.data.settings
    );
  }

  private clients(): Clients {
    return { clients: this.clientIndex().views };
  }

  private host(name: string): HostDetail {
    const host = this.data.hosts.get(name);
    if (!host) throw new HttpError(404, `unknown host ${name}`);

    return {
      ...host,
      ...this.summary(host, this.plan(), true),
      leases: this.data.leases.filter(l => l.host === name).sort((a, b) => b.grantedAt.localeCompare(a.grantedAt)),
      history: this.data.audit.filter(e => e.target === name).sort((a, b) => b.id - a.id),
    };
  }

  /**
   * packages is one page of the fleet's packages, as the controller pages
   * them: every name in alphabetical order, or the names that contain the
   * query (the exact match, then those that start with it, then the rest).
   * A page continues after the name the cursor holds.
   */
  private packages(query: string, limit: number, after: string): Packages {
    const needle = query.trim().toLowerCase();
    const byName = new Map<string, Map<string, string[]>>();

    for (const host of this.data.hosts.values()) {
      for (const p of list(host.inventory?.packages)) {
        if (!p.name.toLowerCase().includes(needle)) continue;
        const versions = byName.get(p.name) ?? new Map<string, string[]>();
        versions.set(p.version, [...(versions.get(p.version) ?? []), host.name]);
        byName.set(p.name, versions);
      }
    }

    const rank = (name: string) => (needle === '' ? 0 : name === needle ? 0 : name.startsWith(needle) ? 1 : 2);
    const cursor = (name: string) => `${rank(name)}${name}`;
    if (after !== '' && !/^[012]./.test(after)) throw new HttpError(400, 'after is not the next of a page');

    const names = [...byName.keys()].sort((a, b) => rank(a) - rank(b) || (a < b ? -1 : 1));
    const rest = names.filter(name => after === '' || cursor(name) > after);
    const page = rest.slice(0, limit);

    const matches: PackageMatch[] = page.map(name => ({
      name,
      versions: [...(byName.get(name) ?? new Map<string, string[]>()).entries()]
        .sort(([a], [b]) => b.localeCompare(a, undefined, { numeric: true }))
        .map(([version, hosts]) => ({ version, hosts: hosts.sort() })),
    }));

    return {
      query: query.trim(),
      matches,
      ...(after === '' ? { total: names.length } : {}),
      ...(rest.length > limit ? { next: cursor(page[page.length - 1] ?? '') } : {}),
    };
  }

  /** images is one page of the images the fleet's containers run, paged like packages. */
  private images(query: string, limit: number, after: string): Images {
    const needle = query.trim().toLowerCase();
    const byName = new Map<string, Map<string, { revision?: string; hosts: Set<string> }>>();
    const lookups = new Map<string, { digest?: string; checkedAt: string; error?: string }>();

    for (const host of this.data.hosts.values()) {
      for (const c of list(host.containers)) {
        if (!c.image || !c.image.toLowerCase().includes(needle)) continue;
        const versions = byName.get(c.image) ?? new Map<string, { revision?: string; hosts: Set<string> }>();
        const known = lookups.get(c.image);
        if (c.registry && (!known || c.registry.checkedAt > known.checkedAt)) {
          const { digest, checkedAt, error } = c.registry;
          lookups.set(c.image, { checkedAt, ...(digest ? { digest } : {}), ...(error ? { error } : {}) });
        }

        const version = versions.get(c.digest ?? '') ?? { hosts: new Set<string>() };
        version.revision ??= c.revision;
        version.hosts.add(host.name);
        versions.set(c.digest ?? '', version);
        byName.set(c.image, versions);
      }
    }

    const rank = (name: string) => {
      const lower = name.toLowerCase();
      return needle === '' || lower === needle ? 0 : lower.startsWith(needle) ? 1 : 2;
    };
    const cursor = (name: string) => `${rank(name)}${name}`;
    if (after !== '' && !/^[012]./.test(after)) throw new HttpError(400, 'after is not the next of a page');

    const names = [...byName.keys()].sort((a, b) => (cursor(a) < cursor(b) ? -1 : 1));
    const rest = names.filter(name => cursor(name) > after);
    const page = rest.slice(0, limit);

    const matches: ImageMatch[] = page.map(name => ({
      name,
      ...(lookups.has(name) ? { registry: lookups.get(name) } : {}),
      versions: [...(byName.get(name) ?? new Map<string, { revision?: string; hosts: Set<string> }>()).entries()]
        .map(([digest, v]) => ({
          digest,
          ...(v.revision ? { revision: v.revision } : {}),
          hosts: [...v.hosts].sort(),
        }))
        .sort((a, b) => b.hosts.length - a.hosts.length || (a.digest < b.digest ? -1 : 1)),
    }));

    return {
      query: query.trim(),
      matches,
      ...(after === '' ? { total: names.length } : {}),
      ...(rest.length > limit ? { next: cursor(page[page.length - 1] ?? '') } : {}),
    };
  }

  /**
   * refreshImages marks every followed tag that was not looked up within the
   * last half minute as looked up now, and says how many that are and when
   * the others may be looked up again, like the controller.
   */
  private refreshImages(): ImagesRefresh {
    const now = this.now();
    const at = new Date(now).toISOString();
    const floor = 30_000;
    const tags = new Set<string>();
    const recent = new Set<string>();
    let next = Infinity;

    for (const host of this.data.hosts.values()) {
      for (const c of list(host.containers)) {
        if (!c.registry) continue;
        const checked = Date.parse(c.registry.checkedAt);
        if (now - checked < floor && !tags.has(c.image)) {
          recent.add(c.image);
          next = Math.min(next, checked + floor);
          continue;
        }
        c.registry.checkedAt = at;
        tags.add(c.image);
      }
    }

    if (recent.size === 0) return { tags: tags.size, message: `${tags.size} tag(s) are looked up now` };

    return {
      tags: tags.size,
      recent: recent.size,
      next: new Date(next).toISOString(),
      message: `${tags.size} tag(s) are looked up now; the next lookup on request is possible later`,
    };
  }

  private rollouts(limit: number): Rollouts {
    const terminal = (l: Lease) => ['released', 'failed', 'expired'].includes(l.state);
    const newest = <T extends { createdAt?: string; grantedAt?: string }>(a: T, b: T) =>
      (b.createdAt ?? b.grantedAt ?? '').localeCompare(a.createdAt ?? a.grantedAt ?? '');

    // A rollout is served with the policy it answers to (views.RolloutView).
    const view = (r: MockRollout): Rollout => ({
      ...r,
      policy: this.policy(r),
      ...(r.kind === 'group' ? { policySource: imagePolicy(this.data.settings, r.team, r.containerKind)[1] } : {}),
    });

    return {
      open: this.openRollouts().map(view),
      recent: this.data.rollouts
        .filter(r => r.state !== 'active' && r.state !== 'halted')
        .sort((a, b) => (b.endedAt ?? '').localeCompare(a.endedAt ?? ''))
        .slice(0, limit)
        .map(view),
      leases: this.data.leases.filter(l => !terminal(l)).sort(newest),
      recentLeases: this.data.leases
        .filter(terminal)
        .sort((a, b) => (b.endedAt ?? '').localeCompare(a.endedAt ?? ''))
        .slice(0, limit),
    };
  }

  private settingsBody(): SettingsBody {
    return { settings: { ...this.data.settings }, defaults: { ...this.data.defaults }, kinds: this.kinds() };
  }

  /** kinds are the kinds of the managed containers of the fleet (views.FleetKinds). */
  private kinds(): string[] {
    const kinds = new Set<string>();
    for (const host of this.data.hosts.values()) {
      for (const c of list(host.containers)) if (c.managed && c.kind) kinds.add(c.kind);
    }
    const rank = (k: string) => {
      const i = ['el', 'cl', 'vc'].indexOf(k);
      return i < 0 ? 3 : i;
    };
    return [...kinds].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
  }

  /**
   * putSettings answers like the controller: the keys the body names are
   * replaced and the others keep their value, so a page that does not know a
   * setting cannot reset it. 400 for an unknown key, for null, for a value of
   * the wrong type and for a result out of range.
   */
  private putSettings(body: unknown): SettingsBody {
    if (typeof body !== 'object' || body === null || Array.isArray(body)) {
      throw new HttpError(400, 'settings must be a JSON object');
    }

    const specs = new Map<string, (typeof FIELDS)[number]>(FIELDS.map(f => [f.key, f]));

    for (const [key, value] of Object.entries(body as Record<string, unknown>)) {
      // The image policies per client are an object of client to policy, replaced as a whole.
      if (key === 'policyClients') {
        const ok =
          typeof value === 'object' &&
          value !== null &&
          !Array.isArray(value) &&
          Object.entries(value).every(([client, p]) => CLIENT_NAME.test(client) && (p === 'auto' || p === 'manual'));
        if (!ok) throw new HttpError(400, 'policyClients must be an object of client to policy');
        continue;
      }
      // The image policies per container kind, replaced as a whole.
      if (key === 'policyKinds') {
        const ok =
          typeof value === 'object' &&
          value !== null &&
          !Array.isArray(value) &&
          Object.entries(value).every(
            ([kind, p]) => CLIENT_NAME.test(kind) && kind !== 'none' && (p === 'auto' || p === 'manual')
          );
        if (!ok) throw new HttpError(400, 'policyKinds must be an object of kind to policy');
        continue;
      }
      // The policies per host action, replaced as a whole.
      if (key === 'actionPolicies') {
        const ok =
          typeof value === 'object' &&
          value !== null &&
          !Array.isArray(value) &&
          Object.values(value).every(p => p === 'auto' || p === 'manual');
        if (!ok) throw new HttpError(400, 'actionPolicies must be an object of action to policy');
        continue;
      }

      const spec = specs.get(key);
      if (!spec) throw new HttpError(400, `unknown field "${key}"`);
      if (value === null) throw new HttpError(400, `${key} must not be null`);

      const wanted =
        spec.kind === 'switch'
          ? 'boolean'
          : spec.kind === 'percent' || spec.kind === 'share' || spec.kind === 'count'
            ? 'number'
            : 'string';
      if (typeof value !== wanted) throw new HttpError(400, `${key} must be a ${wanted}`);
    }

    const next = { ...this.data.settings, ...(body as Partial<Settings>) };
    const form = toForm(next);
    for (const spec of FIELDS) {
      const problem = validateField(spec, form[spec.key]);
      if (problem) throw new HttpError(400, `${spec.key} ${problem}`);
    }

    // A body that changes nothing is no change and no entry in the audit log.
    const before = { ...this.data.settings };
    if (JSON.stringify(before) === JSON.stringify(next)) return this.settingsBody();

    this.data.settings = next;
    this.log('settings.update', 'settings', { before, after: { ...next } });

    return this.settingsBody();
  }

  private auditPage(target: string, actionPrefix: string, before: number, limit: number): Audit {
    const matching = this.data.audit
      .filter(e => (target === '' || e.target === target) && (actionPrefix === '' || e.action.startsWith(actionPrefix)))
      .filter(e => before === 0 || e.id < before)
      .sort((a, b) => b.id - a.id);

    const entries = matching.slice(0, limit);
    const last = entries[entries.length - 1];

    return { entries, nextBefore: matching.length > limit && last ? last.id : 0 };
  }

  // ------------------------------------------------------------ actions

  private log(action: string, target: string, detail?: Record<string, unknown>, actor: string = this.actor()): void {
    const id = this.data.audit.reduce((max, e) => Math.max(max, e.id), 0) + 1;
    // What a person did is recorded with what allowed it.
    if (this.authorization && actor === this.actor()) detail = { ...detail, ...this.authorization };
    const entry: AuditEntry = { id, time: this.iso(this.now()), actor, action, target, detail };
    this.data.audit.push(entry);
  }

  private newId(prefix: string): string {
    this.data.seq += 1;
    return `${prefix}-${hex(`${prefix}/${this.data.seq}`, 16)}`;
  }

  private rollout(id: string): MockRollout {
    const r = this.data.rollouts.find(x => x.id === id);
    if (!r) throw new HttpError(404, `unknown rollout ${id}`);
    return r;
  }

  private selectHosts(req: { hosts: string[]; all: boolean }): HostDetail[] {
    if (req.all) return [...this.data.hosts.values()];
    if (list(req.hosts).length === 0) throw new HttpError(400, 'no hosts selected');
    return list(req.hosts).map(name => {
      const host = this.data.hosts.get(name);
      if (!host) throw new HttpError(404, `unknown host ${name}`);
      return host;
    });
  }

  private action(name: string, body: unknown): ActionResult {
    switch (name) {
      case 'roll':
        return this.manual('roll', body as RollRequest);
      case 'reboot':
        return this.manual('reboot', { ...(body as RebootRequest), containers: [] });
      case 'hold':
      case 'unhold':
        return this.hold(name, body as HoldRequest);
      case 'mark-offline':
      case 'unmark-offline':
        return this.markOffline(name === 'mark-offline', body as OfflineRequest);
      case 'forget':
        return this.forget(body as HoldRequest);
      case 'end-lease':
        return this.endLease(body as EndLeaseRequest);
      case 'ignore-gate':
        return this.ignoreGate(body as ScopeRequest);
      case 'approve-lease':
        return this.approveLease(body as ApproveLeaseRequest);
      case 'pause':
      case 'resume':
        return this.pause(name, body as ScopeRequest);
      case 'abort':
        return this.abort(body as ScopeRequest);
      case 'skip':
        return this.skip(body as RolloutHostsRequest);
      case 'retry':
        return this.retry(body as RolloutHostsRequest);
      case 'client-policy':
        return this.clientPolicy(body as ClientPolicyRequest);
      case 'image-override':
        return this.imageOverride(body as ImageOverrideRequest);
      case 'flag-override':
        return this.flagOverride(body as FlagOverrideRequest);
      case 'client-override':
        return this.clientOverride(body as ClientOverrideRequest);
      default:
        throw new HttpError(404, `unknown action ${name}`);
    }
  }

  /**
   * clientPolicy sets or clears the image policy of one client. Open image
   * rollouts of the client answer to it from then on; one that was approved
   * stays approved.
   */
  private clientPolicy(req: ClientPolicyRequest): ActionResult {
    if (!CLIENT_NAME.test(req.client)) throw new HttpError(400, `invalid client name "${req.client}"`);
    // The body is whatever was sent.
    const policy: string = req.policy;
    if (!['', 'auto', 'manual'].includes(policy)) {
      throw new HttpError(400, 'policy must be auto, manual or empty (the default)');
    }

    const before = this.data.settings;
    const policyClients = { ...before.policyClients };
    if (req.policy === '') Reflect.deleteProperty(policyClients, req.client);
    else policyClients[req.client] = req.policy;

    const next = { ...before, policyClients };
    const message =
      req.policy === ''
        ? `image policy of ${req.client}: the default of each kind`
        : `image policy of ${req.client}: ${req.policy}`;

    // A request that changes nothing is no change and no entry in the audit log.
    if (JSON.stringify(before.policyClients) === JSON.stringify(policyClients)) return { message };

    this.data.settings = next;
    this.log('action.client-policy', 'settings', {
      before: { ...before },
      after: { ...next },
      ...(req.reason ? { reason: req.reason } : {}),
    });

    return { message };
  }

  // ---------------------------------------------------------- overrides
  //
  // An override is the host's: the controller passes the instruction on and
  // shows what the agent reports. The mock plays the agent too. An override
  // that is set stands in the container's report as not applied, with a need
  // of the host and a rollout of the host's own; the mock grants that
  // rollout a lease when the plan lets the host go and has the container
  // recreated a little later. An override that is cleared, or whose end
  // passed, is put back the same way.

  /** overrideTarget finds the container an override is for, or refuses like the controller does. */
  private overrideTarget(req: { host: string; container: string }, what: 'image' | 'flag'): Container {
    const host = this.data.hosts.get(req.host);
    if (!host) throw new HttpError(404, `unknown host ${req.host}`);
    if (this.isStale(host.name)) throw new HttpError(409, `${host.name}: agent silent`);

    const c = list(host.containers).find(x => x.name === req.container);
    if (!c?.managed) throw new HttpError(409, `${host.name} does not manage a container ${req.container}`);

    if (c.overridable !== true) throw new HttpError(409, `${host.name} does not allow ${what} overrides of ${c.name}`);

    return c;
  }

  /** overrideEnd is when an override ends: what the request says, at most seven days from now, a day by default. */
  private overrideEnd(until: string | undefined): string {
    if (until === undefined || until === '') return this.iso(this.now() + OVERRIDE_DEFAULT_MS);

    const at = Date.parse(until);
    if (Number.isNaN(at)) throw new HttpError(400, 'until must be a time (RFC 3339)');
    if (at <= this.now()) throw new HttpError(400, 'until must be in the future');
    if (at > this.now() + OVERRIDE_MAX_MS) throw new HttpError(400, 'until must be at most 7 days from now');

    return this.iso(at);
  }

  /**
   * clientOverride is the override of each named host's container of the
   * client and kind: the single-host override per host, with the hosts that
   * cannot take it listed instead of failing the request.
   */
  private clientOverride(req: ClientOverrideRequest): ActionResult {
    const hosts = list(req.hosts);
    if (hosts.length === 0) throw new HttpError(400, 'hosts must not be empty');
    if (!['image', 'flags'].includes(req.part)) throw new HttpError(400, 'part must be image or flags');

    const sent: string[] = [];
    const rejected: Record<string, string> = {};

    for (const name of hosts) {
      const container = list(this.data.hosts.get(name)?.containers).find(
        c => c.managed && c.team === req.client && c.kind === req.kind
      );
      if (!this.data.hosts.has(name)) {
        rejected[name] = 'unknown host';
        continue;
      }
      if (!container) {
        rejected[name] = `host ${name} runs no ${req.kind} container of ${req.client}`;
        continue;
      }

      try {
        const target = { host: name, container: container.name, until: req.until, reason: req.reason };
        if (req.part === 'image') this.imageOverride({ ...target, image: req.image ?? '' });
        else
          this.flagOverride({ ...target, addFlags: [...list(req.addFlags)], removeFlags: [...list(req.removeFlags)] });
        sent.push(name);
      } catch (err) {
        if (!(err instanceof HttpError) || err.status === 400) throw err;
        rejected[name] = err.message;
      }
    }

    return {
      message: `sent to ${sent.length} of ${hosts.length} hosts: each agent decides, and its container follows under the host's next lease`,
      hosts: sent,
      ...(Object.keys(rejected).length > 0 ? { rejected } : {}),
    };
  }

  private imageOverride(req: ImageOverrideRequest): ActionResult {
    const c = this.overrideTarget(req, 'image');

    if (req.image !== '') {
      const problem = validateImageRef(req.image);
      if (problem) throw new HttpError(400, `image ${problem}`);
    }

    const until = req.image === '' ? undefined : this.overrideEnd(req.until);
    this.setOverride(req.host, c, { image: req.image === '' ? undefined : req.image }, until, req.reason);
    this.log('action.image-override', req.host, {
      container: c.name,
      image: req.image,
      ...(until ? { until } : {}),
      ...(req.reason ? { reason: req.reason } : {}),
    });

    return { message: `sent to ${req.host}; the agent applies it under its next lease`, hosts: [req.host] };
  }

  private flagOverride(req: FlagOverrideRequest): ActionResult {
    const c = this.overrideTarget(req, 'flag');
    const add = list(req.addFlags);
    const remove = list(req.removeFlags);
    const clears = add.length + remove.length === 0;

    const problem = validateFlags(add, remove);
    if (problem) throw new HttpError(400, problem);

    const until = clears ? undefined : this.overrideEnd(req.until);
    this.setOverride(
      req.host,
      c,
      { addFlags: add.length > 0 ? [...add] : undefined, removeFlags: remove.length > 0 ? [...remove] : undefined },
      until,
      req.reason
    );
    this.log('action.flag-override', req.host, {
      container: c.name,
      addFlags: add,
      removeFlags: remove,
      ...(until ? { until } : {}),
      ...(req.reason ? { reason: req.reason } : {}),
    });

    return { message: `sent to ${req.host}; the agent applies it under its next lease`, hosts: [req.host] };
  }

  /**
   * setOverride writes one part of a container's override (the image, or the
   * flags) the way the agent would into its file, and asks for the lease
   * that brings the container in line. A part that is set to nothing is
   * cleared; an entry without a part is gone.
   */
  private setOverride(
    host: string,
    c: Container,
    part:
      | Pick<NonNullable<Container['override']>, 'image'>
      | Pick<NonNullable<Container['override']>, 'addFlags' | 'removeFlags'>,
    until: string | undefined,
    reason: string
  ): void {
    const key = `${host}/${c.name}`;
    if (!this.overrides.has(key)) this.overrides.set(key, { host, container: c.name });

    const before = c.override?.restoring ? undefined : c.override;
    const next = { ...before, ...part };
    const empty = !next.image && list(next.addFlags).length + list(next.removeFlags).length === 0;

    if (empty) {
      this.dropOverride(host, c);
      return;
    }

    c.override = {
      ...(next.image ? { image: next.image } : {}),
      ...(next.addFlags ? { addFlags: next.addFlags } : {}),
      ...(next.removeFlags ? { removeFlags: next.removeFlags } : {}),
      by: this.actor(),
      ...(reason ? { reason } : {}),
      // One end for the image and the flags together: what was set last says it.
      until: until ?? before?.until,
      applied: false,
    };
    this.askLocal(host, c);
  }

  /** dropOverride takes the entry away: at once when the container never ran it, otherwise it is put back under a lease. */
  private dropOverride(host: string, c: Container): void {
    const o = c.override;
    if (!o) return;

    const ran = o.applied || o.restoring === true || this.overrides.get(`${host}/${c.name}`)?.base !== undefined;
    if (ran) {
      c.override = { ...o, applied: true, restoring: true, error: undefined };
      this.askLocal(host, c);
      return;
    }

    c.override = undefined;
    this.overrides.delete(`${host}/${c.name}`);
    this.closeLocal(host, c.name, 'the host no longer asks for it');
    const h = this.data.hosts.get(host);
    if (h) h.needs = list(h.needs).filter(n => n.override?.container !== c.name);
  }

  /** closeLocal supersedes the open rollout of a host's own request for a container, unless its lease runs. */
  private closeLocal(host: string, container: string, reason: string): void {
    const now = this.iso(this.now());
    for (const r of this.openRollouts()) {
      if (r.kind !== 'local' || !r.key.startsWith(`local:${host}:${container}:`)) continue;
      if (list(r.hosts).some(m => m.state === 'leased')) continue;
      Object.assign(r, { state: 'superseded', reason, updatedAt: now, endedAt: now });
    }
  }

  /** askLocal reports the need that brings a container in line with its override, in a rollout of the host's own. */
  private askLocal(host: string, c: Container): void {
    const h = this.data.hosts.get(host);
    const o = c.override;
    if (!h || !o) return;

    const restore = o.restoring === true;
    const base = this.overrides.get(`${host}/${c.name}`)?.base;
    const image = restore ? (base?.image ?? c.configuredImage ?? c.image) : (o.image ?? base?.image ?? c.image);
    const flags = list(o.addFlags).length + list(o.removeFlags).length > 0;
    const target = hex(JSON.stringify([image, restore, o.addFlags, o.removeFlags]), 12);
    const need: Need = {
      id: `override/${c.name}/${target}`,
      kind: 'override',
      local: true,
      override: {
        container: c.name,
        image,
        ...(c.image !== image ? { fromImage: c.image } : {}),
        fromDigest: c.digest,
        toDigest: currentDigest(image),
        ...(flags ? { flags: true } : {}),
        ...(restore ? { restore: true } : {}),
      },
    };

    this.closeLocal(host, c.name, 'the host asks for something else now');
    h.needs = [...list(h.needs).filter(n => n.override?.container !== c.name), need];

    const now = this.iso(this.now());
    this.data.rollouts.push({
      id: this.newId('ro'),
      kind: 'local',
      key: `local:${host}:${c.name}:${target}`,
      title: `${host}: ${restore ? 'restore' : 'override'} ${c.name}`,
      state: 'active',
      paused: false,
      approved: true,
      // One host: there is no order, and no canary.
      force: true,
      ignoreBudget: false,
      ignoreGate: false,
      createdBy: 'scheduler',
      createdAt: now,
      updatedAt: now,
      hosts: [{ host, state: 'pending', stage: STAGE_REST, needId: need.id, updatedAt: now }],
    });
  }

  /**
   * settleOverrides lets time pass for the overrides the mock was asked for:
   * one whose end passed is taken away; the rollout of a host's request gets
   * its lease once the plan lets the host go; and a lease that has run long
   * enough has recreated the container.
   */
  private settleOverrides(): void {
    if (this.overrides.size === 0) return;
    const now = this.now();

    for (const { host, container } of [...this.overrides.values()]) {
      const c = list(this.data.hosts.get(host)?.containers).find(x => x.name === container);
      if (!c) {
        this.overrides.delete(`${host}/${container}`);
        continue;
      }
      const until = c.override?.until;
      if (c.override && !c.override.restoring && until && Date.parse(until) <= now) this.dropOverride(host, c);
    }

    const waiting = this.openRollouts().filter(r => r.kind === 'local' && r.state === 'active');
    if (waiting.length === 0) return;

    let plan: Plan | undefined;

    for (const r of waiting) {
      const m = list(r.hosts)[0];
      const host = m ? this.data.hosts.get(m.host) : undefined;
      if (!m || !host) continue;
      const need = list(host.needs).find(n => n.id === m.needId);

      if (m.state === 'pending' && need && now - Date.parse(r.createdAt) >= RECONCILE_MS) {
        plan ??= this.plan();
        const slot = plan.hosts?.[host.name];
        // The plan has no reason for the host to wait: it is granted.
        if (slot?.slot !== 'queued' || slot.reason !== undefined) continue;

        const id = this.newId('ls');
        const lease: Lease = {
          id,
          host: host.name,
          state: 'running',
          // The agent pulls first; nothing is stopped yet.
          operation: {
            leaseId: id,
            phase: 'pulling',
            startedAt: this.iso(now),
            updatedAt: this.iso(now),
            steps: [{ needId: need.id, status: 'pending' }],
            disrupted: false,
          },
          needs: [need],
          rollouts: { [need.id]: r.id },
          weight: host.validators,
          charged: this.live(host.name),
          force: true,
          grantedBy: 'scheduler',
          grantedAt: this.iso(now),
          expiresAt: this.iso(now + (parseGoDuration(this.data.settings.leaseTtl) ?? 1_800_000)),
        };
        this.data.leases.push(lease);
        Object.assign(m, { state: 'leased', leaseId: lease.id, updatedAt: this.iso(now) });
        r.updatedAt = this.iso(now);
        this.log('lease.granted', host.name, { lease: lease.id, rollout: r.id }, 'scheduler');
        // One grant a pass: the next one sees the budget with this lease in it.
        plan = undefined;
        continue;
      }

      const lease = this.data.leases.find(l => l.id === m.leaseId);
      if (m.state !== 'leased' || !lease || !isOpen(lease) || lease.cancelled) continue;
      if (now - Date.parse(lease.grantedAt) < OVERRIDE_APPLY_MS) continue;

      const container = need?.override?.container ?? '';
      const c = list(host.containers).find(x => x.name === container);
      if (c) this.recreate(host.name, c);

      const at = this.iso(now);
      Object.assign(lease, {
        state: 'released',
        agentDoneAt: at,
        endedAt: at,
        readiness: 'ready: node at head with peers',
        operation: {
          leaseId: lease.id,
          phase: 'done',
          startedAt: lease.grantedAt,
          updatedAt: at,
          steps: [{ needId: m.needId ?? '', status: 'done' }],
          disrupted: true,
        },
      });
      Object.assign(m, { state: 'done', updatedAt: at });
      Object.assign(r, { state: 'completed', updatedAt: at, endedAt: at });
      host.needs = list(host.needs).filter(n => n.id !== m.needId);
      this.log('lease.released', host.name, { lease: lease.id, readiness: lease.readiness }, 'scheduler');
    }
  }

  /** recreate is the agent's step under the lease: the container runs what its override says, or what it ran before. */
  private recreate(host: string, c: Container): void {
    const key = `${host}/${c.name}`;
    const entry = this.overrides.get(key);
    const o = c.override;
    if (!entry || !o) return;

    // Without a record of what the container ran before, that is what its override changed, taken back.
    const base: OverrideBase = entry.base ?? {
      image: o.applied && o.image ? (c.configuredImage ?? c.image) : c.image,
      imageId: c.imageId,
      digest: o.applied && o.image ? c.registry?.digest : c.digest,
      revision: o.applied && o.image ? c.registry?.revision : c.revision,
      command: o.applied ? list(c.command).filter(a => !list(o.addFlags).includes(a)) : [...list(c.command)],
    };

    const image = o.restoring ? base.image : (o.image ?? base.image);
    const other = image !== base.image;

    Object.assign(c, {
      image,
      imageId: other ? `sha256:${hex(`imageid/${image}`)}` : base.imageId,
      digest: other ? currentDigest(image) : base.digest,
      revision: other ? hex(`revision/${image}`, 40) : base.revision,
      command: o.restoring ? base.command : applyFlags(base.command, list(o.addFlags), list(o.removeFlags)),
      startedAt: this.iso(this.now()),
    });

    if (o.restoring) {
      c.override = undefined;
      this.overrides.delete(key);
    } else {
      c.override = { ...o, applied: true };
      entry.base = base;
    }
  }

  private manual(verb: 'roll' | 'reboot', req: RollRequest): ActionResult {
    const selected = this.selectHosts(req);
    const rejected: Record<string, string> = {};
    const members: RolloutHost[] = [];
    const rolled: Need[] = [];
    const id = this.newId('ro');
    const now = this.iso(this.now());

    for (const host of selected) {
      if (!list(host.verbs).includes(verb)) {
        rejected[host.name] = `the agent does not allow ${verb}`;
        continue;
      }
      if (isHeld(host)) {
        rejected[host.name] = 'the host is on hold';
        continue;
      }

      const directive = this.newId('dir');
      const needs: Need[] = [];

      if (verb === 'reboot') {
        needs.push({
          id: `manual/${directive}`,
          kind: 'reboot',
          directiveId: directive,
          reboot: { reasons: ['manual'] },
        });
      } else {
        const wanted = list(req.containers);
        const targets = list(host.containers).filter(
          c => c.managed && (wanted.length === 0 || wanted.includes(c.name))
        );
        if (targets.length === 0) {
          rejected[host.name] = 'no managed container matches';
          continue;
        }
        for (const c of targets) {
          const to = c.registry?.digest ?? c.digest ?? '';
          needs.push({
            id: `manual/${directive}/${c.name}`,
            kind: 'image',
            directiveId: directive,
            image: {
              container: c.name,
              kind: c.kind,
              team: c.team,
              image: c.image,
              fromDigest: c.digest,
              toDigest: to,
            },
          });
        }
      }

      host.needs = [...list(host.needs), ...needs];
      rolled.push(...needs.filter(n => n.image));
      members.push({ host: host.name, state: 'pending', stage: STAGE_REST, needId: needs[0]?.id, updatedAt: now });
    }

    if (members.length === 0) {
      return { message: `Nothing to ${verb}: every selected host was rejected.`, rejected };
    }

    // A roll of one team's containers is that team's (and of their kind when they share one).
    const teams = [...new Set(rolled.map(n => n.image?.team ?? ''))];
    const kinds = [...new Set(rolled.map(n => n.image?.kind ?? ''))];
    const team = teams.length === 1 && teams[0] ? teams[0] : undefined;

    const what = verb === 'roll' ? 'roll' : 'reboot';
    const title = `Manual ${what} of ${req.all ? 'the fleet' : members.length === 1 ? (members[0]?.host ?? '') : `${members.length} hosts`}`;

    this.data.rollouts.push({
      id,
      kind: 'manual',
      key: `manual:${id}`,
      title,
      ...(team ? { team, ...(kinds.length === 1 && kinds[0] ? { containerKind: kinds[0] } : {}) } : {}),
      state: 'active',
      paused: false,
      approved: true,
      force: req.force || !req.staggered,
      ignoreBudget: req.ignoreBudget,
      ignoreGate: req.ignoreGate === true,
      createdBy: this.actor(),
      createdAt: now,
      updatedAt: now,
      hosts: members,
    });

    this.log(`action.${verb}`, id, {
      hosts: req.all ? 'all' : members.map(m => m.host),
      containers: verb === 'roll' ? list(req.containers) : undefined,
      staggered: req.staggered,
      force: req.force,
      ignoreBudget: req.ignoreBudget,
      ignoreGate: req.ignoreGate === true,
      reason: req.reason,
    });

    return {
      message: `${title} queued (${req.staggered ? 'staggered' : 'now'}${req.ignoreBudget ? ', ignoring the budget' : ''}).`,
      rollout: id,
      hosts: members.map(m => m.host),
      rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
    };
  }

  private hold(action: 'hold' | 'unhold', req: HoldRequest): ActionResult {
    const hosts = this.selectHosts({ hosts: req.hosts, all: false });
    const rejected: Record<string, string> = {};
    const applied: string[] = [];

    for (const host of hosts) {
      // Through team_manage the holds of the login's teams that own a container of the host; otherwise staff's, or
      // for an unhold the holders named.
      const viaTeam = this.authorization?.authorizedBy.startsWith(TEAM_PREFIX) === true;
      const login = this.authorization?.teams ?? [];
      const owning = login.filter(t => list(host.containers).some(c => c.managed && c.team === t)).sort();
      const named = list(req.holders);
      const holders = viaTeam ? (named.length > 0 ? named : owning) : named.length > 0 ? named : ['staff'];

      const before = list(host.hold.holds);
      const kept = before.filter(h => !holders.includes(h.holder));
      // Lifting a hold that is not there changes nothing and is not audited.
      if (action === 'unhold' && kept.length === before.length) {
        rejected[host.name] = 'the host has no such hold';
        continue;
      }
      const at = new Date(this.now()).toISOString();
      const by = this.actor();
      // The mock agent answers the directive at once.
      const holds =
        action === 'hold'
          ? [...kept, ...holders.map(holder => ({ holder, by, at, ...(req.reason ? { reason: req.reason } : {}) }))]
          : kept;
      host.hold = { ...host.hold, holds: holds.slice().sort(holdOrder) };
      host.holders = host.hold.holds?.map(h => h.holder) ?? [];
      applied.push(host.name);
      this.log(`action.${action}`, host.name, { reason: req.reason, holders });
    }

    return {
      message: `${action === 'hold' ? 'Hold set on' : 'Hold lifted on'} ${applied.length} of ${hosts.length} hosts.`,
      hosts: applied,
      rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
    };
  }

  private markOffline(mark: boolean, req: OfflineRequest): ActionResult {
    const hosts = this.selectHosts({ hosts: req.hosts, all: false });
    const applied: string[] = [];

    for (const host of hosts) {
      if (!mark && !host.offline) continue;
      host.offline = mark ? { reason: req.reason, ...(req.until ? { until: req.until } : {}) } : undefined;
      applied.push(host.name);
      this.log(mark ? 'action.mark-offline' : 'action.unmark-offline', host.name, { reason: req.reason });
    }

    return {
      message: mark
        ? `${applied.length} host(s) marked known offline`
        : `offline mark lifted on ${applied.length} host(s)`,
      hosts: applied,
    };
  }

  private forget(req: HoldRequest): ActionResult {
    const hosts = this.selectHosts({ hosts: req.hosts, all: false });
    const rejected: Record<string, string> = {};
    const applied: string[] = [];

    for (const host of hosts) {
      if (this.openLease(host.name)) {
        rejected[host.name] = 'the host holds a lease';
        continue;
      }
      // The host's mark goes with it; its members in open rollouts are left out.
      this.data.hosts.delete(host.name);
      for (const r of this.openRollouts()) {
        for (const m of list(r.hosts)) {
          if (m.host !== host.name || m.state === 'done' || (m.state === 'skipped' && !m.skippedBy)) continue;
          m.state = 'skipped';
          m.skippedBy = undefined;
          m.error = 'host forgotten';
          m.updatedAt = this.iso(this.now());
        }
      }
      applied.push(host.name);
      this.log('action.forget', host.name, { reason: req.reason });
    }

    return {
      message: `Forgot ${applied.length} of ${hosts.length} hosts.`,
      hosts: applied,
      rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
    };
  }

  /** endLease ends one open lease at once, as cancelled: the mock plays no agent that reports first. */
  private endLease(req: EndLeaseRequest): ActionResult {
    if (!req.lease) throw new HttpError(400, 'scheduler: invalid action: no lease named');

    const l = this.data.leases.find(x => x.id === req.lease);
    if (!l) throw new HttpError(404, `scheduler: unknown lease: ${req.lease}`);
    if (!isOpen(l)) throw new HttpError(409, `scheduler: invalid action: lease ${l.id} is ${l.state}`);

    this.log('action.end-lease', l.host, { lease: l.id, state: l.state, reason: req.reason });
    this.cancelLease(l, this.actor());
    return { message: `lease ${l.id} of ${l.host} is ending`, hosts: [l.host] };
  }

  /** ignoreGate lets one open rollout pass a closed network gate. */
  private ignoreGate(req: ScopeRequest): ActionResult {
    if (req.rollout === '') throw new HttpError(400, 'rollout must not be empty');
    const r = this.openRollout(req.rollout);
    if (!r.ignoreGate && !r.ignoreBudget) {
      r.ignoreGate = true;
      r.updatedAt = this.iso(this.now());
      this.log('action.ignore-gate', r.id, {
        before: { ignoreGate: false },
        after: { ignoreGate: true },
        reason: req.reason,
      });
    }
    return { message: 'rollout passes the network gate', rollout: r.id };
  }

  /** approveLease releases the verifying lease of one member at once, as the next reconcile would. */
  private approveLease(req: ApproveLeaseRequest): ActionResult {
    const r = this.rollout(req.rollout);
    const m = list(r.hosts).find(x => x.host === req.host);
    if (!m) throw new HttpError(404, `scheduler: unknown host: ${req.host} is no host of rollout ${r.id}`);
    const l = this.data.leases.find(x => x.id === m.leaseId);
    if (m.state !== 'leased' || !l) {
      throw new HttpError(409, `scheduler: invalid action: ${req.host} holds no lease in rollout ${r.id}`);
    }
    if (l.state !== 'verifying') {
      throw new HttpError(409, `scheduler: invalid action: lease ${l.id} is ${l.state}, not verifying`);
    }

    const at = this.iso(this.now());
    const by = this.actor();
    this.log('action.approve-lease', l.host, { lease: l.id, readiness: l.readiness, reason: req.reason });
    Object.assign(l, { state: 'released', endedAt: at, approved: { at, by, reason: req.reason } });
    for (const { member } of this.leaseMembers(l)) {
      Object.assign(member, { state: 'done', updatedAt: at });
    }
    this.log('lease.released', l.host, { lease: l.id, readiness: l.readiness, approvedBy: by }, 'scheduler');
    return { message: `lease ${l.id} of ${l.host} approved`, rollout: r.id, hosts: [l.host] };
  }

  /** leaseMembers is the host's member, leased by the lease, in every open rollout the lease served. */
  private leaseMembers(l: Lease): { rollout: MockRollout; member: RolloutHost }[] {
    const out: { rollout: MockRollout; member: RolloutHost }[] = [];
    for (const rollout of this.openRollouts()) {
      const member = list(rollout.hosts).find(x => x.host === l.host && x.state === 'leased' && x.leaseId === l.id);
      if (member) out.push({ rollout, member });
    }
    return out;
  }

  /** cancelLease ends a lease a person ended: its members wait as cancelled, its active rollouts pause. */
  private cancelLease(l: Lease, by: string): void {
    const now = this.iso(this.now());
    const members = this.leaseMembers(l);
    l.state = 'cancelled';
    l.endedAt = now;
    l.cancelled = { at: now, by, state: l.state };

    const paused: string[] = [];

    for (const { rollout: r, member: m } of members) {
      m.updatedAt = now;
      m.state = 'cancelled';
      m.error = ENDED_BY_OPERATOR;

      if (r.state === 'active' && !r.paused) {
        r.paused = true;
        r.pauseReason = `${l.host} cancelled by ${by}`;
        r.updatedAt = now;
        paused.push(r.id);
        this.log('rollout.paused', r.id, { host: l.host, lease: l.id, reason: r.pauseReason }, 'scheduler');
      }
    }

    this.log('lease.cancelled', l.host, { lease: l.id, by, rollouts: paused }, 'scheduler');
  }

  private pause(action: 'pause' | 'resume', req: ScopeRequest): ActionResult {
    if (req.rollout === '') {
      const before = { paused: this.data.settings.paused };
      this.data.settings = { ...this.data.settings, paused: action === 'pause' };
      this.log(`action.${action}`, 'settings', { before, after: { paused: action === 'pause' }, reason: req.reason });
      return { message: action === 'pause' ? 'Everything is paused.' : 'The global pause is lifted.' };
    }

    const r = this.openRollout(req.rollout);

    r.paused = action === 'pause';
    // A resume lifts every pause, whoever set it, and takes its reason along.
    if (action === 'resume') {
      r.approved = true;
      r.pauseReason = undefined;
    }
    r.updatedAt = this.iso(this.now());
    this.log(`action.${action}`, r.id, { reason: req.reason });

    return {
      message: action === 'pause' ? `Rollout paused: ${r.title}` : `Rollout resumed and approved: ${r.title}`,
      rollout: r.id,
    };
  }

  /** openRollout is a rollout that an action may still change: a closed one is refused. */
  private openRollout(id: string): MockRollout {
    const r = this.rollout(id);
    if (r.state !== 'active' && r.state !== 'halted') {
      throw new HttpError(409, `scheduler: invalid action: rollout ${r.id} is ${r.state}`);
    }
    return r;
  }

  /**
   * abort closes a rollout for good; its waiting members are aborted. The
   * reboot and local rollouts, whose key does not change with their target,
   * are paused instead, with a reason that says so; a resume lifts that.
   */
  private abort(req: ScopeRequest): ActionResult {
    if (req.rollout === '') throw new HttpError(400, 'scheduler: invalid action: abort needs a rollout');

    const r = this.openRollout(req.rollout);
    const now = this.iso(this.now());
    r.updatedAt = now;

    if (r.kind === 'reboot' || r.kind === 'local') {
      const before = { paused: r.paused };
      r.paused = true;
      r.pauseReason = `aborted${req.reason ? `: ${req.reason}` : ''}${RESUME}`;
      this.log('action.abort', r.id, { before, after: { paused: true }, reason: req.reason });

      return {
        message: 'rollout paused: its cause is still reported, so it stays open; resume to continue',
        rollout: r.id,
      };
    }

    const before = { state: r.state };
    r.state = 'aborted';
    r.reason = req.reason || 'aborted';
    r.endedAt = now;
    for (const m of list(r.hosts)) {
      if (!['done', 'skipped', 'leased', 'failed'].includes(m.state)) m.state = 'aborted';
    }
    this.log('action.abort', r.id, { before, after: { state: r.state }, reason: req.reason });

    const message =
      r.kind === 'manual'
        ? 'rollout aborted'
        : 'rollout aborted: its target is not rolled again until it changes or someone rolls it by hand';
    return { message, rollout: r.id };
  }

  /**
   * skip leaves hosts out of an open rollout: the member records who skipped
   * it and the reason given. A host that holds a lease for the rollout, is
   * done or is skipped already cannot be skipped.
   */
  private skip(req: RolloutHostsRequest): ActionResult {
    const r = this.openRollout(req.rollout);
    const wanted = list(req.hosts);
    if (wanted.length === 0) throw new HttpError(400, 'scheduler: invalid action: no hosts named');

    const now = this.iso(this.now());
    const rejected: Record<string, string> = {};
    const applied: string[] = [];

    for (const name of wanted) {
      const m = list(r.hosts).find(x => x.host === name);

      if (!m) rejected[name] = 'not a member of the rollout';
      else if (m.state === 'leased') rejected[name] = 'holds a lease for the rollout';
      else if (m.state === 'done' || m.state === 'skipped') rejected[name] = `already ${m.state}`;
      else {
        m.state = 'skipped';
        m.skippedBy = this.actor();
        m.error = req.reason || undefined;
        m.updatedAt = now;
        applied.push(name);
      }
    }

    if (applied.length === 0) throw new HttpError(409, 'scheduler: invalid action: no host could be skipped');

    r.updatedAt = now;
    this.log('action.skip', r.id, { hosts: applied, reason: req.reason });

    return {
      message: `${applied.length} host(s) skipped`,
      rollout: r.id,
      hosts: applied,
      rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
    };
  }

  /**
   * retry returns hosts of an open rollout to pending. Without hosts it is
   * about the rollout: every failed member, and a halted rollout is active
   * again. With hosts it is about those: the named members that failed or
   * that a person skipped; the rollout leaves its halt only when one of them
   * had failed and no member is failed after that.
   */
  private retry(req: RolloutHostsRequest): ActionResult {
    const r = this.openRollout(req.rollout);
    const wanted = list(req.hosts);
    const now = this.iso(this.now());
    const rejected: Record<string, string> = {};
    const applied: string[] = [];
    let retriedFailed = false;

    for (const m of list(r.hosts)) {
      const named = wanted.includes(m.host);
      const failed = (m.state === 'failed' || m.state === 'cancelled') && (wanted.length === 0 || named);
      const skipped = m.state === 'skipped' && Boolean(m.skippedBy) && named;
      if (!failed && !skipped) continue;

      retriedFailed ||= failed;
      m.state = 'pending';
      m.error = undefined;
      m.skippedBy = undefined;
      m.leaseId = undefined;
      m.updatedAt = now;
      applied.push(m.host);
    }

    for (const name of wanted) {
      if (!applied.includes(name))
        rejected[name] = 'has neither failed, been cancelled nor been skipped in the rollout';
    }

    const before = { state: r.state, reason: r.reason ?? '' };
    let message: string;

    if (wanted.length > 0) {
      if (applied.length === 0) {
        const why = wanted.map(name => `${name} ${rejected[name] ?? ''}`).join('; ');
        throw new HttpError(409, `scheduler: invalid action: no host could be retried: ${why}`);
      }

      const stillFailed = list(r.hosts)
        .filter(m => m.state === 'failed')
        .map(m => m.host);
      message = `${applied.length} host(s) retried`;

      if (r.state !== 'halted') {
        // Nothing to lift.
      } else if (stillFailed.length > 0) {
        message += `; the rollout stays halted: ${stillFailed.join(', ')} still failed`;
      } else if (!retriedFailed) {
        message +=
          '; the rollout stays halted: none of the retried hosts had failed (retry the rollout to lift its halt)';
      } else {
        r.state = 'active';
        r.reason = undefined;
      }
    } else {
      if (applied.length === 0 && r.state !== 'halted') {
        throw new HttpError(
          409,
          `scheduler: invalid action: rollout ${r.id} is ${r.state} and has no failed host to retry`
        );
      }

      r.state = 'active';
      r.reason = undefined;
      message = `rollout reopened, ${applied.length} host(s) retried`;
    }

    r.updatedAt = now;
    this.log('action.retry', r.id, { hosts: applied, before, after: { state: r.state }, reason: req.reason });

    return {
      message,
      rollout: r.id,
      hosts: applied,
      rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
    };
  }
}

/** hostListing is a host's row of the host list (api.hostListing). */
function hostListing(s: HostSummary): HostListing {
  return {
    name: s.name,
    roles: s.roles,
    validators: s.validators,
    lastSeen: s.lastSeen,
    stale: s.stale,
    agentVersion: s.agentVersion,
    agentProtocol: s.agentProtocol,
    ...(s.incompatible ? { incompatible: s.incompatible } : {}),
    containers: list(s.containers).map(c => ({
      name: c.name,
      image: c.image,
      ...(c.digest ? { digest: c.digest } : {}),
      ...(c.revision ? { revision: c.revision } : {}),
      state: c.state,
      managed: c.managed,
      ...(c.kind ? { kind: c.kind } : {}),
      ...(c.impact ? { impact: c.impact } : {}),
      ...(c.team ? { team: c.team } : {}),
    })),
    os: s.os,
    pending: s.pending,
    uptimeSeconds: s.uptimeSeconds,
    ...(s.resources ? { usage: hostUsage(s.resources) } : {}),
    hold: s.hold,
    holders: [...list(s.holders)],
    verbs: s.verbs,
    actions: s.actions,
    needs: list(s.needs).map(n => ({ kind: n.kind })),
    plan: s.plan,
  };
}

/** holdOrder sorts holds as the agent reports them: the host's own, staff's, then the teams by name. */
function holdOrder(a: HostHold, b: HostHold): number {
  const rank = (holder: string) => (holder === 'host' ? 0 : holder === 'staff' ? 1 : 2);
  return rank(a.holder) - rank(b.holder) || a.holder.localeCompare(b.holder);
}
