// The live views: one event stream from the controller, kept open and
// reopened when it breaks, and the last state of every view it carries.
// Nothing here polls. The stream starts with a snapshot of its tables and
// then carries the rows and cells that changed (api/wire.ts); a reconnect
// starts with a new snapshot, so nothing has to be replayed.

import { ApiError, type ApiClient, type StreamEvent } from './client';
import { PRIVATE_VIEWS, type HostDetail, type LiveView, type LiveViews } from './types';
import { WIRE_VERSION, WireDoc, buildHostDetail, buildViews, without, type DocBody } from './wire';

/** The most host details one stream carries (maxStreamHosts in events.go). */
export const MAX_LIVE_HOSTS = 16;

/** Reconnect delays: doubled from MIN to MAX, with jitter. */
const BACKOFF_MIN_MS = 1000;
const BACKOFF_MAX_MS = 30_000;
/** The controller sends a heartbeat every 15 s; a stream silent for this long is dead. */
const STALL_MS = 45_000;
/** A hidden page keeps its stream for this long, then closes it until it is shown again. */
const HIDDEN_GRACE_MS = 60_000;
/** Opening and closing host details in quick succession reopens the stream once. */
const HOSTS_SETTLE_MS = 150;
/** A stream that the controller ended for an expired token is reopened at once, unless it was this short. */
const EXPIRED_MIN_LIFE_MS = 5000;

export type LiveState = 'connecting' | 'live' | 'reconnecting' | 'paused' | 'stopped';

export interface LiveStatus {
  state: LiveState;
  /** error is why the stream is not live, when it broke. */
  error: unknown;
}

/** HostEntry is what the stream said last about one host's details. */
interface HostEntry {
  detail?: HostDetail;
  /** gone: the controller does not know the host (any more). */
  gone: boolean;
}

interface LiveOptions {
  /** backoffMinMs and backoffMaxMs bound the delay before a broken stream is reopened. */
  backoffMinMs?: number;
  backoffMaxMs?: number;
}

/**
 * LiveStore holds the views and runs the stream. With private access it
 * reads the authenticated stream, otherwise the public one; a change of that
 * (login, logout) reopens the stream and drops what the other one carried.
 */
export class LiveStore {
  private readonly api: ApiClient;
  private readonly backoffMinMs: number;
  private readonly backoffMaxMs: number;
  private readonly listeners = new Set<() => void>();

  private views: Partial<LiveViews> = {};
  private hosts = new Map<string, HostEntry>();
  /** The tables of the stream as it last described them, and what reads them. */
  private doc = new WireDoc('public');
  private strings: string[] = [''];
  private version = 0;
  private details = new Map<string, WireDoc | null>();
  private status: LiveStatus = { state: 'stopped', error: undefined };
  /** synced: the snapshot of the current scope has arrived at least once. */
  private synced = false;

  private started = false;
  private priv = false;
  private readonly watched = new Map<string, number>();
  /** generation counts the streams opened; a stream that is not the current one stops. */
  private generation = 0;
  private abort: AbortController | undefined;
  private stall: ReturnType<typeof setTimeout> | undefined;
  private hidden: ReturnType<typeof setTimeout> | undefined;
  private settle: ReturnType<typeof setTimeout> | undefined;
  private wake: (() => void) | undefined;

  constructor(api: ApiClient, options: LiveOptions = {}) {
    this.api = api;
    this.backoffMinMs = options.backoffMinMs ?? BACKOFF_MIN_MS;
    this.backoffMaxMs = options.backoffMaxMs ?? BACKOFF_MAX_MS;
  }

  // --------------------------------------------------------------- state

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  /** view returns the last state of a view; the same object until it changes. */
  view<K extends LiveView>(name: K): LiveViews[K] | undefined {
    return this.views[name];
  }

  /** host returns what is known of a watched host's details. */
  host(name: string): HostEntry | undefined {
    return this.hosts.get(name);
  }

