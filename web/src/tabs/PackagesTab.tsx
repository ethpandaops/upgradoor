import { useEffect, useMemo, useState, type ReactNode } from 'react';

import type { ApiClient } from '../api/client';
import { list, type PackageCounts, type PackageMatch, type PackageShow, type PackageSort } from '../api/types';
import { useApp } from '../appContext';
import { Icon } from '../components/icons';
import {
  Badge,
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
import { RefreshImages } from '../components/RefreshImages';
import { RelTime } from '../components/RelTime';
import { useDebounced } from '../hooks/useDebounced';
import { useHashQuery } from '../hooks/useHashQuery';
import { useHeaderHeight } from '../hooks/useHeaderHeight';
import { useKept } from '../hooks/useKept';
import { useQuery } from '../hooks/useQuery';
import { plural, shortDigest, shortRevision } from '../lib/format';
import {
  PAGE_SIZES,
  START_PLACE,
  placeFromHash,
  placeToHash,
  summarize,
  type PackageSummary,
  type PackagesKind,
  type PackagesPlace,
} from '../lib/packagesView';

const KIND_OPTIONS: readonly { value: PackagesKind; label: string; title: string }[] = [
  { value: 'apt', label: 'APT packages', title: 'The packages installed on the hosts' },
  { value: 'docker', label: 'Docker images', title: 'The images the containers of the hosts run' },
];

/** The state of the tab: the list shown, and the place in each list. */
interface TabState {
  kind: PackagesKind;
  places: Record<PackagesKind, PackagesPlace>;
}

const START: TabState = {
  kind: 'apt',
  places: { apt: START_PLACE, docker: { ...START_PLACE, kind: 'docker' } },
};

function hashKey(place: PackagesPlace): string {
  const { path, params } = placeToHash(place);
  return `${path}?${params.toString()}`;
}

/**
 * PackagesTab lists what is installed across the fleet, in two lists: the apt
 * packages of the hosts, and the images their containers run. Each has one
 * row per name with its versions and how many hosts have each. The packages
 * also show what apt offers the hosts, from the upgradable lists of their
 * inventories, and can be narrowed to the names that differ across hosts or
 * are outdated. The list, the filter, the page size and the page are kept in
 * the URL and while another tab is shown.
 */
export function PackagesTab() {
  const [hash, writeHash] = useHashQuery('packages');
  const [state, setState] = useKept<TabState>('packages', START);

  // A linked place (#packages/images?q=geth) wins over the kept one.
  const linked = useMemo(
    () =>
      hash && (hash.path !== '' || hash.params.toString() !== '') ? placeFromHash(hash.path, hash.params) : undefined,
    [hash]
  );
  const place = state.places[state.kind];

  useEffect(() => {
    if (!linked) return;
    setState(prev =>
      prev.kind === linked.kind && hashKey(prev.places[prev.kind]) === hashKey(linked)
        ? prev
        : { kind: linked.kind, places: { ...prev.places, [linked.kind]: linked } }
    );
  }, [linked, setState]);

  useEffect(() => {
    const { path, params } = placeToHash(place);
    writeHash(path, params);
  }, [place, writeHash]);

  const setPlace = (next: (prev: PackagesPlace) => PackagesPlace) => {
    setState(prev => ({ ...prev, places: { ...prev.places, [prev.kind]: next(prev.places[prev.kind]) } }));
  };

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-3">
      <div>
        <Segmented
          label="List"
          options={KIND_OPTIONS}
          value={state.kind}
          onChange={kind => {
            setState(prev => ({ ...prev, kind }));
          }}
        />
      </div>
      {/* The key gives each list its own state. */}
      <ListingView key={state.kind} place={place} setPlace={setPlace} />
    </div>
  );
}

/** One page of either list. */
interface Page<T> {
  rows: readonly T[];
  total?: number;
  counts?: PackageCounts;
  next?: string;
}

interface Listing {
  noun: string;
  label: string;
  placeholder: string;
  none: string;
}

const LISTINGS: Record<PackagesKind, Listing> = {
  apt: {
    noun: 'package',
    label: 'Packages',
    placeholder: 'Filter by package name, e.g. openssl, docker-ce, linux-image',
    none: 'No host has uploaded its package inventory yet.',
  },
  docker: {
    noun: 'image',
    label: 'Images',
    placeholder: 'Filter by image reference, e.g. geth, ethpandaops/, :master',
    none: 'No host has reported a container yet.',
  },
};

