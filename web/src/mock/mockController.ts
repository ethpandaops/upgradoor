// An in-browser stand-in for the controller API, serving the fixture through
// the same routes and JSON shapes (pkg/controller/api/schema.go). Actions
// change the in-memory state, so the UI can be exercised end to end.

import type { FetchFn } from '../api/client';
import {
  list,
  type ActionResult,
  type Audit,
  type AuditEntry,
  type Budget,
  type Config,
  type Gate,
  type HoldRequest,
  type HostDetail,
  type HostPlan,
  type HostSummary,
  type Hosts,
  type Lease,
  type Liveness,
  type Need,
  type PackageMatch,
  type Packages,
  type Plan,
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
} from '../api/types';
import { formatPercent } from '../lib/format';
import { parseGoDuration } from '../lib/goDuration';
import { FIELDS, toForm, validateField } from '../lib/settingsForm';
import { EPOCH_MS, SECONDS_PER_SLOT, SLOTS_PER_EPOCH, buildMockData, hash, hex, type MockData } from './fixture';

/** The bearer token the mock auth client hands out. */
export const MOCK_TOKEN = 'mock-token';
/** The auth provider URL the mock reports in auth mode. */
export const MOCK_AUTH_URL = 'https://auth.mock.invalid';

const LIVENESS_EPOCHS = 16;

class HttpError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export interface MockOptions {
  /** auth true makes the user routes require the mock bearer token. */
  auth?: boolean;
  /** latencyMs delays every answer; 0 answers on the next tick. */
  latencyMs?: number;
  /** now is the clock; tests pin it. */
  now?: () => number;
  /** actor is recorded in the audit log for actions. */
  actor?: () => string;
}

/** MockController holds the fixture and answers API requests. */
export class MockController {
  readonly data: MockData;
  private readonly auth: boolean;
  private readonly latencyMs: number;
  private readonly now: () => number;
  private readonly actor: () => string;

  constructor(opts: MockOptions = {}) {
    this.now = opts.now ?? (() => Date.now());
    this.data = buildMockData(this.now());
    this.auth = opts.auth ?? false;
    this.latencyMs = opts.latencyMs ?? 0;
    this.actor = opts.actor ?? (() => (this.auth ? 'mock-user@example.org' : 'anonymous'));
  }

  /** gateFailing reports whether the network gate is forced to fail. */
  get gateFailing(): boolean {
    return this.data.gateFailing;
  }

  /** setGateFailing toggles a failing network gate (low participation, finality lag). */
  setGateFailing(failing: boolean): void {
    this.data.gateFailing = failing;
  }

