import { describe, expect, it } from 'vitest';

import {
  list,
  type ActionResult,
  type HostDetail,
  type Plan,
  type Rollout,
  type Rollouts,
  type SettingsBody,
  type StepStatus,
} from '../api/types';
import { clearsByItself } from '../lib/timeline';
import { MockController } from './mockController';

async function call<T>(mock: MockController, path: string, body?: unknown, method = 'POST'): Promise<T> {
  const res = await mock.fetch(
    `/api/v1/${path}`,
    body === undefined ? undefined : { method, body: JSON.stringify(body) }
  );
  expect(res.status).toBe(200);
  return (await res.json()) as T;
}

/** roomy raises the availability budget so that every host fits it. */
async function roomy(mock: MockController): Promise<MockController> {
  const { settings } = await call<SettingsBody>(mock, 'settings');
  await call<SettingsBody>(mock, 'settings', { ...settings, maxUnavailablePercent: 100 }, 'PUT');
  return mock;
}

/** stagger starts a staggered manual reboot of the hosts and returns its rollout and the plan. */
async function stagger(mock: MockController, hosts: string[], force = false) {
  const res = await call<ActionResult>(mock, 'actions/reboot', {
    hosts,
    all: false,
    staggered: true,
    force,
    ignoreBudget: false,
    reason: 'test',
  });
  const plan = await call<Plan>(mock, 'plan');
  const rollouts = await call<Rollouts>(mock, 'rollouts');
  const rollout = list(rollouts.open).find(r => r.id === res.rollout);
  if (!rollout) throw new Error('the manual rollout is not open');

  const reasons = new Map(list(plan.queue).map(q => [q.host, q]));
  return { rollout, reasons };
}

const stagesOf = (r: Rollout) => Object.fromEntries(list(r.hosts).map(m => [m.host, m.stage]));

describe('MockController, a rollout paused after its canary', () => {
  const REASON = 'paused after the canary; resume to continue';

  it('queues its hosts with the reason, as a paused rollout, and resume lifts it', async () => {
    const mock = new MockController();
    const paused = list((await call<Rollouts>(mock, 'rollouts')).open).find(r => r.pauseReason !== undefined);
    if (!paused) throw new Error('the fixture has no rollout paused after its canary');

    expect(paused.paused).toBe(true);
    expect(paused.pauseReason).toBe(REASON);
    expect(paused.canary).toBe('grandine-erigon-1');
    expect(list(paused.hosts).find(m => m.host === paused.canary)?.state).toBe('done');

    const waiting = list((await call<Plan>(mock, 'plan')).queue).find(q => q.host === 'grandine-erigon-2');
    expect(waiting?.reason).toBe(REASON);
    expect(waiting?.wait).toBe('rollout-paused');
    expect(waiting?.rollout).toBe(paused.id);
    expect(clearsByItself(waiting?.wait)).toBe(false);

    // The audit trail says who paused it.
    expect(mock.data.audit.some(e => e.action === 'rollout.paused' && e.actor === 'scheduler')).toBe(true);

    await call<ActionResult>(mock, 'actions/resume', { rollout: paused.id, reason: 'looks good' });

    const resumed = list((await call<Rollouts>(mock, 'rollouts')).open).find(r => r.id === paused.id);
    expect(resumed?.paused).toBe(false);
    expect(resumed?.pauseReason).toBeUndefined();

    const after = list((await call<Plan>(mock, 'plan')).queue).find(q => q.host === 'grandine-erigon-2');
    expect(after?.reason).not.toBe(REASON);
    expect(after?.wait).not.toBe('rollout-paused');
  });

  it('shows the halt of a rollout that is halted and paused, like the scheduler', async () => {
    const mock = new MockController();
    const halted = list((await call<Rollouts>(mock, 'rollouts')).open).find(r => r.state === 'halted');
    if (!halted) throw new Error('the fixture has no halted rollout');

    const waiting = async () =>
      list((await call<Plan>(mock, 'plan')).queue).filter(
        q => q.rollout === halted.id && (q.wait === 'halted' || q.wait === 'rollout-paused')
      );
    const before = await waiting();
    expect(before.length).toBeGreaterThan(0);
    expect(before.every(q => q.wait === 'halted')).toBe(true);

    await call<ActionResult>(mock, 'actions/pause', { rollout: halted.id, reason: 'wait' });

    expect(await waiting()).toStrictEqual(before);
  });

  it('keeps the plain reason for a rollout that a person paused', async () => {
    const mock = new MockController();
    const paused = list((await call<Rollouts>(mock, 'rollouts')).open).find(r => r.pauseReason !== undefined);
    if (!paused) throw new Error('the fixture has no rollout paused after its canary');

    await call<ActionResult>(mock, 'actions/resume', { rollout: paused.id, reason: '' });
    await call<ActionResult>(mock, 'actions/pause', { rollout: paused.id, reason: 'wait' });

    const waiting = list((await call<Plan>(mock, 'plan')).queue).find(q => q.host === 'grandine-erigon-2');
    expect(waiting?.reason).toBe('rollout paused');
    expect(waiting?.wait).toBe('rollout-paused');
  });

  it('carries the comparison with the fleet on the lease that was released on it', async () => {
    const mock = new MockController();
    const rollouts = await call<Rollouts>(mock, 'rollouts');
    const lease = list(rollouts.recentLeases).find(l => l.host === 'grandine-erigon-1');

    expect(lease?.peerChecks).toStrictEqual([
      { layer: 'EL', peers: 27, median: 31, needs: 8 },
      { layer: 'CL', peers: 49, median: 52, needs: 13 },
    ]);
    // A lease that still waits for something else was not compared yet.
    expect(list(rollouts.leases).find(l => l.state === 'verifying')?.peerChecks).toBeUndefined();
  });
});