/** What the first page of a filter said: the number of names, and of each show. */
interface Totals {
  key: string;
  total?: number;
  counts?: PackageCounts;
}

function ListingView({
  place,
  setPlace,
}: {
  place: PackagesPlace;
  setPlace: (next: (prev: PackagesPlace) => PackagesPlace) => void;
}) {
  const { api } = useApp();
  const { kind } = place;
  const listing = LISTINGS[kind];
  const term = useDebounced(place.q.trim(), 300);
  const [totals, setTotals] = useKept<Totals>(`packages/totals/${kind}`, { key: '' });

  // A page belongs to the filter it was opened under: while the typed text
  // settles, the first page of the new filter is what will be asked for.
  const typing = place.q.trim() !== term;
  const pageIndex = typing ? 0 : place.cursors.length - 1;
  const after = typing ? '' : (place.cursors[pageIndex] ?? '');
  const filterKey = `${term}/${place.show}/${place.sort}`;

  const result = useQuery<Page<PackageMatch> | Page<ImageRow>>(
    async s => {
      const page =
        kind === 'apt'
          ? await fetchPackages(api, place, term, after, s)
          : await fetchImages(api, place, term, after, s);
      if (page.total !== undefined) setTotals({ key: filterKey, total: page.total, counts: page.counts });
      return page;
    },
    { key: `${kind}/${filterKey}/${place.size}/${after}`, keepPrevious: true }
  );

  const waiting = typing || result.loading;
  const total = totals.key === filterKey ? totals.total : undefined;
  const counts = totals.key.startsWith(`${term}/`) ? totals.counts : undefined;
  const rows = result.data?.rows ?? [];
  const next = waiting ? undefined : result.data?.next;
  const what = listing.label.toLowerCase();

  const reset = (change: Partial<PackagesPlace>) => {
    setPlace(prev => ({ ...prev, ...change, cursors: [''] }));
  };

  const pager = (name: string) => (
    <Pager
      name={name}
      label={listing.noun}
      pageIndex={pageIndex}
      size={place.size}
      shown={rows.length}
      total={total}
      waiting={waiting}
      next={next}
      onPrevious={() => {
        setPlace(prev => {
          const back = prev.cursors.slice(0, -1);
          // A linked page does not know the pages before it: back to the first.
          return { ...prev, cursors: back[back.length - 1] === null ? [''] : back };
        });
      }}
      onNext={() => {
        if (next !== undefined) setPlace(prev => ({ ...prev, cursors: [...prev.cursors, next] }));
      }}
    />
  );

  const filtered = term !== '' || place.show !== '';

  return (
    <>
      <div className="flex flex-col gap-3 rounded-lg border border-border bg-surface p-3">
        <div className="flex flex-wrap items-center gap-2">
          <input
            type="search"
            aria-label={`Filter ${what}`}
            placeholder={listing.placeholder}
            value={place.q}
            onChange={e => {
              reset({ q: e.target.value });
            }}
            className={cx(INPUT_CLASS, 'w-full sm:w-96 max-sm:h-10')}
          />
          {kind === 'docker' && <RefreshImages onDone={result.refresh} />}
          <div className="flex flex-wrap items-center gap-2 sm:ml-auto">
            {kind === 'apt' && (
              <select
                aria-label="Sort"
                value={place.sort}
                onChange={e => {
                  reset({ sort: e.target.value as PackageSort });
                }}
                className={cx(INPUT_CLASS, 'md:hidden max-sm:h-10')}
              >
                {SORT_OPTIONS.map(o => (
                  <option key={o.value} value={o.value}>
                    Sort: {o.label}
                  </option>
                ))}
              </select>
            )}
            <select
              aria-label="Page size"
              value={place.size}
              onChange={e => {
                reset({ size: Number(e.target.value) });
              }}
              className={cx(INPUT_CLASS, 'max-sm:h-10')}
            >
              {PAGE_SIZES.map(size => (
                <option key={size} value={size}>
                  {size} per page
                </option>
              ))}
            </select>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
          {kind === 'apt' && (
            <ShowFilter
              value={place.show}
              counts={counts}
              onChange={show => {
                reset({ show });
              }}
            />
          )}
          <div className="ml-auto">{pager('Pages')}</div>
        </div>
      </div>

      {result.error !== undefined && <ErrorBanner error={result.error} what={`the ${what}`} onRetry={result.refresh} />}

      {!result.data ? (
        result.error === undefined && <Loading what={what} />
      ) : rows.length === 0 ? (
        <Empty>
          <span className="flex flex-col items-start gap-2">
            {emptyText(kind, term, place.show, listing)}
            {filtered && (
              <Button
                onClick={() => {
                  reset({ q: '', show: '' });
                }}
              >
                Clear filters
              </Button>
            )}
          </span>
        </Empty>
      ) : kind === 'apt' ? (
        <PackageList
          rows={rows as readonly PackageMatch[]}
          sort={place.sort}
          dimmed={waiting}
          onSort={sort => {
            reset({ sort });
          }}
        />
      ) : (
        <VersionTable rows={rows as readonly ImageRow[]} dimmed={waiting} />
      )}

      {rows.length > 20 && <div className="flex justify-end">{pager('Pages, end of the list')}</div>}
    </>
  );
}

