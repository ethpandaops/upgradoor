import { useMemo, useState } from 'react';

import { errorText } from '../api/client';
import {
  list,
  type BeaconEndpoint,
  type ChainErrors,
  type DataSources,
  type ValidatorRangesSource,
} from '../api/types';
import { useApp } from '../appContext';
import { Disclosure } from '../components/icons';
import { useLive } from '../components/Live';
import { RelTime } from '../components/RelTime';
import { ShortId } from '../components/Digest';
import {
  Badge,
  Button,
  Card,
  Dash,
  ErrorBanner,
  INPUT_CLASS,
  Loading,
  Table,
  Td,
  Th,
  cx,
  type Tone,
} from '../components/primitives';
import { useQuery } from '../hooks/useQuery';
import { plural } from '../lib/format';

/** MAX_RANGE_ROWS bounds the rows of the range table; a host filter narrows it. */
const MAX_RANGE_ROWS = 500;

/**
 * DataSourcesTab shows where the controller reads the chain and the validator
 * ranges from, and what it saw there. It is loaded on demand; the refresh of
 * the validator ranges answers with the sources after it.
 */
export function DataSourcesTab() {
  const { api } = useApp();
  const loaded = useQuery(s => api.dataSources(s), { key: 'data-sources' });
  const [fresh, setFresh] = useState<DataSources | undefined>(undefined);
  const data = fresh ?? loaded.data;

  return (
    <div className="mx-auto flex w-full max-w-screen-2xl flex-col gap-4">
      {loaded.error !== undefined && (
        <ErrorBanner error={loaded.error} what="the data sources" onRetry={loaded.refresh} />
      )}
      {!data ? (
        loaded.loading && <Loading what="data sources" />
      ) : (
        <>
          <BeaconEndpoints
            endpoints={list(data.beacon)}
            chain={data.chain}
            onReload={() => {
              setFresh(undefined);
              loaded.refresh();
            }}
          />
          <ValidatorRanges source={data.validatorRanges} onRefreshed={setFresh} />
        </>
      )}
    </div>
  );
}

const STATE_TONE: Record<BeaconEndpoint['state'], Tone> = {
  ok: 'success',
  behind: 'warning',
  syncing: 'info',
  forked: 'danger',
  down: 'danger',
};

/** stateLabel is the endpoint's state; behind says by how much, against the highest head of the endpoints that are ok. */
export function stateLabel(e: BeaconEndpoint, okHead: number | undefined): string {
  if (e.state !== 'behind' || okHead === undefined || e.headSlot === undefined) return e.state;
  return `behind by ${plural(Math.max(0, okHead - e.headSlot), 'slot')}`;
}

/** CHAIN_ERRORS label the error texts of the beacon service, which the other pages show as fixed texts. */
const CHAIN_ERRORS: readonly [keyof ChainErrors, string][] = [
  ['error', 'Beacon service'],
  ['timingError', 'Chain timing'],
  ['livenessError', 'Participation count'],
];

function BeaconEndpoints({
  endpoints,
  chain,
  onReload,
}: {
  endpoints: readonly BeaconEndpoint[];
  chain: ChainErrors | undefined;
  onReload: () => void;
}) {
  const split = useLive('public-network').data?.network.split;
  const errors = CHAIN_ERRORS.flatMap(([key, label]) => (chain?.[key] ? [{ label, text: chain[key] }] : []));
  const okHeads = endpoints.filter(e => e.state === 'ok' && e.headSlot !== undefined).map(e => e.headSlot ?? 0);
  const okHead = okHeads.length > 0 ? Math.max(...okHeads) : undefined;

  return (
    <Card
      title="Beacon endpoints"
      count={endpoints.length}
      aside={
        <Button variant="ghost" dense onClick={onReload}>
          Reload
        </Button>
      }
    >
      <p className="text-xs/5 text-muted">
        The endpoints are queried in parallel; the controller follows the chain most of them agree on.
      </p>
      {split && (
        <p role="alert" className="rounded-sm border border-danger bg-danger-subtle px-2 py-1 text-xs/5 text-danger">
          Chain split: {split}. The network gate stays closed until one chain has a strict majority.
        </p>
      )}
      {errors.map(e => (
        <p key={e.label} className="text-xs/5 text-danger">
          {e.label}: {e.text}
        </p>
      ))}
      <Table label="Beacon endpoints">
        <thead>
          <tr>
            <Th>Endpoint</Th>
            <Th>State</Th>
            <Th>Head slot</Th>
            <Th>Finalized epoch</Th>
            <Th>Last answer</Th>
            <Th>Error</Th>
          </tr>
        </thead>
        <tbody>
          {endpoints.map(e => (
            <tr key={e.label}>
              <Td className="font-mono">{e.label}</Td>
              <Td>
                <Badge tone={STATE_TONE[e.state]}>{stateLabel(e, okHead)}</Badge>
              </Td>
              <Td className="tabular-nums">
                {e.headSlot === undefined ? (
                  <Dash />
                ) : (
                  <span className="inline-flex items-center gap-1.5">
                    {e.headSlot}
                    {e.headRoot && <ShortId value={e.headRoot} label="head root" />}
                  </span>
                )}
              </Td>
              <Td className="tabular-nums">{e.finalizedEpoch ?? <Dash />}</Td>
              <Td>
                <RelTime value={e.successAt} />
              </Td>
              <Td className="whitespace-normal">
                {e.error ? (
                  <span className="text-danger">
                    {e.error} (<RelTime value={e.errorAt} />)
                  </span>
                ) : (
                  <Dash />
                )}
              </Td>
            </tr>
          ))}
        </tbody>
      </Table>
    </Card>
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
    <Card
      title="Validator ranges"
      aside={
        <>
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
        </>
      }
    >
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
    </Card>
  );
}

function UnknownBadge() {
  return (
    <Badge tone="warning" title="No agent of this name reports to upgradoor">
      unknown
    </Badge>
  );
}
