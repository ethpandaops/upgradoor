import { describe, expect, it, vi } from 'vitest';

import { AuthStore, canViewPrivate, type AuthTokenInfo, type AuthenticatoorClient } from './authStore';

const noScript = () => Promise.reject(new Error('unexpected script load'));

function status(authenticated: boolean, user = 'alice@example.org'): AuthTokenInfo {
  return {
    status: authenticated ? 'authenticated' : 'unauthenticated',
    authenticated,
    user: authenticated ? user : '',
    exp: authenticated ? 2_000_000_000 : 0,
  };
}

function fakeClient(initial: AuthTokenInfo) {
  let current = initial;
  let listener: ((info: AuthTokenInfo) => void) | undefined;
  let tokenCount = 0;

  const client = {
    addEventListener: vi.fn((_type: 'status', cb: (info: AuthTokenInfo) => void) => {
      listener = cb;
    }),
    removeEventListener: vi.fn(),
    getStatus: vi.fn(() => Promise.resolve(current)),
    getToken: vi.fn(() => Promise.resolve(current.authenticated ? `token-${++tokenCount}` : null)),
    login: vi.fn(() => Promise.resolve(true)),
    logout: vi.fn(() => Promise.resolve()),
  } satisfies AuthenticatoorClient;

  return {
    client,
    /** push changes the session and fires the status event, like the real client. */
    push: (next: AuthTokenInfo) => {
      current = next;
      listener?.(next);
    },
    /** set changes the session silently (no event). */
    set: (next: AuthTokenInfo) => {
      current = next;
    },
  };
}

describe('AuthStore', () => {
  it('runs open when no auth provider is configured', async () => {
    const load = vi.fn(noScript);
    const store = new AuthStore(load);

    expect(canViewPrivate(store.getState())).toBe(false);
    await store.start('');

    expect(store.getState()).toMatchObject({ ready: true, enabled: false, providerError: null });
    expect(canViewPrivate(store.getState())).toBe(true);
    expect(store.required()).toBe(false);
    expect(await store.getToken()).toBeNull();
    expect(load).not.toHaveBeenCalled();
  });

  it('loads <authProviderUrl>/client-v2.js and mirrors the session', async () => {
    const fake = fakeClient(status(false));
    const load = vi.fn((_src: string) => {
      window.ethpandaops = { authenticatoor: fake.client };
      return Promise.resolve();
    });
    const store = new AuthStore(load);
    const seen = vi.fn();
    store.subscribe(seen);

    await store.start('https://auth.example.org/');

    expect(load).toHaveBeenCalledWith('https://auth.example.org/client-v2.js');
    expect(fake.client.addEventListener).toHaveBeenCalledWith('status', expect.any(Function));
    expect(store.getState()).toMatchObject({ ready: true, enabled: true, loggedIn: false, user: null });
    expect(canViewPrivate(store.getState())).toBe(false);
    expect(store.required()).toBe(true);

    fake.push(status(true));

    expect(store.getState()).toMatchObject({ loggedIn: true, user: 'alice@example.org' });
    expect(canViewPrivate(store.getState())).toBe(true);
    expect(seen).toHaveBeenCalled();

    fake.push(status(false));
    expect(store.getState()).toMatchObject({ loggedIn: false, user: null });
  });

  it('asks the client for a token every time', async () => {
    const fake = fakeClient(status(true));
    window.ethpandaops = { authenticatoor: fake.client };
    const store = new AuthStore(noScript);
    await store.start('https://auth.example.org');

    expect(await store.getToken()).toBe('token-1');
    expect(await store.getToken()).toBe('token-2');
    expect(fake.client.getToken).toHaveBeenCalledTimes(2);
  });

  it('stays public and reports the provider when the client script cannot be loaded', async () => {
    const error = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const store = new AuthStore(() => Promise.reject(new Error('net::ERR_NAME_NOT_RESOLVED')));

    await store.start('https://auth.example.org');

    expect(store.getState()).toMatchObject({
      ready: true,
      enabled: true,
      loggedIn: false,
      providerError: 'auth provider unreachable',
    });
    expect(canViewPrivate(store.getState())).toBe(false);
    expect(await store.getToken()).toBeNull();
    expect(error).toHaveBeenCalled();
  });

  it('stays public when the script loads but provides no client', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const store = new AuthStore(() => Promise.resolve());

    await store.start('https://auth.example.org');

    expect(store.getState().providerError).toBe('auth provider unreachable');
  });

  it('refreshes the status after a 401', async () => {
    const fake = fakeClient(status(true));
    window.ethpandaops = { authenticatoor: fake.client };
    const store = new AuthStore(noScript);
    await store.start('https://auth.example.org');
    expect(store.getState().loggedIn).toBe(true);

    // The session ended without an event (e.g. revoked on the server).
    fake.set(status(false));
    store.onUnauthorized();

    await vi.waitFor(() => {
      expect(store.getState().loggedIn).toBe(false);
    });
    expect(fake.client.getStatus).toHaveBeenCalledTimes(2);
  });

  it('delegates login and logout', async () => {
    const fake = fakeClient(status(true));
    window.ethpandaops = { authenticatoor: fake.client };
    const store = new AuthStore(noScript);
    await store.start('https://auth.example.org');

    store.login();
    expect(fake.client.login).toHaveBeenCalledTimes(1);

    store.logout();
    expect(fake.client.logout).toHaveBeenCalledTimes(1);
    expect(store.getState()).toMatchObject({ loggedIn: false, user: null });
  });

  it('starts once, even when asked twice', async () => {
    const fake = fakeClient(status(false));
    const load = vi.fn(() => {
      window.ethpandaops = { authenticatoor: fake.client };
      return Promise.resolve();
    });
    const store = new AuthStore(load);

    await Promise.all([store.start('https://auth.example.org'), store.start('https://auth.example.org')]);

    expect(load).toHaveBeenCalledTimes(1);
    expect(fake.client.addEventListener).toHaveBeenCalledTimes(1);
  });
});
