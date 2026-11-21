import { useState } from 'react';

import type { ApiClient } from '../api/client';
import { list } from '../api/types';
import { useApp } from '../appContext';
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
import { usePermissions } from '../components/Permissions';
import { RelTime } from '../components/RelTime';
import { useDebounced } from '../hooks/useDebounced';
import { useKept } from '../hooks/useKept';
import { useQuery } from '../hooks/useQuery';
import { plural, shortDigest, shortRevision } from '../lib/format';

/** The number of names on a page. It is fixed, so the pages have one height. */
export const PAGE_SIZE = 25;

/** What the tab lists: the apt packages of the hosts, or the images their containers run. */
type Kind = 'apt' | 'docker';

/** One version of a row: a package version, or the digest of an image. */
interface Version {
  /** id tells the versions of a row apart. */
  id: string;
  label: string;
  /** title is the version in full; note stands next to the label. */
  title: string;
  note?: string;
  /** current: the registry's tag points at this digest. */
  current?: boolean;
  hosts: readonly string[];
}

/** One row group of the table: a package name or an image reference. */
interface Row {
  name: string;
  versions: Version[];
  /** lookup is the controller's newest registry lookup of an image's tag. */
  lookup?: { checkedAt: string; error?: string };
}

/** One page, whatever it lists. */
interface Page {
  rows: Row[];
  total?: number;
  next?: string;
}

interface PageQuery {
  q: string;
  limit: number;
  after: string;
}

/** What differs between the two lists. */
interface Listing {
  /** noun names one entry; label is the table's and the filter's name. */
  noun: string;
  label: string;
  column: string;
  versionColumn: string;
  placeholder: string;
  none: string;
  fetch: (api: ApiClient, query: PageQuery, signal: AbortSignal) => Promise<Page>;
}

const LISTINGS: Record<Kind, Listing> = {
  apt: {
    noun: 'package',
    label: 'Packages',
    column: 'Package',
    versionColumn: 'Version',
    placeholder: 'Filter by package name, e.g. openssl, docker-ce, linux-image',
    none: 'No host has uploaded its package inventory yet.',
    fetch: async (api, query, signal) => {
      const page = await api.packages(query, signal);
      return {
        rows: list(page.matches).map(m => ({
          name: m.name,
          versions: list(m.versions).map(v => ({
            id: v.version,
            label: v.version,
            title: v.version,
            hosts: list(v.hosts),
          })),
        })),
        total: page.total,
        next: page.next,
      };
    },
  },
  docker: {
    noun: 'image',
    label: 'Images',
    column: 'Image',
    versionColumn: 'Digest',
    placeholder: 'Filter by image reference, e.g. geth, ethpandaops/, :master',
    none: 'No host has reported a container yet.',
    fetch: async (api, query, signal) => {
      const page = await api.images(query, signal);
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
    },
  },
};

const KIND_OPTIONS: readonly { value: Kind; label: string; title: string }[] = [
  { value: 'apt', label: 'APT packages', title: 'The packages installed on the hosts' },
  { value: 'docker', label: 'Docker images', title: 'The images the containers of the hosts run' },
];

/** Where the user is in a list: what was typed, and the cursor of every page opened so far. */
interface Place {
  input: string;
  /** cursors[i] is the `after` of page i; the first page has none. */
  cursors: string[];
  /** total is the number of names the filter selects, as the first page reported it. */
  total: number | undefined;
}

const START: Place = { input: '', cursors: [''], total: undefined };

/**
 * PackagesTab lists what is installed across the fleet, in two lists: the apt
 * packages of the hosts, and the images their containers run. Each has one
 * row per name with its versions and how many hosts have each; the hosts are
 * shown on demand. Without a filter a list holds everything, a page at a
 * time; typing filters by a part of the name. The pages are fetched from the
 * controller and are not part of the event stream. The list shown, and the
 * filter and the page of each, are kept while another tab is shown.
 */
export function PackagesTab() {
  const [kind, setKind] = useKept<Kind>('packages/kind', 'apt');

  return (
    <div className="flex flex-col gap-2">
      <div>
        <Segmented label="List" options={KIND_OPTIONS} value={kind} onChange={setKind} />
      </div>
      {/* The key gives each list its own state. */}
      <ListingView key={kind} kind={kind} />
    </div>
  );
}

