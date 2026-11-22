// The live views for the components: the provider that runs the event
// stream, and the hooks that read a view from it.

import { createContext, useContext, useEffect, useState, useSyncExternalStore, type ReactNode } from 'react';

import { errorText } from '../api/client';
import { LiveStore, MAX_LIVE_HOSTS, type LiveState, type LiveStatus } from '../api/live';
import type { HostDetail, LiveView, LiveViews } from '../api/types';
import { useApp, usePrivateAccess } from '../appContext';
import { hasUnsavedWork } from '../hooks/useUnsavedWork';
import { Button, cx } from './primitives';

const LiveContext = createContext<LiveStore | null>(null);

/**
 * LiveProvider opens the one event stream of the page and keeps it open: the
 * authenticated stream for a login (or an open controller), the public one
 * otherwise. Every view below it reads from that stream.
 */
export function LiveProvider({ children }: { children: ReactNode }) {
  const { api } = useApp();
  const priv = usePrivateAccess();
  const [store] = useState(() => new LiveStore(api));

  useEffect(() => {
    store.setPrivate(priv);
  }, [store, priv]);

  useEffect(() => {
    store.start();
    return () => {
      store.stop();
    };
  }, [store]);

  return <LiveContext value={store}>{children}</LiveContext>;
}

function useLiveStore(): LiveStore {
  const store = useContext(LiveContext);
  if (!store) throw new Error('useLive used outside LiveProvider');
  return store;
}

export interface Live<T> {
  /** data is the view as the controller last sent it; it stays while the stream reconnects. */
  data: T | undefined;
  /** error is why there is no data: the stream could not be opened. */
  error: unknown;
  /** loading is true until the view (or an error) arrives. */
  loading: boolean;
}

/** useLiveStatus is the state of the stream itself, for the header. */
function useLiveStatus(): LiveStatus {
  const store = useLiveStore();
  return useSyncExternalStore(store.subscribe, store.getStatus);
}

/**
 * useLive returns one view of the event stream and re-renders when the
 * controller sends it again. enabled false is for a view the login may not
 * see: it holds no data and does not load.
 */
export function useLive<K extends LiveView>(view: K, enabled = true): Live<LiveViews[K]> {
  const store = useLiveStore();
  const data = useSyncExternalStore(store.subscribe, () => store.view(view));
  const status = useSyncExternalStore(store.subscribe, store.getStatus);

  if (!enabled) return { data: undefined, error: undefined, loading: false };

  // While there is data a broken stream is the header's to report: the view
  // keeps what it has and does not move.
  const error = data === undefined ? status.error : undefined;

  return { data, error, loading: data === undefined && error === undefined };
}

interface LiveHost extends Live<HostDetail> {
  /** gone: the controller does not know the host (any more). */
  gone: boolean;
  /** crowded: too many host details are open for one stream; this one is not loaded. */
  crowded: boolean;
}

/** useLiveHost asks the stream for one host's details while the component is mounted. */
export function useLiveHost(name: string): LiveHost {
  const store = useLiveStore();

  useEffect(() => store.watchHost(name), [store, name]);

  const entry = useSyncExternalStore(store.subscribe, () => store.host(name));
  const status = useSyncExternalStore(store.subscribe, store.getStatus);
  const crowded = useSyncExternalStore(
    store.subscribe,
    () => !store.watchedHosts().includes(name) && store.watchedHosts().length >= MAX_LIVE_HOSTS
  );

  const data = entry?.detail;
  const gone = entry?.gone ?? false;
  const error = data === undefined && !gone ? status.error : undefined;

  return { data, gone, crowded, error, loading: data === undefined && !gone && !crowded && error === undefined };
}

const STATE_LABEL: Record<LiveState, string> = {
  connecting: 'connecting',
  live: 'live',
  reconnecting: 'reconnecting',
  paused: 'paused',
  stopped: 'offline',
  outdated: 'outdated',
};

/**
 * LiveIndicator says in the header whether the page is up to date: the event
 * stream is open, or it is being reopened and the views show what they had.
 * Its width is fixed, so a change of state moves nothing.
 */
export function LiveIndicator() {
  const status = useLiveStatus();
  const live = status.state === 'live';
  const waiting = status.state === 'connecting' || status.state === 'reconnecting';

  const title = live
    ? 'Updates arrive as they happen'
    : status.state === 'paused'
      ? 'Updates are paused while the page is in the background'
      : status.state === 'outdated'
        ? 'upgradoor was updated: reload the page'
        : status.error !== undefined
          ? `No updates right now: ${errorText(status.error)}. Trying again.`
          : 'Opening the update stream';

  return (
    <span
      aria-label={`Updates: ${STATE_LABEL[status.state]}`}
      title={title}
      className="hidden w-24 shrink-0 items-center gap-1.5 text-xs/5 text-muted sm:flex"
    >
      <span
        aria-hidden="true"
        className={cx(
          'size-2 shrink-0 rounded-full',
          live
            ? 'bg-success'
            : waiting
              ? 'animate-pulse bg-warning'
              : status.state === 'outdated'
                ? 'bg-danger'
                : 'bg-border-strong'
        )}
      />
      {STATE_LABEL[status.state]}
    </span>
  );
}

/** A stream that is not live for this long gets the stale banner. */
export const STALE_AFTER_MS = 10_000;

/** LiveBanner warns under the header when the views are not current. */
export function LiveBanner() {
  return <LiveNotice status={useLiveStatus()} reload={reloadPage} />;
}

function reloadPage(): void {
  location.reload();
}

/**
 * LiveNotice reloads the page once the controller speaks another wire
 * version, unless that loses unsaved input; then it asks for the reload.
 * A stream that has been reconnecting for STALE_AFTER_MS says since when the
 * views are old.
 */
export function LiveNotice({ status, reload }: { status: LiveStatus; reload: () => void }) {
  const outdated = status.state === 'outdated';
  const { lostAt, waitingSince } = status;
  const [overdue, setOverdue] = useState<number | undefined>(undefined);

  useEffect(() => {
    if (outdated && !hasUnsavedWork()) reload();
  }, [outdated, reload]);

  useEffect(() => {
    if (waitingSince === undefined) return;
    const timer = setTimeout(
      () => {
        setOverdue(waitingSince);
      },
      Math.max(0, waitingSince + STALE_AFTER_MS - Date.now())
    );
    return () => {
      clearTimeout(timer);
    };
  }, [waitingSince]);

  if (outdated) {
    return (
      <div
        role="alert"
        className="flex flex-wrap items-center gap-2 border-b border-danger bg-danger-subtle px-4 py-2 text-sm/6 text-danger"
      >
        <span className="font-medium">upgradoor was updated. Reload to see current data.</span>
        <Button variant="primary" onClick={reload}>
          Reload
        </Button>
      </div>
    );
  }

  if (lostAt === undefined || waitingSince === undefined || overdue !== waitingSince) return null;

  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-x-2 border-b border-warning bg-warning-subtle px-4 py-2 text-sm/6 text-warning"
    >
      <span className="font-medium">
        Not live since {new Date(lostAt).toLocaleTimeString([], { hour12: false })} — showing the last known state.
      </span>
      {status.error !== undefined && <span>{errorText(status.error)}</span>}
    </div>
  );
}
