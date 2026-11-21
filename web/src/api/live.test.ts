import { afterEach, describe, expect, it, vi } from 'vitest';

import { MOCK_LOGINS, MockController } from '../mock/mockController';
import { ApiClient, type FetchFn, type TokenSource } from './client';
import { LiveStore } from './live';
import { list } from './types';

const staff: TokenSource = {
  required: () => true,
  getToken: () => Promise.resolve(MOCK_LOGINS.staff.token),
  onUnauthorized: () => undefined,
};

/** setup runs a store against a mock controller and records the streams it opens. */
function setup(opts: { auth?: boolean } = {}) {
  const controller = new MockController({ auth: opts.auth ?? false });
  const paths: string[] = [];
  const fetch: FetchFn = (input, init) => {
    paths.push(input);
    return controller.fetch(input, init);
  };
  const api = new ApiClient({ fetch, tokens: opts.auth ? staff : undefined });
  // Hardly any waiting between attempts.
  const store = new LiveStore(api, { backoffMinMs: 2, backoffMaxMs: 10 });

  return { controller, paths, store };
}

const stores: LiveStore[] = [];
function started(store: LiveStore): LiveStore {
  stores.push(store);
  store.start();
  return store;
}

afterEach(() => {
  for (const store of stores.splice(0)) store.stop();
});

describe('LiveStore', () => {
  it('reads the public stream until it is told that it may read more', async () => {
    const { store, paths } = setup();
    started(store);

    await vi.waitFor(() => {
      expect(store.getStatus().state).toBe('live');
    });
    expect(store.view('public-hosts')?.hosts).toHaveLength(24);
    expect(store.view('hosts')).toBeUndefined();
    expect(store.view('plan')).toBeUndefined();

    store.setPrivate(true);

    await vi.waitFor(() => {
      expect(store.view('hosts')?.hosts).toHaveLength(24);
    });
    expect(store.view('plan')?.queue).toBeDefined();
    expect(paths).toEqual(['/api/v1/public/events', '/api/v1/events']);

    // After a logout nothing of the login stays.
    store.setPrivate(false);
    expect(store.view('hosts')).toBeUndefined();
    expect(store.view('plan')).toBeUndefined();
    expect(store.view('public-hosts')).toBeDefined();
  });

  it('takes pushed changes in, the host table row by row', async () => {
    const { store, controller, paths } = setup({ auth: true });
    store.setPrivate(true);
    started(store);

    await vi.waitFor(() => {
      expect(store.view('hosts')).toBeDefined();
    });
    const before = store.view('hosts');
    const name = list(before?.hosts)[0]?.name ?? '';
    expect(list(before?.hosts).find(h => h.name === name)?.holdWanted).toBe(false);

    const res = await controller.fetch('/api/v1/actions/hold', {
      method: 'POST',
      headers: { Authorization: `Bearer ${MOCK_LOGINS.staff.token}` },
      body: JSON.stringify({ hosts: [name], reason: 'test' }),
    });
    expect(res.status).toBe(200);

    await vi.waitFor(() => {
      expect(list(store.view('hosts')?.hosts).find(h => h.name === name)?.holdWanted).toBe(true);
    });
    expect(store.view('hosts')?.hosts).toHaveLength(24);
    expect(paths).toEqual(['/api/v1/events']);
  });

  it('reopens a stream that ended and starts from a new snapshot', async () => {
    const { store, controller, paths } = setup();
    started(store);
    await vi.waitFor(() => {
      expect(store.getStatus().state).toBe('live');
    });

    controller.setGateFailing(true);
    controller.closeStreams();

    await vi.waitFor(() => {
      expect(paths).toHaveLength(2);
      expect(store.getStatus().state).toBe('live');
    });
    expect(store.view('public-network')?.ok).toBe(false);
  });

  it('keeps what it has and says why while the controller is away', async () => {
    const { store, controller } = setup();
    const fetchFn = vi.spyOn(controller, 'fetch');
    started(store);
    await vi.waitFor(() => {
      expect(store.getStatus().state).toBe('live');
    });

    fetchFn.mockRejectedValue(new TypeError('connection refused'));
    controller.closeStreams();

    await vi.waitFor(() => {
      expect(store.getStatus()).toMatchObject({
        state: 'reconnecting',
        error: { message: expect.stringContaining('connection refused') as string },
      });
    });
    expect(store.view('public-hosts')?.hosts).toHaveLength(24);

    fetchFn.mockRestore();
    await vi.waitFor(() => {
      expect(store.getStatus().state).toBe('live');
    });
  });

  it('asks the stream for the details of the hosts that are watched', async () => {
    const { store, paths } = setup();
    store.setPrivate(true);
    started(store);

    const release = store.watchHost('lighthouse-geth-2');
    const gone = store.watchHost('no-such-host');

    await vi.waitFor(() => {
      expect(store.host('lighthouse-geth-2')?.detail?.name).toBe('lighthouse-geth-2');
    });
    expect(store.host('no-such-host')).toEqual({ gone: true });
    expect(paths.at(-1)).toBe('/api/v1/events?host=lighthouse-geth-2&host=no-such-host');

    release();
    gone();
    expect(store.host('lighthouse-geth-2')).toBeUndefined();
    await vi.waitFor(() => {
      expect(paths.at(-1)).toBe('/api/v1/events');
    });
  });
});