function ListingView({ kind }: { kind: Kind }) {
  const { api } = useApp();
  const listing = LISTINGS[kind];
  const [place, setPlace] = useKept<Place>(kind === 'apt' ? 'packages' : 'packages/images', START);
  const term = useDebounced(place.input.trim(), 300);

  // A page belongs to the filter it was opened under: while the typed text
  // settles, the first page of the new filter is what will be asked for.
  const typing = place.input.trim() !== term;
  const pageIndex = typing ? 0 : place.cursors.length - 1;
  const after = typing ? '' : (place.cursors[pageIndex] ?? '');

  const result = useQuery(
    async s => {
      const page = await listing.fetch(api, { q: term, limit: PAGE_SIZE, after }, s);
      if (page.total !== undefined) setPlace(prev => ({ ...prev, total: page.total }));
      return page;
    },
    { key: `${kind}/${term}/${after}`, keepPrevious: true }
  );

  const rows = result.data?.rows ?? [];
  const waiting = typing || result.loading;
  const total = place.total;
  const first = pageIndex * PAGE_SIZE + 1;
  const next = waiting ? undefined : result.data?.next;
  const what = listing.label.toLowerCase();

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <input
          type="search"
          aria-label={`Filter ${what}`}
          placeholder={listing.placeholder}
          value={place.input}
          onChange={e => {
            setPlace({ input: e.target.value, cursors: [''], total: undefined });
          }}
          className={cx(INPUT_CLASS, 'w-96')}
        />

        {kind === 'docker' && <RefreshImages onDone={result.refresh} />}

        {/* The pager keeps its place and its width, whatever page is shown. */}
        <div role="group" aria-label="Pages" className="ml-auto flex items-center gap-2 text-xs/5 text-muted">
          <span className="min-w-40 text-right tabular-nums" aria-live="polite">
            {total === undefined
              ? ''
              : total === 0
                ? `no ${listing.noun}`
                : `${first}–${Math.min(first + PAGE_SIZE - 1, total)} of ${plural(total, listing.noun)}`}
          </span>
          <Button
            dense
            disabled={pageIndex === 0 || waiting}
            onClick={() => {
              setPlace(prev => ({ ...prev, cursors: prev.cursors.slice(0, -1) }));
            }}
          >
            ‹ Previous
          </Button>
          <Button
            dense
            disabled={next === undefined}
            onClick={() => {
              if (next !== undefined) setPlace(prev => ({ ...prev, cursors: [...prev.cursors, next] }));
            }}
          >
            Next ›
          </Button>
        </div>
      </div>

      {result.error !== undefined && <ErrorBanner error={result.error} what={`the ${what}`} onRetry={result.refresh} />}

      {!result.data ? (
        result.error === undefined && <Loading what={what} />
      ) : rows.length === 0 ? (
        <Empty>{term === '' ? listing.none : `No ${listing.noun} matches “${term}”.`}</Empty>
      ) : (
        <VersionTable listing={listing} rows={rows} dimmed={waiting} />
      )}
    </>
  );
}

/**
 * RefreshImages asks the controller to look every followed tag up at its
 * registry now, and says what came of it. The hosts report a new digest with
 * their next sync, so the list is fetched again right away and shows it later.
 */
function RefreshImages({ onDone }: { onDone: () => void }) {
  const { api } = useApp();
  const allowed = usePermissions().refreshImages;
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState('');

  const refresh = () => {
    setBusy(true);
    setNote('');

    api
      .refreshImages()
      .then(res => {
        const again = res.next
          ? ` The next lookup on request is possible at ${new Date(res.next).toLocaleTimeString()}.`
          : '';
        setNote(
          res.tags > 0
            ? `Looking up ${plural(res.tags, 'tag')}; the hosts report the result with their next sync.${again}`
            : (res.recent ?? 0) > 0
              ? `Every tag was looked up within the last half minute.${again}`
              : 'No tag is followed.'
        );
        onDone();
      })
      .catch((err: unknown) => {
        setNote(`The lookup could not be asked for: ${err instanceof Error ? err.message : String(err)}`);
      })
      .finally(() => {
        setBusy(false);
      });
  };

  return (
    <>
      <Button
        dense
        disabled={!allowed || busy}
        title={
          allowed
            ? 'Look every followed tag up at its registry now, instead of at its next interval'
            : 'This login lacks the feature roll'
        }
        onClick={refresh}
      >
        Check registries now
      </Button>
      <span role="status" className="text-xs/5 text-muted">
        {note}
      </span>
    </>
  );
}

function VersionTable({ listing, rows, dimmed }: { listing: Listing; rows: readonly Row[]; dimmed: boolean }) {
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
      <Table label={listing.label} fixed>
        <thead>
          <tr>
            <Th className="w-80">{listing.column}</Th>
            <Th className="w-72">{listing.versionColumn}</Th>
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