  /** fetch is a drop-in for the browser's fetch, limited to the API routes. */
  fetch: FetchFn = async (input, init) => {
    if (this.latencyMs > 0) await new Promise(resolve => setTimeout(resolve, this.latencyMs));
    if (init?.signal?.aborted) throw new DOMException('aborted', 'AbortError');

    try {
      const body = this.route(input, init);
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } });
    } catch (err) {
      const status = err instanceof HttpError ? err.status : 500;
      const message = err instanceof Error ? err.message : String(err);
      return new Response(JSON.stringify({ error: message }), {
        status,
        headers: { 'Content-Type': 'application/json' },
      });
    }
  };

  // ------------------------------------------------------------ routing

  private route(input: string, init?: RequestInit): unknown {
    const url = new URL(input, 'http://mock.invalid');
    const method = (init?.method ?? 'GET').toUpperCase();
    const path = url.pathname;
    const q = url.searchParams;

    if (!path.startsWith('/api/v1/')) throw new HttpError(404, `no such route: ${path}`);
    const route = path.slice('/api/v1/'.length);

    if (method === 'GET') {
      switch (route) {
        case 'config':
          return this.config();
        case 'public/hosts':
          return this.publicHosts();
        case 'public/images':
          return this.publicImages();
        case 'public/network':
          return this.gate();
      }
    }

    // Everything below is a user route.
    if (this.auth) {
      const header = new Headers(init?.headers).get('Authorization');
      if (header !== `Bearer ${MOCK_TOKEN}`) throw new HttpError(401, 'missing or invalid bearer token');
    }

    if (method === 'GET') {
      if (route === 'hosts') return this.hosts();
      if (route.startsWith('hosts/')) return this.host(decodeURIComponent(route.slice('hosts/'.length)));
      if (route === 'packages') return this.packages(q.get('q') ?? '', Number(q.get('limit')) || 50);
      if (route === 'rollouts') return this.rollouts(Number(q.get('limit')) || 20);
      if (route === 'plan') return this.plan();
      if (route === 'settings') return this.settingsBody();
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

    if (method === 'PUT' && route === 'settings') return this.putSettings(body as Settings);

    if (method === 'POST' && route.startsWith('actions/')) {
      const result = this.action(route.slice('actions/'.length), body);
      return result;
    }

    throw new HttpError(404, `no such route: ${method} ${path}`);
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

    if (list(host.containers).some(c => c.kind === 'el' && c.state !== 'running')) return back < 14 ? 0 : weight;
    if (host.name === 'lighthouse-geth-2' && this.openLease(host.name))
      return back === 1 || back === 2 ? 0 : weight - 2;
    if (this.data.gateFailing && back < 2 && hash(`gate/${host.name}`) % 4 === 0) return 0;

    return hash(`live/${host.name}/${epoch}`) % 11 === 0 ? weight - 1 : weight;
  }

  private liveness(host: HostDetail): Liveness[] {
    if (host.validators === 0) return [];
    const newest = this.currentEpoch() - 1;
    return Array.from({ length: LIVENESS_EPOCHS }, (_, back) => ({
      host: host.name,
      epoch: newest - back,
      live: this.liveAt(host, back, newest - back),
      total: host.validators,
    }));
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
      // Hosts holding a lease are left out of the gate's share.
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

  private openRollouts(): Rollout[] {
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

  private policy(r: Rollout): Settings['policyImage'] {
    const s = this.data.settings;
    switch (r.kind) {
      case 'image':
        return s.policyImage;
      case 'packages':
        return s.policyPackages;
      case 'reboot':
        return s.policyReboot;
      case 'manual':
        return 'auto';
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
      maxLeasedHosts: Math.max(1, Math.floor(share * this.data.hosts.size)),
    };
  }

  /** waitingReason follows the order of docs/scheduler.md, "Waiting reasons". */
  private waitingReason(host: HostDetail, memberOf: Rollout[], gate: Gate, budget: Budget): string {
    if (host.holdWanted || host.hold.local || host.hold.controller) return 'held';
    if (this.isStale(host.name)) return 'agent silent';
    if (this.data.settings.paused) return 'paused';

    const active = memberOf.filter(r => r.state === 'active');
    const usable = active.filter(r => !r.paused);
    if (active.length > 0 && usable.length === 0) return 'rollout paused';

    const approved = usable.filter(r => this.policy(r) !== 'manual' || r.approved);
    if (usable.length > 0 && approved.length === 0) return 'waiting for approval (policy manual)';

    const allowed = approved.filter(r => this.policy(r) !== 'paused');
    if (approved.length > 0 && allowed.length === 0) return 'policy paused';

    if (allowed.length === 0) {
      const halted = memberOf.find(r => r.state === 'halted');
      return halted ? `rollout halted: ${halted.reason ?? 'a host failed'}` : 'next';
    }

    const ignoresBudget = allowed.some(r => r.ignoreBudget);
    if (!gate.ok && !ignoresBudget) return `network gate: ${list(gate.reasons)[0] ?? 'failing'}`;

    for (const r of allowed) {
      if (r.force) continue;
      const me = list(r.hosts).find(h => h.host === host.name);
      const blocker = list(r.hosts).find(
        h => me && h.stage < me.stage && h.state !== 'done' && h.state !== 'skipped' && h.state !== 'expected'
      );
      if (blocker) return `after ${blocker.host}`;
    }

    if (!ignoresBudget) {
      const cost = this.liveAt(host, 0, this.currentEpoch() - 1);
      if (cost > 0 && budget.unavailableWeight + cost > budget.allowedWeight && budget.unavailableWeight > 0) {
        const pct = (w: number) => formatPercent(budget.totalWeight > 0 ? (w / budget.totalWeight) * 100 : 0);
        return `budget ${pct(budget.unavailableWeight)} of ${pct(budget.allowedWeight)}`;
      }
      if (budget.leasedHosts >= budget.maxLeasedHosts) {
        return `host limit ${budget.leasedHosts} of ${budget.maxLeasedHosts}`;
      }
    }

    return 'next';
  }

  private plan(): Plan {
    const gate = this.gate();
    const budget = this.budget();
    const open = this.openRollouts();
    const epoch = this.currentEpoch() - 1;

    const hosts: Record<string, HostPlan> = {};
    const queue: QueueEntry[] = [];

    // Queue order: oldest rollout first, members in stage order.
    const order: string[] = [];
    for (const r of open) {
      for (const m of [...list(r.hosts)].sort((a, b) => a.stage - b.stage)) {
        if (m.state === 'pending' && !order.includes(m.host)) order.push(m.host);
      }
    }

    for (const host of this.data.hosts.values()) {
      const lease = this.openLease(host.name);
      const memberOf = open.filter(r => list(r.hosts).some(m => m.host === host.name && m.state === 'pending'));
      const failed = open.some(r => list(r.hosts).some(m => m.host === host.name && m.state === 'failed'));
      const held = host.holdWanted || host.hold.local || host.hold.controller;

      const plan: HostPlan = {
        slot: 'idle',
        weight: host.validators,
        live: this.liveAt(host, 0, epoch),
        liveKnown: host.validators > 0,
      };

      if (lease) {
        plan.slot = lease.state === 'verifying' ? 'verifying' : 'leased';
        plan.leaseId = lease.id;
      } else if (held) {
        plan.slot = 'held';
      } else if (this.isStale(host.name)) {
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

    for (const name of order) {
      const host = this.data.hosts.get(name);
      const plan = hosts[name];
      if (!host || !plan || plan.leaseId) continue;
      queue.push({
        host: name,
        position: queue.length + 1,
        reason: plan.reason ?? 'next',
        rollouts: open.filter(r => list(r.hosts).some(m => m.host === name && m.state === 'pending')).map(r => r.id),
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
    };
  }

  private imageRef(host: HostDetail, kind: 'el' | 'cl'): PublicImageRef | undefined {
    const c = list(host.containers).find(x => x.managed && x.kind === kind);
    if (!c) return undefined;
    return { image: c.image, digest: c.digest, revision: c.revision, custom: c.custom };
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
        if (!c.managed || c.custom || (c.kind !== 'el' && c.kind !== 'cl')) continue;

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

  private summary(host: HostDetail, plan: Plan): HostSummary {
    return {
      name: host.name,
      roles: host.roles,
      validators: host.validators,
      lastSeen: this.iso(this.now() - this.seenAgo(host.name) * 1000),
      stale: this.isStale(host.name),
      agentVersion: host.agentVersion,
      containers: host.containers,
      node: host.node,
      os: host.os,
      pending: host.pending,
      uptimeSeconds: host.uptimeSeconds + Math.floor((this.now() - this.data.createdAt) / 1000),
      hold: host.hold,
      holdWanted: host.holdWanted,
      verbs: host.verbs,
      needs: host.needs,
      plan: plan.hosts?.[host.name] ?? host.plan,
    };
  }

  private hosts(): Hosts {
    const plan = this.plan();
    return { hosts: [...this.data.hosts.values()].map(h => this.summary(h, plan)) };
  }

  private host(name: string): HostDetail {
    const host = this.data.hosts.get(name);
    if (!host) throw new HttpError(404, `unknown host ${name}`);

    return {
      ...host,
      ...this.summary(host, this.plan()),
      liveness: this.liveness(host),
      leases: this.data.leases.filter(l => l.host === name).sort((a, b) => b.grantedAt.localeCompare(a.grantedAt)),
      history: this.data.audit.filter(e => e.target === name).sort((a, b) => b.id - a.id),
    };
  }

  private packages(query: string, limit: number): Packages {
    const needle = query.trim().toLowerCase();
    const byName = new Map<string, Map<string, string[]>>();

    if (needle !== '') {
      for (const host of this.data.hosts.values()) {
        for (const p of list(host.inventory?.packages)) {
          if (!p.name.toLowerCase().includes(needle)) continue;
          const versions = byName.get(p.name) ?? new Map<string, string[]>();
          versions.set(p.version, [...(versions.get(p.version) ?? []), host.name]);
          byName.set(p.name, versions);
        }
      }
    }

    const matches: PackageMatch[] = [...byName.entries()]
      .sort(([a], [b]) => Number(b === needle) - Number(a === needle) || a.localeCompare(b))
      .slice(0, limit)
      .map(([name, versions]) => ({
        name,
        versions: [...versions.entries()]
          .sort(([a], [b]) => b.localeCompare(a, undefined, { numeric: true }))
          .map(([version, hosts]) => ({ version, hosts: hosts.sort() })),
      }));

    return { query, matches };
  }

  private rollouts(limit: number): Rollouts {
    const terminal = (l: Lease) => ['released', 'failed', 'expired'].includes(l.state);
    const newest = <T extends { createdAt?: string; grantedAt?: string }>(a: T, b: T) =>
      (b.createdAt ?? b.grantedAt ?? '').localeCompare(a.createdAt ?? a.grantedAt ?? '');

    return {
      open: this.openRollouts(),
      recent: this.data.rollouts
        .filter(r => r.state !== 'active' && r.state !== 'halted')
        .sort((a, b) => (b.endedAt ?? '').localeCompare(a.endedAt ?? ''))
        .slice(0, limit),
      leases: this.data.leases.filter(l => !terminal(l)).sort(newest),
      recentLeases: this.data.leases
        .filter(terminal)
        .sort((a, b) => (b.endedAt ?? '').localeCompare(a.endedAt ?? ''))
        .slice(0, limit),
    };
  }

  private settingsBody(): SettingsBody {
    return { settings: { ...this.data.settings }, defaults: { ...this.data.defaults } };
  }

  private putSettings(next: Settings): SettingsBody {
    const form = toForm(next);
    for (const spec of FIELDS) {
      const problem = validateField(spec, form[spec.key]);
      if (problem) throw new HttpError(400, `${spec.key} ${problem}`);
    }

    const before = { ...this.data.settings };
    this.data.settings = { ...next };
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
    const entry: AuditEntry = { id, time: this.iso(this.now()), actor, action, target, detail };
    this.data.audit.push(entry);
  }

  private newId(prefix: string): string {
    this.data.seq += 1;
    return `${prefix}-${hex(`${prefix}/${this.data.seq}`, 16)}`;
  }

  private rollout(id: string): Rollout {
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
      case 'forget':
        return this.forget(body as HoldRequest);
      case 'pause':
      case 'resume':
        return this.pause(name, body as ScopeRequest);
      case 'abort':
        return this.abort(body as ScopeRequest);
      case 'skip':
      case 'retry':
        return this.skipOrRetry(name, body as RolloutHostsRequest);
      default:
        throw new HttpError(404, `unknown action ${name}`);
    }
  }

  private manual(verb: 'roll' | 'reboot', req: RollRequest): ActionResult {
    const selected = this.selectHosts(req);
    const rejected: Record<string, string> = {};
    const members: RolloutHost[] = [];
    const id = this.newId('ro');
    const now = this.iso(this.now());

    for (const host of selected) {
      if (!list(host.verbs).includes(verb)) {
        rejected[host.name] = `the agent does not allow ${verb}`;
        continue;
      }
      if (host.hold.local || host.hold.controller || host.holdWanted) {
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
          c => c.managed && !c.custom && (wanted.length === 0 || wanted.includes(c.name))
        );
        if (targets.length === 0) {
          rejected[host.name] = 'no managed, non-custom container matches';
          continue;
        }
        for (const c of targets) {
          const to = c.registry?.digest ?? c.digest ?? '';
          needs.push({
            id: `manual/${directive}/${c.name}`,
            kind: 'image',
            directiveId: directive,
            image: { container: c.name, image: c.image, fromDigest: c.digest, toDigest: to },
          });
        }
      }

      host.needs = [...list(host.needs), ...needs];
      const bootnode = list(host.roles).includes('bootnode');
      members.push({
        host: host.name,
        state: 'pending',
        stage: bootnode ? 3 : host.validators === 0 ? 0 : 2,
        needId: needs[0]?.id,
        updatedAt: now,
      });
    }

    if (members.length === 0) {
      return { message: `Nothing to ${verb}: every selected host was rejected.`, rejected };
    }

    const what = verb === 'roll' ? 'roll' : 'reboot';
    const title = `Manual ${what} of ${req.all ? 'the fleet' : members.length === 1 ? (members[0]?.host ?? '') : `${members.length} hosts`}`;

    this.data.rollouts.push({
      id,
      kind: 'manual',
      key: `manual:${id}`,
      title,
      state: 'active',
      paused: false,
      approved: true,
      force: req.force || !req.staggered,
      ignoreBudget: req.ignoreBudget,
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
      if (action === 'unhold' && host.hold.local && !host.hold.controller && !host.holdWanted) {
        rejected[host.name] = 'only the local hold file is set; it can only be removed on the host';
        continue;
      }
      host.holdWanted = action === 'hold';
      // The mock agent agrees at once.
      host.hold = { ...host.hold, controller: action === 'hold' };
      if (action === 'hold') host.needs = [];
      applied.push(host.name);
      this.log(`action.${action}`, host.name, { reason: req.reason });
    }

    return {
      message: `${action === 'hold' ? 'Hold set on' : 'Hold lifted on'} ${applied.length} of ${hosts.length} hosts.`,
      hosts: applied,
      rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
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
      this.data.hosts.delete(host.name);
      for (const r of this.openRollouts()) {
        r.hosts = list(r.hosts).filter(m => m.host !== host.name);
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

  private pause(action: 'pause' | 'resume', req: ScopeRequest): ActionResult {
    if (req.rollout === '') {
      const before = { paused: this.data.settings.paused };
      this.data.settings = { ...this.data.settings, paused: action === 'pause' };
      this.log(`action.${action}`, 'settings', { before, after: { paused: action === 'pause' }, reason: req.reason });
      return { message: action === 'pause' ? 'Everything is paused.' : 'The global pause is lifted.' };
    }

    const r = this.rollout(req.rollout);
    if (r.state !== 'active' && r.state !== 'halted') throw new HttpError(409, `rollout ${r.id} is ${r.state}`);

    r.paused = action === 'pause';
    if (action === 'resume') r.approved = true;
    r.updatedAt = this.iso(this.now());
    this.log(`action.${action}`, r.id, { reason: req.reason });

    return {
      message: action === 'pause' ? `Rollout paused: ${r.title}` : `Rollout resumed and approved: ${r.title}`,
      rollout: r.id,
    };
  }

  private abort(req: ScopeRequest): ActionResult {
    if (req.rollout === '') throw new HttpError(400, 'abort needs a rollout');

    const r = this.rollout(req.rollout);
    if (r.state !== 'active' && r.state !== 'halted') throw new HttpError(409, `rollout ${r.id} is already ${r.state}`);

    const now = this.iso(this.now());
    r.state = 'aborted';
    r.reason = req.reason || 'aborted';
    r.updatedAt = now;
    r.endedAt = now;
    this.log('action.abort', r.id, { reason: req.reason });

    return { message: `Rollout aborted: ${r.title}`, rollout: r.id };
  }

  private skipOrRetry(action: 'skip' | 'retry', req: RolloutHostsRequest): ActionResult {
    const r = this.rollout(req.rollout);
    const now = this.iso(this.now());
    const rejected: Record<string, string> = {};
    const applied: string[] = [];
    const wanted = list(req.hosts);

    if (action === 'skip' && wanted.length === 0) throw new HttpError(400, 'skip needs at least one host');

    for (const m of list(r.hosts)) {
      if (wanted.length > 0 && !wanted.includes(m.host)) continue;

      if (action === 'skip') {
        if (m.state === 'leased' || m.state === 'done') {
          rejected[m.host] = `the host is ${m.state}`;
          continue;
        }
        m.state = 'skipped';
      } else {
        if (m.state !== 'failed') {
          if (wanted.length > 0) rejected[m.host] = `the host is ${m.state}, not failed`;
          continue;
        }
        m.state = 'pending';
        m.error = undefined;
        m.leaseId = undefined;
      }

      m.updatedAt = now;
      applied.push(m.host);
    }

    for (const name of wanted) {
      if (!list(r.hosts).some(m => m.host === name)) rejected[name] = 'not a member of the rollout';
    }

    if (action === 'retry') {
      r.state = 'active';
      r.reason = undefined;
      r.endedAt = undefined;
    }
    r.updatedAt = now;
    this.log(`action.${action}`, r.id, { hosts: applied, reason: req.reason });

    return {
      message:
        action === 'skip'
          ? `Skipped ${applied.length} host(s) in: ${r.title}`
          : `Rollout active again, ${applied.length} host(s) back to pending: ${r.title}`,
      rollout: r.id,
      hosts: applied,
      rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
    };
  }
}
