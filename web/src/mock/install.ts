// Mock mode for development and screenshots: the UI runs against the
// in-browser MockController instead of a controller.
//
//   VITE_MOCK=1 npm run dev        every page load is mocked (open mode)
//   ?mock                          open mode: no login, everything visible
//   ?mock=auth                     an auth provider is configured; a fake
//                                  authenticatoor client provides Login / Logout
//   ?mock=unreachable              an auth provider is configured but its
//                                  client script cannot be loaded
//   &gate=fail                     start with a failing network gate
//   &fleet=large                   three hundred hosts in the middle of a
//                                  rolling update, some of them offline
//   &fleet=huge                    the same with twelve hundred hosts
//   &fleet=quiet                   the small fleet with nothing going on, in
//                                  its initial deployment window
//   &window=open                   start in the initial deployment window
//   ?mock=auth&as=team             who the fake client is logged in as:
//                                  staff (every feature), team (the team
//                                  features, member of lighthouse) or reader
//                                  (no feature); the header has the same switch
//
// The URL parameters work on the development server and in a VITE_MOCK build
// only (see mode.ts). A normal build ignores them.

import type { FetchFn } from '../api/client';
import type { AuthTokenInfo, AuthenticatoorClient } from '../auth/authStore';
import { MOCK_AUTH_SERVICE, MOCK_LOGINS, MockController, mockHasFeature, type MockProfile } from './mockController';
import type { MockMode } from './mode';

interface MockHandle {
  fetch: FetchFn;
  controller: MockController;
  /** client is the fake auth client of auth mode. */
  client?: FakeAuthenticatoor;
}

/** How often the mock publishes what the passing of time changed. */
const MOCK_SYNC_MS = 5000;

const LOGIN_KEY = 'upgradoor.mock.loggedIn';
const PROFILE_KEY = 'upgradoor.mock.profile';
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

function readProfile(): MockProfile {
  try {
    const stored = sessionStorage.getItem(PROFILE_KEY);
    return isMockProfile(stored) ? stored : 'staff';
  } catch {
    return 'staff';
  }
}

function writeProfile(profile: MockProfile): void {
  try {
    sessionStorage.setItem(PROFILE_KEY, profile);
  } catch {
    // Without storage the choice lasts until the page reloads.
  }
}

/** isMockProfile reports whether a value names one of the mock's logins. */
export function isMockProfile(value: unknown): value is MockProfile {
  return typeof value === 'string' && Object.hasOwn(MOCK_LOGINS, value);
}

/** FakeAuthenticatoor is the fake auth client and the switch of who it is logged in as. */
export interface FakeAuthenticatoor extends AuthenticatoorClient {
  profile: () => MockProfile;
  /** setProfile changes who is logged in: the next token, the features and the teams are that login's. */
  setProfile: (profile: MockProfile) => void;
}

/**
 * fakeAuthenticatoor stands in for <authProviderUrl>/client-v2.js. It is
 * logged in as one of the mock's logins (staff unless told otherwise) and
 * answers hasFeature and getTeams for it, like the real client does from the
 * token.
 */
export function fakeAuthenticatoor(
  initiallyLoggedIn: boolean = readLogin(),
  initialProfile: MockProfile = readProfile()
): FakeAuthenticatoor {
  let loggedIn = initiallyLoggedIn;
  let profile = initialProfile;
  const listeners = new Set<(info: AuthTokenInfo) => void>();

  const login = () => MOCK_LOGINS[profile];

  const info = (): AuthTokenInfo => ({
    status: loggedIn ? 'authenticated' : 'unauthenticated',
    authenticated: loggedIn,
    user: loggedIn ? MOCK_USER : '',
    exp: loggedIn ? Math.floor(Date.now() / 1000) + 3600 : 0,
    teams: loggedIn ? [...login().teams] : [],
    services: loggedIn ? `mock:${profile}` : '',
  });

  const emit = () => {
    for (const l of listeners) l(info());
  };

  const set = (next: boolean) => {
    loggedIn = next;
    writeLogin(next);
    emit();
  };

  return {
    addEventListener: (_type, cb) => {
      listeners.add(cb);
    },
    removeEventListener: (_type, cb) => {
      listeners.delete(cb);
    },
    getStatus: () => Promise.resolve(info()),
    getToken: () => Promise.resolve(loggedIn ? login().token : null),
    login: () => {
      set(true);
      return Promise.resolve(true);
    },
    logout: () => {
      set(false);
      return Promise.resolve();
    },
    hasFeature: (service, feature) => loggedIn && service === MOCK_AUTH_SERVICE && mockHasFeature(login(), feature),
    getTeams: () => (loggedIn ? [...login().teams] : []),
    profile: () => profile,
    setProfile: next => {
      profile = next;
      writeProfile(next);
      emit();
    },
  };
}

/** installMock creates the mock controller and, in auth mode, the fake auth client. */
export function installMock(mode: MockMode, search: string): MockHandle {
  const params = new URLSearchParams(search);
  const fleet = params.get('fleet');
  const controller = new MockController({
    auth: mode !== 'open',
    latencyMs: 120,
    fleet: fleet === 'large' || fleet === 'huge' || fleet === 'quiet' ? fleet : 'small',
  });
  let client: FakeAuthenticatoor | undefined;

  if (params.get('gate') === 'fail') controller.setGateFailing(true);
  if (params.get('window') === 'open') controller.openDeployWindow();

  if (mode === 'auth') {
    const as = params.get('as');
    client = isMockProfile(as) ? fakeAuthenticatoor(readLogin(), as) : fakeAuthenticatoor();
    window.ethpandaops = { ...window.ethpandaops, authenticatoor: client };
  }

  // Nobody syncs with the mock, but its views move with the clock (last
  // seen, the chain's head). Publishing now and then stands for the agents'
  // syncs, so the page gets its updates over the stream as it would live.
  window.setInterval(() => {
    controller.publish();
  }, MOCK_SYNC_MS);

  return { fetch: controller.fetch, controller, client };
}
