// Authentication state, driven by the authenticatoor v2 browser client
// (<authProviderUrl>/client-v2.js, window.ethpandaops.authenticatoor).
//
// The auth service owns the session; this store mirrors its "status" events
// and asks it for a token on every API call. No token is ever cached here.

import type { TokenSource } from '../api/client';

/** TokenInfo is what the authenticatoor client reports on every session change. */
export interface AuthTokenInfo {
  status: 'unauthenticated' | 'authenticated' | 'refreshing';
  authenticated: boolean;
  user: string;
  exp: number;
  /** teams and services are the token's claims; a client from before they existed sends neither. */
  teams?: string[];
  services?: string;
}

/** The part of window.ethpandaops.authenticatoor (v2) this UI relies on. */
export interface AuthenticatoorClient {
  addEventListener: (type: 'status', cb: (info: AuthTokenInfo) => void) => void;
  removeEventListener?: (type: 'status', cb: (info: AuthTokenInfo) => void) => void;
  getStatus: () => Promise<AuthTokenInfo>;
  getToken: () => Promise<string | null>;
  login: () => Promise<unknown>;
  logout: () => Promise<unknown>;
  /**
   * hasFeature reports whether the current token grants a feature of a
   * service, default-deny. An authenticatoor from before features serves a
   * client without it: then the login has no feature.
   */
  hasFeature?: (service: string, feature: string) => boolean;
  /** getTeams returns the current token's teams. */
  getTeams?: () => string[];
}

declare global {
  interface Window {
    ethpandaops?: { authenticatoor?: AuthenticatoorClient };
  }
}

export interface AuthState {
  /** ready is false until the config was applied and the first status settled. */
  ready: boolean;
  /** enabled is false in open mode (no auth provider): everything is visible. */
  enabled: boolean;
  loggedIn: boolean;
  user: string | null;
  /** providerError is set when the auth client could not be loaded; the UI then stays public. */
  providerError: string | null;
  /** teams are the teams of the login's token; empty when logged out and in open mode. */
  teams: readonly string[];
  /**
   * services is the token's "services" claim as the client reported it. It
   * is here so that a login whose grants changed is a new state; what the
   * login may do is asked with AuthStore.hasFeature, never read from it.
   */
  services: string;
}

/** teamList keeps the strings of what a client reported as teams. */
function teamList(teams: unknown): string[] {
  return Array.isArray(teams) ? teams.filter((t): t is string => typeof t === 'string') : [];
}

/** canViewPrivate reports whether the authenticated views may be requested. */
export function canViewPrivate(state: AuthState): boolean {
  return state.ready && (!state.enabled || state.loggedIn);
}

export type ScriptLoader = (src: string) => Promise<void>;

/** loadScript appends a script tag and resolves once it has run. */
export const loadScript: ScriptLoader = src =>
  new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = src;
    script.async = true;
    script.onload = () => {
      resolve();
    };
    script.onerror = () => {
      script.remove();
      reject(new Error(`failed to load ${src}`));
    };
    document.head.appendChild(script);
  });

/** NO_CLAIMS is the part of the state of somebody who is not logged in. */
const NO_CLAIMS = { loggedIn: false, user: null, teams: [], services: '' } as const;

const INITIAL: AuthState = { ready: false, enabled: false, providerError: null, ...NO_CLAIMS };

export class AuthStore implements TokenSource {
  private state: AuthState = INITIAL;
  private readonly listeners = new Set<() => void>();
  private client: AuthenticatoorClient | null = null;
  private readonly load: ScriptLoader;
  private starting: Promise<void> | null = null;

  constructor(load: ScriptLoader = loadScript) {
    this.load = load;
  }

  getState = (): AuthState => this.state;

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  /**
   * start applies the controller config. An empty provider URL means open
   * mode. Otherwise the auth client is loaded; when that fails the UI stays
   * in public mode and reports the provider as unreachable.
   */
  start(authProviderUrl: string): Promise<void> {
    // A second call (React StrictMode, a retry) joins the first.
    this.starting ??= this.run(authProviderUrl);
    return this.starting;
  }

