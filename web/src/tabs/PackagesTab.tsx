import { useState } from 'react';

import { list, type PackageMatch } from '../api/types';
import { useApp } from '../appContext';
import { Badge, Button, Empty, ErrorBanner, INPUT_CLASS, Loading, Table, Td, Th, cx } from '../components/primitives';
import { useDebounced } from '../hooks/useDebounced';
import { useKept } from '../hooks/useKept';
import { useQuery } from '../hooks/useQuery';
import { plural } from '../lib/format';

/** The number of package names on a page. It is fixed, so the pages have one height. */
export const PAGE_SIZE = 25;

/** Where the user is in the list: what was typed, and the cursor of every page opened so far. */
interface Place {
  input: string;
  /** cursors[i] is the `after` of page i; the first page has none. */
  cursors: string[];
  /** total is the number of names the filter selects, as the first page reported it. */
  total: number | undefined;
}

const START: Place = { input: '', cursors: [''], total: undefined };

/**
 * PackagesTab lists the packages installed across the fleet, one row per
 * package name with its versions and how many hosts have each; the hosts are
 * shown on demand. Without a filter it lists every package, a page at a time;
 * typing filters by a part of the name. The list is fetched page by page from
 * the controller and is not part of the event stream. The filter and the page
 * are kept while another tab is shown.
 */
export function PackagesTab() {
  const { api } = useApp();
  const [place, setPlace] = useKept<Place>('packages', START);
  const term = useDebounced(place.input.trim(), 300);

  // A page belongs to the filter it was opened under: while the typed text
  // settles, the first page of the new filter is what will be asked for.
  const typing = place.input.trim() !== term;
  const pageIndex = typing ? 0 : place.cursors.length - 1;
  const after = typing ? '' : (place.cursors[pageIndex] ?? '');

  const result = useQuery(
    async s => {
      const page = await api.packages({ q: term, limit: PAGE_SIZE, after }, s);
      if (page.total !== undefined) setPlace(prev => ({ ...prev, total: page.total }));
      return page;
    },
    { key: `packages/${term}/${after}`, keepPrevious: true }
  );

  const matches = list(result.data?.matches);
  const waiting = typing || result.loading;
  const total = place.total;
  const first = pageIndex * PAGE_SIZE + 1;
  const next = waiting ? undefined : result.data?.next;

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <input
          type="search"
          aria-label="Filter packages"
          placeholder="Filter by package name, e.g. openssl, docker-ce, linux-image"
          value={place.input}
          onChange={e => {
            setPlace({ input: e.target.value, cursors: [''], total: undefined });
          }}
          className={cx(INPUT_CLASS, 'w-96')}
        />

        {/* The pager keeps its place and its width, whatever page is shown. */}
        <div role="group" aria-label="Pages" className="ml-auto flex items-center gap-2 text-xs/5 text-muted">
          <span className="min-w-40 text-right tabular-nums" aria-live="polite">
            {total === undefined
              ? ''
              : total === 0
                ? 'no package'
                : `${first}–${Math.min(first + PAGE_SIZE - 1, total)} of ${plural(total, 'package')}`}
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

      {result.error !== undefined && <ErrorBanner error={result.error} what="the packages" onRetry={result.refresh} />}

      {!result.data ? (
        result.error === undefined && <Loading what="packages" />
      ) : matches.length === 0 ? (
        <Empty>
          {term === '' ? 'No host has uploaded its package inventory yet.' : `No installed package matches “${term}”.`}
        </Empty>
      ) : (
        <PackageTable matches={matches} dimmed={waiting} />
      )}
    </div>
  );
}

function PackageTable({ matches, dimmed }: { matches: readonly PackageMatch[]; dimmed: boolean }) {
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
      <Table label="Packages" fixed>
        <thead>
          <tr>
            <Th className="w-80">Package</Th>
            <Th className="w-72">Version</Th>
            <Th className="w-36">Hosts</Th>
            <Th>On</Th>
          </tr>
        </thead>
        <tbody>
          {matches.map(m => {
            const versions = [...list(m.versions)].sort((a, b) => list(b.hosts).length - list(a.hosts).length);
            const total = versions.reduce((n, v) => n + list(v.hosts).length, 0);

            return versions.map((v, i) => {
              const hosts = list(v.hosts);
              const key = `${m.name}\n${v.version}`;
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
                    </Td>
                  )}
                  <Td className="truncate font-mono" title={v.version}>
                    {v.version}
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
                      aria-label={`${isOpen ? 'Hide' : 'Show'} hosts on ${m.name} ${v.version}`}
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
