// Test harness: the app services wired to the in-browser mock controller,
// with every request recorded.

import { render, type RenderResult } from '@testing-library/react';
import userEvent, { type UserEvent } from '@testing-library/user-event';
import type { ReactNode } from 'react';
import { vi } from 'vitest';

import { ApiClient, type FetchFn } from '../api/client';
import { AppContext } from '../appContext';
import { AuthStore } from '../auth/authStore';
import { LiveProvider } from '../components/Live';
import { PermissionsProvider } from '../components/Permissions';
import type { MockFleet } from '../mock/fixture';
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

export const PUBLIC_PATHS = ['/api/v1/config', '/api/v1/public/events', '/api/v1/public/slots'];

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
 * fakeClock puts the test on fake timers that move only when the test, user
 * events or waitFor move them, so debounces fire at the same point however
 * slow the machine is. waitFor steps the clock as it does Jest's fake timers.
 * setup.ts restores the real timers after the test.
 */
export function fakeClock(): UserEvent {
  vi.useFakeTimers();
  vi.stubGlobal('jest', { advanceTimersByTime: (ms: number) => vi.advanceTimersByTime(ms) });
  return userEvent.setup({ advanceTimers: ms => vi.advanceTimersByTime(ms) });
}

/**
 * createHarness builds the services against a fresh mock controller.
 * mode "open": no auth provider. mode "auth": a fake authenticatoor client,
 * logged in or out.
 */
export async function createHarness(
  opts: { mode?: 'open' | 'auth'; loggedIn?: boolean; profile?: MockProfile; fleet?: MockFleet } = {}
): Promise<Harness> {
  const mode = opts.mode ?? 'open';
  const controller = new MockController({ auth: mode === 'auth', fleet: opts.fleet });
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
      <LiveProvider>
        <PermissionsProvider>{ui}</PermissionsProvider>
      </LiveProvider>
    </AppContext>
  );
}

/** PRIMARY is the query of the primary button of an action dialog: it names what the action does. */
export const PRIMARY = {
  name: /^(Confirm|Roll|Reboot|Hold|Unhold|Forget|Pause|Resume|Abort|Skip|Retry|End the lease|Set the policy|Override image|Override flags|Clear|Approve|Approve now|Arm now|Roll despite network state)$/,
};
