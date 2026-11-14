import { useState } from 'react';

import { list, type PackageMatch } from '../api/types';
import { useApp } from '../appContext';
import { Badge, Empty, ErrorBanner, INPUT_CLASS, Loading, Table, Td, Th, cx } from '../components/primitives';
import { useDebounced } from '../hooks/useDebounced';
import { usePoll } from '../hooks/usePoll';
import { plural } from '../lib/format';

const SEARCH_LIMIT = 50;

/**
 * PackagesTab searches installed packages across the fleet: per package the
 * installed versions with host counts, and the hosts behind each version.
 */
export function PackagesTab() {
  const { api } = useApp();
  const [input, setInput] = useState('');
  const term = useDebounced(input.trim(), 300);

  const result = usePoll(s => api.packages(term, SEARCH_LIMIT, s), {
    key: `packages/${term}`,
    enabled: term !== '',
    intervalMs: 0,
  });

  const matches = list(result.data?.matches);
  const waiting = input.trim() !== term || result.loading;

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <input
          type="search"
          aria-label="Search packages"
          placeholder="Package name, e.g. openssl, docker-ce, linux-image"
          value={input}
          onChange={e => {
            setInput(e.target.value);
          }}
          className={cx(INPUT_CLASS, 'w-96')}
        />
        {term !== '' && !waiting && (
          <span className="text-xs/5 text-muted">
            {plural(matches.length, 'package')} match “{result.data?.query ?? term}”
            {matches.length >= SEARCH_LIMIT && ` (first ${SEARCH_LIMIT}; refine the search)`}
          </span>
        )}
      </div>

      {result.error !== undefined && <ErrorBanner error={result.error} what="the packages" onRetry={result.refresh} />}

      {input.trim() === '' ? (
        <Empty>Type a package name to see which versions are installed across the fleet, and on which hosts.</Empty>
      ) : waiting && !result.data ? (
        <Loading what="packages" />
      ) : matches.length === 0 && result.error === undefined ? (
        <Empty>No installed package matches “{term}”.</Empty>
      ) : (
        matches.length > 0 && <PackageTable matches={matches} />
      )}
    </div>
  );
}

function PackageTable({ matches }: { matches: readonly PackageMatch[] }) {
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
    <Table label="Package versions">
      <thead>
        <tr>
          <Th>Package</Th>
          <Th>Version</Th>
          <Th>Hosts</Th>
          <Th className="w-full">On</Th>
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
                    <span className="font-mono font-medium">{m.name}</span>
                    <div className="text-muted">
                      {plural(versions.length, 'version')} on {plural(total, 'host')}
                    </div>
                    {versions.length > 1 && <Badge tone="warning">mixed</Badge>}
                  </Td>
                )}
                <Td className="font-mono">{v.version}</Td>
                <Td>
                  <span className="inline-flex items-center gap-2">
                    <span className="w-6 text-right font-mono">{hosts.length}</span>
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
  );
}
