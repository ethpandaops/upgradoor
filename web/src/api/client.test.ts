import { describe, expect, it, vi } from 'vitest';

import { AuthStore } from '../auth/authStore';
import { ApiClient, ApiError, errorText, type FetchFn, type StreamEvent, type TokenSource } from './client';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function tokens(over: Partial<TokenSource> = {}): TokenSource & { onUnauthorized: ReturnType<typeof vi.fn> } {
  return {
    required: () => true,
    getToken: () => Promise.resolve('tok-1'),
    onUnauthorized: vi.fn(),
    ...over,
  } as TokenSource & { onUnauthorized: ReturnType<typeof vi.fn> };
}

function headersOf(fetchFn: ReturnType<typeof vi.fn<FetchFn>>, call = 0): Headers {
  return new Headers(fetchFn.mock.calls[call]?.[1]?.headers);
}

describe('ApiClient authentication', () => {
  it('attaches the bearer token to user requests', async () => {
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({ hosts: [] })));
    const api = new ApiClient({ fetch: fetchFn, tokens: tokens() });

    await api.settings();

    expect(fetchFn).toHaveBeenCalledTimes(1);
    expect(fetchFn.mock.calls[0]?.[0]).toBe('/api/v1/settings');
    expect(headersOf(fetchFn).get('Authorization')).toBe('Bearer tok-1');
  });

  it('asks for a token on every request and never caches it', async () => {
    let n = 0;
    const getToken = vi.fn(() => Promise.resolve(`tok-${++n}`));
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({})));
    const api = new ApiClient({ fetch: fetchFn, tokens: tokens({ getToken }) });

    await api.settings();
    await api.settings();
    await api.action({ action: 'pause', body: { rollout: '', reason: '' } });

    expect(getToken).toHaveBeenCalledTimes(3);
    expect(headersOf(fetchFn, 0).get('Authorization')).toBe('Bearer tok-1');
    expect(headersOf(fetchFn, 1).get('Authorization')).toBe('Bearer tok-2');
    expect(headersOf(fetchFn, 2).get('Authorization')).toBe('Bearer tok-3');
  });

  it('never sends a token to public routes', async () => {
    const getToken = vi.fn(() => Promise.resolve('tok-1'));
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({})));
    const api = new ApiClient({ fetch: fetchFn, tokens: tokens({ getToken }) });

    await api.config();
    await api.publicSlots(60);

    expect(getToken).not.toHaveBeenCalled();
    expect(fetchFn.mock.calls.map(c => c[0])).toEqual(['/api/v1/config', '/api/v1/public/slots?window=60']);
    for (let i = 0; i < 2; i++) expect(headersOf(fetchFn, i).has('Authorization')).toBe(false);
  });

  it('sends no Authorization header in open mode', async () => {
    const getToken = vi.fn(() => Promise.resolve('unused'));
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({ hosts: [] })));
    const api = new ApiClient({ fetch: fetchFn, tokens: tokens({ required: () => false, getToken }) });

    await api.settings();

    expect(getToken).not.toHaveBeenCalled();
    expect(headersOf(fetchFn).has('Authorization')).toBe(false);
  });

  it('refuses a user request without a token instead of sending it', async () => {
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({})));
    const api = new ApiClient({ fetch: fetchFn, tokens: tokens({ getToken: () => Promise.resolve(null) }) });

    await expect(api.settings()).rejects.toMatchObject({ name: 'ApiError', status: 401, message: 'login required' });
    expect(fetchFn).not.toHaveBeenCalled();
  });

  it('reports a request refused for a missing token to the token source, like a 401', async () => {
    const t = tokens({ getToken: () => Promise.resolve(null) });
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({})));
    const api = new ApiClient({ fetch: fetchFn, tokens: t });

    await expect(api.settings()).rejects.toMatchObject({ status: 401 });
    expect(t.onUnauthorized).toHaveBeenCalledTimes(1);

    // Public routes need no token and report nothing.
    await api.config();
    expect(t.onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it('falls back to logged out when the session ended silently and no token is left', async () => {
    const info = (authenticated: boolean) => ({
      status: authenticated ? ('authenticated' as const) : ('unauthenticated' as const),
      authenticated,
      user: authenticated ? 'alice@example.org' : '',
      exp: 0,
    });
    let current = info(true);
    const client = {
      addEventListener: vi.fn(),
      getStatus: vi.fn(() => Promise.resolve(current)),
      getToken: vi.fn(() => Promise.resolve<string | null>(null)),
      login: vi.fn(() => Promise.resolve()),
      logout: vi.fn(() => Promise.resolve()),
    };
    window.ethpandaops = { authenticatoor: client };
    const store = new AuthStore(() => Promise.reject(new Error('unexpected script load')));
    await store.start('https://auth.example.org');
    expect(store.getState().loggedIn).toBe(true);
    expect(client.getStatus).toHaveBeenCalledTimes(1);

    // The session ended without a status event; getToken has nothing to give.
    current = info(false);
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({})));
    const api = new ApiClient({ fetch: fetchFn, tokens: store });

    await expect(api.settings()).rejects.toMatchObject({ status: 401, message: 'login required' });
    expect(fetchFn).not.toHaveBeenCalled();
    await vi.waitFor(() => {
      expect(store.getState().loggedIn).toBe(false);
    });
    expect(client.getStatus).toHaveBeenCalledTimes(2);
  });

  it('reports a 401 to the token source and throws the server error', async () => {
    const t = tokens();
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({ error: 'token expired' }, 401)));
    const api = new ApiClient({ fetch: fetchFn, tokens: t });

    const err = await api.settings().catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status: 401, message: 'token expired' });
    expect(t.onUnauthorized).toHaveBeenCalledTimes(1);
  });

  it('does not report other errors as unauthorized', async () => {
    const t = tokens();
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({ error: 'unknown host x' }, 404)));
    const api = new ApiClient({ fetch: fetchFn, tokens: t });

    await expect(api.clientHosts('x')).rejects.toMatchObject({ status: 404, message: 'unknown host x' });
    expect(t.onUnauthorized).not.toHaveBeenCalled();
  });
});

