import { Fragment, useMemo, useState } from 'react';

import { list, type HostSummary } from '../api/types';
import { useApp, useAuthState, usePrivateAccess } from '../appContext';
import { useActions } from '../components/ActionDialog';
import { ActionButton } from '../components/Permissions';
import { RelTime } from '../components/RelTime';
import {
  Badge,
  Button,
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
import { ImageCell, needSummary } from '../components/status';
import { usePoll } from '../hooks/usePoll';
import { formatDuration, formatPercent, plural } from '../lib/format';
import { holdIntent } from '../lib/intents';
import { HostDetailPanel } from './HostDetailPanel';
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

/**
 * HostsTab is the fleet table. Logged out it shows only the public data
 * (name, EL / CL image, update marker); logged in the same table gains the
 * host's state, and a row opens to the host's details and actions. Actions on
 * several hosts go through the selection.
 */
export function HostsTab() {
  const { api, auth } = useApp();
  const priv = usePrivateAccess();
  const authEnabled = useAuthState(auth).enabled;

  const publicHosts = usePoll(s => api.publicHosts(s), { key: 'public-hosts' });
  const hosts = usePoll(s => api.hosts(s), { key: 'hosts', enabled: priv });
  const plan = usePoll(s => api.plan(s), { key: 'plan', enabled: priv });

  const [filter, setFilter] = useState('');
  const [sort, setSort] = useState<{ key: SortKey; direction: SortDirection }>({ key: 'name', direction: 'asc' });
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const refreshAll = () => {
    publicHosts.refresh();
    hosts.refresh();
    plan.refresh();
  };
  const actions = useActions(refreshAll);

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
  // Bulk actions act on the whole selection, also on hosts the filter hides
  // right now: the toolbar and the confirmation dialog say so.
  const hiddenSelection = useMemo(() => {
    const visible = new Set(rows.map(r => r.name));
    return selection.filter(n => !visible.has(n));
  }, [rows, selection]);
  const allVisibleSelected = visibleNames.length > 0 && visibleNames.every(n => selected.has(n));
  const columnCount = priv ? 11 : 4;

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
          }}
          className={cx(INPUT_CLASS, 'w-72')}
        />
        <span className="text-xs/5 text-muted">
          {rows.length === allRows.length
            ? plural(allRows.length, 'host')
            : `${rows.length} of ${plural(allRows.length, 'host')}`}
        </span>

        {priv && (
          <div role="group" aria-label="Bulk actions" className="ml-auto flex flex-wrap items-center gap-1.5">
            {selection.length > 0 ? (
              <>
                <span className="text-xs/5 font-medium">
                  {selection.length} selected
                  {hiddenSelection.length > 0 && (
                    <span className="text-warning" title={hiddenSelection.join(', ')}>
                      {' '}
                      ({hiddenSelection.length} hidden by the filter)
                    </span>
                  )}
                  :
                </span>
                {hiddenSelection.length > 0 && (
                  <Button
                    dense
                    variant="ghost"
                    title="Deselect the hosts the filter hides"
                    onClick={() => {
                      const hidden = new Set(hiddenSelection);
                      setSelected(prev => new Set([...prev].filter(n => !hidden.has(n))));
                    }}
                  >
                    Drop hidden
                  </Button>
                )}
                <ActionButton
                  dense
                  onAction={actions.request}
                  intent={{
                    kind: 'roll',
                    hosts: selection,
                    all: false,
                    containerChoices: rollableContainers(selectedSummaries),
                    hiddenHosts: hiddenSelection,
                  }}
                >
                  Roll…
                </ActionButton>
                <ActionButton
                  dense
                  onAction={actions.request}
                  intent={{ kind: 'reboot', hosts: selection, all: false, hiddenHosts: hiddenSelection }}
                >
                  Reboot…
                </ActionButton>
                <ActionButton dense onAction={actions.request} intent={holdIntent('hold', selection, hiddenSelection)}>
                  Hold…
                </ActionButton>
                <ActionButton
                  dense
                  onAction={actions.request}
                  intent={holdIntent('unhold', selection, hiddenSelection)}
                >
                  Unhold…
                </ActionButton>
                <ActionButton
                  dense
                  variant="danger"
                  onAction={actions.request}
                  intent={holdIntent('forget', selection, hiddenSelection)}
                >
                  Forget…
                </ActionButton>
                <Button
                  dense
                  variant="ghost"
                  onClick={() => {
                    setSelected(new Set());
                  }}
                >
                  Clear
                </Button>
              </>
            ) : (
              <>
                <ActionButton
                  dense
                  variant="ghost"
                  onAction={actions.request}
                  intent={{
                    kind: 'roll',
                    hosts: [],
                    all: true,
                    containerChoices: rollableContainers(list(hosts.data?.hosts)),
                  }}
                >
                  Roll all hosts…
                </ActionButton>
                <ActionButton
                  dense
                  variant="ghost"
                  onAction={actions.request}
                  intent={{ kind: 'reboot', hosts: [], all: true }}
                >
                  Reboot all hosts…
                </ActionButton>
              </>
            )}
          </div>
        )}

        {!priv && authEnabled && (
          <span className="text-xs/5 text-muted">Log in for host details, rollouts and actions.</span>
        )}
      </div>

      {actions.notice}
      {publicHosts.error !== undefined && (
        <ErrorBanner error={publicHosts.error} what="the hosts" onRetry={publicHosts.refresh} />
      )}
      {hosts.error !== undefined && <ErrorBanner error={hosts.error} what="the host details" onRetry={hosts.refresh} />}
      {plan.error !== undefined && <ErrorBanner error={plan.error} what="the plan" onRetry={plan.refresh} />}

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
                <>
                  <Th tight className="w-6 text-center">
                    <input
                      type="checkbox"
                      aria-label="Select all shown hosts"
                      checked={allVisibleSelected}
                      onChange={() => {
                        setSelected(allVisibleSelected ? new Set() : new Set(visibleNames));
                      }}
                      className="accent-accent"
                    />
                  </Th>
                  <Th tight className="w-6 text-center">
                    <span className="sr-only">Expand</span>
                  </Th>
                </>
              )}
              <SortTh columns={[sortColumn('name', 'Host')]} />
              <SortTh columns={[sortColumn('el', 'Execution')]} />
              <SortTh columns={[sortColumn('cl', 'Consensus')]} />
              {priv ? (
                <>
                  <SortTh columns={[sortColumn('slot', 'State', 'What the scheduler does with the host')]} />
                  <SortTh
                    columns={[sortColumn('attesting', 'Attesting', 'Validators live in the last complete epoch')]}
                  />
                  <SortTh columns={[sortColumn('kernel', 'Kernel')]} />
                  <SortTh className="hidden xl:table-cell" columns={[sortColumn('uptime', 'Uptime')]} />
                  <SortTh columns={[sortColumn('security', 'Updates', 'Pending package updates')]} />
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
            {rows.map(row => {
              const isOpen = expanded.has(row.name);
              return (
                <Fragment key={row.name}>
                  <HostTableRow
                    row={row}
                    priv={priv}
                    selected={selected.has(row.name)}
                    expanded={isOpen}
                    onSelect={() => {
                      setSelected(prev => toggle(prev, row.name));
                    }}
                    onExpand={() => {
                      setExpanded(prev => toggle(prev, row.name));
                    }}
                  />
                  {priv && isOpen && (
                    <tr>
                      <td colSpan={columnCount} className="border-b border-border-strong bg-background p-4">
                        <HostDetailPanel name={row.name} onAction={actions.request} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        </Table>
      )}

      {actions.dialog}
    </div>
  );
}

function HostTableRow({
  row,
  priv,
  selected,
  expanded,
  onSelect,
  onExpand,
}: {
  row: HostRow;
  priv: boolean;
  selected: boolean;
  expanded: boolean;
  onSelect: () => void;
  onExpand: () => void;
}) {
  const s = row.summary;

  return (
    <tr
      className={cx('hover:bg-surface-hover', selected && 'bg-surface-selected', priv && 'cursor-pointer')}
      onClick={
        priv
          ? e => {
              // A click on a control inside the row is that control's.
              if (!(e.target as HTMLElement).closest('button, input, a')) onExpand();
            }
          : undefined
      }
    >
      {priv && (
        <>
          <Td tight className="pl-3 text-center">
            <input
              type="checkbox"
              aria-label={`Select ${row.name}`}
              checked={selected}
              onChange={onSelect}
              className="accent-accent"
            />
          </Td>
          <Td tight className="text-center">
            <button
              type="button"
              aria-expanded={expanded}
              aria-label={`${expanded ? 'Collapse' : 'Expand'} ${row.name}`}
              onClick={onExpand}
              className="rounded-xs px-1 text-muted hover:text-foreground"
            >
              {expanded ? '▾' : '▸'}
            </button>
          </Td>
        </>
      )}
      <Td>
        <span className="flex items-center gap-2">
          <span className="text-sm/6 font-medium">{row.name}</span>
          {s &&
            list(s.roles)
              .filter(role => role !== 'ethereum_node')
              .map(role => (
                <Badge key={role} tone={role === 'bootnode' ? 'info' : 'neutral'}>
                  {role}
                </Badge>
              ))}
          {s && hostOwners(s).length > 0 && (
            <span
              className="rounded-sm border border-border px-1.5 text-xs/5 whitespace-nowrap text-muted"
              title="Owners: the teams that own a container of this host"
            >
              {hostOwners(s).join(' · ')}
            </span>
          )}
        </span>
      </Td>
      <Td>
        <ImageCell image={row.el} compact />
      </Td>
      <Td>
        <ImageCell image={row.cl} compact />
      </Td>
      {!priv && (
        <Td>
          {row.updatePending ? (
            <Badge tone="warning" title="The host has an update waiting (image, packages or reboot)">
              pending
            </Badge>
          ) : (
            <span className="text-faint">up to date</span>
          )}
        </Td>
      )}
      {priv && (s ? <PrivateCells row={row} summary={s} /> : <MissingCells />)}
    </tr>
  );
}

function MissingCells() {
  return (
    <Td colSpan={6} className="text-faint">
      no details yet
    </Td>
  );
}

function PrivateCells({ row, summary: s }: { row: HostRow; summary: HostSummary }) {
  const share = attestingShare(s);
  const state = hostState(row);
  const needs = list(s.needs);
  const kernelBehind = s.os.newestKernel !== '' && s.os.newestKernel !== s.os.runningKernel;

  return (
    <>
      <Td>
        <span className="flex items-center gap-2" title={needs.map(needSummary).join('\n') || undefined}>
          {state ? <Badge tone={state.tone}>{state.label}</Badge> : <span className="text-faint">up to date</span>}
          {s.hold.local && (
            <Badge tone="warning" title={`Local hold file${s.hold.localReason ? `: ${s.hold.localReason}` : ''}`}>
              local hold
            </Badge>
          )}
          {state?.detail && (
            <span className="max-w-36 truncate text-muted 2xl:max-w-80" title={state.detail}>
              {state.detail}
            </span>
          )}
        </span>
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
            <span className={cx(share < 0.9 && 'font-medium', share === 0 && 'text-danger')}>
              {formatPercent(share * 100)}
            </span>
            <span className="hidden text-faint 2xl:inline">
              {s.plan.live}/{s.plan.weight}
            </span>
          </span>
        )}
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
        <span className="tabular-nums" title={`${s.uptimeSeconds} s`}>
          {formatDuration(s.uptimeSeconds)}
        </span>
      </Td>
      <Td title={`${s.pending.upgradable} upgradable, ${s.pending.security} security, ${s.pending.blocked} blocked`}>
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
            <Badge tone="warning" title={`Reboot required by: ${list(s.os.rebootPackages).join(', ') || 'unknown'}`}>
              reboot
            </Badge>
          )}
        </span>
      </Td>
      <Td className="hidden 2xl:table-cell">
        <span className={cx(s.stale ? 'font-medium text-warning' : 'text-muted')}>
          <RelTime value={s.lastSeen} />
        </span>
      </Td>
    </>
  );
}