describe('MockController, the order inside a rollout', () => {
  it('makes a host with validators that fits the budget the canary and lets the rest wait, the bootnode too', async () => {
    const mock = await roomy(new MockController());
    const { rollout, reasons } = await stagger(mock, ['bootnode-1', 'rpc-1', 'prysm-geth-1', 'prysm-geth-2']);

    expect(rollout.canary).toBe('prysm-geth-1');
    expect(stagesOf(rollout)).toStrictEqual({ 'bootnode-1': 3, 'rpc-1': 3, 'prysm-geth-1': 2, 'prysm-geth-2': 3 });

    for (const host of ['rpc-1', 'prysm-geth-2']) {
      expect(reasons.get(host)).toMatchObject({
        reason: 'after canary prysm-geth-1',
        wait: 'order',
        rollout: rollout.id,
      });
    }
    // bootnode-1 also waits in the lighthouse rollout, which is past its
    // canary: a host is served for the rollouts whose order lets it go.
    expect(reasons.get('bootnode-1')?.wait).not.toBe('order');
    expect(reasons.get('prysm-geth-1')?.wait).not.toBe('order');
  });

  it('makes a host that costs nothing the canary while no host with validators fits the budget', async () => {
    // The fixture's budget is used up by the host that is being verified.
    const mock = new MockController();
    const { rollout, reasons } = await stagger(mock, ['bootnode-1', 'rpc-1', 'prysm-geth-1', 'prysm-geth-2']);

    expect(rollout.canary).toBe('bootnode-1');
    expect(reasons.get('prysm-geth-1')?.reason).toBe('after canary bootnode-1');
    expect(reasons.get('bootnode-1')?.wait).not.toBe('order');
  });

  it('takes the first host when none has validators', async () => {
    const mock = new MockController();
    const { rollout, reasons } = await stagger(mock, ['rpc-1', 'bootnode-1']);

    expect(rollout.canary).toBe('bootnode-1');
    expect(reasons.get('rpc-1')?.reason).toBe('after canary bootnode-1');
  });

  it('has no canary for a single host or a forced rollout', async () => {
    const single = await stagger(new MockController(), ['prysm-geth-1']);
    expect(single.rollout.canary).toBeUndefined();
    expect(list(single.rollout.hosts).map(m => m.stage)).toStrictEqual([3]);

    const forced = await stagger(new MockController(), ['prysm-geth-1', 'prysm-geth-2'], true);
    expect(forced.rollout.canary).toBeUndefined();
    expect([...forced.reasons.values()].some(q => q.wait === 'order')).toBe(false);
  });

  it('picks another canary when the first one is skipped', async () => {
    const mock = await roomy(new MockController());
    const { rollout } = await stagger(mock, ['prysm-geth-1', 'prysm-geth-2', 'rpc-1']);
    await call<ActionResult>(mock, 'actions/skip', { rollout: rollout.id, hosts: ['prysm-geth-1'], reason: 'test' });

    const after = list((await call<Rollouts>(mock, 'rollouts')).open).find(r => r.id === rollout.id);
    expect(after?.canary).toBe('prysm-geth-2');
  });

  it('treats a host whose validators are all offline as an ordinary member, never as the preferred canary', async () => {
    // With the gate failing, the mock takes the validators of some hosts down
    // for the two newest epochs.
    const mock = await roomy(new MockController());
    mock.setGateFailing(true);

    const plan = await call<Plan>(mock, 'plan');
    const idle = Object.entries(plan.hosts ?? {}).filter(
      ([, h]) => h.weight > 0 && (h.slot === 'idle' || h.slot === 'queued')
    );
    const down = idle
      .filter(([, h]) => h.live === 0)
      .map(([name]) => name)
      .sort();
    const up = idle
      .filter(([, h]) => h.live > 0)
      .map(([name]) => name)
      .sort();
    if (down.length < 2 || up.length === 0) throw new Error('the fixture lacks hosts that are down and up');
    const [first, second] = down as [string, string];
    const healthy = up[0] as string;

    const { rollout } = await stagger(mock, [first, second, healthy]);

    // No stage of its own, and the canary is the host with live validators.
    expect(stagesOf(rollout)).toStrictEqual({ [first]: 3, [second]: 3, [healthy]: 2 });
    expect(rollout.canary).toBe(healthy);
  });

  it('lets one repair at a time pass the closed gate, whatever the order of its rollout', async () => {
    const mock = await roomy(new MockController());
    mock.setGateFailing(true);

    const before = await call<Plan>(mock, 'plan');
    const down = Object.entries(before.hosts ?? {})
      .filter(([, h]) => h.weight > 0 && h.live === 0 && (h.slot === 'idle' || h.slot === 'queued'))
      .map(([name]) => name)
      .sort();
    if (down.length < 2) throw new Error('the fixture lacks two hosts that are down');

    await stagger(mock, down.slice(0, 2));

    const plan = await call<Plan>(mock, 'plan');
    expect(plan.gate.ok).toBe(false);

    // Everybody the gate concerns waits for it, but the hosts that are
    // offline: one passes, the others wait for its repair.
    const passing = list(plan.queue).filter(q => clearsByItself(q.wait));
    const first = passing.filter(q => q.wait !== 'order');
    expect(first).toHaveLength(1);
    expect(passing.length).toBeGreaterThan(1);

    for (const q of passing) {
      expect(plan.hosts?.[q.host]?.live).toBe(0);
      if (q === first[0]) continue;
      expect(q).toMatchObject({ wait: 'order', reason: `after repair of ${first[0]?.host}`, after: first[0]?.host });
    }

    expect(list(plan.queue).filter(q => q.wait === 'gate').length).toBeGreaterThan(1);
  });

  it('names the host an order wait is for', async () => {
    const mock = await roomy(new MockController());
    const { rollout, reasons } = await stagger(mock, ['rpc-1', 'prysm-geth-1', 'prysm-geth-2']);

    expect(reasons.get('prysm-geth-2')).toMatchObject({ wait: 'order', after: rollout.canary });
    expect(reasons.get(rollout.canary ?? '')?.after).toBeUndefined();

    // tooling-1 does not allow reboots: the action rejects it and the others form the rollout.
    const other = await stagger(await roomy(new MockController()), ['tooling-1', 'rpc-1', 'bootnode-1']);
    expect(other.rollout.canary).toBe('bootnode-1');
    expect(other.reasons.get('rpc-1')?.reason).toBe('after canary bootnode-1');
  });
});

