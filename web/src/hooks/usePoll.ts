import { useCallback, useEffect, useRef, useState } from 'react';

/** The refresh interval of every auto-refreshing view. */
export const POLL_INTERVAL_MS = 5000;

export interface PollState<T> {
  /** data is the last good answer; it stays while a later refresh fails. */
  data: T | undefined;
  /** error is the last refresh error, cleared by the next good answer. */
  error: unknown;
  /** loading is true until the first answer (or error) for the current key. */
  loading: boolean;
  /** updatedAt is when data was last refreshed (ms since epoch). */
  updatedAt: number | undefined;
  /** refresh fetches now, outside the timer. */
  refresh: () => void;
}

export interface PollOptions {
  /** key identifies what is fetched; a change drops the data and refetches. */
  key: string;
  /** enabled false fetches nothing and holds no data. */
  enabled?: boolean;
  /** intervalMs 0 fetches once per key (and on refresh). */
  intervalMs?: number;
}

interface Snapshot<T> {
  key: string;
  data: T | undefined;
  error: unknown;
  loading: boolean;
  updatedAt: number | undefined;
}

/**
 * usePoll fetches on mount, then every intervalMs after the previous answer.
 * Polling pauses while the document is hidden and resumes with an immediate
 * fetch when it becomes visible again. Requests never overlap.
 */
export function usePoll<T>(fetcher: (signal: AbortSignal) => Promise<T>, options: PollOptions): PollState<T> {
  const { key, enabled = true, intervalMs = POLL_INTERVAL_MS } = options;

  const fetcherRef = useRef(fetcher);
  useEffect(() => {
    fetcherRef.current = fetcher;
  });

  const [snap, setSnap] = useState<Snapshot<T>>({
    key,
    data: undefined,
    error: undefined,
    loading: enabled,
    updatedAt: undefined,
  });
  const kickRef = useRef<() => void>(() => undefined);

  useEffect(() => {
    if (!enabled) {
      setSnap({ key, data: undefined, error: undefined, loading: false, updatedAt: undefined });
      kickRef.current = () => undefined;
      return;
    }

    // Read through a function: the value changes across an await (cleanup).
    const life = { stopped: false };
    const stopped = () => life.stopped;
    let running = false;
    let again = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | undefined;

    setSnap(prev =>
      prev.key === key && prev.data !== undefined
        ? prev
        : { key, data: undefined, error: undefined, loading: true, updatedAt: undefined }
    );

    const schedule = () => {
      if (stopped() || intervalMs <= 0 || document.hidden) return;
      clearTimeout(timer);
      timer = setTimeout(() => void run(), intervalMs);
    };

    const run = async (): Promise<void> => {
      if (stopped()) return;
      if (running) {
        again = true;
        return;
      }

      running = true;
      clearTimeout(timer);
      controller = new AbortController();

      try {
        const data = await fetcherRef.current(controller.signal);
        if (!stopped()) setSnap({ key, data, error: undefined, loading: false, updatedAt: Date.now() });
      } catch (error) {
        if (!stopped()) setSnap(prev => ({ ...prev, key, error, loading: false }));
      } finally {
        running = false;
      }

      if (again) {
        again = false;
        void run();
      } else {
        schedule();
      }
    };

    const onVisibility = () => {
      if (document.hidden) clearTimeout(timer);
      else void run();
    };

    document.addEventListener('visibilitychange', onVisibility);
    kickRef.current = () => void run();
    void run();

    return () => {
      life.stopped = true;
      clearTimeout(timer);
      controller?.abort();
      document.removeEventListener('visibilitychange', onVisibility);
    };
  }, [key, enabled, intervalMs]);

  const refresh = useCallback(() => {
    kickRef.current();
  }, []);

  // Data of another key is never shown: it belongs to a previous query.
  const current = snap.key === key;

  return {
    data: current ? snap.data : undefined,
    error: current ? snap.error : undefined,
    loading: current ? snap.loading : enabled,
    updatedAt: current ? snap.updatedAt : undefined,
    refresh,
  };
}