  getStatus = (): LiveStatus => this.status;

  // ------------------------------------------------------------- control

  /** start opens the stream; stop closes it and keeps the views. */
  start(): void {
    if (this.started) return;
    this.started = true;

    if (typeof document !== 'undefined') document.addEventListener('visibilitychange', this.onVisibility);
    this.open();
  }

  stop(): void {
    if (!this.started) return;
    this.started = false;

    if (typeof document !== 'undefined') document.removeEventListener('visibilitychange', this.onVisibility);
    clearTimeout(this.hidden);
    clearTimeout(this.settle);
    this.close();
    this.setStatus('stopped', undefined);
  }

  /** setPrivate says which stream to read: the authenticated one or the public one. */
  setPrivate(priv: boolean): void {
    if (this.priv === priv) return;
    this.priv = priv;
    this.synced = false;

    if (!priv) {
      // What a login saw does not stay on the page after it.
      this.views = Object.fromEntries(
        Object.entries(this.views).filter(([name]) => !PRIVATE_VIEWS.includes(name as LiveView))
      );
      this.hosts = new Map();
      this.details = new Map();
    }

    this.emit();
    if (this.started) this.open();
  }

  /**
   * watchHost asks for one host's details on the stream; the returned
   * function takes the request back. The stream is reopened with the new set.
   */
  watchHost(name: string): () => void {
    this.watched.set(name, (this.watched.get(name) ?? 0) + 1);
    if (this.watched.get(name) === 1) this.hostsChanged();

    let released = false;
    return () => {
      if (released) return;
      released = true;

      const left = (this.watched.get(name) ?? 1) - 1;
      if (left > 0) {
        this.watched.set(name, left);
        return;
      }

      this.watched.delete(name);
      this.hosts.delete(name);
      this.details.delete(name);
      this.hostsChanged();
    };
  }

  /** watchedHosts are the hosts the stream is asked for: the first MAX_LIVE_HOSTS watched. */
  watchedHosts(): string[] {
    return [...this.watched.keys()].slice(0, MAX_LIVE_HOSTS);
  }

  // ------------------------------------------------------------ internal

  private emit(): void {
    for (const listener of this.listeners) listener();
  }

  private setStatus(state: LiveState, error: unknown): void {
    if (this.status.state === state && this.status.error === error) return;
    this.status = { state, error };
    this.emit();
  }

  private hostsChanged(): void {
    if (!this.started || !this.priv) return;

    clearTimeout(this.settle);
    this.settle = setTimeout(() => {
      if (this.started && this.status.state !== 'paused') this.open(true);
    }, HOSTS_SETTLE_MS);
  }

  private onVisibility = (): void => {
    clearTimeout(this.hidden);

    if (document.hidden) {
      this.hidden = setTimeout(() => {
        this.close();
        this.setStatus('paused', undefined);
      }, HIDDEN_GRACE_MS);
    } else if (this.started && this.status.state === 'paused') {
      this.open();
    }
  };

  private close(): void {
    this.generation++;
    clearTimeout(this.stall);
    this.abort?.abort();
    this.abort = undefined;
    this.wake?.();
  }

  /**
   * open closes the current stream, if any, and runs a new one. quiet keeps
   * the status as it is: the stream is reopened on purpose, not because it
   * broke.
   */
  private open(quiet = false): void {
    this.close();
    if (!quiet || this.status.state !== 'live') {
      this.setStatus(this.synced ? 'reconnecting' : 'connecting', undefined);
    }
    void this.run(this.generation);
  }

  /** sleep waits before the next attempt; close() ends the wait early. */
  private sleep(ms: number): Promise<void> {
    return new Promise(resolve => {
      const done = () => {
        clearTimeout(timer);
        this.wake = undefined;
        resolve();
      };
      const timer = setTimeout(done, ms);
      this.wake = done;
    });
  }

  private watch(controller: AbortController): void {
    clearTimeout(this.stall);
    this.stall = setTimeout(() => {
      controller.abort();
    }, STALL_MS);
  }

