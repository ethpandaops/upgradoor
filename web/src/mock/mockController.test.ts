import { describe, expect, it } from 'vitest';

import { list, type ActionResult, type Audit, type Plan } from '../api/types';
import { actorKindOf, auditActionOf, groupOf } from '../lib/auditCatalog';
import { MockController } from './mockController';

async function call<T>(mock: MockController, path: string, body?: unknown, method = 'POST'): Promise<T> {
  const res = await mock.fetch(
    `/api/v1/${path}`,
    body === undefined ? undefined : { method, body: JSON.stringify(body) }
  );
  expect(res.status).toBe(200);
  return (await res.json()) as T;
}

/** readEvents reads what an event stream has sent so far and returns the event names in order. */
async function readEvents(res: Response, until: string): Promise<{ event: string; data: unknown }[]> {
  const reader = res.body?.getReader();
  if (!reader) throw new Error('the stream has no body');

  const decoder = new TextDecoder();
  const events: { event: string; data: unknown }[] = [];
  let text = '';

  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    text += decoder.decode(value, { stream: true });

    for (;;) {
      const end = text.indexOf('\n\n');
      if (end < 0) break;
      const [event, data] = text.slice(0, end).split('\n');
      text = text.slice(end + 2);
      events.push({
        event: (event ?? '').replace('event: ', ''),
        data: JSON.parse((data ?? '').replace('data: ', '')),
      });
    }

    if (events.some(e => e.event === until)) break;
  }

  reader.releaseLock();
  return events;
}

describe('MockController event streams', () => {
  it('sends the public stream the public tables only', async () => {
    const mock = new MockController({ auth: true });
    const res = await mock.fetch('/api/v1/public/events?host=lighthouse-geth-1');
    expect(res.status).toBe(200);

    const [snap] = await readEvents(res, 'snap');
    expect(snap?.event).toBe('snap');
    expect(Object.keys(snap?.data as object).sort()).toEqual(['h', 'i', 'im', 'n', 's', 'sl', 't', 'v']);

    // A slot is its sums: without a login, no row says which host attested.
    const rows = (snap?.data as { sl: { r: unknown[][] } }).sl.r;
    expect(rows.length).toBeGreaterThan(4);
    expect(rows.every(row => row.length <= 4)).toBe(true);

    mock.setGateFailing(true);
    const [patch] = await readEvents(res, 'patch');
    // The gate closed: its row, and what participation it changed.
    expect(Object.keys(patch?.data as object)).toContain('n');
    expect(Object.keys(patch?.data as object)).not.toContain('h');
  });

  it('refuses the authenticated stream without a token', async () => {
    const mock = new MockController({ auth: true });
    expect((await mock.fetch('/api/v1/events')).status).toBe(401);
    expect(mock.openStreams).toBe(0);
  });

  it('sends a snapshot, then the cells an action changed, and closes with the request', async () => {
    const now = Date.now();
    const mock = new MockController({ now: () => now });
    const abort = new AbortController();
    const res = await mock.fetch('/api/v1/events?host=lighthouse-geth-1', { signal: abort.signal });

    const [snap] = await readEvents(res, 'snap');
    expect(Object.keys(snap?.data as object).sort()).toEqual(
      ['a', 'c', 'd', 'h', 'i', 'im', 'le', 'm', 'n', 'pl', 'q', 'ro', 's', 'sl', 't', 'v'].sort()
    );

    // Nothing changed: publishing again says nothing.
    mock.publish();

    await call<ActionResult>(mock, 'actions/hold', { hosts: ['lighthouse-geth-1'], reason: 'test' });

    const [patch] = await readEvents(res, 'patch');
    const data = patch?.data as { i: number; h: { r?: unknown[]; u: unknown[][] }; d: object };
    expect(data.i).toBe(2);
    expect(data.h.r).toBeUndefined();
    expect(data.h.u.map(u => u[0])).toEqual(['lighthouse-geth-1']);
    expect(Object.keys(data.d)).toEqual(['lighthouse-geth-1']);

    expect(mock.openStreams).toBe(1);
    abort.abort();
    expect(mock.openStreams).toBe(0);
  });
});

describe('MockController deployment window', () => {
  it('holds the quiet fleet in its window until it is armed', async () => {
    const mock = new MockController({ fleet: 'quiet' });

    const before = await call<Plan>(mock, 'plan', undefined, 'GET');
    expect(before.deployWindow.open).toBe(true);
    expect(before.deployWindow.armedAt).toBeUndefined();

    const res = await call<ActionResult>(mock, 'actions/arm-deploy-window', { reason: 'deployed' });
    expect(res.message).toBe('armed: the deployment window is over');

    const after = await call<Plan>(mock, 'plan', undefined, 'GET');
    expect(after.deployWindow.open).toBe(false);
    expect(after.deployWindow.armedAt).toBeDefined();

    const again = await call<ActionResult>(mock, 'actions/arm-deploy-window', {});
    expect(again.message).toBe('the deployment window is not open');
  });

  it('makes the hosts of a rollout that is no Ansible deploy wait while it is open', async () => {
    const mock = new MockController();
    const open = await call<Plan>(mock, 'plan', undefined, 'GET');
    expect(open.deployWindow.open).toBe(false);
    expect(list(open.queue).some(q => q.wait === 'deploy-window')).toBe(false);

    mock.openDeployWindow();

    const plan = await call<Plan>(mock, 'plan', undefined, 'GET');
    const waiting = list(plan.queue).filter(q => q.wait === 'deploy-window');
    expect(waiting.length).toBeGreaterThan(0);
    expect(waiting[0]?.reason).toMatch(/^initial deployment window until \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/);
  });
});

describe('MockController audit log', () => {
  it('records only actions the controller records, from every kind of actor', async () => {
    const mock = new MockController({});
    const page = await call<Audit>(mock, 'audit?who=all&limit=100000&actors=1', undefined, 'GET');
    const entries = list(page.entries);

    const unknown = entries.filter(e => !auditActionOf(e.action)).map(e => e.action);
    expect(unknown).toEqual([]);
    expect(new Set(entries.map(e => actorKindOf(e.actor)))).toEqual(new Set(['people', 'scheduler', 'agents', 'ssh']));
    // Mostly the scheduler, a person of every group.
    const scheduler = entries.filter(e => e.actor === 'scheduler').length;
    expect(scheduler).toBeGreaterThan(entries.length / 2);
    const peopleGroups = new Set(entries.filter(e => actorKindOf(e.actor) === 'people').map(e => groupOf(e)));
    expect([...peopleGroups].sort()).toEqual(
      ['access', 'holds', 'host-actions', 'hosts', 'leases', 'overrides', 'rollouts', 'settings', 'sources'].sort()
    );
    expect(page.actors?.agents).toBe(entries.filter(e => e.actor.startsWith('agent:')).length);
    expect(list(page.actors?.people ?? null).map(p => p.actor)).toContain('alice@example.org');
  });

  it('filters and pages as the controller does', async () => {
    const mock = new MockController({});
    const people = await call<Audit>(mock, 'audit?who=people&group=settings&limit=2', undefined, 'GET');
    expect(list(people.entries)).toHaveLength(2);
    expect(list(people.entries).every(e => e.target === 'settings')).toBe(true);
    expect(people.nextBefore).toBeGreaterThan(0);
    expect(people.actors).toBeUndefined();

    const older = await call<Audit>(
      mock,
      `audit?who=people&group=settings&before=${people.nextBefore}`,
      undefined,
      'GET'
    );
    expect(list(older.entries).every(e => e.id < people.nextBefore && e.target === 'settings')).toBe(true);
  });
});
