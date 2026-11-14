// An in-browser stand-in for the controller API, serving the fixture through
// the same routes and JSON shapes (pkg/controller/api/schema.go). Actions
// change the in-memory state, so the UI can be exercised end to end.

import type { FetchFn } from '../api/client';
import {
  dict,
  list,
  type ActionResult,
  type Audit,
  type AuditEntry,
  type Budget,
  type Config,
  type EndLeaseRequest,
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
  type ParticipationPoint,
  type Packages,
  type Plan,
  type PublicHistory,
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
import { formatPercent } from '../lib/format';
import { parseGoDuration } from '../lib/goDuration';
import { FIELDS, toForm, validateField } from '../lib/settingsForm';
import { EPOCH_MS, SECONDS_PER_SLOT, SLOTS_PER_EPOCH, buildMockData, hash, hex, type MockData } from './fixture';

/** The bearer token the mock auth client hands out. */
export const MOCK_TOKEN = 'mock-token';
/** The auth provider URL the mock reports in auth mode. */
export const MOCK_AUTH_URL = 'https://auth.mock.invalid';

const LIVENESS_EPOCHS = 16;
/** The controller's default and maximum history window, in epochs. */
const HISTORY_DEFAULT = 128;
const HISTORY_MAX = 512;

/** waitKind classifies a waiting reason the way the scheduler does. */
function waitKind(reason: string): WaitKind {
  if (reason === 'held') return 'held';
  if (reason === 'agent silent') return 'silent';
  if (reason === 'paused') return 'paused';
  if (reason === 'rollout paused') return 'rollout-paused';
  if (reason.startsWith('waiting for approval')) return 'approval';
  if (reason === 'policy paused') return 'policy-paused';
  if (reason.startsWith('rollout halted')) return 'halted';
  if (reason.startsWith('network gate')) return 'gate';
  if (reason.startsWith('verb not allowed')) return 'verb';
  if (reason.startsWith('after ')) return 'order';
  if (reason.startsWith('budget ')) return 'budget';
  if (reason.startsWith('host limit')) return 'host-limit';
  return 'next';
}

/** The stages of fleet.Stage. */
const STAGE_REPAIR = 1;
const STAGE_CANARY = 2;
const STAGE_REST = 3;

/** BROKEN_NOTE starts the note of a waiting member whose host is broken. */
const BROKEN_NOTE = 'broken since lease ';

/** needGroup names what a need is about, as far as a host can be broken for it: an image reference, or the packages. */
function needGroup(n: Need): string {
  if (n.directiveId) return '';
  if (n.kind === 'image' && n.image) return `image:${n.image.image}`;
  if (n.kind === 'packages') return 'packages';
  return '';
}

/** rolloutGroup names the rollouts that are about the same thing; reboot and manual rollouts belong to none. */
function rolloutGroup(r: Rollout): string {
  if (r.kind === 'image') return `image:${r.image ?? ''}`;
  if (r.kind === 'packages') return 'packages';
  return '';
}

/**
 * leaseVerdict is what the end of a lease says about its host for a group:
 * released with every step of the group done repairs it, failed breaks it
 * (unless those steps were done and another step failed), anything else
 * (expired, released without the step) says nothing.
 */
function leaseVerdict(l: Lease, group: string): 'repaired' | 'broke' | undefined {
  const mine = list(l.needs).filter(n => needGroup(n) === group);
  if (mine.length === 0) return undefined;

  const steps = list(l.operation?.steps);
  const done = mine.every(n => steps.find(s => s.needId === n.id)?.status === 'done');

  if (l.state === 'released') return done ? 'repaired' : undefined;
  if (l.state === 'failed') return done && steps.some(s => s.status === 'failed') ? undefined : 'broke';
  return undefined;
}

/** succeeded: the rollout has served a member successfully (one is done through a lease). */
function succeeded(r: Rollout): boolean {
  return list(r.hosts).some(m => m.state === 'done' && Boolean(m.leaseId));
}

/** keepsCanary: the canary holds a lease, is done by one, or failed and the rollout is still halted by it. */
function keepsCanary(r: Rollout, m: RolloutHost): boolean {
  if (m.state === 'leased') return true;
  if (m.state === 'done') return Boolean(m.leaseId);
  return m.state === 'failed' && r.state === 'halted';
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
  private livenessUnsupported = false;

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

  /** setLivenessUnsupported makes the beacon node one that does not serve liveness. */
  setLivenessUnsupported(unsupported: boolean): void {
    this.livenessUnsupported = unsupported;
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
        case 'public/history':
          return this.history(q.get('limit'));
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

    if (method === 'PUT' && route === 'settings') return this.putSettings(body);

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

    if (list(host.containers).some(c => c.kind === 'el' && c.state !== 'running')) return back < 27 ? 0 : weight;
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

  /** history sums the hosts' liveness per epoch; a host is dark while it holds (or held) a lease. */
  private history(raw: string | null): PublicHistory {
    const limit = raw === null ? HISTORY_DEFAULT : Number(raw);
    if (!Number.isInteger(limit) || limit < 1) throw new HttpError(400, 'limit must be a positive integer');

    // No rows before genesis.
    const newest = this.currentEpoch() - 1;
    const epochs: ParticipationPoint[] = [];

    for (let back = Math.min(limit, HISTORY_MAX, newest + 1) - 1; back >= 0; back--) {
      const epoch = newest - back;
      const start = this.data.genesis + epoch * EPOCH_MS;
      const end = start + EPOCH_MS;
      let live = 0;
      let total = 0;

      for (const host of this.data.hosts.values()) {
        total += host.validators;
        const dark = this.data.leases.some(l => {
          if (l.host !== host.name || !l.endedAt) return false;
          const from = Date.parse(l.grantedAt);
          const to = Date.parse(l.agentDoneAt ?? l.endedAt);
          return from < end && to > start;
        });
        if (!dark) live += this.liveAt(host, back, epoch);
      }

      epochs.push({ epoch, time: this.iso(end), live, total });
    }

    return {
      epochs,
      minParticipationPercent: this.data.settings.minParticipationPercent,
      epochSeconds: EPOCH_MS / 1000,
    };
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
      // A lease takes out of the gate's share what it took offline: the
      // validators that attested when it was granted. Those that were
      // offline already stay in, as not live.
      const lease = this.openLease(host.name);
      if (!lease) {
        gateTracked += host.validators;
        gateLive += l;
      } else {
        gateTracked += host.validators - Math.min(lease.charged, host.validators);
      }
    }

    const s = this.data.settings;
    const participation = gateTracked > 0 ? (gateLive / gateTracked) * 100 : 100;
    const reasons: string[] = [];
    if (lag > s.maxFinalityLagEpochs) reasons.push(`finality lag ${lag} epochs > ${s.maxFinalityLagEpochs}`);
    if (!this.livenessUnsupported && participation < s.minParticipationPercent) {
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
        livenessKnown: !this.livenessUnsupported,
        livenessEpoch: epoch - 1,
        livenessUnsupported: this.livenessUnsupported,
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

  /** live is the number of the host's validators that attested in the newest epoch. */
  private live(name: string): number {
    const host = this.data.hosts.get(name);
    return host ? this.liveAt(host, 0, this.currentEpoch() - 1) : 0;
  }

  /** lastLeaseEnd is when the host's most recent lease ended, or 0. */
  private lastLeaseEnd(name: string): number {
    return this.data.leases
      .filter(l => l.host === name && l.endedAt)
      .reduce((latest, l) => Math.max(latest, Date.parse(l.endedAt as string)), 0);
  }

  /**
   * offline: the host is established as offline. It has validators, holds no
   * lease, and none of them attested in the two newest epochs, both of which
   * began after its last lease ended and after the host was first seen.
   */
  private offline(host: HostDetail): boolean {
    if (host.validators === 0 || this.openLease(host.name)) return false;

    const newest = this.currentEpoch() - 1;
    if (this.liveAt(host, 0, newest) !== 0 || this.liveAt(host, 1, newest) !== 0) return false;

    const began = this.data.genesis + (newest - 1) * EPOCH_MS;
    return began > Math.max(this.lastLeaseEnd(host.name), Date.parse(host.firstSeen));
  }

  /**
   * repairs names the hosts whose repair the closed gate would let pass, in
   * serving order: those that are established as offline, while the gate
   * fails on data it has. One at a time: the first passes, unless a lease
   * that was granted this way is open (then `open` names its host); the
   * others wait for that repair.
   */
  private repairs(
    order: readonly string[],
    open: readonly Rollout[],
    gate: Gate
  ): { hosts: readonly string[]; open?: string } {
    const reasons = list(gate.reasons);
    if (gate.ok || reasons.includes('beacon node unreachable') || reasons.includes('liveness data stale')) {
      return { hosts: [] };
    }
    if (this.data.settings.paused) return { hosts: [] };

    const held = this.data.leases.find(l => l.repair && !['released', 'failed', 'expired'].includes(l.state));

    // Only a host that one of its rollouts would grant but for the gate.
    const grants = (r: Rollout, host: HostDetail) =>
      r.state === 'active' &&
      !r.paused &&
      !r.ignoreBudget &&
      this.policy(r) !== 'paused' &&
      (this.policy(r) !== 'manual' || r.approved) &&
      list(r.hosts).some(m => m.host === host.name && m.state === 'pending') &&
      this.hasVerb(r, host);

    const hosts = order.filter(name => {
      const host = this.data.hosts.get(name);
      const onHold = !host || host.holdWanted || host.hold.local || host.hold.controller;
      return (
        host !== undefined && !onHold && !this.isStale(name) && this.offline(host) && open.some(r => grants(r, host))
      );
    });

    return { hosts, open: held?.host };
  }

  /** hasVerb: the agent allows the verb of a need it reports for the rollout. */
  private hasVerb(r: Rollout, host: HostDetail): boolean {
    const kinds = r.kind === 'manual' ? list(host.needs).map(n => n.kind) : [r.kind];
    return kinds.some(kind => list(host.verbs).includes(kind === 'image' ? 'roll' : kind));
  }

  /**
   * servable: the member can be served as far as its host goes. It waits
   * with a need whose verb the agent allows and is neither held nor silent;
   * or it is expected and still waited for: neither held nor silent, the
   * rollout no older than hostStaleAfter, and, in an image rollout, the host
   * allows the roll verb and still runs the image at another digest.
   */
  private servable(r: Rollout, m: RolloutHost): boolean {
    const host = this.data.hosts.get(m.host);
    if (!host || host.holdWanted || host.hold.local || host.hold.controller || this.isStale(m.host)) return false;

    if (m.state === 'pending') return this.hasVerb(r, host);
    if (m.state !== 'expected') return false;

    const staleAfter = parseGoDuration(this.data.settings.hostStaleAfter) ?? 120_000;
    if (this.now() - Date.parse(r.createdAt) > staleAfter) return false;
    if (r.kind !== 'image') return r.kind === 'manual';

    return (
      list(host.verbs).includes('roll') &&
      list(host.containers).some(c => c.managed && !c.custom && c.image === r.image && c.digest !== r.digest)
    );
  }

  /** awaited: the member holds a lease or is servable; the members behind it wait for it. */
  private awaited(r: Rollout, m: RolloutHost): boolean {
    return m.state === 'leased' || this.servable(r, m);
  }

  /**
   * brokenSince is the lease that broke the host for what the rollout is
   * about and that no later lease has repaired, read off the lease table as
   * the scheduler does (docs/scheduler.md, Rollouts).
   */
  private brokenSince(r: Rollout, host: string): Lease | undefined {
    const group = rolloutGroup(r);
    if (group === '') return undefined;

    let last: Lease | undefined;
    for (const l of this.data.leases) {
      if (l.host !== host || !l.endedAt || leaseVerdict(l, group) === undefined) continue;
      if (!last?.endedAt || l.endedAt > last.endedAt) last = l;
    }

    return last && leaseVerdict(last, group) === 'broke' ? last : undefined;
  }

  private broken(r: Rollout, m: RolloutHost): boolean {
    return this.brokenSince(r, m.host) !== undefined;
  }

  /** brokenAwaited is the first member whose host is broken and that is still to be served. */
  private brokenAwaited(r: Rollout): RolloutHost | undefined {
    return list(r.hosts).find(m => this.broken(r, m) && this.awaited(r, m));
  }

  /**
   * inFlight is the host of an open lease that carries a step of what the
   * rollout is about for another rollout that is closed by now: nobody who is
   * not broken is granted until it has ended.
   */
  private inFlight(r: Rollout): string | undefined {
    const group = rolloutGroup(r);
    if (group === '') return undefined;

    const open = new Set(this.openRollouts().map(o => o.id));

    return this.data.leases
      .filter(
        l =>
          !['released', 'failed', 'expired'].includes(l.state) &&
          list(l.needs).some(n => {
            const id = dict(l.rollouts)[n.id];
            return needGroup(n) === group && id !== r.id && !(id !== undefined && open.has(id));
          })
      )
      .map(l => l.host)
      .sort()[0];
  }

  /**
   * orderWait names the host that the order inside the rollout makes the
   * host wait for, or undefined: every member whose host is broken and that
   * is still to be served, a lease in flight and, until the rollout has its
   * first success, the canary. A broken host waits for nobody.
   */
  private orderWait(r: Rollout, host: string): { reason: string; after: string } | undefined {
    const me = list(r.hosts).find(m => m.host === host);
    if (r.force || !me || this.broken(r, me)) return undefined;

    const first = this.brokenAwaited(r);
    if (first) return { reason: `after ${first.host}`, after: first.host };

    const flying = this.inFlight(r);
    if (flying !== undefined) return { reason: `after ${flying}`, after: flying };

    if (!r.canary || r.canary === host || succeeded(r)) return undefined;
    return { reason: `after canary ${r.canary}`, after: r.canary };
  }

  /** waitingReason follows the order of docs/scheduler.md, "Waiting reasons". */
  private waitingReason(
    host: HostDetail,
    memberOf: Rollout[],
    gate: Gate,
    budget: Budget,
    repairs: { hosts: readonly string[]; open?: string }
  ): string {
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

    const able = allowed.filter(r => this.hasVerb(r, host));
    if (able.length === 0) return 'verb not allowed by the agent';

    const ignoresBudget = able.some(r => r.ignoreBudget);
    const throughGate = !gate.ok && !ignoresBudget;
    if (throughGate) {
      // One repair at a time: the first in serving order passes, the others
      // wait for it, or for the repair that is open already.
      const first = repairs.open ?? repairs.hosts[0];
      if (!repairs.hosts.includes(host.name)) return `network gate: ${list(gate.reasons)[0] ?? 'failing'}`;
      if (first !== host.name) return `after repair of ${first ?? ''}`;
    }

    // The host is served for every rollout whose order lets it go: it waits
    // for the order only when the order of each of them holds it back. A
    // repair through the closed gate does not wait for its place.
    const waits = able.map(r => this.orderWait(r, host.name));
    if (!throughGate && waits.every(w => w !== undefined)) return waits[0]?.reason ?? 'next';

    if (!ignoresBudget) {
      if (!fits(this.live(host.name), budget)) {
        const pct = (w: number) => formatPercent(budget.totalWeight > 0 ? (w / budget.totalWeight) * 100 : 0);
        return `budget ${pct(budget.unavailableWeight)} of ${pct(budget.allowedWeight)}`;
      }
      if (budget.leasedHosts >= budget.maxLeasedHosts) {
        return `host limit ${budget.leasedHosts} of ${budget.maxLeasedHosts}`;
      }
    }

    return 'next';
  }

  /** orderAfter names the host that a host waiting for its place in the order waits for. */
  private orderAfter(host: string, memberOf: Rollout[], reason: string): string | undefined {
    const repair = 'after repair of ';
    if (reason.startsWith(repair)) return reason.slice(repair.length);

    for (const r of memberOf) {
      const wait = r.state === 'active' ? this.orderWait(r, host) : undefined;
      if (wait) return wait.after;
    }

    return undefined;
  }

  /** aboutRollout is the rollout a waiting reason is about, as the scheduler names it in the plan. */
  private aboutRollout(host: string, memberOf: Rollout[], wait: WaitKind): string | undefined {
    const active = memberOf.filter(r => r.state === 'active');

    switch (wait) {
      case 'rollout-paused':
        return active.find(r => r.paused)?.id;
      case 'approval':
        return active.find(r => !r.paused && this.policy(r) === 'manual' && !r.approved)?.id;
      case 'policy-paused':
        return active.find(r => this.policy(r) === 'paused')?.id;
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

  /**
   * settleStages mirrors the scheduler's order inside a rollout
   * (docs/scheduler.md, Ordering): the canary is settled and every member
   * gets its stage: the canary is in its own, a member whose host is broken
   * in the repair stage, everybody else in the rest. A waiting member of a
   * broken host carries the note of the lease that broke it. The canary is
   * the first member that is not broken and can go, in this order: it fits
   * the budget, it has live validators, it reports the need, it is the
   * lightest, by name. There is none in a forced rollout, for a single
   * member, after the first success, while a broken member is still to be
   * served and while a lease is in flight for the rollout.
   */
  private settleStages(r: Rollout, budget: Budget): void {
    const members = list(r.hosts);
    const current = members.find(m => m.host === r.canary);

    if (r.force || !current || !keepsCanary(r, current)) {
      r.canary = undefined;

      if (!r.force && !succeeded(r) && !this.brokenAwaited(r) && this.inFlight(r) === undefined) {
        const weight = (m: RolloutHost) => this.data.hosts.get(m.host)?.validators ?? 0;
        const best = members
          .filter(m => !this.broken(r, m) && this.awaited(r, m) && !this.openLease(m.host)?.repair)
          .sort(
            (a, b) =>
              Number(a.state !== 'leased') - Number(b.state !== 'leased') ||
              Number(!fits(this.live(a.host), budget)) - Number(!fits(this.live(b.host), budget)) ||
              Number(this.live(a.host) === 0) - Number(this.live(b.host) === 0) ||
              Number(a.state === 'expected') - Number(b.state === 'expected') ||
              weight(a) - weight(b) ||
              a.host.localeCompare(b.host)
          )[0];
        const counted = members.filter(
          m => m.state === 'pending' || m.state === 'leased' || m.state === 'failed' || this.servable(r, m)
        ).length;

        if (best && counted > 1) r.canary = best.host;
      }
    }

    for (const m of members) {
      const lease = this.brokenSince(r, m.host);
      const waits = m.state === 'pending' || m.state === 'expected';
      const noted = m.error?.startsWith(BROKEN_NOTE) ?? false;

      if (lease && waits && (!m.error || noted)) {
        const from = list(lease.needs)
          .filter(n => needGroup(n) === rolloutGroup(r))
          .map(n => dict(lease.rollouts)[n.id])
          .find(id => id !== undefined);
        m.error = `${BROKEN_NOTE}${lease.id}${from ? ` (rollout ${from})` : ''}`;
      } else if (!lease && noted) {
        m.error = undefined;
      }

      if (m.host === r.canary) m.stage = STAGE_CANARY;
      else if (lease) m.stage = STAGE_REPAIR;
      // A member that is done through the lease that repaired it keeps its stage.
      else if (!(m.stage === STAGE_REPAIR && m.state === 'done' && m.leaseId)) m.stage = STAGE_REST;
    }
  }

  private plan(): Plan {
    const gate = this.gate();
    const budget = this.budget();
    const open = this.openRollouts();
    for (const r of open) this.settleStages(r, budget);
    const epoch = this.currentEpoch() - 1;

    const hosts: Record<string, HostPlan> = {};
    const queue: QueueEntry[] = [];

    // Queue order: oldest rollout first, inside a rollout by stage (broken
    // members, the canary, the rest), then ascending weight and name.
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

    const repairs = this.repairs(order, open, gate);

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
        plan.reason = this.waitingReason(host, memberOf, gate, budget, repairs);
      }

      hosts[host.name] = plan;
    }

    for (const name of order) {
      const host = this.data.hosts.get(name);
      const plan = hosts[name];
      if (!host || !plan || plan.leaseId) continue;
      const memberOf = open.filter(r => list(r.hosts).some(m => m.host === name && m.state === 'pending'));
      const wait = waitKind(plan.reason ?? 'next');
      queue.push({
        host: name,
        position: queue.length + 1,
        reason: plan.reason ?? 'next',
        wait,
        after: wait === 'order' ? this.orderAfter(name, memberOf, plan.reason ?? '') : undefined,
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
    const budget = this.budget();
    for (const r of this.openRollouts()) this.settleStages(r, budget);
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

  /** putSettings answers like the controller: 400 for an unknown key, a missing one or a value out of range. */
  private putSettings(body: unknown): SettingsBody {
    if (typeof body !== 'object' || body === null || Array.isArray(body)) {
      throw new HttpError(400, 'settings must be a JSON object');
    }

    const given = body as Record<string, unknown>;
    const known = new Set<string>(FIELDS.map(f => f.key));

    for (const key of Object.keys(given)) {
      if (!known.has(key)) throw new HttpError(400, `unknown field "${key}"`);
    }

    for (const spec of FIELDS) {
      const value = given[spec.key];
      const wanted =
        spec.kind === 'switch' ? 'boolean' : spec.kind === 'percent' || spec.kind === 'count' ? 'number' : 'string';
      if (typeof value !== wanted) throw new HttpError(400, `${spec.key} must be a ${wanted}`);
    }

    const next = body as Settings;
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
      case 'end-lease':
        return this.endLease(body as EndLeaseRequest);
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
      members.push({ host: host.name, state: 'pending', stage: STAGE_REST, needId: needs[0]?.id, updatedAt: now });
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

  /**
   * endLease ends one open lease that its agent has started as failed
   * ("ended by operator"): the host is failed in the open rollouts the lease
   * served, those halt, and the host is broken from then on.
   */
  private endLease(req: EndLeaseRequest): ActionResult {
    const l = this.data.leases.find(x => x.id === req.lease);
    if (!l) throw new HttpError(404, `scheduler: unknown lease: ${req.lease}`);
    if (['released', 'failed', 'expired'].includes(l.state)) {
      throw new HttpError(409, `scheduler: invalid action: lease ${l.id} is ${l.state}`);
    }
    if (l.state === 'granted') {
      throw new HttpError(409, `scheduler: invalid action: the agent has not started lease ${l.id}; it ends by itself`);
    }

    const now = this.iso(this.now());
    const reason = 'ended by operator';
    const before = l.state;
    l.state = 'failed';
    l.error = reason;
    l.endedAt = now;

    const halted: string[] = [];
    for (const r of this.openRollouts()) {
      const m = list(r.hosts).find(x => x.host === l.host && x.state === 'leased' && x.leaseId === l.id);
      if (!m) continue;

      m.state = 'failed';
      m.error = reason;
      m.updatedAt = now;
      halted.push(r.id);

      if (r.state === 'active') {
        r.state = 'halted';
        r.reason = `${l.host}: ${reason}`;
        r.updatedAt = now;
        this.log('rollout.halted', r.id, { host: l.host, lease: l.id, reason }, 'scheduler');
      }
    }

    this.log('action.end-lease', l.host, {
      lease: l.id,
      before: { state: before },
      after: { state: l.state },
      reason: req.reason,
    });
    this.log('lease.failed', l.host, { lease: l.id, reason, rollouts: halted }, 'scheduler');

    return { message: `lease ${l.id} of ${l.host} ended as failed`, hosts: [l.host] };
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