  /** run keeps one stream open, reopening it with a growing delay, until it is replaced. */
  private async run(generation: number): Promise<void> {
    let attempt = 0;

    while (this.generation === generation) {
      const controller = new AbortController();
      this.abort = controller;
      this.watch(controller);

      const opened = Date.now();
      // Read through an object: it is written from the event callback.
      const seen = { expired: false };
      let error: unknown;

      try {
        await this.api.events({
          user: this.priv,
          hosts: this.priv ? this.watchedHosts() : [],
          signal: controller.signal,
          onActivity: () => {
            if (this.generation === generation) this.watch(controller);
          },
          onEvent: event => {
            if (this.generation !== generation) return;
            if (event.event === 'expired') seen.expired = true;
            if (event.event === 'snap') attempt = 0;
            this.apply(event);
          },
        });
        error = new ApiError(0, 'the controller closed the event stream');
      } catch (err) {
        error =
          err instanceof DOMException && err.name === 'AbortError' ? new ApiError(0, 'the event stream stalled') : err;
      }

      if (this.generation !== generation) return;
      clearTimeout(this.stall);

      // A token that ran out is replaced by the next request for one.
      if (seen.expired && Date.now() - opened >= EXPIRED_MIN_LIFE_MS) {
        this.setStatus('reconnecting', undefined);
        continue;
      }

      this.setStatus('reconnecting', error);

      const delay = Math.min(this.backoffMaxMs, this.backoffMinMs * 2 ** attempt);
      attempt++;
      await this.sleep(delay / 2 + Math.random() * (delay / 2));
    }
  }

  private apply({ event, data }: StreamEvent): void {
    if (event !== 'snap' && event !== 'patch') return;

    const body = data as StreamBody;
    const { v, i, s: strings, d: details } = body;
    const tables = without(body, 'v', 'i', 's', 'd', 't');

    if (event === 'snap') {
      // A controller that encodes differently was deployed after this page was loaded.
      if (v !== WIRE_VERSION) throw new ApiError(0, 'the controller was updated: reload the page');

      this.doc = new WireDoc(this.priv ? '' : 'public');
      this.strings = strings ?? [''];
      this.version = i ?? 0;
      this.details = new Map();
      this.synced = true;
      this.status = { state: 'live', error: undefined };
    } else {
      // A patch makes the version after the one it applies to. A gap means
      // that one was missed: start again from a snapshot.
      if (i !== undefined && i !== this.version + 1) throw new ApiError(0, 'the event stream skipped an update');
      if (i !== undefined) this.version = i;
      if (strings) this.strings = [...this.strings, ...strings];
    }

    this.doc.apply(tables as DocBody, event === 'snap');

    for (const [name, detail] of Object.entries(details ?? {})) {
      if (detail === 0) {
        this.details.set(name, null);
        continue;
      }

      const known = this.details.get(name);
      const doc = known ?? new WireDoc('d');
      doc.apply(detail, !known);
      this.details.set(name, doc);
    }

    this.views = buildViews(this.doc, this.strings, this.priv, Date.now());
    this.hosts = new Map(
      [...this.details].map(([name, doc]): [string, HostEntry] => [
        name,
        doc ? { detail: buildHostDetail(doc), gone: false } : { gone: true },
      ])
    );

    this.emit();
  }
}

/** StreamBody is the data of a "snap" or "patch" event: its tables by name, and the keys below (events.go). */
interface StreamBody {
  /** v: the version of the encoding (snapshot). */
  v?: string;
  /** i: the version of the tables after this event. */
  i?: number;
  /** t: the controller's clock in seconds (snapshot). */
  t?: number;
  /** s: the string table (snapshot), or what is new in it. */
  s?: string[];
  /** d: host details by host name; 0 for a host the controller does not know. */
  d?: Record<string, DocBody | 0>;
  [table: string]: unknown;
}
