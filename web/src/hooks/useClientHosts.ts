import { useEffect, useRef } from 'react';

import type { ClientHostsQuery } from '../api/client';
import type { ClientHosts } from '../api/types';
import { useApp } from '../appContext';
import { useDebounced } from './useDebounced';
import { useQuery } from './useQuery';

/** How long a client's version has to stand still before its list is asked for again. */
const SETTLE_MS = 400;

interface ClientHostsState {
  /**
   * page is the last page that arrived. While the next one loads (another
   * page, another filter, a refresh) it stays, so the list does not jump; it
   * names its client, its offset and its version.
   */
  page: ClientHosts | undefined;
  error: unknown;
  /** loading: a page other than the one shown has been asked for and has not arrived. */
  loading: boolean;
  refresh: () => void;
}

/**
 * useClientHosts fetches one page of the hosts that run a client
 * (GET /api/v1/clients/{client}/hosts). The list is not on the event stream;
 * version is the version of the client's row on the stream, and the page is
 * asked for again when it changes: once per change, a little after the last
 * one, and never on a timer.
 */
export function useClientHosts(client: string, query: ClientHostsQuery, version: string | undefined): ClientHostsState {
  const { api } = useApp();
  const states = (query.states ?? []).join(',');
  const key = `client-hosts/${client}/${states}/${query.q ?? ''}/${query.offset ?? 0}/${query.limit ?? 0}`;

  const result = useQuery(signal => api.clientHosts(client, query, signal), {
    key,
    enabled: client !== '',
    keepPrevious: true,
  });

  const settled = useDebounced(version, SETTLE_MS);
  const shown = useRef<string | undefined>(undefined);
  const { refresh, data } = result;

  useEffect(() => {
    shown.current = data?.client === client ? data.version : undefined;
  }, [data, client]);

  // Only a new version asks again, not a page that arrived: a page that is
  // ahead of the stream must not be fetched in a loop until the stream follows.
  useEffect(() => {
    if (settled !== undefined && shown.current !== undefined && shown.current !== settled) refresh();
  }, [settled, refresh]);

  return { page: data, error: result.error, loading: result.loading, refresh };
}