function emptyText(kind: PackagesKind, term: string, show: PackageShow, listing: Listing): string {
  if (term === '' && show === '') return listing.none;
  const which = {
    '': '',
    differing: ' installed in more than one version',
    outdated: ' with an upgrade available',
    security: ' with a security upgrade available',
  }[kind === 'apt' ? show : ''];
  if (term === '') {
    return show === 'outdated' || show === 'security'
      ? `No ${listing.noun}${which}: every host is up to date with its apt sources.`
      : `No ${listing.noun}${which}.`;
  }
  return `No ${listing.noun}${which} matches “${term}”.`;
}

async function fetchPackages(
  api: ApiClient,
  place: PackagesPlace,
  term: string,
  after: string,
  signal: AbortSignal
): Promise<Page<PackageMatch>> {
  const page = await api.packages(
    { q: term, limit: place.size, after, show: place.show, sort: place.sort, compact: true },
    signal
  );
  return { rows: list(page.matches), total: page.total, counts: page.counts, next: page.next };
}

// ---------------------------------------------------------------- pager

function Pager({
  name,
  label,
  pageIndex,
  size,
  shown,
  total,
  waiting,
  next,
  onPrevious,
  onNext,
}: {
  name: string;
  label: string;
  pageIndex: number;
  size: number;
  shown: number;
  total: number | undefined;
  waiting: boolean;
  next: string | undefined;
  onPrevious: () => void;
  onNext: () => void;
}) {
  const first = pageIndex * size + 1;
  const pages = total === undefined ? undefined : Math.max(1, Math.ceil(total / size));

  return (
    <div role="group" aria-label={name} className="flex items-center gap-2 text-xs/5 text-muted">
      <span className="text-right tabular-nums" aria-live="polite">
        {total === undefined
          ? `page ${pageIndex + 1}`
          : total === 0
            ? `no ${label}`
            : `${first}–${first + Math.max(shown, 1) - 1} of ${plural(total, label)}`}
        {pages !== undefined && pages > 1 && (
          <span className="text-faint">
            {' '}
            · page {pageIndex + 1} of {pages}
          </span>
        )}
      </span>
      <Button dense disabled={pageIndex === 0 || waiting} onClick={onPrevious} className="max-sm:h-10 max-sm:px-3">
        ‹ Previous
      </Button>
      <Button dense disabled={next === undefined} onClick={onNext} className="max-sm:h-10 max-sm:px-3">
        Next ›
      </Button>
    </div>
  );
}

// ---------------------------------------------------------------- filters

const SHOW_OPTIONS: readonly { value: PackageShow; label: string; title: string; count: keyof PackageCounts }[] = [
  { value: '', label: 'All', title: 'Every installed package', count: 'all' },
  {
    value: 'differing',
    label: 'Differing',
    title: 'Packages installed in more than one version across the hosts',
    count: 'differing',
  },
  {
    value: 'outdated',
    label: 'Outdated',
    title: 'Packages for which apt offers a newer version to a host, from its apt sources',
    count: 'outdated',
  },
  {
    value: 'security',
    label: 'Security',
    title: 'Packages with an upgrade from a security archive on a host',
    count: 'security',
  },
];

