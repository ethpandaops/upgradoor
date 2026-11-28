import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { errorText, type SSHLoginQuery } from '../api/client';
import { list, type SSHLoginCluster, type SSHLoginGroup, type SSHLogins } from '../api/types';
import { useApp } from '../appContext';
import { Icon } from '../components/icons';
import { useLive } from '../components/Live';
import { Duration, GroupsTable, HostLink, Identity, TimeRange } from '../components/SSHLogins';
import {
  Button,
  Empty,
  ErrorBanner,
  INPUT_CLASS,
  Loading,
  Segmented,
  Table,
  Td,
  Th,
  cx,
} from '../components/primitives';
import { plural } from '../lib/format';
import {
  DEFAULT_SSH_FILTER,
  clusterQuery,
  sshApiQuery,
  sshFilterFromParams,
  sshFilterToParams,
  type SSHFilter,
  type SSHView,
} from '../lib/sshLogins';
import { useHashQuery } from '../hooks/useHashQuery';
import { Field, TextFilter } from './AuditTab';

const PAGE_SIZE = 50;
const CLUSTER_PAGE_SIZE = 100;

const VIEW_OPTIONS: readonly { value: SSHView; label: string; title: string }[] = [
  {
    value: 'fleet',
    label: 'Fleet',
    title: 'One row per person and login user, the hosts of overlapping logins together',
  },
  { value: 'groups', label: 'Per host', title: 'One row per login group of a host' },
];

interface Paged<T> {
  items: T[];
  next: string;
  loading: boolean;
  error: unknown;
  loadMore: () => void;
  reload: () => void;
}

/**
 * usePaged loads the first page of a query and further pages on demand,
 * through the cursor each page gives. A new key starts over; the items of the
 * key before stay, dimmed by the caller, until the first page arrives.
 */
function usePaged<T>(
  key: string,
  fetch: (cursor: string, signal: AbortSignal) => Promise<{ items: T[]; next: string }>
): Paged<T> & { current: boolean } {
  const [state, setState] = useState<{ key: string; items: T[]; next: string }>({ key: '', items: [], next: '' });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(undefined);
  const [generation, setGeneration] = useState(0);
  const fetchRef = useRef(fetch);
  useEffect(() => {
    fetchRef.current = fetch;
  });

  useEffect(() => {
    const ctl = new AbortController();
    setLoading(true);
    setError(undefined);
    fetchRef
      .current('', ctl.signal)
      .then(page => {
        setState({ key, items: page.items, next: page.next });
      })
      .catch((err: unknown) => {
        if (!ctl.signal.aborted) setError(err);
      })
      .finally(() => {
        if (!ctl.signal.aborted) setLoading(false);
      });
    return () => {
      ctl.abort();
    };
  }, [key, generation]);

  const stateRef = useRef(state);
  useEffect(() => {
    stateRef.current = state;
  });

  const loadMore = useCallback(() => {
    const { key: at, next } = stateRef.current;
    if (at !== key || next === '' || loading) return;
    setLoading(true);
    setError(undefined);
    fetchRef
      .current(next, new AbortController().signal)
      .then(page => {
        setState(prev =>
          prev.key === at ? { key: at, items: [...prev.items, ...page.items], next: page.next } : prev
        );
      })
      .catch((err: unknown) => {
        setError(err);
      })
      .finally(() => {
        setLoading(false);
      });
  }, [key, loading]);

  const reload = useCallback(() => {
    setGeneration(g => g + 1);
  }, []);

  const current = state.key === key;
  return { items: state.items, next: current ? state.next : '', loading, error, loadMore, reload, current };
}

