import { useMemo, useState } from 'react';

import { errorText } from '../api/client';
import { list, type BeaconEndpoint, type DataSources, type ValidatorRangesSource } from '../api/types';
import { useApp } from '../appContext';
import { Disclosure } from '../components/icons';
import { RelTime } from '../components/RelTime';
import { Badge, Button, Card, ErrorBanner, INPUT_CLASS, Loading, Table, Td, Th, cx } from '../components/primitives';
import { useQuery } from '../hooks/useQuery';
import { plural } from '../lib/format';

/** MAX_RANGE_ROWS bounds the rows of the range table; a host filter narrows it. */
const MAX_RANGE_ROWS = 500;

/**
 * DataSourcesCard shows where the controller reads the chain and the
 * validator ranges from, and what it saw there. It is loaded on demand; the
 * refresh of the validator ranges answers with the sources after it.
 */
export function DataSourcesCard() {
  const { api } = useApp();
  const loaded = useQuery(s => api.dataSources(s), { key: 'data-sources' });
  const [fresh, setFresh] = useState<DataSources | undefined>(undefined);
  const data = fresh ?? loaded.data;

  return (
    <Card
      title="Data sources"
      aside={
        <Button
          variant="ghost"
          dense
          onClick={() => {
            setFresh(undefined);
            loaded.refresh();
          }}
        >
          Reload
        </Button>
      }
    >
      {loaded.error !== undefined && (
        <ErrorBanner error={loaded.error} what="the data sources" onRetry={loaded.refresh} />
      )}
      {!data ? (
        loaded.loading && <Loading what="data sources" />
      ) : (
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-3">
          <BeaconEndpoints endpoints={list(data.beacon)} />
          <ValidatorRanges source={data.validatorRanges} onRefreshed={setFresh} />
        </div>
      )}
    </Card>
  );
}

const HEADING = 'text-xs/5 font-semibold tracking-wide text-muted uppercase';

function BeaconEndpoints({ endpoints }: { endpoints: readonly BeaconEndpoint[] }) {
  return (
    <section aria-label="Beacon endpoints" className="flex min-w-0 flex-col gap-2">
      <h3 className={HEADING}>Beacon endpoints</h3>
      <ul className="flex flex-col">
        {endpoints.map(e => (
          <li
            key={e.label}
            className="flex flex-col gap-0.5 border-b border-border py-2 text-xs/5 first:pt-0 last:border-b-0"
          >
            <span className="flex flex-wrap items-center gap-2">
              <span className="font-mono break-all">{e.label}</span>
              {e.active && <Badge tone="success">in use</Badge>}
            </span>
            <span className="flex flex-wrap gap-x-4 text-muted">
              <span>
                Last answer <RelTime value={e.successAt} />
              </span>
              {e.headSlot !== undefined && <span className="tabular-nums">Head slot {e.headSlot}</span>}
            </span>
            {e.error && (
              <span className="text-danger">
                {e.error} (<RelTime value={e.errorAt} />)
              </span>
            )}
          </li>
        ))}
      </ul>
    </section>
  );
}