const SORT_OPTIONS: readonly { value: PackageSort; label: string }[] = [
  { value: '', label: 'Name' },
  { value: 'versions', label: 'Most versions' },
  { value: 'hosts', label: 'Most hosts' },
  { value: 'outdated', label: 'Most outdated' },
];

function ShowFilter({
  value,
  counts,
  onChange,
}: {
  value: PackageShow;
  counts: PackageCounts | undefined;
  onChange: (show: PackageShow) => void;
}) {
  return (
    <div role="group" aria-label="Show" className="flex flex-wrap gap-1.5">
      {SHOW_OPTIONS.map(o => {
        const active = o.value === value;
        const n = counts?.[o.count];
        return (
          <button
            key={o.value}
            type="button"
            title={o.title}
            aria-pressed={active}
            onClick={() => {
              onChange(o.value);
            }}
            className={cx(
              'inline-flex h-7 items-center gap-1.5 rounded-full border px-3 text-xs/5 font-medium max-sm:h-10',
              active
                ? 'border-accent bg-surface-selected text-foreground'
                : 'border-border-strong text-muted hover:bg-surface-hover hover:text-foreground'
            )}
          >
            {o.label}
            {n !== undefined && (
              <span
                className={cx(
                  'rounded-full px-1.5 tabular-nums',
                  active ? 'bg-accent text-on-accent' : 'bg-surface-raised text-muted',
                  !active && n > 0 && o.value === 'security' && 'bg-danger-subtle text-danger',
                  !active &&
                    n > 0 &&
                    (o.value === 'outdated' || o.value === 'differing') &&
                    'bg-warning-subtle text-warning'
                )}
              >
                {n}
              </span>
            )}
          </button>
        );
      })}
    </div>
  );
}

// ---------------------------------------------------------------- packages

/** The columns of the package list from md up; below it each row is a card. */
const GRID = 'md:grid md:grid-cols-[minmax(0,1.2fr)_minmax(0,1.4fr)_minmax(0,9rem)_minmax(0,1.3fr)] md:gap-x-4';

function PackageList({
  rows,
  sort,
  dimmed,
  onSort,
}: {
  rows: readonly PackageMatch[];
  sort: PackageSort;
  dimmed: boolean;
  onSort: (sort: PackageSort) => void;
}) {
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());
  const headerHeight = useHeaderHeight();
  const summaries = useMemo(() => rows.map(m => ({ match: m, summary: summarize(m) })), [rows]);

  const toggle = (name: string) => {
    setOpen(prev => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  };

  const header = (label: string, value: PackageSort, title: string, className?: string) => {
    const active = sort === value;
    return (
      <div
        role="columnheader"
        aria-sort={active ? (value === '' ? 'ascending' : 'descending') : 'none'}
        className={className}
      >
        <button
          type="button"
          title={title}
          onClick={() => {
            onSort(active && value !== '' ? '' : value);
          }}
          className={cx(
            'inline-flex items-center gap-1 font-medium hover:text-foreground',
            active && 'text-foreground'
          )}
        >
          {label}
          {active && (
            <span aria-hidden="true" className="text-accent">
              {value === '' ? '▲' : '▼'}
            </span>
          )}
        </button>
      </div>
    );
  };

  return (
    <div
      role="table"
      aria-label="Packages"
      className={cx('rounded-lg border border-border bg-surface text-xs/5', dimmed && 'opacity-60')}
    >
      <div role="rowgroup" className="sticky z-10 max-md:hidden" style={{ top: headerHeight }}>
        <div
          role="row"
          className={cx(GRID, 'rounded-t-lg border-b border-border bg-surface-raised py-2 pr-3 pl-10 text-muted')}
        >
          {header('Package', '', 'Sort by name')}
          {header('Installed', 'versions', 'Most versions first')}
          {header('Hosts', 'hosts', 'Most hosts first')}
          {header('Latest available', 'outdated', 'Most outdated hosts first')}
        </div>
      </div>
      <div role="rowgroup">
        {summaries.map(({ match, summary }) => (
          <PackageRow
            key={match.name}
            match={match}
            summary={summary}
            open={open.has(match.name)}
            onToggle={() => {
              toggle(match.name);
            }}
          />
        ))}
      </div>
    </div>
  );
}