describe('MockController, broken hosts and leases that a person ends', () => {
  const open = async (mock: MockController, title: string) => {
    const rollouts = await call<Rollouts>(mock, 'rollouts');
    const rollout = list(rollouts.open).find(r => r.title.includes(title));
    if (!rollout) throw new Error(`no open rollout ${title}`);
    return { rollout, leases: list(rollouts.leases), recent: list(rollouts.recentLeases) };
  };
  const member = (r: Rollout, host: string) => list(r.hosts).find(m => m.host === host);

  it('puts a host whose last lease for the image failed first, with the lease in its note', async () => {
    const mock = await roomy(new MockController());
    const before = await open(mock, 'nethermind');
    const failed = member(before.rollout, 'nimbus-nethermind-1');
    expect(failed).toMatchObject({ state: 'failed' });

    await call<ActionResult>(mock, 'actions/retry', { rollout: before.rollout.id, hosts: [], reason: 'again' });
    const plan = await call<Plan>(mock, 'plan');
    const { rollout } = await open(mock, 'nethermind');

    expect(member(rollout, 'nimbus-nethermind-1')).toMatchObject({
      state: 'pending',
      stage: 1,
      error: `broken since lease ${failed?.leaseId ?? ''} (rollout ${rollout.id})`,
    });
    expect(rollout.canary).toBeUndefined();

    const waits = new Map(list(plan.queue).map(q => [q.host, q]));
    expect(waits.get('nimbus-nethermind-2')).toMatchObject({
      reason: 'after nimbus-nethermind-1',
      wait: 'order',
      after: 'nimbus-nethermind-1',
    });
    expect(waits.get('nimbus-nethermind-1')?.wait).not.toBe('order');
  });

  const HOST = 'lighthouse-geth-2';
  type AuditRows = { entries: { action: string; target?: string; detail?: Record<string, unknown> }[] };
  const endEntries = async (mock: MockController) =>
    (await call<AuditRows>(mock, 'audit?limit=50')).entries.filter(e => e.action === 'action.end-lease');

  /** clocked is a mock whose clock the test moves; the fixture's open lease is put into the given state. */
  const clocked = async (state: 'granted' | 'running' | 'verifying', steps: StepStatus[] = []) => {
    const clock = { now: Date.parse('2026-01-01T00:00:00Z') };
    const mock = await roomy(new MockController({ now: () => clock.now }));
    const lease = mock.data.leases.find(l => l.host === HOST && l.state === 'verifying');
    if (!lease?.operation) throw new Error('the fixture lacks the verifying lease');

    if (state !== 'verifying') {
      lease.state = state;
      lease.agentDoneAt = undefined;
      lease.readiness = undefined;
      if (state === 'granted') lease.operation = undefined;
      else {
        lease.operation = {
          ...lease.operation,
          phase: 'applying',
          steps: list(lease.needs).map((n, i) => ({ needId: n.id, status: steps[i] ?? 'pending' })),
        };
      }
    }

    const end = (reason = 'host taken away') =>
      call<ActionResult>(mock, 'actions/end-lease', { lease: lease.id, reason });
    const pass = (seconds: number) => {
      clock.now += seconds * 1000;
    };

    return { mock, id: lease.id, end, pass };
  };

  /** retried: the rollout is retried; the member of the host, as the next plan leaves it. */
  const retried = async (mock: MockController, rollout: string) => {
    await call<ActionResult>(mock, 'actions/retry', { rollout, hosts: [], reason: 'again' });
    await call<Plan>(mock, 'plan');
    return member((await open(mock, 'lighthouse')).rollout, HOST);
  };

  it('takes ending a lease as a request: the lease stays open and counted, and the host is held', async () => {
    const { mock, id, end } = await clocked('running', ['running']);
    const budget = (await call<Plan>(mock, 'plan')).budget;

    const res = await end();
    expect(res).toMatchObject({ message: `lease ${id} of ${HOST} is ending; the host is on hold`, hosts: [HOST] });

    const { rollout, leases } = await open(mock, 'lighthouse');
    expect(leases.find(l => l.id === id)).toMatchObject({
      state: 'running',
      cancelled: { by: 'anonymous', state: 'running', reason: 'host taken away' },
    });
    expect(rollout.state).toBe('active');
    expect(member(rollout, HOST)).toMatchObject({ state: 'leased', leaseId: id });

    const plan = await call<Plan>(mock, 'plan');
    expect(plan.budget).toStrictEqual(budget);
    expect(plan.hosts?.[HOST]).toMatchObject({ slot: 'leased', leaseId: id });
    expect(await call<HostDetail>(mock, `hosts/${HOST}`)).toMatchObject({
      holdWanted: true,
      hold: { controller: true },
    });

    expect(await endEntries(mock)).toStrictEqual([
      expect.objectContaining({
        target: HOST,
        actor: 'anonymous',
        detail: {
          lease: id,
          state: 'running',
          reason: 'host taken away',
          authorizedBy: 'open',
          teams: [],
          hold: {
            before: { holdWanted: false, holdReason: '' },
            after: { holdWanted: true, holdReason: 'lease ended by operator' },
          },
        },
      }),
    ]);
  });

  it('takes ending a lease twice as ending it once', async () => {
    const { mock, id, end } = await clocked('verifying');
    await end();

    const again = await end('once more');
    expect(again.message).toContain(`lease ${id} of ${HOST} was already ended by anonymous`);
    expect(again.hosts).toStrictEqual([HOST]);

    const { leases } = await open(mock, 'lighthouse');
    expect(leases.find(l => l.id === id)?.cancelled).toMatchObject({ state: 'verifying', reason: 'host taken away' });
    expect(await endEntries(mock)).toHaveLength(1);
  });

  it('keeps the hold of a host that is held already, and names the cancel in its reason', async () => {
    const { mock, id, end } = await clocked('verifying');
    await call<ActionResult>(mock, 'actions/hold', { hosts: [HOST], reason: 'mine' });

    expect((await end()).message).toBe(`lease ${id} of ${HOST} is ending; the host stays on hold`);
    expect((await endEntries(mock))[0]?.detail).toStrictEqual({
      lease: id,
      state: 'verifying',
      reason: 'host taken away',
      authorizedBy: 'open',
      teams: [],
      hold: {
        before: { holdWanted: true, holdReason: 'mine' },
        after: { holdWanted: true, holdReason: 'mine; lease ended by operator' },
      },
    });
    expect(await call<HostDetail>(mock, `hosts/${HOST}`)).toMatchObject({
      holdWanted: true,
      holdReason: 'mine; lease ended by operator',
    });
  });

  it('refuses to lift the hold while the lease that was ended is open', async () => {
    const { mock, id, end, pass } = await clocked('running', ['running', 'pending']);
    await end();

    const unhold = async () =>
      mock.fetch('/api/v1/actions/unhold', { method: 'POST', body: JSON.stringify({ hosts: [HOST], reason: '' }) });

    const refused = await unhold();
    expect(refused.status).toBe(409);
    expect(await refused.json()).toMatchObject({
      error: expect.stringContaining(`lease ${id} of ${HOST} was ended by anonymous and is still open`) as unknown,
    });
    expect(await call<HostDetail>(mock, `hosts/${HOST}`)).toMatchObject({
      holdWanted: true,
      holdReason: 'lease ended by operator',
    });

    // The agent reports; the lease has ended; the hold can be lifted.
    pass(30);
    expect((await unhold()).status).toBe(200);

    const host = await call<HostDetail>(mock, `hosts/${HOST}`);
    expect(host.holdWanted).toBe(false);
    expect(host.holdReason).toBeUndefined();
  });

  it('puts the hold back when a lease that was ended is ended again and its host is not held', async () => {
    const { mock, id, end } = await clocked('running', ['running']);
    await end();

    // A row of a build that let the hold be lifted.
    const host = mock.data.hosts.get(HOST);
    if (!host) throw new Error('no host');
    host.holdWanted = false;
    host.holdReason = undefined;

    expect((await end()).message).toBe(`lease ${id} of ${HOST} was already ended by anonymous; the host is on hold`);
    expect(await call<HostDetail>(mock, `hosts/${HOST}`)).toMatchObject({
      holdWanted: true,
      holdReason: 'lease ended by operator',
    });
    expect(await endEntries(mock)).toHaveLength(2);
  });

  it('fails a cancelled lease that is verifying at the next pass: its rollout halts and the host is broken', async () => {
    const { mock, id, end, pass } = await clocked('verifying');
    await end();
    pass(5);

    const { rollout, leases, recent } = await open(mock, 'lighthouse');
    expect(leases.some(l => l.id === id)).toBe(false);
    expect(recent.find(l => l.id === id)).toMatchObject({ state: 'failed', error: 'ended by operator' });
    expect(rollout).toMatchObject({ state: 'halted', reason: `${HOST}: ended by operator` });
    expect(member(rollout, HOST)).toMatchObject({ state: 'failed', error: 'ended by operator', leaseId: id });

    // Retried, it goes first.
    expect(await retried(mock, rollout.id)).toMatchObject({
      stage: 1,
      error: `broken since lease ${id} (rollout ${rollout.id})`,
    });
  });

  it('expires a cancelled lease that the agent has not started: nothing halts and the host is not broken', async () => {
    const { mock, id, end, pass } = await clocked('granted');
    const res = await end();
    expect(res.message).toBe(`lease ${id} of ${HOST} is ending; the host is on hold`);

    pass(29);
    const before = await open(mock, 'lighthouse');
    expect(before.leases.find(l => l.id === id)).toMatchObject({ state: 'granted', cancelled: { state: 'granted' } });
    expect((await call<Plan>(mock, 'plan')).hosts?.[HOST]).toMatchObject({ slot: 'leased', leaseId: id });

    // The agent's next sync: it declines the lease it is no longer offered.
    pass(1);
    const { rollout, leases, recent } = await open(mock, 'lighthouse');
    expect(leases.some(l => l.id === id)).toBe(false);
    expect(recent.find(l => l.id === id)).toMatchObject({ state: 'expired' });
    expect(recent.find(l => l.id === id)?.error).toBeUndefined();
    expect(rollout.state).toBe('active');

    const m = member(rollout, HOST);
    expect(m).toMatchObject({ state: 'pending', stage: 3 });
    expect(m?.leaseId).toBeUndefined();
    expect(m?.error).toBeUndefined();

    // Held until a person unholds it.
    const plan = await call<Plan>(mock, 'plan');
    expect(plan.hosts?.[HOST]).toMatchObject({ slot: 'held', reason: 'held' });
    await call<ActionResult>(mock, 'actions/unhold', { hosts: [HOST], reason: 'in order' });
    expect((await call<Plan>(mock, 'plan')).hosts?.[HOST]?.slot).toBe('queued');
  });

  it('lets a cancelled lease of a silent agent run out', async () => {
    const { mock, id, end, pass } = await clocked('granted');
    mock.data.seenAgo.set(HOST, 600);
    await end();

    pass(60);
    expect((await open(mock, 'lighthouse')).leases.some(l => l.id === id)).toBe(true);

    pass(3600);
    expect((await open(mock, 'lighthouse')).recent.find(l => l.id === id)).toMatchObject({ state: 'expired' });
  });

  it('ends a cancelled running lease after the step in progress; one step of the group was applied, so the host is broken', async () => {
    const { mock, id, end, pass } = await clocked('running', ['running', 'pending']);
    await end();

    pass(29);
    expect((await open(mock, 'lighthouse')).leases.find(l => l.id === id)).toMatchObject({ state: 'running' });

    pass(1);
    const { rollout, leases, recent } = await open(mock, 'lighthouse');
    expect(leases.some(l => l.id === id)).toBe(false);

    const ended = recent.find(l => l.id === id);
    expect(ended).toMatchObject({ state: 'failed', error: 'ended by operator', cancelled: { state: 'running' } });
    expect(list(ended?.operation?.steps).map(s => s.status)).toStrictEqual(['done', 'cancelled']);

    // Its member failed and the rollout halts.
    expect(rollout).toMatchObject({ state: 'halted', reason: `${HOST}: ended by operator` });
    expect(member(rollout, HOST)).toMatchObject({ state: 'failed', error: 'ended by operator' });

    // One container of the image was recreated and the host was never shown
    // to be back: retried, it goes first.
    expect(await retried(mock, rollout.id)).toMatchObject({
      state: 'pending',
      stage: 1,
      error: `broken since lease ${id} (rollout ${rollout.id})`,
    });

    // The lease is over: ending it now is refused.
    const res = await mock.fetch('/api/v1/actions/end-lease', {
      method: 'POST',
      body: JSON.stringify({ lease: id, reason: '' }),
    });
    expect(res.status).toBe(409);
    expect(await res.json()).toMatchObject({ error: expect.stringContaining('is failed') as unknown });
  });

  it('does not break the host when nothing of the group was applied before the agent stopped', async () => {
    const { mock, id, end, pass } = await clocked('running', ['superseded', 'pending']);
    const lease = mock.data.leases.find(l => l.id === id);
    if (lease?.operation) lease.operation.disrupted = true;
    await end();
    pass(30);

    const { rollout, recent } = await open(mock, 'lighthouse');
    expect(list(recent.find(l => l.id === id)?.operation?.steps).map(s => s.status)).toStrictEqual([
      'superseded',
      'cancelled',
    ]);
    expect(rollout.state).toBe('halted');

    const m = await retried(mock, rollout.id);
    expect(m).toMatchObject({ state: 'pending', stage: 3 });
    expect(m?.error).toBeUndefined();
  });

  it('breaks the host when the stopped operation had done every step, whenever the lease was ended', async () => {
    const { mock, id, end, pass } = await clocked('running', ['done', 'running']);
    await end();
    pass(30);

    // The change was applied and the host was never shown to be back: the
    // same as for a lease that is ended while it is verified.
    const { rollout, recent } = await open(mock, 'lighthouse');
    expect(recent.find(l => l.id === id)).toMatchObject({ state: 'failed', error: 'ended by operator' });
    expect(rollout).toMatchObject({ state: 'halted', reason: `${HOST}: ended by operator` });
    expect(member(rollout, HOST)).toMatchObject({ state: 'failed', error: 'ended by operator', leaseId: id });

    expect(await retried(mock, rollout.id)).toMatchObject({
      stage: 1,
      error: `broken since lease ${id} (rollout ${rollout.id})`,
    });
  });

  it('breaks the host when a step of a cancelled lease failed', async () => {
    const { mock, id, end, pass } = await clocked('running', ['failed', 'pending']);
    await end();
    pass(30);

    const { rollout } = await open(mock, 'lighthouse');
    expect(rollout.state).toBe('halted');
    expect(await retried(mock, rollout.id)).toMatchObject({
      stage: 1,
      error: `broken since lease ${id} (rollout ${rollout.id})`,
    });
  });

  it('refuses to end a lease that is unknown, unnamed or over', async () => {
    const mock = new MockController();
    const end = async (lease: string) =>
      mock.fetch('/api/v1/actions/end-lease', { method: 'POST', body: JSON.stringify({ lease, reason: '' }) });

    expect((await end('nosuchlease')).status).toBe(404);
    expect((await end('')).status).toBe(400);

    const { recent } = await open(mock, 'lighthouse');
    const over = recent.find(l => l.state === 'released');
    const res = await end(over?.id ?? '');
    expect(res.status).toBe(409);
    expect(await res.json()).toMatchObject({ error: expect.stringContaining('is released') as unknown });
  });
});

