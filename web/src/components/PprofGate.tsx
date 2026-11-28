import { useEffect, useRef, useState } from 'react';

import type { Config } from '../api/types';
import { useAuthState } from '../appContext';
import type { AuthStore } from '../auth/authStore';
import { useTheme } from '../hooks/useTheme';
import { FEATURE } from '../lib/permissions';
import { Header } from './Header';
import { DEFAULT_SERVICE } from './Permissions';
import { Card, Loading } from './primitives';

// The controller serves Go pprof below PPROF_PATH to logins with the feature
// admin. Tools send the bearer token; a browser without a valid token gets
// this page, which hands the session's token to the server as a cookie scoped
// to PPROF_PATH and reloads. Must match PprofTokenCookie in
// pkg/controller/userapi/pprof.go.
export const PPROF_PATH = '/debug/pprof';
export const PPROF_TOKEN_COOKIE = 'upgradoor_debug_token';

/** ATTEMPT_KEY keeps the tail of the token tried last and when, to stop a reload loop. */
const ATTEMPT_KEY = 'upgradoor.pprofAttempt';
/** A token tried again within this is one the server refused. */
const REFUSED_WITHIN_MS = 15_000;
const MAX_COOKIE_AGE_SECONDS = 30 * 60;

export const REFUSED_MESSAGE = 'The server did not accept your login for the profiling pages.';

/** isPprofPath reports whether the page was loaded as the profiling gate. */
export function isPprofPath(pathname: string): boolean {
  return pathname === PPROF_PATH || pathname.startsWith(PPROF_PATH + '/');
}

/** tokenExpiry is the exp claim of a JWT in ms, undefined when the token is not one. */
export function tokenExpiry(token: string): number | undefined {
  const payload = token.split('.')[1];
  if (!payload) return undefined;
  try {
    const json = atob(payload.replace(/-/g, '+').replace(/_/g, '/'));
    const exp = (JSON.parse(json) as { exp?: unknown }).exp;
    return typeof exp === 'number' ? exp * 1000 : undefined;
  } catch {
    return undefined;
  }
}

/** pprofCookie is the cookie that carries the token; it never outlives the token nor the cap. */
export function pprofCookie(token: string, now: number, https: boolean): string {
  const exp = tokenExpiry(token);
  const remaining = exp === undefined ? MAX_COOKIE_AGE_SECONDS : Math.floor((exp - now) / 1000);
  const maxAge = Math.max(1, Math.min(remaining, MAX_COOKIE_AGE_SECONDS));
  return (
    `${PPROF_TOKEN_COOKIE}=${token}; Path=${PPROF_PATH}; Max-Age=${maxAge}; SameSite=Strict` + (https ? '; Secure' : '')
  );
}

function readAttempt(): string {
  try {
    return sessionStorage.getItem(ATTEMPT_KEY) ?? '';
  } catch {
    return '';
  }
}

function writeAttempt(value: string): void {
  try {
    sessionStorage.setItem(ATTEMPT_KEY, value);
  } catch {
    // Without storage a refused token shows the error after one reload.
  }
}

/** The browser effects of the gate; tests replace them. */
export interface GateEffects {
  setCookie: (cookie: string) => void;
  reload: () => void;
  now: () => number;
  https: () => boolean;
}

const BROWSER: GateEffects = {
  setCookie: cookie => {
    document.cookie = cookie;
  },
  reload: () => {
    window.location.reload();
  },
  now: () => Date.now(),
  https: () => window.location.protocol === 'https:',
};

/**
 * PprofGate is the page served at the profiling URLs to a browser without a
 * valid token. With an admin login it sets the token cookie and reloads; a
 * login made here continues on its own.
 */
export function PprofGate({
  auth,
  config,
  effects = BROWSER,
}: {
  auth: AuthStore;
  config: Config;
  effects?: GateEffects;
}) {
  const authState = useAuthState(auth);
  const { preference, setPreference } = useTheme();
  const [error, setError] = useState<string | null>(null);
  const attempted = useRef(false);

  const service = config.authService || DEFAULT_SERVICE;
  const admin = authState.loggedIn && auth.hasFeature(service, FEATURE.admin);
  const handover = authState.ready && authState.enabled && !authState.providerError && admin;

  useEffect(() => {
    if (!handover || attempted.current) return;
    attempted.current = true;

    void auth.getToken().then(token => {
      if (!token) {
        attempted.current = false;
        setError('Your session did not provide a login token.');
        return;
      }

      const tail = token.slice(-16);
      const now = effects.now();
      const [lastTail, lastAt] = readAttempt().split(':');
      if (lastTail === tail && now - Number(lastAt) < REFUSED_WITHIN_MS) {
        setError(REFUSED_MESSAGE);
        return;
      }
      writeAttempt(`${tail}:${now}`);

      effects.setCookie(pprofCookie(token, now, effects.https()));
      effects.reload();
    });
  }, [handover, auth, effects]);

  let body;
  if (!authState.ready) {
    body = <Loading what="the login" />;
  } else if (!authState.enabled) {
    body = <p>This controller has no login, and it does not serve the profiling pages without one.</p>;
  } else if (authState.providerError) {
    body = (
      <p role="alert" className="text-danger">
        The auth provider could not be loaded: {authState.providerError}
      </p>
    );
  } else if (!authState.loggedIn) {
    body = (
      <p>
        The profiling pages need an admin login. Log in with the button in the header; this page continues on its own
        afterwards.
      </p>
    );
  } else if (!admin) {
    body = (
      <p>
        The profiling pages need the feature <code className="font-mono">{service}:admin</code>, which this login does
        not have.
      </p>
    );
  } else if (error) {
    body = (
      <p role="alert" className="text-danger">
        {error}
      </p>
    );
  } else {
    body = <Loading what="the profiling pages" />;
  }

  return (
    <div className="flex min-h-dvh flex-col bg-background text-foreground">
      <Header config={config} auth={auth} authState={authState} theme={preference} onTheme={setPreference} />
      <main className="grow p-4">
        <Card title="Profiling" className="mx-auto max-w-xl text-sm/6">
          {body}
        </Card>
      </main>
    </div>
  );
}