/** useSSHPage pages GET /api/v1/ssh-logins for one query. */
function useSSHPage<T>(q: SSHLoginQuery, limit: number, pick: (page: SSHLogins) => T[]) {
  const { api } = useApp();
  const key = JSON.stringify(q);
  const pickRef = useRef(pick);
  useEffect(() => {
    pickRef.current = pick;
  });
  return usePaged<T>(key, async (cursor, signal) => {
    const page = await api.sshLogins({ ...q, cursor, limit }, signal);
    return { items: pickRef.current(page), next: page.next };
  });
}

/** Row is one row of the page, of the view it was loaded for. */
type Row = { kind: 'group'; group: SSHLoginGroup } | { kind: 'cluster'; cluster: SSHLoginCluster };

const pickGroups = (p: SSHLogins) => [...list(p.groups)];
const pickRows = (p: SSHLogins): Row[] => [
  ...list(p.groups).map(group => ({ kind: 'group' as const, group })),
  ...list(p.clusters).map(cluster => ({ kind: 'cluster' as const, cluster })),
];

function LoadMore({ paged, what }: { paged: Paged<unknown>; what: string }) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      {paged.next !== '' ? (
        <Button onClick={paged.loadMore} disabled={paged.loading}>
          {paged.loading ? 'Loading…' : `Load more ${what}`}
        </Button>
      ) : (
        <span className="text-xs/5 text-muted">That is all.</span>
      )}
      {paged.error !== undefined && paged.items.length > 0 && (
        <span role="alert" className="text-xs/5 text-danger">
          Could not load more: {errorText(paged.error)}
        </span>
      )}
    </div>
  );
}

/** Hosts names a cluster's hosts: a few by name, a crowd by its number. */
function Hosts({ c }: { c: SSHLoginCluster }) {
  const names = list(c.hostNames);
  if (c.hosts <= 2 && names.length === c.hosts) {
    return (
      <span className="inline-flex flex-wrap gap-x-2">
        {names.map(n => (
          <HostLink key={n} name={n} />
        ))}
      </span>
    );
  }
  return (
    <span className="font-medium tabular-nums" title={`${names.join(', ')}${c.hosts > names.length ? ', …' : ''}`}>
      {plural(c.hosts, 'host')}
    </span>
  );
}

function FromList({ from }: { from: readonly string[] }) {
  const [first, ...rest] = from;
  return (
    <span className="font-mono" title={from.join(', ')}>
      {first}
      {rest.length > 0 && <span className="font-sans text-muted"> +{rest.length}</span>}
    </span>
  );
}

/** ClusterGroups loads the groups of one cluster, host by host. */
function ClusterGroups({ c }: { c: SSHLoginCluster }) {
  const q = useMemo(() => clusterQuery(c), [c]);
  const paged = useSSHPage(q, CLUSTER_PAGE_SIZE, pickGroups);

  if (paged.error !== undefined && paged.items.length === 0)
    return <ErrorBanner error={paged.error} what="the hosts of the logins" onRetry={paged.reload} />;
  if (!paged.current) return <Loading what="the hosts of the logins" />;

  return (
    <div className="flex flex-col gap-2">
      <GroupsTable groups={paged.items} label={`Hosts of the logins of ${c.identity ?? c.fingerprint ?? c.user}`} />
      {paged.next !== '' && <LoadMore paged={paged} what="hosts" />}
    </div>
  );
}

function clusterKey(c: SSHLoginCluster): string {
  return `${c.identity ?? c.fingerprint ?? c.keyType ?? ''}|${c.user}|${c.firstLogin}`;
}

