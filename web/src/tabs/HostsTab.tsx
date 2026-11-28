import { useCallback, useEffect, useMemo, useState } from 'react';

import { list, type HostListing, type PublicImageRef } from '../api/types';
import { useApp, useAuthState, usePrivateAccess } from '../appContext';
import { useActions, type ActionIntent } from '../components/ActionDialog';
import { usePermissions } from '../components/Permissions';
import { Digest } from '../components/Digest';
import { useLive } from '../components/Live';
import { BulkAction, SelectionBar } from '../components/SelectionBar';
import { RelTime, useSharedNow } from '../components/RelTime';
import { useKept } from '../hooks/useKept';
import { useSearchParam } from '../hooks/useSearchParam';
import {
  Button,
  Badge,
  Dash,
  Empty,
  ErrorBanner,
  INPUT_CLASS,
  Loading,
  SortTh,
  Table,
  Td,
  Th,
  cx,
  type SortColumn,
  type SortDirection,
} from '../components/primitives';
import { ClientBadge, OfflineBadge } from '../components/status';
import { FleetUsage, UsageCell, usageLabel } from '../components/Usage';
import { absoluteTime, displayImage, formatDuration, formatPercent, isZeroTime, plural } from '../lib/format';
import { hostActionIntent } from '../lib/hostActions';
import { holdIntent } from '../lib/intents';
import { HostDrawer } from './HostDetailPanel';
import {
  attestingShare,
  hostOwners,
  hostState,
  matchesFilter,
  mergeHostRows,
  rollableContainers,
  sortHostRows,
  type HostRow,
  type SortKey,
} from './hostRows';

/** HOST_PAGE_SIZE is how many hosts the table shows at a time. */
const HOST_PAGE_SIZE = 100;
/**
 * HostsTab is the fleet table. Logged out it shows only the public data
 * (name, EL / CL image, update marker); logged in the same table gains the
 * host's state, and a row opens the host's details and actions in a drawer
 * (?host=<name>). Actions on several hosts go through the selection.
 */