describe('ApiClient requests', () => {
  it('encodes paths and query parameters', async () => {
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({})));
    const api = new ApiClient({ fetch: fetchFn });

    await api.clientHosts('a/b c', { limit: 20 });
    await api.packages({ q: 'lib ssl', limit: 50 });
    await api.audit({ target: 'geth-1', action: 'action.', before: 120, limit: 50 });
    await api.audit();

    expect(fetchFn.mock.calls.map(c => c[0])).toEqual([
      '/api/v1/clients/a%2Fb%20c/hosts?limit=20',
      '/api/v1/packages?q=lib+ssl&limit=50',
      '/api/v1/audit?target=geth-1&action=action.&before=120&limit=50',
      '/api/v1/audit',
    ]);
  });

  it('PUTs settings and POSTs actions as JSON', async () => {
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(json({})));
    const api = new ApiClient({ fetch: fetchFn });
    const body = { rollout: 'ro-1', hosts: ['a'], reason: 'why' };

    await api.action({ action: 'skip', body });

    const init = fetchFn.mock.calls[0]?.[1];
    expect(fetchFn.mock.calls[0]?.[0]).toBe('/api/v1/actions/skip');
    expect(init?.method).toBe('POST');
    expect(new Headers(init?.headers).get('Content-Type')).toBe('application/json');
    expect(JSON.parse(init?.body as string)).toEqual(body);
  });

  it('falls back to the status line when the error body is not JSON', async () => {
    const fetchFn = vi.fn<FetchFn>(() =>
      Promise.resolve(new Response('<html>bad gateway</html>', { status: 502, statusText: 'Bad Gateway' }))
    );
    const api = new ApiClient({ fetch: fetchFn });

    await expect(api.settings()).rejects.toMatchObject({ status: 502, message: '502 Bad Gateway' });
  });

  it('turns a network failure into an ApiError with status 0', async () => {
    const fetchFn = vi.fn<FetchFn>(() => Promise.reject(new TypeError('Failed to fetch')));
    const api = new ApiClient({ fetch: fetchFn });

    const err = await api.settings().catch((e: unknown) => e);

    expect(err).toMatchObject({ status: 0 });
    expect(errorText(err)).toBe('controller unreachable: Failed to fetch');
  });

  it('lets an abort pass through', async () => {
    const fetchFn = vi.fn<FetchFn>(() => Promise.reject(new DOMException('aborted', 'AbortError')));
    const api = new ApiClient({ fetch: fetchFn });

    await expect(api.settings()).rejects.toMatchObject({ name: 'AbortError' });
  });

  it('renders errors for display', () => {
    expect(errorText(new ApiError(409, 'rollout is completed'))).toBe('rollout is completed (HTTP 409)');
    expect(errorText(new Error('boom'))).toBe('boom');
    expect(errorText('text')).toBe('text');
  });
});

/** sse answers with an event stream made of the given chunks, then ends. */
function sse(chunks: string[], status = 200): Response {
  const encoder = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
  return new Response(body, { status, headers: { 'Content-Type': 'text/event-stream' } });
}

describe('ApiClient event stream', () => {
  it('reads events across chunk boundaries and reports heartbeats as activity only', async () => {
    const fetchFn = vi.fn<FetchFn>(() =>
      Promise.resolve(
        sse(['event: public-ho', 'sts\ndata: {"hosts":[]}\n\n: ping\n\nevent: ready\r\ndata: {}\r\n', '\r\n'])
      )
    );
    const api = new ApiClient({ fetch: fetchFn, tokens: tokens() });
    const events: StreamEvent[] = [];
    const onActivity = vi.fn();

    await api.events({
      user: false,
      signal: new AbortController().signal,
      onActivity,
      onEvent: e => events.push(e),
    });

    expect(events).toEqual([
      { event: 'public-hosts', data: { hosts: [] } },
      { event: 'ready', data: {} },
    ]);
    expect(onActivity).toHaveBeenCalledTimes(3);
    expect(fetchFn.mock.calls[0]?.[0]).toBe('/api/v1/public/events');
    expect(headersOf(fetchFn).get('Authorization')).toBeNull();
  });

  it('sends the token in the header of the authenticated stream, never in the URL', async () => {
    const fetchFn = vi.fn<FetchFn>(() => Promise.resolve(sse([])));
    const api = new ApiClient({ fetch: fetchFn, tokens: tokens() });

    await api.events({
      user: true,
      hosts: ['a', 'b c'],
      signal: new AbortController().signal,
      onEvent: () => undefined,
    });

    expect(fetchFn.mock.calls[0]?.[0]).toBe('/api/v1/events?host=a&host=b+c');
    expect(headersOf(fetchFn).get('Authorization')).toBe('Bearer tok-1');
  });

  it('reports a refused stream like a refused request', async () => {
    const t = tokens();
    const api = new ApiClient({
      fetch: () => Promise.resolve(json({ error: 'a valid bearer token is required' }, 401)),
      tokens: t,
    });

    await expect(
      api.events({ user: true, signal: new AbortController().signal, onEvent: () => undefined })
    ).rejects.toMatchObject({ status: 401, message: 'a valid bearer token is required' });
    expect(t.onUnauthorized).toHaveBeenCalledTimes(1);
  });
});