describe('MockController, PUT settings', () => {
  const put = async (mock: MockController, body: unknown) =>
    mock.fetch('/api/v1/settings', { method: 'PUT', body: JSON.stringify(body) });

  it('answers 400 for an unknown key, like the controller', async () => {
    const mock = new MockController();
    const { settings } = await call<SettingsBody>(mock, 'settings');

    expect((await put(mock, { ...settings, soakEpochs: 2 })).status).toBe(400);
    expect((await call<SettingsBody>(mock, 'settings')).settings).toStrictEqual(settings);
  });

  it('validates the peer share and the pause switches like the controller', async () => {
    const mock = new MockController();
    const { settings, defaults } = await call<SettingsBody>(mock, 'settings');

    expect(defaults.readyPeerShare).toBe(0);
    expect(settings.readyPeerShare).toBe(0.25);
    expect(defaults.pauseAfterCanaryImage).toBe(false);

    expect((await put(mock, { ...settings, readyPeerShare: 1.5 })).status).toBe(400);
    expect((await put(mock, { ...settings, readyPeerShare: -0.1 })).status).toBe(400);
    expect((await put(mock, { ...settings, readyPeerShare: '0.5' })).status).toBe(400);
    expect((await put(mock, { ...settings, pauseAfterCanaryReboot: 'yes' })).status).toBe(400);
    expect((await put(mock, { ...settings, readyPeerShare: 0, pauseAfterCanaryReboot: true })).status).toBe(200);

    const stored = (await call<SettingsBody>(mock, 'settings')).settings;
    expect(stored.readyPeerShare).toBe(0);
    expect(stored.pauseAfterCanaryReboot).toBe(true);
  });

  it('changes the keys a PUT names and keeps the others, like the controller', async () => {
    const mock = new MockController();
    const { settings } = await call<SettingsBody>(mock, 'settings');

    // The body of a page that knows neither the peer share nor the pause switches.
    const old = Object.fromEntries(
      Object.entries({ ...settings, minPeers: 5 }).filter(
        ([key]) => key !== 'readyPeerShare' && !key.startsWith('pauseAfterCanary')
      )
    );
    expect((await put(mock, old)).status).toBe(200);
    expect((await call<SettingsBody>(mock, 'settings')).settings).toStrictEqual({ ...settings, minPeers: 5 });

    expect((await put(mock, { readyPeerShare: 0.4 })).status).toBe(200);
    expect((await call<SettingsBody>(mock, 'settings')).settings).toStrictEqual({
      ...settings,
      minPeers: 5,
      readyPeerShare: 0.4,
    });

    expect((await put(mock, { readyPeerShare: null })).status).toBe(400);
    expect((await put(mock, { paused: null })).status).toBe(400);
    expect((await put(mock, { minPeers: 0 })).status).toBe(400);
    expect((await call<SettingsBody>(mock, 'settings')).settings.readyPeerShare).toBe(0.4);
  });

  it('answers 400 for a mistyped key instead of failing', async () => {
    const mock = new MockController();
    const { settings } = await call<SettingsBody>(mock, 'settings');

    expect((await put(mock, { ...settings, minPeers: '3' })).status).toBe(400);
    expect((await put(mock, { ...settings, minPeers: 0 })).status).toBe(400);
    expect((await put(mock, null)).status).toBe(400);
    expect((await put(mock, settings)).status).toBe(200);
  });
});
