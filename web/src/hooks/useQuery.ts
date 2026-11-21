import { useCallback, useEffect, useRef, useState } from 'react';

interface QueryState<T> {
  /** data is the last good answer; it stays while a later fetch fails. */
  data: T | undefined;
  /** error is the last fetch error, cleared by the next good answer. */
  error: unknown;
  /** loading is true until the first answer (or error) for the current key. */
  loading: boolean;
  /** refresh fetches again. */
  refresh: () => void;
}

interface QueryOptions {
  /** key identifies what is fetched; a change drops the data and fetches again. */
  key: string;
  /** enabled false fetches nothing and holds no data. */
  enabled?: boolean;
  /**
   * keepPrevious shows the answer of the key before while the new one loads
   * (a list that is paged or filtered then changes once, not twice).
   */
  keepPrevious?: boolean;
}

interface Snapshot<T> {
  key: string;
  data: T | undefined;
  error: unknown;
  loading: boolean;
}

/**
 * useQuery fetches once per key, and again on refresh. It is for what is
 * asked for on demand (a search, a page of the audit log, the settings
 * form); what changes by itself comes over the event stream (useLive).
 * Requests never overlap.
 */
export function useQuery<T>(fetcher: (signal: AbortSignal) => Promise<T>, options: QueryOptions): QueryState<T> {
  const { key, enabled = true, keepPrevious = false } = options;

  const fetcherRef = useRef(fetcher);
  useEffect(() => {
    fetcherRef.current = fetcher;
  });

  const [snap, setSnap] = useState<Snapshot<T>>({ key, data: undefined, error: undefined, loading: enabled });
  const kickRef = useRef<() => void>(() => undefined);

  useEffect(() => {
    if (!enabled) {
      setSnap({ key, data: undefined, error: undefined, loading: false });
      kickRef.current = () => undefined;
      return;
    }

    // Read through a function: the value changes across an await (cleanup).
    const life = { stopped: false };
    const stopped = () => life.stopped;
    let running = false;
    let again = false;
    let controller: AbortController | undefined;

    setSnap(prev =>
      prev.key === key && prev.data !== undefined
        ? prev
        : {
            key: keepPrevious ? prev.key : key,
            data: keepPrevious ? prev.data : undefined,
            error: undefined,
            loading: true,
          }
    );

    const run = async (): Promise<void> => {
      if (stopped()) return;
      if (running) {
        again = true;
        return;
      }

      running = true;
      controller = new AbortController();

      try {
        const data = await fetcherRef.current(controller.signal);
        if (!stopped()) setSnap({ key, data, error: undefined, loading: false });
      } catch (error) {
        if (!stopped()) setSnap(prev => ({ ...prev, key, error, loading: false }));
      } finally {
        running = false;
      }

      if (again) {
        again = false;
        void run();
      }
    };

    kickRef.current = () => void run();
    void run();

    return () => {
      life.stopped = true;
      controller?.abort();
    };
  }, [key, enabled, keepPrevious]);

  const refresh = useCallback(() => {
    kickRef.current();
  }, []);

  // Data of another key is not shown: it belongs to a previous query. With
  // keepPrevious it stays until this key has an answer.
  const current = snap.key === key;

  return {
    data: current || (keepPrevious && enabled) ? snap.data : undefined,
    error: current ? snap.error : undefined,
    loading: current ? snap.loading : enabled,
    refresh,
  };
}
