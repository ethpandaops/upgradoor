import { describe, expect, it } from 'vitest';

import type { ActionResult } from '../api/types';
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
      ['a', 'c', 'd', 'h', 'i', 'im', 'le', 'm', 'n', 'pl', 'q', 'ro', 's', 'se', 'sl', 't', 'v'].sort()
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
