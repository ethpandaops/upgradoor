import { useEffect, useMemo, useRef, useState } from 'react';

import { list, type Outage, type SlotHistory } from '../api/types';
import { useApp, usePrivateAccess } from '../appContext';
import { useLive } from '../components/Live';
import { liveOutages, mergeSlotRows } from '../lib/slotHistory';
import { useQuery } from './useQuery';

/** A window is asked for again because of what the stream cannot say at most this often. */
const REFETCH_MIN_MS = 15_000;

interface SlotHistoryState {
  data: SlotHistory | undefined;
  /** The spans during which hosts were off, kept current from the stream; undefined without a login. */
  outages: Outage[] | undefined;
  error: unknown;
  loading: boolean;
}

/**
 * useSlotHistory is the one place the participation graph gets its data
 * from: the slot history of the last windowSeconds, with the split per host
 * when the viewer may see it.
 *
 * The window is asked for once, when the graph is shown and when the window
 * changes: it can be hours of slots, served in buckets. After that nothing is
 * fetched. The event stream carries the newest slots, the only ones that
 * still change; each one that settles, and each late inclusion, is merged
 * into the window as it arrives, and the outages are extended from the
 * stream's leases and from the hosts a slot names as having missed. Only a
 * hole in the stream's slots (the page was disconnected for longer than the
 * stream reaches back), and a change to a span that stands for several hosts,
 * ask again.
 */
export function useSlotHistory(windowSeconds: number): SlotHistoryState {
  const { api } = useApp();
  const priv = usePrivateAccess();

  const window = useQuery(s => (priv ? api.slots(windowSeconds, s) : api.publicSlots(windowSeconds, s)), {
    key: `slots/${priv ? 'user' : 'public'}/${windowSeconds}`,
    keepPrevious: true,
  });
  const rows = useLive('slots').data?.rows;

  // What the stream has added to the window so far. It starts again with
  // every answer for the window.
  const [merged, setMerged] = useState<{ base: SlotHistory; history: SlotHistory } | undefined>(undefined);
  const newest = useRef<number | undefined>(undefined);
  const refresh = window.refresh;

  useEffect(() => {
    const base = window.data;
    if (!base) return;

    const first = rows?.[0]?.slot;
    const last = rows?.[rows.length - 1]?.slot;

    // Slots are missing between what was merged and what the stream holds now.
    if (first !== undefined && newest.current !== undefined && first > newest.current + 1) {
      newest.current = last;
      refresh();
      return;
    }
    if (last !== undefined) newest.current = last;

    setMerged(prev => {
      const from = prev?.base === base ? prev.history : base;
      const history = rows ? mergeSlotRows(from, rows) : from;
      return prev?.base === base && prev.history === history ? prev : { base, history };
    });
  }, [window.data, rows, refresh]);

  const data = useMemo(
    () => (merged && merged.base === window.data ? merged.history : window.data),
    [merged, window.data]
  );

  // The outages of the window, brought up to the stream's leases and newest
  // slots. With a large fleet the answer merges spans of several hosts; what
  // happens to one of those the page cannot apply, so it asks for the window
  // again: because something happened, and not more often than now and then.
  const leases = useLive('rollouts', priv).data;
  const hosts = useLive('hosts', priv).data;
  const live = useMemo(() => {
    if (!priv || !data) return undefined;
    const validators = new Map(list(hosts?.hosts).map(h => [h.name, h.validators]));

    return liveOutages({
      history: data,
      rows: rows ?? [],
      leases: [...list(leases?.leases), ...list(leases?.recentLeases)],
      validators: host => validators.get(host) ?? 0,
      nowMs: Date.now(),
    });
  }, [priv, data, rows, leases, hosts]);

  const fetchedAt = useRef(0);
  useEffect(() => {
    fetchedAt.current = Date.now();
  }, [window.data]);
  const stale = live?.stale ?? false;
  useEffect(() => {
    if (!stale) return;
    const timer = setTimeout(refresh, Math.max(0, fetchedAt.current + REFETCH_MIN_MS - Date.now()));
    return () => {
      clearTimeout(timer);
    };
  }, [stale, refresh, window.data]);

  return { data, outages: live?.outages, error: window.error, loading: window.loading };
}