export function HostsTab() {
  const { auth } = useApp();
  const priv = usePrivateAccess();
  const authEnabled = useAuthState(auth).enabled;

  const publicHosts = useLive('public-hosts');
  const hosts = useLive('hosts', priv);
  const plan = useLive('plan', priv);

  // Kept across tabs: the graph on the Overview leads here with a host's name.
  const [filter, setFilter] = useKept('hosts.filter', '');
  const [sort, setSort] = useState<{ key: SortKey; direction: SortDirection }>({ key: 'name', direction: 'asc' });
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [openHost, setOpenHost] = useSearchParam('host');
  const [page, setPage] = useState(0);

  const actions = useActions();
  const { login, hostDetails } = usePermissions();

  const allRows = useMemo(
    () => mergeHostRows(list(publicHosts.data?.hosts), list(hosts.data?.hosts), list(plan.data?.queue)),
    [publicHosts.data, hosts.data, plan.data]
  );
  const rows = useMemo(
    () =>
      sortHostRows(
        allRows.filter(r => matchesFilter(r, filter)),
        sort.key,
        sort.direction
      ),
    [allRows, filter, sort]
  );

  // One page of the table at a time: a fleet of a thousand hosts is not a
  // thousand rows in the page. A new filter or sort starts at the first.
  const pages = Math.max(1, Math.ceil(rows.length / HOST_PAGE_SIZE));
  const current = Math.min(page, pages - 1);
  const pageRows = rows.slice(current * HOST_PAGE_SIZE, (current + 1) * HOST_PAGE_SIZE);

  const known = useMemo(() => new Set(allRows.map(r => r.name)), [allRows]);
  // A selected host that was forgotten meanwhile drops out of the selection.
  const selection = useMemo(() => [...selected].filter(n => known.has(n)).sort(), [selected, known]);
  const selectedSummaries = useMemo(
    () => list(hosts.data?.hosts).filter(h => selection.includes(h.name)),
    [hosts.data, selection]
  );

  const toggle = (set: ReadonlySet<string>, name: string): Set<string> => {
    const next = new Set(set);
    if (next.has(name)) next.delete(name);
    else next.add(name);
    return next;
  };

  const sortBy = (key: SortKey) => {
    setSort(prev => ({ key, direction: prev.key === key && prev.direction === 'asc' ? 'desc' : 'asc' }));
    setPage(0);
  };
  const sortColumn = (key: SortKey, label: string, title?: string): SortColumn => ({
    label,
    title,
    active: sort.key === key,
    direction: sort.direction,
    onSort: () => {
      sortBy(key);
    },
  });

  const visibleNames = rows.map(r => r.name);
  const openIndex = openHost ? visibleNames.indexOf(openHost) : -1;
  const closeHost = useCallback(() => {
    setOpenHost('');
  }, [setOpenHost]);
  const stepTo = (index: number) => {
    const name = visibleNames[index];
    if (name === undefined) return undefined;
    return () => {
      setOpenHost(name);
      setPage(Math.floor(index / HOST_PAGE_SIZE));
    };
  };

  // The open host stays in view when the drawer steps through the list.
  useEffect(() => {
    if (openHost)
      document.querySelector(`tr[data-host="${CSS.escape(openHost)}"]`)?.scrollIntoView({ block: 'nearest' });
  }, [openHost]);

  // Bulk actions act on the whole selection, also on hosts the filter hides
  // right now: the toolbar and the confirmation dialog say so.
  const hiddenSelection = useMemo(() => {
    const visible = new Set(rows.map(r => r.name));
    return selection.filter(n => !visible.has(n));
  }, [rows, selection]);
  const allVisibleSelected = visibleNames.length > 0 && visibleNames.every(n => selected.has(n));
  const columnCount = priv ? (hostDetails ? 17 : 12) : 8;
  const rollSelection: ActionIntent = {
    kind: 'roll',
    hosts: selection,
    all: false,
    containerChoices: rollableContainers(selectedSummaries),
    hiddenHosts: hiddenSelection,
  };
  const actionSelection = hostActionIntent(login, selectedSummaries, undefined, hiddenSelection);

  const loading = publicHosts.loading || (priv && hosts.loading);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <input
          type="search"
          aria-label="Filter hosts"
          placeholder={priv ? 'Filter by name, image, role, state…' : 'Filter by name or image…'}
          value={filter}
          onChange={e => {
            setFilter(e.target.value);
            setPage(0);
          }}
          className={cx(INPUT_CLASS, 'w-72')}
        />
        <span className="text-xs/5 text-muted">
          {rows.length === allRows.length
            ? plural(allRows.length, 'host')
            : `${rows.length} of ${plural(allRows.length, 'host')}`}
        </span>
        {pages > 1 && (
          <div role="group" aria-label="Pages" className="flex items-center gap-2 text-xs/5 text-muted">
            <span className="min-w-24 text-right tabular-nums" aria-live="polite">
              {current * HOST_PAGE_SIZE + 1}–{current * HOST_PAGE_SIZE + pageRows.length} of {rows.length}
            </span>
            <Button
              dense
              aria-label="Previous page"
              disabled={current === 0}
              onClick={() => {
                setPage(current - 1);
              }}
            >
              ‹ Previous
            </Button>
            <Button
              dense
              aria-label="Next page"
              disabled={current >= pages - 1}
              onClick={() => {
                setPage(current + 1);
              }}
            >
              Next ›
            </Button>
          </div>
        )}

        {!priv && authEnabled && (
          <span className="text-xs/5 text-muted">Log in for host details, rollouts and actions.</span>
        )}
      </div>

      {priv && (
        <SelectionBar
          count={selection.length}
          onClear={() => {
            setSelected(new Set());
          }}
          note={
            hiddenSelection.length > 0 && (
              <span className="flex items-center gap-2 text-xs/5 text-warning">
                {hiddenSelection.length} hidden by the filter
                <button
                  type="button"
                  className="font-medium text-accent hover:underline"
                  onClick={() => {
                    const hidden = new Set(hiddenSelection);
                    setSelected(prev => new Set([...prev].filter(n => !hidden.has(n))));
                  }}
                >
                  Drop them
                </button>
              </span>
            )
          }
        >
          {(
            [
              ['roll', 'Roll', rollSelection],
              ['reboot', 'Reboot', { kind: 'reboot', hosts: selection, all: false, hiddenHosts: hiddenSelection }],
              ['hold', 'Hold', holdIntent('hold', selection, hiddenSelection)],
              ['unhold', 'Unhold', holdIntent('unhold', selection, hiddenSelection)],
              ['forget', 'Forget', holdIntent('forget', selection, hiddenSelection)],
            ] as const
          ).map(([icon, label, intent]) => (
            <BulkAction
              key={icon}
              icon={icon}
              label={label}
              tone={icon === 'forget' ? 'danger' : 'default'}
              blocked={selection.length === 0 ? 'Select hosts first' : ''}
              check={intent}
              run={() => {
                actions.request(intent);
              }}
            />
          ))}
          <BulkAction
            icon="action"
            label="Run host action…"
            blocked={selection.length === 0 ? 'Select hosts first' : ''}
            check={actionSelection}
            run={() => {
              actions.request(actionSelection);
            }}
          />
        </SelectionBar>
      )}

      {priv && hostDetails && (
        <FleetUsage
          hosts={list(hosts.data?.hosts)}
          filter={filter}
          onFilter={next => {
            setFilter(next);
            setPage(0);
          }}
        />
      )}

      {actions.notice}
      {publicHosts.error !== undefined && <ErrorBanner error={publicHosts.error} what="the hosts" />}

      {loading && allRows.length === 0 ? (
        <Loading what="hosts" />
      ) : allRows.length === 0 ? (
        publicHosts.error === undefined && (
          <Empty>No host has talked to this controller yet. A host appears once its agent has synced.</Empty>
        )
      ) : (
        <Table label="Hosts" roomy>
          <thead>
            <tr>
              {priv && (
                <Th tight className="w-6 text-center">
                  <input
                    type="checkbox"
                    aria-label="Select all hosts the filter shows, on every page"
                    checked={allVisibleSelected}
                    onChange={() => {
                      setSelected(allVisibleSelected ? new Set() : new Set(visibleNames));
                    }}
                    className="accent-accent"
                  />
                </Th>
              )}
              <SortTh columns={[sortColumn('name', 'Host')]} />
              <Th tight title="Execution client">
                <span className="sr-only">Execution client</span>
              </Th>
              <SortTh columns={[sortColumn('el', 'Execution')]} />
              <Th title="The digest of the execution client's image">EL digest</Th>
              <Th tight title="Consensus client">
                <span className="sr-only">Consensus client</span>
              </Th>
              <SortTh columns={[sortColumn('cl', 'Consensus')]} />
              <Th title="The digest of the consensus client's image">CL digest</Th>
              {priv ? (
                <>
                  <SortTh columns={[sortColumn('slot', 'State', 'What the scheduler does with the host')]} />
                  <Th title="What the host waits for, or why it is in its state">Reason</Th>
                  <SortTh
                    columns={[sortColumn('attesting', 'Attesting', 'Validators live in the last complete epoch')]}
                  />
                  {hostDetails && (
                    <>
                      <SortTh columns={[sortColumn('cpu', 'CPU', 'CPU busy since the previous sync')]} />
                      <SortTh columns={[sortColumn('memory', 'Memory', 'Memory not available')]} />
                      <SortTh columns={[sortColumn('kernel', 'Kernel')]} />
                      <SortTh className="hidden xl:table-cell" columns={[sortColumn('uptime', 'Uptime')]} />
                      <SortTh columns={[sortColumn('security', 'Updates', 'Pending package updates')]} />
                    </>
                  )}
                  <SortTh
                    className="hidden 2xl:table-cell"
                    columns={[sortColumn('lastSeen', 'Seen', 'When the agent last synced')]}
                  />
                </>
              ) : (
                <SortTh columns={[sortColumn('pending', 'State', 'The host has an update waiting')]} />
              )}
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr>
                <Td colSpan={columnCount} className="py-4 text-center text-muted">
                  No host matches “{filter}”.
                </Td>
              </tr>
            )}
            {pageRows.map(row => (
              <HostTableRow
                key={row.name}
                row={row}
                priv={priv}
                details={hostDetails}
                selected={selected.has(row.name)}
                open={openHost === row.name}
                onSelect={() => {
                  setSelected(prev => toggle(prev, row.name));
                }}
                onOpen={() => {
                  setOpenHost(openHost === row.name ? '' : row.name);
                }}
              />
            ))}
          </tbody>
        </Table>
      )}

      {priv && openHost && (
        <HostDrawer
          name={openHost}
          row={allRows.find(r => r.name === openHost)}
          onClose={closeHost}
          onPrevious={openIndex > 0 ? stepTo(openIndex - 1) : undefined}
          onNext={openIndex >= 0 ? stepTo(openIndex + 1) : undefined}
          onAction={actions.request}
        />
      )}

      {actions.dialog}
    </div>
  );
}

