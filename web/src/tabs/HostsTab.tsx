import { Fragment, useMemo, useState } from 'react';

import { list, type HostSummary } from '../api/types';
import { useApp, useAuthState, usePrivateAccess } from '../appContext';
import { useActions, type ActionIntent } from '../components/ActionDialog';
import { GateIndicator } from '../components/GateIndicator';
import { ImageStrip } from '../components/ImageStrip';
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
import { ImageCell, SlotBadge } from '../components/status';
import { usePoll } from '../hooks/usePoll';
import { buildHoldRequest } from '../lib/actionRequests';
import { formatDuration, formatPercent, plural } from '../lib/format';
import { HostDetailPanel } from './HostDetailPanel';
import {
  attestingShare,
  matchesFilter,
  mergeHostRows,
  rollableContainers,
  sortHostRows,
  type HostRow,
  type SortKey,
} from './hostRows';

/** holdIntent builds the confirm dialog of hold / unhold / forget. */
function holdIntent(
  action: 'hold' | 'unhold' | 'forget',
  hosts: readonly string[],
  hiddenHosts: readonly string[] = []
): ActionIntent {
  const verb = action === 'hold' ? 'Hold' : action === 'unhold' ? 'Unhold' : 'Forget';
  return {
    kind: 'call',
    title: `${verb} ${plural(hosts.length, 'host')}`,
    danger: action === 'forget',
    hiddenHosts,
    make: reason => ({ action, body: buildHoldRequest(hosts, reason) }),
  };
}

/**
 * HostsTab is the default view. Logged out it shows only the public data
 * (name, EL / CL image, update marker); logged in the same table gains the
 * host details, the expandable rows and the actions.
 */