function ClustersTable({ clusters }: { clusters: readonly SSHLoginCluster[] }) {
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const toggle = (key: string) => {
    setExpanded(prev => {
      const next = new Set(prev);
      if (!next.delete(key)) next.add(key);
      return next;
    });
  };

  return (
    <Table label="SSH logins across the fleet" stack>
      <thead>
        <tr>
          <Th tight>
            <span className="sr-only">Expand</span>
          </Th>
          <Th>Time</Th>
          <Th>Identity</Th>
          <Th>User</Th>
          <Th>Hosts</Th>
          <Th>From</Th>
          <Th className="text-right">Sessions</Th>
          <Th>Duration</Th>
        </tr>
      </thead>
      <tbody>
        {clusters.map(c => {
          const key = clusterKey(c);
          const open = expanded.has(key);
          return (
            <Fragment key={key}>
              <tr className={cx(open && 'bg-surface-raised')}>
                <Td tight data-stack-hidden>
                  <button
                    type="button"
                    aria-expanded={open}
                    aria-label={open ? 'Hide the hosts' : 'Show the hosts'}
                    onClick={() => {
                      toggle(key);
                    }}
                    className="rounded-xs p-1 text-muted hover:bg-surface-hover hover:text-foreground"
                  >
                    <Icon name="chevron" className={cx('size-3.5 transition-transform', !open && '-rotate-90')} />
                  </button>
                </Td>
                <Td data-label="Time">
                  <TimeRange first={c.firstLogin} last={c.lastLogout} open={c.open} />
                </Td>
                <Td data-label="Identity">
                  <Identity identity={c.identity} fingerprint={c.fingerprint} keyType={c.keyType} />
                </Td>
                <Td data-label="User" className="font-mono">
                  {c.user}
                </Td>
                <Td data-label="Hosts">
                  <span className="inline-flex items-center gap-2">
                    <Hosts c={c} />
                    <button
                      type="button"
                      aria-expanded={open}
                      onClick={() => {
                        toggle(key);
                      }}
                      className="text-accent hover:underline sm:hidden"
                    >
                      {open ? 'hide' : 'show'}
                    </button>
                  </span>
                </Td>
                <Td data-label="From">
                  <FromList from={list(c.from)} />
                </Td>
                <Td data-label="Sessions" className="tabular-nums sm:text-right">
                  {c.sessions.toLocaleString()}
                </Td>
                <Td data-label="Duration">
                  <Duration item={c} />
                </Td>
              </tr>
              {open && (
                <tr>
                  <td colSpan={8} className="border-b border-border bg-surface-raised p-2 sm:pl-8">
                    <ClusterGroups c={c} />
                  </td>
                </tr>
              )}
            </Fragment>
          );
        })}
      </tbody>
    </Table>
  );
}

/**
 * SSHLoginsTab shows the SSH logins the hosts reported: by default across the
 * fleet, where one person's overlapping logins on many hosts (an Ansible run)
 * are one row that unfolds into its hosts, or one row per host's login group.
 * The filter is kept in the URL hash; the newest page is asked for again when
 * the audit log grows, which is when a host delivers a closed group.
 */
