// Mock mode for development and screenshots: the UI runs against the
// in-browser MockController instead of a controller.
//
//   VITE_MOCK=1 npm run dev        every page load is mocked (open mode)
//   VITE_MOCK=1 npm run build      a build that is always mocked
//   ?mock                          open mode: no login, everything visible
//   ?mock=auth                     an auth provider is configured; a fake
//                                  authenticatoor client provides Login / Logout
//   ?mock=unreachable              an auth provider is configured but its
//                                  client script cannot be loaded
//   &gate=fail                     start with a failing network gate
//
// The URL parameters work on the development server and in a VITE_MOCK build
// only (see mode.ts). A normal build ignores them.

import type { FetchFn } from '../api/client';
import type { AuthTokenInfo, AuthenticatoorClient } from '../auth/authStore';
import { MOCK_TOKEN, MockController } from './mockController';
import type { MockMode } from './mode';

export interface MockHandle {
  mode: MockMode;
  fetch: FetchFn;
  controller: MockController;
}

const LOGIN_KEY = 'upgradoor.mock.loggedIn';
const MOCK_USER = 'mock-user@example.org';

function readLogin(): boolean {
  try {
    return sessionStorage.getItem(LOGIN_KEY) === '1';
  } catch {
    return false;
  }
}

function writeLogin(loggedIn: boolean): void {
  try {
    sessionStorage.setItem(LOGIN_KEY, loggedIn ? '1' : '0');
  } catch {
    // Without storage the login lasts until the page reloads.
  }
}

/** fakeAuthenticatoor stands in for <authProviderUrl>/client-v2.js. */
export function fakeAuthenticatoor(initiallyLoggedIn: boolean = readLogin()): AuthenticatoorClient {
  let loggedIn = initiallyLoggedIn;
  const listeners = new Set<(info: AuthTokenInfo) => void>();

  const info = (): AuthTokenInfo => ({
    status: loggedIn ? 'authenticated' : 'unauthenticated',
    authenticated: loggedIn,
    user: loggedIn ? MOCK_USER : '',
    exp: loggedIn ? Math.floor(Date.now() / 1000) + 3600 : 0,
  });

  const set = (next: boolean) => {
    loggedIn = next;
    writeLogin(next);
    for (const l of listeners) l(info());
  };

  return {
    addEventListener: (_type, cb) => {
      listeners.add(cb);
    },
    removeEventListener: (_type, cb) => {
      listeners.delete(cb);
    },
    getStatus: () => Promise.resolve(info()),
    getToken: () => Promise.resolve(loggedIn ? MOCK_TOKEN : null),
    login: () => {
      set(true);
      return Promise.resolve(true);
    },
    logout: () => {
      set(false);
      return Promise.resolve();
    },
  };
}

/** installMock creates the mock controller and, in auth mode, the fake auth client. */
export function installMock(mode: MockMode, search: string): MockHandle {
  const controller = new MockController({ auth: mode !== 'open', latencyMs: 120 });

  if (new URLSearchParams(search).get('gate') === 'fail') controller.setGateFailing(true);

  if (mode === 'auth') {
    window.ethpandaops = { ...window.ethpandaops, authenticatoor: fakeAuthenticatoor() };
  }

  return { mode, fetch: controller.fetch, controller };
}