function PackageRow({
  match,
  summary,
  open,
  onToggle,
}: {
  match: PackageMatch;
  summary: PackageSummary;
  open: boolean;
  onToggle: () => void;
}) {
  const { top, others, hosts, outdated, newest } = summary;
  const otherHosts = others.reduce((n, v) => n + v.count, 0);
  const topShare = hosts > 0 && top ? (top.count / hosts) * 100 : 0;

  return (
    <div role="rowgroup" className={cx('border-b border-border last:border-b-0', open && 'bg-surface-raised/40')}>
      <div role="row" className="relative flex hover:bg-surface-hover">
        <button
          type="button"
          aria-expanded={open}
          aria-label={`${open ? 'Hide' : 'Show'} the hosts of ${match.name}`}
          onClick={onToggle}
          className="flex w-10 shrink-0 items-center justify-center text-muted hover:text-foreground max-md:absolute max-md:inset-0 max-md:w-full max-md:items-start max-md:justify-end max-md:pt-3 max-md:pr-3"
        >
          <Icon name="chevron" className={cx('size-4 transition-transform', !open && '-rotate-90')} />
        </button>
        <div
          className={cx(
            GRID,
            'flex min-w-0 flex-1 flex-col gap-1 py-2 pr-3 md:items-center max-md:pointer-events-none max-md:pl-3'
          )}
        >
          <div role="cell" className="flex min-w-0 items-center gap-2 max-md:pr-8">
            <span className="truncate font-mono font-medium max-md:text-sm/6" title={match.name}>
              {match.name}
            </span>
            {summary.security && (
              <Badge tone="danger" title="A host has an upgrade from a security archive">
                security
              </Badge>
            )}
            {summary.blocked && (
              <Badge
                tone="accent"
                title="In the blocked set: unattended-upgrades leaves it alone and upgradoor upgrades it under a slot"
              >
                upgradoor
              </Badge>
            )}
          </div>

          <div role="cell" className="flex min-w-0 items-baseline gap-2">
            <span className="w-14 shrink-0 text-faint md:hidden">Installed</span>
            <span className="truncate font-mono" title={top?.version}>
              {top?.version}
            </span>
            {others.length > 0 && (
              <span
                className="shrink-0 rounded-sm bg-warning-subtle px-1.5 whitespace-nowrap text-warning"
                title={others.map(v => `${v.version} on ${plural(v.count, 'host')}`).join('\n')}
              >
                +{plural(others.length, 'version')} on {otherHosts}
              </span>
            )}
          </div>

          <div role="cell" className="flex items-center gap-2">
            <span className="w-14 shrink-0 text-faint md:hidden">Hosts</span>
            <span className="font-mono tabular-nums md:w-10 md:text-right">{hosts}</span>
            <span
              aria-hidden="true"
              className="flex h-1.5 w-20 overflow-hidden rounded-xs bg-surface-raised"
              title={`${top?.count ?? 0} on ${top?.version ?? ''}, ${otherHosts} on other versions`}
            >
              <span className="block h-full bg-accent" style={{ width: `${topShare}%` }} />
              {otherHosts > 0 && <span className="block h-full flex-1 bg-warning" />}
            </span>
          </div>

          <div role="cell" className="flex min-w-0 items-baseline gap-2">
            <span className="w-14 shrink-0 text-faint md:hidden">Latest</span>
            {newest === undefined ? (
              <span className="inline-flex items-center gap-1 self-center text-success">
                <Icon name="check" className="size-3.5" />
                up to date
              </span>
            ) : (
              <>
                <span className="truncate font-mono" title={newest}>
                  {newest}
                </span>
                <span
                  className={cx(
                    'shrink-0 rounded-sm px-1.5 whitespace-nowrap',
                    summary.security ? 'bg-danger-subtle text-danger' : 'bg-warning-subtle text-warning'
                  )}
                >
                  {outdated === hosts ? 'all' : `${outdated}/${hosts}`} behind
                </span>
              </>
            )}
          </div>
        </div>
      </div>
      {open && <PackageDetail name={match.name} />}
    </div>
  );
}

/** HOST_CAP is how many hosts a version lists before "show all". */
const HOST_CAP = 60;