export function SSHLoginsTab() {
  const [hash, writeHash] = useHashQuery('ssh');
  const filter = useMemo(() => sshFilterFromParams(hash?.params ?? new URLSearchParams()), [hash]);
  const filterRef = useRef(filter);
  useEffect(() => {
    filterRef.current = filter;
  });
  const update = useCallback(
    (patch: Partial<SSHFilter>) => {
      writeHash('', sshFilterToParams({ ...filterRef.current, ...patch }));
    },
    [writeHash]
  );

  const query = useMemo(() => sshApiQuery(filter), [filter]);
  const paged = useSSHPage(query, PAGE_SIZE, pickRows);
  const rows = paged.items;
  const shownGroups = rows.flatMap(r => (r.kind === 'group' ? [r.group] : []));
  const shownClusters = rows.flatMap(r => (r.kind === 'cluster' ? [r.cluster] : []));
  const view: SSHView = shownClusters.length > 0 ? 'fleet' : shownGroups.length > 0 ? 'groups' : filter.view;

  const latestId = useLive('audit').data?.latestId;
  const seenId = useRef(latestId);
  const reload = paged.reload;
  useEffect(() => {
    if (latestId === seenId.current) return;
    const first = seenId.current === undefined;
    seenId.current = latestId;
    if (!first) reload();
  }, [latestId, reload]);

  const filtered = sshFilterToParams({ ...filter, view: 'fleet' }).toString() !== '';

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-3">
      <section
        aria-label="Filters"
        className="flex flex-col gap-3 rounded-lg border border-border bg-surface p-3 sm:p-4"
      >
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          <div className="max-sm:w-full max-sm:[&>div]:flex max-sm:[&>div]:w-full max-sm:[&_button]:flex-1">
            <Segmented
              label="View"
              options={VIEW_OPTIONS}
              value={filter.view}
              onChange={view => {
                update({ view });
              }}
            />
          </div>
          <label className="flex items-center gap-1.5 text-xs/5 text-muted">
            <input
              type="checkbox"
              checked={filter.open}
              onChange={e => {
                update({ open: e.target.checked });
              }}
              className="accent-accent max-sm:size-4"
            />
            Logged in now
          </label>
          <label className="flex items-center gap-1.5 text-xs/5 text-muted">
            <input
              type="checkbox"
              checked={filter.unknown}
              onChange={e => {
                update({ unknown: e.target.checked });
              }}
              className="accent-accent max-sm:size-4"
            />
            Unknown keys only
          </label>
          {filtered && (
            <Button
              dense
              variant="ghost"
              className="ml-auto"
              onClick={() => {
                writeHash('', sshFilterToParams({ ...DEFAULT_SSH_FILTER, view: filter.view }));
              }}
            >
              Reset
            </Button>
          )}
        </div>
        <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
          <Field label="Identity">
            <TextFilter
              type="text"
              label="Identity"
              placeholder="github:name"
              value={filter.identity}
              onCommit={identity => {
                update({ identity });
              }}
            />
          </Field>
          <Field label="Login user">
            <TextFilter
              type="text"
              label="Login user"
              placeholder="devops"
              value={filter.user}
              onCommit={user => {
                update({ user });
              }}
            />
          </Field>
          <Field label="Host" className="col-span-2 sm:col-span-1">
            <TextFilter
              type="text"
              label="Host"
              placeholder="Host name"
              value={filter.host}
              onCommit={host => {
                update({ host });
              }}
            />
          </Field>
          <Field label="From">
            <input
              type="datetime-local"
              className={cx(INPUT_CLASS, 'w-full max-sm:h-10')}
              value={filter.from}
              onChange={e => {
                update({ from: e.target.value });
              }}
            />
          </Field>
          <Field label="Until">
            <input
              type="datetime-local"
              className={cx(INPUT_CLASS, 'w-full max-sm:h-10')}
              value={filter.to}
              onChange={e => {
                update({ to: e.target.value });
              }}
            />
          </Field>
        </div>
      </section>

      {paged.error !== undefined && paged.items.length === 0 && (
        <ErrorBanner error={paged.error} what="the SSH logins" onRetry={paged.reload} />
      )}

      {!paged.current && paged.items.length === 0 ? (
        paged.error === undefined && <Loading what="the SSH logins" />
      ) : paged.current && paged.items.length === 0 ? (
        <Empty>{filtered ? 'No SSH login matches the filters.' : 'No host has reported an SSH login yet.'}</Empty>
      ) : (
        <div
          className={cx('flex flex-col gap-2 transition-opacity', !paged.current && 'opacity-50')}
          aria-busy={!paged.current}
        >
          <p className="text-xs/5 text-muted">
            {view === 'fleet'
              ? 'Logins of one identity and login user whose times overlap are one row across hosts; newest first.'
              : 'One row per login group of a host: the sessions of one key, user and address within ten minutes of each other; newest first.'}
          </p>
          {view === 'fleet' ? (
            <ClustersTable clusters={shownClusters} />
          ) : (
            <GroupsTable groups={shownGroups} label="SSH login groups" />
          )}
          {paged.current && <LoadMore paged={paged} what="logins" />}
        </div>
      )}
    </div>
  );
}
