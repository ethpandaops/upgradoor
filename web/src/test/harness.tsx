// Test harness: the app services wired to the in-browser mock controller,
// with every request recorded.

import { render, type RenderResult } from '@testing-library/react';
import type { ReactNode } from 'react';

import { ApiClient, type FetchFn } from '../api/client';
import { AppContext } from '../appContext';
import { AuthStore } from '../auth/authStore';
import { PermissionsProvider } from '../components/Permissions';
import { fakeAuthenticatoor, type FakeAuthenticatoor } from '../mock/install';
import { MOCK_AUTH_SERVICE, MOCK_AUTH_URL, MockController, type MockProfile } from '../mock/mockController';

export interface RecordedCall {
  method: string;
  /** path is the URL path with its query string. */
  path: string;
  authorization: string | null;
  body: unknown;
}

export interface Harness {
  api: ApiClient;
  auth: AuthStore;
  controller: MockController;
  /** client is the fake authenticatoor in auth mode. */
  client: FakeAuthenticatoor | null;
  calls: RecordedCall[];
  /** userCalls are the recorded requests to non-public routes. */
  userCalls: () => RecordedCall[];
}

export const PUBLIC_PATHS = [
  '/api/v1/config',
  '/api/v1/public/hosts',
  '/api/v1/public/images',
  '/api/v1/public/network',
  '/api/v1/public/history',
];

/** recordingFetch wraps a fetch and notes every call. */
export function recordingFetch(inner: FetchFn, calls: RecordedCall[]): FetchFn {
  return (input, init) => {
    calls.push({
      method: (init?.method ?? 'GET').toUpperCase(),
      path: input,
      authorization: new Headers(init?.headers).get('Authorization'),
      body: typeof init?.body === 'string' ? (JSON.parse(init.body) as unknown) : undefined,
    });
    return inner(input, init);
  };
}

/**
 * createHarness builds the services against a fresh mock controller.
 * mode "open": no auth provider. mode "auth": a fake authenticatoor client,
 * logged in or out.
 */
export async function createHarness(
  opts: { mode?: 'open' | 'auth'; loggedIn?: boolean; profile?: MockProfile } = {}
): Promise<Harness> {
  const mode = opts.mode ?? 'open';
  const controller = new MockController({ auth: mode === 'auth' });
  const calls: RecordedCall[] = [];
  const auth = new AuthStore(() => Promise.reject(new Error('no script loading in tests')));
  const api = new ApiClient({ fetch: recordingFetch(controller.fetch, calls), tokens: auth });

  let client: FakeAuthenticatoor | null = null;
  if (mode === 'auth') {
    client = fakeAuthenticatoor(opts.loggedIn ?? false, opts.profile ?? 'staff');
    window.ethpandaops = { authenticatoor: client };
  }

  await auth.start(mode === 'auth' ? MOCK_AUTH_URL : '');

  return {
    api,
    auth,
    controller,
    client,
    calls,
    userCalls: () => calls.filter(c => !PUBLIC_PATHS.includes(c.path.split('?')[0] ?? '')),
  };
}

/** renderWithApp renders below the app context of a harness. */
export function renderWithApp(ui: ReactNode, h: Harness): RenderResult {
  const config = {
    network: 'mock-devnet-0',
    version: 'test',
    authProviderUrl: h.auth.getState().enabled ? MOCK_AUTH_URL : '',
    authService: MOCK_AUTH_SERVICE,
  };
  return render(
    <AppContext value={{ api: h.api, auth: h.auth, config }}>
      <PermissionsProvider>{ui}</PermissionsProvider>
    </AppContext>
  );
}