function ValidatorRanges({
  source,
  onRefreshed,
}: {
  source: ValidatorRangesSource;
  onRefreshed: (sources: DataSources) => void;
}) {
  const { api } = useApp();
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState('');
  const [filter, setFilter] = useState('');

  const refresh = () => {
    setBusy(true);
    setNote('');

    api
      .refreshValidatorRanges()
      .then(res => {
        const again = res.next ? ` Again at ${new Date(res.next).toLocaleTimeString()}.` : '';
        setNote(`${res.message}.${again}`);
        onRefreshed(res.sources);
      })
      .catch((err: unknown) => {
        setNote(`The refresh could not be asked for: ${errorText(err)}`);
      })
      .finally(() => {
        setBusy(false);
      });
  };

  const ranges = list(source.ranges);
  const hosts = list(source.hosts);
  const needle = filter.trim().toLowerCase();
  const shownHosts = useMemo(
    () => (needle ? hosts.filter(h => h.host.toLowerCase().includes(needle)) : hosts),
    [hosts, needle]
  );
  const matchingRanges = useMemo(
    () => (needle ? ranges.filter(r => r.host.toLowerCase().includes(needle)) : ranges),
    [ranges, needle]
  );
  const shownRanges = matchingRanges.slice(0, MAX_RANGE_ROWS);

  return (
    <section aria-label="Validator ranges" className="flex min-w-0 flex-col gap-2 lg:col-span-2">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className={HEADING}>Validator ranges</h3>
        <span className="ml-auto inline-flex items-center gap-2">
          <span role="status" className="text-xs/5 text-muted">
            {note}
          </span>
          <Button
            dense
            disabled={busy || !source.url}
            title="Fetch the validator ranges inventory now"
            onClick={refresh}
          >
            {busy ? 'Refreshing…' : 'Refresh'}
          </Button>
        </span>
      </div>

      {!source.url ? (
        <p className="text-xs/5 text-muted">No validator ranges URL is configured: no host has validators.</p>
      ) : (
        <>
          <p className="font-mono text-xs/5 break-all">{source.url}</p>
          <p aria-label="Fetch status" className="flex flex-wrap gap-x-4 text-xs/5 text-muted">
            <span>
              Last fetch <RelTime value={source.attemptAt} />
            </span>
            <span>
              Last success <RelTime value={source.successAt} />
            </span>
            <span>
              Last change <RelTime value={source.changedAt} />
            </span>
            {source.etag && <span className="font-mono">ETag {source.etag}</span>}
            {source.lastModified && <span>Last-Modified {source.lastModified}</span>}
          </p>
          {source.error && (
            <p
              role="alert"
              className="rounded-sm border border-danger bg-danger-subtle px-2 py-1 text-xs/5 text-danger"
            >
              The last fetch failed: {source.error}. The last good version stays in use.
            </p>
          )}
        </>
      )}

      {hosts.length > 0 && (
        <>
          <div className="flex flex-wrap items-center gap-2 text-xs/5">
            <span>
              {source.validators} validators in {plural(ranges.length, 'range')} on {plural(hosts.length, 'host')}
              {source.unknownHosts > 0 && (
                <span className="text-warning">; {source.unknownHosts} unknown to upgradoor</span>
              )}
            </span>
            <input
              type="search"
              aria-label="Filter by host"
              placeholder="Filter by host"
              value={filter}
              onChange={e => {
                setFilter(e.target.value);
              }}
              className={cx(INPUT_CLASS, 'ml-auto w-48')}
            />
          </div>

          <div className="max-h-64 overflow-y-auto">
            <Table label="Validators per host">
              <thead>
                <tr>
                  <Th className="sticky top-0">Host</Th>
                  <Th className="sticky top-0 text-right">Ranges</Th>
                  <Th className="sticky top-0 text-right">Validators</Th>
                </tr>
              </thead>
              <tbody>
                {shownHosts.map(h => (
                  <tr key={h.host}>
                    <Td>
                      <span className="inline-flex items-center gap-2">
                        <span className="font-mono">{h.host}</span>
                        {h.unknown && <UnknownBadge />}
                      </span>
                    </Td>
                    <Td className="text-right tabular-nums">{h.ranges}</Td>
                    <Td className="text-right tabular-nums">{h.validators}</Td>
                  </tr>
                ))}
              </tbody>
            </Table>
          </div>

          <Disclosure summary={`Ranges (${matchingRanges.length})`}>
            <div className="max-h-80 overflow-y-auto">
              <Table label="Validator ranges">
                <thead>
                  <tr>
                    <Th className="sticky top-0">Range</Th>
                    <Th className="sticky top-0">Host</Th>
                    <Th className="sticky top-0 text-right">Count</Th>
                  </tr>
                </thead>
                <tbody>
                  {shownRanges.map(r => (
                    <tr key={r.start}>
                      <Td className="font-mono tabular-nums">
                        {r.start}–{r.end - 1}
                      </Td>
                      <Td>
                        <span className="inline-flex items-center gap-2">
                          <span className="font-mono">{r.host}</span>
                          {r.unknown && <UnknownBadge />}
                        </span>
                      </Td>
                      <Td className="text-right tabular-nums">{r.count}</Td>
                    </tr>
                  ))}
                </tbody>
              </Table>
            </div>
            {matchingRanges.length > shownRanges.length && (
              <p className="text-muted">
                Showing the first {MAX_RANGE_ROWS} of {matchingRanges.length} ranges; filter by host to see others.
              </p>
            )}
          </Disclosure>
        </>
      )}
    </section>
  );
}

function UnknownBadge() {
  return (
    <Badge tone="warning" title="No agent of this name reports to upgradoor">
      unknown
    </Badge>
  );
}