export function HostsTab() {
  const { api, auth } = useApp();
  const priv = usePrivateAccess();
  const authEnabled = useAuthState(auth).enabled;

  const publicHosts = usePoll(s => api.publicHosts(s), { key: 'public-hosts' });
  const images = usePoll(s => api.publicImages(s), { key: 'public-images' });
  const gate = usePoll(s => api.publicNetwork(s), { key: 'public-network' });
  const hosts = usePoll(s => api.hosts(s), { key: 'hosts', enabled: priv });
  const plan = usePoll(s => api.plan(s), { key: 'plan', enabled: priv });

  const [filter, setFilter] = useState('');
  const [sort, setSort] = useState<{ key: SortKey; direction: SortDirection }>({ key: 'name', direction: 'asc' });
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const refreshAll = () => {
    publicHosts.refresh();
    images.refresh();
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
  const columnCount = priv ? 13 : 4;

  const loading = publicHosts.loading || (priv && hosts.loading);

  return (
    <div className="flex flex-col gap-2">
      {gate.data && <GateIndicator gate={gate.data} />}
      {gate.error !== undefined && <ErrorBanner error={gate.error} what="the network gate" onRetry={gate.refresh} />}

      <ImageStrip images={list(images.data?.images)} />
      {images.error !== undefined && (
        <ErrorBanner error={images.error} what="the image summary" onRetry={images.refresh} />
      )}

      <div className="flex flex-wrap items-center gap-2">
        <input
          type="search"
          aria-label="Filter hosts"
          placeholder={priv ? 'Filter: name, image, role, slot…' : 'Filter: name, image…'}
          value={filter}
          onChange={e => {
            setFilter(e.target.value);
          }}
          className={cx(INPUT_CLASS, 'w-64')}
        />
        <span className="text-xs/5 text-muted">
          {rows.length === allRows.length
            ? plural(allRows.length, 'host')
            : `${rows.length} of ${plural(allRows.length, 'host')}`}
        </span>

        {priv && (
          <div role="group" aria-label="Bulk actions" className="flex flex-wrap items-center gap-1.5">
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
                <Button
                  dense
                  onClick={() => {
                    actions.request({
                      kind: 'roll',
                      hosts: selection,
                      all: false,
                      containerChoices: rollableContainers(selectedSummaries),
                      hiddenHosts: hiddenSelection,
                    });
                  }}
                >
                  Roll…
                </Button>
                <Button
                  dense
                  onClick={() => {
                    actions.request({ kind: 'reboot', hosts: selection, all: false, hiddenHosts: hiddenSelection });
                  }}
                >
                  Reboot…
                </Button>
                <Button
                  dense
                  onClick={() => {
                    actions.request(holdIntent('hold', selection, hiddenSelection));
                  }}
                >
                  Hold…
                </Button>
                <Button
                  dense
                  onClick={() => {
                    actions.request(holdIntent('unhold', selection, hiddenSelection));
                  }}
                >
                  Unhold…
                </Button>
                <Button
                  dense
                  variant="danger"
                  onClick={() => {
                    actions.request(holdIntent('forget', selection, hiddenSelection));
                  }}
                >
                  Forget…
                </Button>
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
                <span className="text-xs/5 text-muted">Whole fleet:</span>
                <Button
                  dense
                  onClick={() => {
                    actions.request({
                      kind: 'roll',
                      hosts: [],
                      all: true,
                      containerChoices: rollableContainers(list(hosts.data?.hosts)),
                    });
                  }}
                >
                  Roll all…
                </Button>
                <Button
                  dense
                  onClick={() => {
                    actions.request({ kind: 'reboot', hosts: [], all: true });
                  }}
                >
                  Reboot all…
                </Button>
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
        <Table label="Hosts">
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
              <SortTh columns={[sortColumn('el', 'EL image')]} />
              <SortTh columns={[sortColumn('cl', 'CL image')]} />
              <SortTh columns={[sortColumn('pending', 'Update', 'The host has an open need')]} />
              {priv && (
                <>
                  <SortTh columns={[sortColumn('kernel', 'Kernel'), sortColumn('os', 'OS')]} />
                  <SortTh
                    columns={[
                      sortColumn('uptime', 'Uptime'),
                      sortColumn('reboot', 'Reboot', '/var/run/reboot-required exists'),
                    ]}
                  />
                  <SortTh columns={[sortColumn('security', 'Sec.', 'Pending security updates')]} />
                  <SortTh
                    columns={[sortColumn('attesting', 'Attesting', 'Validators live in the last complete epoch')]}
                  />
                  <SortTh columns={[sortColumn('lastSeen', 'Seen', 'When the agent last synced')]} />
                  <SortTh columns={[sortColumn('slot', 'Slot')]} />
                  <Th>Actions</Th>
                </>
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
                    onAction={actions.request}
                  />
                  {priv && isOpen && (
                    <tr>
                      <td colSpan={columnCount} className="border-b border-border-strong bg-background p-2">
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
  onAction,
}: {
  row: HostRow;
  priv: boolean;
  selected: boolean;
  expanded: boolean;
  onSelect: () => void;
  onExpand: () => void;
  onAction: (intent: ActionIntent) => void;
}) {
  const s = row.summary;

  return (
    <tr className={cx('hover:bg-surface-hover', selected && 'bg-surface-selected')}>
      {priv && (
        <>
          <Td tight className="text-center">
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
              className="rounded-xs px-1 text-muted hover:bg-surface-hover hover:text-foreground"
            >
              {expanded ? '▾' : '▸'}
            </button>
          </Td>
        </>
      )}
      <Td>
        <span className={cx('flex gap-x-1.5', priv ? 'flex-col' : 'items-center')}>
          <span className="font-medium">{row.name}</span>
          {s && (
            <span className="flex items-center gap-1.5">
              {list(s.roles).map(role => (
                <Badge key={role} tone={role === 'bootnode' ? 'info' : 'neutral'}>
                  {role}
                </Badge>
              ))}
              {s.validators > 0 && (
                <span className="text-faint" title={`${s.validators} validators`}>
                  {s.validators}v
                </span>
              )}
            </span>
          )}
        </span>
      </Td>
      <Td>
        <ImageCell image={row.el} stacked={priv} />
      </Td>
      <Td>
        <ImageCell image={row.cl} stacked={priv} />
      </Td>
      <Td>
        {row.updatePending ? (
          <Badge tone="warning" title="The host has an open need (image, packages or reboot)">
            pending
          </Badge>
        ) : (
          <span className="text-faint">up to date</span>
        )}
      </Td>
      {priv &&
        (s ? <PrivateCells summary={s} queuePosition={row.queuePosition} onAction={onAction} /> : <MissingCells />)}
    </tr>
  );
}

function MissingCells() {
  return (
    <Td colSpan={7} className="text-faint">
      no details yet
    </Td>
  );
}

function PrivateCells({
  summary: s,
  queuePosition,
  onAction,
}: {
  summary: HostSummary;
  queuePosition: number | undefined;
  onAction: (intent: ActionIntent) => void;
}) {
  const share = attestingShare(s);
  const verbs = list(s.verbs);
  const held = s.holdWanted || s.hold.controller;
  const kernelBehind = s.os.newestKernel !== '' && s.os.newestKernel !== s.os.runningKernel;

  return (
    <>
      <Td>
        <span className="flex flex-col">
          <span>
            <span className="font-mono">{s.os.runningKernel || <Dash />}</span>
            {kernelBehind && (
              <span className="text-warning" title={`A newer kernel is installed: ${s.os.newestKernel}`}>
                {' '}
                ↑
              </span>
            )}
          </span>
          <span className="text-muted" title={`${s.os.prettyName} (${s.os.arch})`}>
            {s.os.id} {s.os.versionId}
          </span>
        </span>
      </Td>
      <Td>
        <span className="flex flex-col items-start">
          <span title={`${s.uptimeSeconds} s`}>{formatDuration(s.uptimeSeconds)}</span>
          {s.os.rebootRequired && (
            <Badge tone="warning" title={`Reboot required by: ${list(s.os.rebootPackages).join(', ') || 'unknown'}`}>
              reboot
            </Badge>
          )}
        </span>
      </Td>
      <Td title={`${s.pending.upgradable} upgradable, ${s.pending.security} security, ${s.pending.blocked} blocked`}>
        {s.pending.security > 0 ? (
          <span className="font-medium text-danger">{s.pending.security}</span>
        ) : (
          <span className="text-faint">0</span>
        )}
        {s.pending.blocked > 0 && <span className="text-warning"> +{s.pending.blocked}b</span>}
      </Td>
      <Td>
        {s.plan.weight === 0 ? (
          <span className="text-faint" title="No validators">
            –
          </span>
        ) : share === undefined ? (
          <span className="text-muted" title="No liveness data yet">
            ? <span className="text-faint">(?/{s.plan.weight})</span>
          </span>
        ) : (
          <span
            className={cx(
              'flex flex-col font-mono',
              share >= 0.9 ? 'text-success' : share > 0 ? 'text-warning' : 'text-danger'
            )}
            title={`${s.plan.live} of ${s.plan.weight} validators attested in the last complete epoch`}
          >
            <span>{formatPercent(share * 100)}</span>
            <span className="text-faint">
              {s.plan.live}/{s.plan.weight}
            </span>
          </span>
        )}
      </Td>
      <Td>
        <span className={cx('flex flex-col items-start', s.stale && 'font-medium text-warning')}>
          <RelTime value={s.lastSeen} />
          {s.stale && <Badge tone="warning">stale</Badge>}
        </span>
      </Td>
      <Td>
        <span className="flex flex-col">
          <span className="flex items-center gap-1.5">
            <SlotBadge slot={s.plan.slot} />
            {queuePosition !== undefined && <span className="text-faint">#{queuePosition}</span>}
            {s.hold.local && (
              <Badge tone="warning" title={`Local hold file${s.hold.localReason ? `: ${s.hold.localReason}` : ''}`}>
                local hold
              </Badge>
            )}
          </span>
          {s.plan.reason && (
            <span className="max-w-32 truncate text-muted" title={`Waiting reason: ${s.plan.reason}`}>
              {s.plan.reason}
            </span>
          )}
        </span>
      </Td>
      <Td>
        <span className="grid min-w-max grid-cols-2 gap-0.5">
          <Button
            dense
            disabled={!verbs.includes('roll')}
            title={verbs.includes('roll') ? undefined : 'The agent does not allow the roll verb'}
            aria-label={`Roll ${s.name}`}
            onClick={() => {
              onAction({ kind: 'roll', hosts: [s.name], all: false, containerChoices: rollableContainers([s]) });
            }}
          >
            Roll
          </Button>
          <Button
            dense
            disabled={!verbs.includes('reboot')}
            title={verbs.includes('reboot') ? undefined : 'The agent does not allow the reboot verb'}
            aria-label={`Reboot ${s.name}`}
            onClick={() => {
              onAction({ kind: 'reboot', hosts: [s.name], all: false });
            }}
          >
            Reboot
          </Button>
          <Button
            dense
            aria-label={`${held ? 'Unhold' : 'Hold'} ${s.name}`}
            onClick={() => {
              onAction(holdIntent(held ? 'unhold' : 'hold', [s.name]));
            }}
          >
            {held ? 'Unhold' : 'Hold'}
          </Button>
          <Button
            dense
            variant="ghost"
            aria-label={`Forget ${s.name}`}
            title="Delete this host from the controller"
            onClick={() => {
              onAction(holdIntent('forget', [s.name]));
            }}
          >
            Forget
          </Button>
        </span>
      </Td>
    </>
  );
}