/** PackageDetail lists the hosts of every version of a package, asked for when its row is opened. */
function PackageDetail({ name }: { name: string }) {
  const { api } = useApp();
  const result = useQuery(
    async s => {
      const page = await api.packages({ q: name, limit: 1 }, s);
      return list(page.matches).find(m => m.name === name);
    },
    { key: name }
  );

  const match = result.data;
  const summary = useMemo(() => (match ? summarize(match) : undefined), [match]);

  return (
    <div role="row" className="border-t border-border px-3 py-3 md:pl-10">
      <div role="cell" className="flex flex-col gap-3">
        {result.error !== undefined && (
          <ErrorBanner error={result.error} what={`the hosts of ${name}`} onRetry={result.refresh} />
        )}
        {result.loading && <Loading what={`the hosts of ${name}`} />}
        {!result.loading && result.error === undefined && !match && (
          <p className="text-muted">No host has {name} installed any more.</p>
        )}
        {match &&
          summary &&
          [summary.top, ...summary.others].flatMap(v => {
            if (!v) return [];
            const upgrades = list(match.upgrades).filter(u => u.current === v.version);
            const behindHosts = new Set(upgrades.flatMap(u => list(u.hosts)));
            return [
              <VersionHosts
                key={v.version}
                version={v.version}
                hosts={list(v.hosts)}
                behind={behindHosts}
                upgrades={upgrades.map(u => ({
                  candidate: u.candidate,
                  count: u.count,
                  security: u.security,
                }))}
              />,
            ];
          })}
      </div>
    </div>
  );
}

function VersionHosts({
  version,
  hosts,
  behind,
  upgrades,
}: {
  version: string;
  hosts: readonly string[];
  behind: ReadonlySet<string>;
  upgrades: readonly { candidate: string; count: number; security: boolean }[];
}) {
  const [all, setAll] = useState(false);
  const shown = all ? hosts : hosts.slice(0, HOST_CAP);

  return (
    <section aria-label={`${version} on ${plural(hosts.length, 'host')}`} className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span className="font-mono font-medium break-all">{version}</span>
        <span className="text-muted">{plural(hosts.length, 'host')}</span>
        {upgrades.length === 0 ? (
          <span className="text-success">up to date</span>
        ) : (
          upgrades.map(u => (
            <span key={u.candidate} className="inline-flex flex-wrap items-baseline gap-1">
              <span className={u.security ? 'text-danger' : 'text-warning'}>
                {u.count === hosts.length ? 'all' : u.count} can upgrade to
              </span>
              <span className="font-mono break-all">{u.candidate}</span>
              {u.security && <Badge tone="danger">security</Badge>}
            </span>
          ))
        )}
      </div>
      <HostChips hosts={shown} behind={behind} />
      {hosts.length > HOST_CAP && (
        <div>
          <Button
            dense
            variant="ghost"
            onClick={() => {
              setAll(v => !v);
            }}
          >
            {all ? 'Show fewer' : `Show all ${hosts.length} hosts`}
          </Button>
        </div>
      )}
    </section>
  );
}

function HostChips({ hosts, behind }: { hosts: readonly string[]; behind: ReadonlySet<string> }): ReactNode {
  return (
    <ul className="flex flex-wrap gap-1">
      {hosts.map(host => (
        <li
          key={host}
          title={behind.has(host) ? `${host}: an upgrade is available` : host}
          className={cx(
            'rounded-sm border px-1.5 font-mono',
            behind.has(host)
              ? 'border-warning/40 bg-warning-subtle text-warning'
              : 'border-border bg-surface text-foreground'
          )}
        >
          {host}
        </li>
      ))}
    </ul>
  );
}

// ---------------------------------------------------------------- images

/** One digest of an image reference. */
interface ImageVersion {
  id: string;
  label: string;
  /** title is the digest in full; note stands next to the label. */
  title: string;
  note?: string;
  /** current: the registry's tag points at this digest. */
  current?: boolean;
  hosts: readonly string[];
}

interface ImageRow {
  name: string;
  versions: ImageVersion[];
  /** lookup is the controller's newest registry lookup of the tag. */
  lookup?: { checkedAt: string; error?: string };
}

