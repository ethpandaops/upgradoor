import type {
  ActionResult,
  Audit,
  ClientHostState,
  ClientHosts,
  ApproveLeaseRequest,
  ClientPolicyRequest,
  Clients,
  Config,
  DataSources,
  ErrorBody,
  EndLeaseRequest,
  ClientOverrideRequest,
  FlagOverrideRequest,
  HoldRequest,
  HostActionRequest,
  OfflineRequest,
  ImageOverrideRequest,
  Images,
  ImagesRefresh,
  Packages,
  SlotHistory,
  RebootRequest,
  RollRequest,
  RolloutHostsRequest,
  ScopeRequest,
  Settings,
  SettingsBody,
  ValidatorRangesRefresh,
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
const openAccess: TokenSource = {
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

/** One event of an event stream: its name and its decoded JSON data. */
export interface StreamEvent {
  event: string;
  data: unknown;
}

interface StreamOptions {
  /** user asks for the authenticated stream (every view); otherwise the public one. */
  user: boolean;
  /** hosts are the hosts whose details the stream should carry (authenticated stream only). */
  hosts?: readonly string[];
  /** signal ends the stream. */
  signal: AbortSignal;
  onEvent: (event: StreamEvent) => void;
  /** onActivity is called for everything received, heartbeats included. */
  onActivity?: () => void;
}

/**
 * EventParser turns the text of a server-sent event stream into events. It is
 * fed the chunks as they arrive; an event is complete at its blank line.
 */
class EventParser {
  private buffer = '';
  private event = '';
  private data: string[] = [];

  constructor(private readonly onEvent: (event: StreamEvent) => void) {}

  push(chunk: string): void {
    this.buffer += chunk;

    for (;;) {
      const end = this.buffer.indexOf('\n');
      if (end < 0) return;

      const line = this.buffer.slice(0, end).replace(/\r$/, '');
      this.buffer = this.buffer.slice(end + 1);
      this.line(line);
    }
  }

  private line(line: string): void {
    if (line === '') {
      if (this.event !== '' && this.data.length > 0) {
        let data: unknown;
        try {
          data = JSON.parse(this.data.join('\n'));
        } catch {
          throw new ApiError(0, `invalid JSON in the event "${this.event}"`);
        }
        this.onEvent({ event: this.event, data });
      }
      this.event = '';
      this.data = [];
      return;
    }

    // A line that starts with a colon is a comment: the heartbeat.
    if (line.startsWith(':')) return;

    const colon = line.indexOf(':');
    const field = colon < 0 ? line : line.slice(0, colon);
    const value = colon < 0 ? '' : line.slice(colon + 1).replace(/^ /, '');

    if (field === 'event') this.event = value;
    else if (field === 'data') this.data.push(value);
  }
}

/** Every action route with its request body type (pkg/controller/views/schema.go). */
interface ActionBodies {
  roll: RollRequest;
  reboot: RebootRequest;
  hold: HoldRequest;
  unhold: HoldRequest;
  'mark-offline': OfflineRequest;
  'unmark-offline': OfflineRequest;
  forget: HoldRequest;
  'end-lease': EndLeaseRequest;
  pause: ScopeRequest;
  resume: ScopeRequest;
  abort: ScopeRequest;
  skip: RolloutHostsRequest;
  retry: RolloutHostsRequest;
  'client-policy': ClientPolicyRequest;
  'image-override': ImageOverrideRequest;
  'flag-override': FlagOverrideRequest;
  'client-override': ClientOverrideRequest;
  'host-action': HostActionRequest;
  'ignore-gate': ScopeRequest;
  'ignore-budget': ScopeRequest;
  'approve-lease': ApproveLeaseRequest;
}

type ActionName = keyof ActionBodies;

/** An action name paired with its body. */
export type ActionCall = { [K in ActionName]: { action: K; body: ActionBodies[K] } }[ActionName];

/** ClientHostsQuery selects one page of a client's hosts. */
export interface ClientHostsQuery {
  /** states keeps the hosts in one of these states; empty keeps all. */
  states?: readonly ClientHostState[];
  /** q is a part of the host name. */
  q?: string;
  /** override keeps the hosts with that part of an override. */
  override?: 'image' | 'flags';
  offset?: number;
  limit?: number;
}

interface AuditQuery {
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
 * ApiClient talks to the controller API: requests, and the event stream.
 * Public routes never carry a token.
 * User routes carry "Authorization: Bearer <token>" when an auth provider is
 * configured; the token is asked from the TokenSource on every request and a
 * request without one is refused locally instead of being sent.
 */
export class ApiClient {
  private readonly fetchFn: FetchFn;
  private readonly baseUrl: string;
  private readonly tokens: TokenSource;

  constructor(opts: { fetch?: FetchFn; tokens?: TokenSource; baseUrl?: string } = {}) {
    this.fetchFn = opts.fetch ?? ((input, init) => globalThis.fetch(input, init));
    this.tokens = opts.tokens ?? openAccess;
    this.baseUrl = opts.baseUrl ?? '';
  }

  // ------------------------------------------------------------ public

  config(signal?: AbortSignal): Promise<Config> {
    return this.request('/api/v1/config', { user: false, signal });
  }

  /** publicSlots is the fleet-wide slot history of the last windowSeconds. */
  publicSlots(windowSeconds: number, signal?: AbortSignal): Promise<SlotHistory> {
    return this.request(`/api/v1/public/slots${query({ window: windowSeconds })}`, { user: false, signal });
  }

  // -------------------------------------------------------------- user

  /** slots is publicSlots with the split per host. */
  slots(windowSeconds: number, signal?: AbortSignal): Promise<SlotHistory> {
    return this.request(`/api/v1/slots${query({ window: windowSeconds })}`, { user: true, signal });
  }

  clients(signal?: AbortSignal): Promise<Clients> {
    return this.request('/api/v1/clients', { user: true, signal });
  }

  /** clientHosts asks for one page of the hosts that run a client, ordered by name. */
  clientHosts(client: string, page: ClientHostsQuery = {}, signal?: AbortSignal): Promise<ClientHosts> {
    const { states, ...rest } = page;
    return this.request(
      `/api/v1/clients/${encodeURIComponent(client)}/hosts${query({ state: states?.join(','), ...rest })}`,
      { user: true, signal }
    );
  }

  /** clientHostNames asks for the names of every host of a client the filter selects: what a selection of the whole filter acts on. */
  async clientHostNames(client: string, filter: ClientHostsQuery, signal?: AbortSignal): Promise<string[]> {
    const { states, q, override } = filter;
    const page: ClientHosts = await this.request(
      `/api/v1/clients/${encodeURIComponent(client)}/hosts${query({ state: states?.join(','), q, override, names: 'true' })}`,
      { user: true, signal }
    );
    return [...(page.names ?? [])];
  }

  /**
   * packages asks for one page of the fleet's packages: all of them, or those
   * whose name contains q. after is the `next` of the page before.
   */
  packages(page: { q?: string; limit?: number; after?: string }, signal?: AbortSignal): Promise<Packages> {
    return this.request(`/api/v1/packages${query({ ...page })}`, { user: true, signal });
  }

  /**
   * images asks for one page of the images the fleet's containers run: all of
   * them, or those whose reference contains q. It pages like packages.
   */
  images(page: { q?: string; limit?: number; after?: string }, signal?: AbortSignal): Promise<Images> {
    return this.request(`/api/v1/images${query({ ...page })}`, { user: true, signal });
  }

  /** refreshImages asks the controller to look every followed tag up at its registry now. */
  refreshImages(): Promise<ImagesRefresh> {
    return this.request('/api/v1/images/refresh', { user: true, method: 'POST', body: {} });
  }

  settings(signal?: AbortSignal): Promise<SettingsBody> {
    return this.request('/api/v1/settings', { user: true, signal });
  }

  /** putSettings changes the settings the body names; the others stay as they are. */
  putSettings(settings: Partial<Settings>): Promise<SettingsBody> {
    return this.request('/api/v1/settings', { user: true, method: 'PUT', body: settings });
  }

  /** dataSources asks for the beacon endpoints and the validator ranges inventory (admin). */
  dataSources(signal?: AbortSignal): Promise<DataSources> {
    return this.request('/api/v1/settings/sources', { user: true, signal });
  }

  /** refreshValidatorRanges asks the controller to fetch the validator ranges inventory now (admin). */
  refreshValidatorRanges(): Promise<ValidatorRangesRefresh> {
    return this.request('/api/v1/settings/sources/validator-ranges/refresh', { user: true, method: 'POST', body: {} });
  }

  audit(q: AuditQuery = {}, signal?: AbortSignal): Promise<Audit> {
    return this.request(`/api/v1/audit${query({ ...q })}`, { user: true, signal });
  }

  action(call: ActionCall): Promise<ActionResult> {
    return this.request(`/api/v1/actions/${call.action}`, { user: true, method: 'POST', body: call.body });
  }

  // ------------------------------------------------------------ stream

  /**
   * events reads an event stream until it ends. The promise resolves when
   * the server closes the stream and rejects when the stream cannot be
   * opened or breaks; an abort through the signal rejects with the abort.
   *
   * The stream is read with fetch, not with EventSource: an EventSource
   * cannot send the Authorization header, and the token never goes into the
   * URL.
   */
  async events(opts: StreamOptions): Promise<void> {
    const headers = await this.headers(opts.user, 'text/event-stream');

    let path = '/api/v1/public/events';
    if (opts.user) {
      const q = new URLSearchParams();
      for (const host of opts.hosts ?? []) q.append('host', host);
      path = `/api/v1/events${q.size > 0 ? `?${q.toString()}` : ''}`;
    }

    let res: Response;
    try {
      res = await this.fetchFn(this.baseUrl + path, { method: 'GET', headers, signal: opts.signal });
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') throw err;
      throw new ApiError(0, `controller unreachable: ${err instanceof Error ? err.message : String(err)}`);
    }

    if (!res.ok) {
      if (res.status === 401 && opts.user) this.tokens.onUnauthorized();
      throw new ApiError(res.status, await errorMessage(res));
    }

    if (!res.body) throw new ApiError(res.status, 'the event stream has no body');

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    const parser = new EventParser(opts.onEvent);

    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return;

        opts.onActivity?.();
        parser.push(decoder.decode(value, { stream: true }));
      }
    } catch (err) {
      if (err instanceof ApiError || (err instanceof DOMException && err.name === 'AbortError')) throw err;
      if (opts.signal.aborted) throw new DOMException('aborted', 'AbortError');
      throw new ApiError(0, `the event stream broke: ${err instanceof Error ? err.message : String(err)}`);
    } finally {
      void reader.cancel().catch(() => undefined);
    }
  }

  // ---------------------------------------------------------- internal

  /** headers builds the request headers; a user route gets the bearer token when auth is configured. */
  private async headers(user: boolean, accept: string): Promise<Record<string, string>> {
    const headers: Record<string, string> = { Accept: accept };

    if (user && this.tokens.required()) {
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

    return headers;
  }

  private async request<T>(path: string, opts: RequestOptions): Promise<T> {
    const headers = await this.headers(opts.user, 'application/json');

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