  private async run(authProviderUrl: string): Promise<void> {
    if (!authProviderUrl) {
      this.set({ ready: true, enabled: false, providerError: null, ...NO_CLAIMS });
      return;
    }

    const anonymous: AuthState = { ready: true, enabled: true, providerError: null, ...NO_CLAIMS };

    try {
      if (!window.ethpandaops?.authenticatoor) {
        await this.load(authProviderUrl.replace(/\/+$/, '') + '/client-v2.js');
      }

      const client = window.ethpandaops?.authenticatoor;
      if (!client || typeof client.addEventListener !== 'function') {
        throw new Error('client-v2.js did not provide window.ethpandaops.authenticatoor');
      }

      this.client = client;
      client.addEventListener('status', this.applyStatus);
      this.applyStatus(await client.getStatus());
    } catch (err) {
      console.error('auth: provider unreachable', err);
      this.client = null;
      this.set({ ...anonymous, providerError: 'auth provider unreachable' });
    }
  }

  /** stop detaches from the auth client. */
  stop(): void {
    this.client?.removeEventListener?.('status', this.applyStatus);
    this.client = null;
    this.listeners.clear();
  }

  // -------------------------------------------------------- TokenSource

  required = (): boolean => this.state.enabled;

  /** getToken asks the auth client for a fresh token; never cached. */
  getToken = async (): Promise<string | null> => {
    if (!this.state.enabled || !this.client) return null;
    try {
      return (await this.client.getToken()) ?? null;
    } catch {
      return null;
    }
  };

  /** onUnauthorized re-reads the session after a 401. */
  onUnauthorized = (): void => {
    void this.refresh();
  };

  // ------------------------------------------------------------ actions

  /** refresh re-reads the session status from the auth client. */
  async refresh(): Promise<void> {
    if (!this.client) return;
    try {
      this.applyStatus(await this.client.getStatus());
    } catch (err) {
      console.error('auth: status refresh failed', err);
    }
  }

  login(): void {
    this.client?.login().catch((err: unknown) => {
      console.error('auth: login failed', err);
    });
  }

  logout(): void {
    if (!this.client) return;
    this.client.logout().catch((err: unknown) => {
      console.error('auth: logout failed', err);
    });
    // React at once; the status event confirms it.
    this.set({ ...this.state, ...NO_CLAIMS });
  }

  // -------------------------------------------------------- permissions

  /**
   * hasFeature reports whether the login has a feature of a service, as far
   * as the page can tell: everything in open mode, nothing while logged out,
   * nothing when the auth client is too old to answer, and otherwise what
   * the client reads from the token (default-deny). It decides what the page
   * offers; the controller decides what happens.
   */
  hasFeature = (service: string, feature: string): boolean => {
    if (!this.state.enabled) return this.state.ready;
    if (!this.state.loggedIn || !this.client || typeof this.client.hasFeature !== 'function') return false;
    try {
      return this.client.hasFeature(service, feature);
    } catch {
      return false;
    }
  };

  // ----------------------------------------------------------- internal

  private applyStatus = (info: AuthTokenInfo): void => {
    // "refreshing" still reports authenticated while the old token is valid.
    if (!info.authenticated) {
      this.set({ ready: true, enabled: true, providerError: null, ...NO_CLAIMS });
      return;
    }

    this.set({
      ready: true,
      enabled: true,
      loggedIn: true,
      user: info.user || null,
      providerError: null,
      teams: teamList(info.teams ?? this.client?.getTeams?.()),
      services: typeof info.services === 'string' ? info.services : '',
    });
  };

  private set(next: AuthState): void {
    const prev = this.state;
    if (
      prev.ready === next.ready &&
      prev.enabled === next.enabled &&
      prev.loggedIn === next.loggedIn &&
      prev.user === next.user &&
      prev.providerError === next.providerError &&
      prev.services === next.services &&
      prev.teams.join(',') === next.teams.join(',')
    ) {
      return;
    }
    this.state = next;
    for (const l of this.listeners) l();
  }
}