async function fetchImages(
  api: ApiClient,
  place: PackagesPlace,
  term: string,
  after: string,
  signal: AbortSignal
): Promise<Page<ImageRow>> {
  const page = await api.images({ q: term, limit: place.size, after }, signal);
  return {
    rows: list(page.matches).map(m => ({
      name: m.name,
      ...(m.registry ? { lookup: { checkedAt: m.registry.checkedAt, error: m.registry.error } } : {}),
      versions: list(m.versions).map(v => ({
        id: v.digest,
        label: v.digest === '' ? 'no registry digest' : shortDigest(v.digest),
        title: v.digest === '' ? 'The image has no registry digest: it was built or loaded on the host.' : v.digest,
        note: shortRevision(v.revision),
        current: v.digest !== '' && v.digest === m.registry?.digest,
        hosts: list(v.hosts),
      })),
    })),
    total: page.total,
    next: page.next,
  };
}

function VersionTable({ rows, dimmed }: { rows: readonly ImageRow[]; dimmed: boolean }) {
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());

  const toggle = (key: string) => {
    setOpen(prev => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  };

  return (
    // The columns have fixed widths: a page with longer names is laid out like the one before it.
    <div className={cx(dimmed && 'opacity-60')}>
      <Table label="Images" fixed>
        <thead>
          <tr>
            <Th className="w-80">Image</Th>
            <Th className="w-72">Digest</Th>
            <Th className="w-36">Hosts</Th>
            <Th>On</Th>
          </tr>
        </thead>
        <tbody>
          {rows.map(m => {
            const versions = [...m.versions].sort((a, b) => b.hosts.length - a.hosts.length);
            const total = versions.reduce((n, v) => n + v.hosts.length, 0);

            return versions.map((v, i) => {
              const hosts = v.hosts;
              const key = `${m.name}\n${v.id}`;
              const isOpen = open.has(key);
              const share = total > 0 ? (hosts.length / total) * 100 : 0;

              return (
                <tr key={key} className="hover:bg-surface-hover">
                  {i === 0 && (
                    <Td rowSpan={versions.length} className="border-r border-border bg-surface">
                      <span className="block truncate font-mono font-medium" title={m.name}>
                        {m.name}
                      </span>
                      {versions.length > 1 && (
                        <span className="flex items-center gap-2 text-muted">
                          {plural(versions.length, 'version')} on {plural(total, 'host')}
                          <Badge tone="warning">mixed</Badge>
                        </span>
                      )}
                      {m.lookup !== undefined && (
                        <span className="block truncate text-muted" title={m.lookup.error}>
                          {m.lookup.error !== undefined && m.lookup.error !== '' ? (
                            <Badge tone="danger">lookup failed</Badge>
                          ) : (
                            'registry checked'
                          )}{' '}
                          <RelTime value={m.lookup.checkedAt} />
                        </span>
                      )}
                    </Td>
                  )}
                  <Td className="truncate font-mono" title={v.title}>
                    {v.label}
                    {v.note !== undefined && v.note !== '' && <span className="ml-2 text-muted">{v.note}</span>}
                    {v.current === true && (
                      <span className="ml-2 font-sans">
                        <Badge tone="success" title="The tag points at this digest in the registry">
                          latest
                        </Badge>
                      </span>
                    )}
                  </Td>
                  <Td>
                    <span className="inline-flex items-center gap-2">
                      <span className="w-6 text-right font-mono tabular-nums">{hosts.length}</span>
                      <span aria-hidden="true" className="h-1.5 w-16 overflow-hidden rounded-xs bg-surface-raised">
                        <span className="block h-full bg-accent" style={{ width: `${share}%` }} />
                      </span>
                    </span>
                  </Td>
                  <Td className="whitespace-normal">
                    <button
                      type="button"
                      aria-expanded={isOpen}
                      aria-label={`${isOpen ? 'Hide' : 'Show'} hosts on ${m.name} ${v.label}`}
                      onClick={() => {
                        toggle(key);
                      }}
                      className="rounded-xs px-1 text-muted hover:bg-surface-hover hover:text-foreground"
                    >
                      {isOpen ? '▾' : '▸'} {isOpen ? 'hide' : plural(hosts.length, 'host')}
                    </button>
                    {isOpen && <span className="text-foreground"> {hosts.join(', ')}</span>}
                  </Td>
                </tr>
              );
            });
          })}
        </tbody>
      </Table>
    </div>
  );
}
