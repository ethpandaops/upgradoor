import { describe, expect, it } from 'vitest';

import { list, type ActionResult, type Plan, type Rollout, type Rollouts, type SettingsBody } from '../api/types';
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

  it('lets one repair at a time pass the closed gate, whatever the order', async () => {
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

    // Everybody the gate concerns waits for it, but one host that is offline.
    const passing = list(plan.queue).filter(q => clearsByItself(q.wait));
    expect(passing).toHaveLength(1);
    expect(plan.hosts?.[passing[0]?.host ?? '']?.live).toBe(0);
    expect(passing[0]?.wait).not.toBe('order');
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

describe('MockController, PUT settings', () => {
  const put = async (mock: MockController, body: unknown) =>
    mock.fetch('/api/v1/settings', { method: 'PUT', body: JSON.stringify(body) });

  it('answers 400 for an unknown key, like the controller', async () => {
    const mock = new MockController();
    const { settings } = await call<SettingsBody>(mock, 'settings');

    expect((await put(mock, { ...settings, soakEpochs: 2 })).status).toBe(400);
    expect((await call<SettingsBody>(mock, 'settings')).settings).toStrictEqual(settings);
  });

  it('answers 400 for a missing or mistyped key instead of failing', async () => {
    const mock = new MockController();
    const { settings } = await call<SettingsBody>(mock, 'settings');
    const without = Object.fromEntries(Object.entries(settings).filter(([key]) => key !== 'minPeers'));

    expect((await put(mock, without)).status).toBe(400);
    expect((await put(mock, { ...settings, minPeers: '3' })).status).toBe(400);
    expect((await put(mock, { ...settings, minPeers: 0 })).status).toBe(400);
    expect((await put(mock, null)).status).toBe(400);
    expect((await put(mock, settings)).status).toBe(200);
  });
});