function HostTableRow({
  row,
  priv,
  details,
  selected,
  open,
  onSelect,
  onOpen,
}: {
  row: HostRow;
  priv: boolean;
  details: boolean;
  selected: boolean;
  open: boolean;
  onSelect: () => void;
  onOpen: () => void;
}) {
  const s = row.summary;

  return (
    <tr
      data-host={row.name}
      aria-current={open || undefined}
      className={cx(
        'hover:bg-surface-hover',
        selected && 'bg-surface-selected',
        open && 'bg-surface-hover',
        priv && 'cursor-pointer'
      )}
      onClick={
        priv
          ? e => {
              // A click on a control inside the row is that control's.
              if (!(e.target as HTMLElement).closest('button, input, a')) onOpen();
            }
          : undefined
      }
    >
      {priv && (
        <Td tight className="pl-3 text-center">
          <input
            type="checkbox"
            aria-label={`Select ${row.name}`}
            checked={selected}
            onChange={onSelect}
            className="accent-accent"
          />
        </Td>
      )}
      <Td>
        <span className="flex items-center gap-2">
          {priv ? (
            <button
              type="button"
              aria-expanded={open}
              aria-label={`Open ${row.name}`}
              onClick={onOpen}
              className={cx('text-left text-sm/6 font-medium hover:underline', open && 'text-accent')}
            >
              {row.name}
            </button>
          ) : (
            <span className="text-sm/6 font-medium">{row.name}</span>
          )}
          {s &&
            list(s.roles)
              .filter(role => role !== 'ethereum_node')
              .map(role => (
                <Badge key={role} tone={role === 'bootnode' ? 'info' : 'neutral'}>
                  {role}
                </Badge>
              ))}
          {/* The clients are named in their columns; an owner of another container is named here. */}
          {s &&
            hostOwners(s)
              .filter(owner => owner !== row.elClient && owner !== row.clClient)
              .map(owner => (
                <span key={owner} title="Owner: a team that owns another container of this host">
                  <ClientBadge name={owner} />
                </span>
              ))}
        </span>
      </Td>
      <Td tight>
        {row.elClient ? (
          <span title="Execution client">
            <ClientBadge name={row.elClient} />
          </span>
        ) : (
          <Dash />
        )}
      </Td>
      <Td>
        <Image image={row.el} />
      </Td>
      <Td>
        <Digest value={row.el?.digest} />
      </Td>
      <Td tight>
        {row.clClient ? (
          <span title="Consensus client">
            <ClientBadge name={row.clClient} />
          </span>
        ) : (
          <Dash />
        )}
      </Td>
      <Td>
        <Image image={row.cl} />
      </Td>
      <Td>
        <Digest value={row.cl?.digest} />
      </Td>
      {!priv && (
        <Td>
          {row.updatePending ? (
            <Badge tone="warning" title="The host has an update waiting (image, packages or reboot)">
              pending
            </Badge>
          ) : (
            <span className="font-medium text-success">✓ up to date</span>
          )}
        </Td>
      )}
      {priv && (s ? <PrivateCells row={row} summary={s} details={details} /> : <MissingCells details={details} />)}
    </tr>
  );
}

