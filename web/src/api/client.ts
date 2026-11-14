import type {
  ActionResult,
  Audit,
  Config,
  ErrorBody,
  Gate,
  EndLeaseRequest,
  HoldRequest,
  HostDetail,
  Hosts,
  Packages,
  Plan,
  PublicHistory,
  PublicHosts,
  PublicImages,
  RebootRequest,
  RollRequest,
  RolloutHostsRequest,
  Rollouts,
  ScopeRequest,
  Settings,
  SettingsBody,
} from './types';

/** ApiError is a non-2xx answer (or a transport failure with status 0). */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

/** TokenSource is what the client needs from the auth layer. */
export interface TokenSource {
  /** required reports whether the API needs a bearer token (an auth provider is configured). */
  required: () => boolean;
  /** getToken returns a fresh token, or null when nobody is logged in. It is asked on every request. */
  getToken: () => Promise<string | null>;
  /**
   * onUnauthorized is called when an authenticated request came back 401, or
   * was refused locally because no token could be had.
   */
  onUnauthorized: () => void;
}

/** openAccess is the token source of a controller that runs without an auth provider. */
export const openAccess: TokenSource = {
  required: () => false,
  getToken: () => Promise.resolve(null),
  onUnauthorized: () => undefined,
};

export type FetchFn = (input: string, init?: RequestInit) => Promise<Response>;

interface RequestOptions {
  /** user marks a route that needs a bearer token when auth is configured. */
  user: boolean;
  method?: 'GET' | 'PUT' | 'POST';
  body?: unknown;
  signal?: AbortSignal;
}

/** Every action route with its request body type (pkg/controller/api/schema.go). */
export interface ActionBodies {
  roll: RollRequest;
  reboot: RebootRequest;
  hold: HoldRequest;
  unhold: HoldRequest;
  forget: HoldRequest;
  'end-lease': EndLeaseRequest;
  pause: ScopeRequest;
  resume: ScopeRequest;
  abort: ScopeRequest;
  skip: RolloutHostsRequest;
  retry: RolloutHostsRequest;
}

export type ActionName = keyof ActionBodies;

/** An action name paired with its body. */
export type ActionCall = { [K in ActionName]: { action: K; body: ActionBodies[K] } }[ActionName];

export interface AuditQuery {
  target?: string;
  action?: string;
  before?: number;
  limit?: number;
}

function query(params: Record<string, string | number | undefined>): string {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '' && v !== 0) q.set(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : '';
}

/**
 * ApiClient talks to the controller API. Public routes never carry a token.
 * User routes carry "Authorization: Bearer <token>" when an auth provider is
 * configured; the token is asked from the TokenSource on every request and a
 * request without one is refused locally instead of being sent.
 */
export class ApiClient {
  private readonly fetchFn: FetchFn;
  private readonly baseUrl: string;
  private tokens: TokenSource;

  constructor(opts: { fetch?: FetchFn; tokens?: TokenSource; baseUrl?: string } = {}) {
    this.fetchFn = opts.fetch ?? ((input, init) => globalThis.fetch(input, init));
    this.tokens = opts.tokens ?? openAccess;
    this.baseUrl = opts.baseUrl ?? '';
  }

  /** setTokenSource swaps the auth layer (used once the config is known). */
  setTokenSource(tokens: TokenSource): void {
    this.tokens = tokens;
  }

  // ------------------------------------------------------------ public

  config(signal?: AbortSignal): Promise<Config> {
    return this.request('/api/v1/config', { user: false, signal });
  }

  publicHosts(signal?: AbortSignal): Promise<PublicHosts> {
    return this.request('/api/v1/public/hosts', { user: false, signal });
  }

  publicImages(signal?: AbortSignal): Promise<PublicImages> {
    return this.request('/api/v1/public/images', { user: false, signal });
  }

  publicNetwork(signal?: AbortSignal): Promise<Gate> {
    return this.request('/api/v1/public/network', { user: false, signal });
  }

  publicHistory(limit?: number, signal?: AbortSignal): Promise<PublicHistory> {
    return this.request(`/api/v1/public/history${query({ limit })}`, { user: false, signal });
  }

  // -------------------------------------------------------------- user

  hosts(signal?: AbortSignal): Promise<Hosts> {
    return this.request('/api/v1/hosts', { user: true, signal });
  }

  host(name: string, signal?: AbortSignal): Promise<HostDetail> {
    return this.request(`/api/v1/hosts/${encodeURIComponent(name)}`, { user: true, signal });
  }

  packages(q: string, limit?: number, signal?: AbortSignal): Promise<Packages> {
    return this.request(`/api/v1/packages${query({ q, limit })}`, { user: true, signal });
  }

  rollouts(limit?: number, signal?: AbortSignal): Promise<Rollouts> {
    return this.request(`/api/v1/rollouts${query({ limit })}`, { user: true, signal });
  }

  plan(signal?: AbortSignal): Promise<Plan> {
    return this.request('/api/v1/plan', { user: true, signal });
  }

  settings(signal?: AbortSignal): Promise<SettingsBody> {
    return this.request('/api/v1/settings', { user: true, signal });
  }

  putSettings(settings: Settings): Promise<SettingsBody> {
    return this.request('/api/v1/settings', { user: true, method: 'PUT', body: settings });
  }

  audit(q: AuditQuery = {}, signal?: AbortSignal): Promise<Audit> {
    return this.request(`/api/v1/audit${query({ ...q })}`, { user: true, signal });
  }

  action(call: ActionCall): Promise<ActionResult> {
    return this.request(`/api/v1/actions/${call.action}`, { user: true, method: 'POST', body: call.body });
  }

  // ---------------------------------------------------------- internal

  private async request<T>(path: string, opts: RequestOptions): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' };

    if (opts.user && this.tokens.required()) {
      const token = await this.tokens.getToken();
      if (!token) {
        // Never send a user request without a token: it can only fail. The
        // auth layer believed somebody was logged in (or the caller would not
        // have asked), so it re-reads the session exactly as after a 401.
        this.tokens.onUnauthorized();
        throw new ApiError(401, 'login required');
      }
      headers.Authorization = `Bearer ${token}`;
    }

    const init: RequestInit = { method: opts.method ?? 'GET', headers, signal: opts.signal };
    if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }

    let res: Response;
    try {
      res = await this.fetchFn(this.baseUrl + path, init);
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') throw err;
      throw new ApiError(0, `controller unreachable: ${err instanceof Error ? err.message : String(err)}`);
    }

    if (!res.ok) {
      if (res.status === 401 && opts.user) this.tokens.onUnauthorized();
      throw new ApiError(res.status, await errorMessage(res));
    }

    try {
      return (await res.json()) as T;
    } catch {
      throw new ApiError(res.status, `invalid JSON answer from ${path}`);
    }
  }
}

async function errorMessage(res: Response): Promise<string> {
  const fallback = `${res.status} ${res.statusText}`.trim();
  try {
    const body = (await res.json()) as Partial<ErrorBody> | null;
    return body && typeof body.error === 'string' && body.error !== '' ? body.error : fallback;
  } catch {
    return fallback;
  }
}

/** errorText renders any thrown value for display. */
export function errorText(err: unknown): string {
  if (err instanceof ApiError) return err.status > 0 ? `${err.message} (HTTP ${err.status})` : err.message;
  if (err instanceof Error) return err.message;
  return String(err);
}