/** Uptime ticks in the page from the time the host booted; the stream does not send it again with every sync. */
function Uptime({ host }: { host: HostListing }) {
  const now = useSharedNow();
  const seconds =
    host.bootTime && !isZeroTime(host.bootTime)
      ? Math.max(0, Math.floor((now - Date.parse(host.bootTime)) / 1000))
      : host.uptimeSeconds;

  return (
    <span
      className="inline-block min-w-16 tabular-nums"
      title={host.bootTime ? absoluteTime(host.bootTime) : undefined}
    >
      {formatDuration(seconds)}
    </span>
  );
}

function MissingCells({ details }: { details: boolean }) {
  return (
    <Td colSpan={details ? 9 : 4} className="text-faint">
      no details yet
    </Td>
  );
}

function PrivateCells({
  row,
  summary: s,
  details,
}: {
  row: HostRow;
  summary: HostListing;
  /** details: the login reads the system of the hosts (details). */
  details: boolean;
}) {
  const share = attestingShare(s);
  const state = hostState(row);
  const needs = list(s.needs);
  const kernelBehind = s.os.newestKernel !== '' && s.os.newestKernel !== s.os.runningKernel;

  return (
    <>
      <Td>
        <span
          className="flex items-center gap-2"
          title={needs.length > 0 ? `Open needs: ${needs.map(n => n.kind).join(', ')}` : undefined}
        >
          {state ? (
            <Badge tone={state.tone} title={state.title}>
              {state.label}
            </Badge>
          ) : (
            <span className="font-medium text-success">✓ up to date</span>
          )}
          <OfflineBadge mark={s.offline} />
          {s.plan.notReady && (
            <Badge tone="warning" title={`Not ready by its last sync: ${s.plan.notReady}`}>
              not ready
            </Badge>
          )}
          {s.hold.local && (
            <Badge tone="warning" title={`Local hold file${s.hold.localReason ? `: ${s.hold.localReason}` : ''}`}>
              local hold
            </Badge>
          )}
        </span>
      </Td>
      <Td>
        {/* A fixed width: the waiting reason changes, the columns do not. */}
        {(state?.detail ?? s.plan.notReady) ? (
          <span className="block w-56 truncate text-muted 2xl:w-80" title={state?.detail ?? s.plan.notReady}>
            {state?.detail ?? s.plan.notReady}
          </span>
        ) : (
          <span className="block w-56 text-faint 2xl:w-80">–</span>
        )}
      </Td>
      <Td>
        {s.plan.weight === 0 ? (
          <span className="text-faint" title="No validators">
            –
          </span>
        ) : share === undefined ? (
          <span className="text-muted" title="No liveness data yet">
            ? <span className="text-faint">of {s.plan.weight}</span>
          </span>
        ) : (
          <span
            className="flex items-center gap-2 tabular-nums"
            title={`${s.plan.live} of ${s.plan.weight} validators attested in the last complete epoch`}
          >
            <span
              aria-hidden="true"
              className={cx(
                'size-2 rounded-full',
                share >= 0.9 ? 'bg-success' : share > 0 ? 'bg-warning' : 'bg-danger-solid'
              )}
            />
            <span className={cx('min-w-11', share < 0.9 && 'font-medium', share === 0 && 'text-danger')}>
              {formatPercent(share * 100)}
            </span>
            <span className="hidden min-w-12 text-faint 2xl:inline">
              {s.plan.live}/{s.plan.weight}
            </span>
          </span>
        )}
      </Td>
      {details && (
        <>
          <Td>
            <UsageCell value={s.usage?.cpu} label={usageLabel('cpu', s.name)} />
          </Td>
          <Td>
            <UsageCell value={s.usage?.memory} label={usageLabel('memory', s.name)} />
          </Td>
          <Td>
            <span className="flex items-baseline gap-2" title={`${s.os.prettyName} (${s.os.arch})`}>
              <span className="font-mono">{s.os.runningKernel || <Dash />}</span>
              {kernelBehind && (
                <span className="text-warning" title={`A newer kernel is installed: ${s.os.newestKernel}`}>
                  ↑
                </span>
              )}
              <span className="hidden text-faint 2xl:inline">
                {s.os.id} {s.os.versionId}
              </span>
            </span>
          </Td>
          <Td className="hidden xl:table-cell">
            <Uptime host={s} />
          </Td>
          <Td
            title={`${s.pending.upgradable} upgradable, ${s.pending.security} security, ${s.pending.blocked} blocked`}
          >
            <span className="flex items-center gap-2">
              {s.pending.security > 0 ? (
                <span className="font-medium text-danger">{s.pending.security} security</span>
              ) : s.pending.upgradable > 0 ? (
                <span className="text-muted">{s.pending.upgradable}</span>
              ) : (
                <span className="text-faint">–</span>
              )}
              {s.pending.blocked > 0 && <span className="text-warning">{s.pending.blocked} blocked</span>}
              {s.os.rebootRequired && (
                <Badge
                  tone="warning"
                  title={`Reboot required by: ${list(s.os.rebootPackages).join(', ') || 'unknown'}`}
                >
                  reboot
                </Badge>
              )}
            </span>
          </Td>
        </>
      )}
      <Td className="hidden 2xl:table-cell">
        {/* A host that syncs is "live": when exactly it last did is not sent. A silent one says since when. */}
        {s.stale ? (
          <span className="font-medium text-warning">
            <RelTime fixed value={s.lastSeen} />
          </span>
        ) : (
          <span className="inline-block min-w-20 text-muted" title="The agent syncs">
            live
          </span>
        )}
      </Td>
    </>
  );
}

/** Image is the tag of a container's image; the full reference is its title. */
function Image({ image }: { image: PublicImageRef | undefined }) {
  if (!image) return <Dash />;

  return (
    <span className="block max-w-40 truncate font-mono text-xs/5 2xl:max-w-64" title={image.image}>
      {displayImage(image.image)}
    </span>
  );
}
