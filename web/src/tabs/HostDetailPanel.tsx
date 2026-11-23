import { useState, type ReactNode } from 'react';

import {
  list,
  type ActionInfo,
  type ActionRun,
  type CLHealth,
  type Container,
  type ELHealth,
  type HostDetail,
  type Lease,
} from '../api/types';
import type { ActionIntent } from '../components/ActionDialog';
import { RunExit, RunOutput } from '../components/ActionRun';
import { AuditTable } from '../components/AuditTable';
import { Digest, ShortId } from '../components/Digest';
import { Drawer } from '../components/Drawer';
import { HostActionsMenu, SecurityUpdates } from '../components/HostActions';
import { Disclosure } from '../components/icons';
import { LeaseTable } from '../components/LeaseTable';
import { useLiveHost } from '../components/Live';
import { OverrideButtons, OverrideSummary, overrideState } from '../components/Overrides';
import { usePermissions } from '../components/Permissions';
import { Readiness } from '../components/Readiness';
import { RelTime } from '../components/RelTime';
import { IntentAction } from '../components/SelectionBar';
import { Badge, Dash, Empty, ErrorBanner, Loading, Table, Td, Th, cx } from '../components/primitives';
import { ImpactBadge, LeaseStateBadge, NeedLine, OfflineBadge } from '../components/status';
import { ContainerUsage, HostResources } from '../components/Usage';
import { displayImage, heldBy, isHeld, percentOf, shortRevision } from '../lib/format';
import { hostActionIntent, runnableActions, verbBlocked } from '../lib/hostActions';
import { holdIntent, offlineIntent } from '../lib/intents';
import { hostOwners, hostState, rollableContainers, type HostRow } from './hostRows';

type DrawerTab = 'overview' | 'containers' | 'actions' | 'history';

const DRAWER_TABS: readonly { id: DrawerTab; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'containers', label: 'Containers' },
  { id: 'actions', label: 'Actions' },
  { id: 'history', label: 'History' },
];

/**
 * HostDrawer shows one host of the Hosts table at the right edge: its state
 * and actions on top, the details in tabs. The tab stays when the drawer
 * steps to another host.
 */
export function HostDrawer({
  name,
  row,
  onClose,
  onPrevious,
  onNext,
  onAction,
}: {
  name: string;
  /** row is the host's row of the table, for its queue position. */
  row?: HostRow;
  onClose: () => void;
  onPrevious?: () => void;
  onNext?: () => void;
  onAction: (intent: ActionIntent) => void;
}) {
  const detail = useLiveHost(name);
  const [tab, setTab] = useState<DrawerTab>('overview');
  const h = detail.data;

  let body: ReactNode;
  if (detail.loading) body = <Loading what={name} />;
  else if (detail.gone) body = <Empty>The controller does not know {name} any more.</Empty>;
  else if (detail.crowded)
    body = <Empty>Too many host details are open at once. Close another host to load {name}.</Empty>;
  else if (!h) body = <ErrorBanner error={detail.error} what={name} />;

  return (
    <Drawer
      label={`Host ${name}`}
      onClose={onClose}
      onPrevious={onPrevious}
      onNext={onNext}
      header={
        h ? <HostHeader host={h} row={row} onAction={onAction} /> : <h2 className="text-sm/6 font-semibold">{name}</h2>
      }
    >
      {body ? (
        <div className="p-4">{body}</div>
      ) : (
        h && (
          <>
            <div role="tablist" aria-label={`Details of ${name}`} className="flex shrink-0 border-b border-border px-2">
              {DRAWER_TABS.filter(t => !(h.reduced && t.id === 'actions')).map(t => (
                <button
                  key={t.id}
                  type="button"
                  role="tab"
                  id={`host-tab-${t.id}`}
                  aria-selected={tab === t.id}
                  aria-controls="host-tab-panel"
                  onClick={() => {
                    setTab(t.id);
                  }}
                  className={cx(
                    'flex items-center gap-1 border-b-2 px-2.5 py-1.5 text-xs/5 font-medium',
                    tab === t.id
                      ? 'border-accent text-foreground'
                      : 'border-transparent text-muted hover:text-foreground'
                  )}
                >
                  {t.label}
                  <TabCount host={h} tab={t.id} />
                </button>
              ))}
            </div>
            <div
              id="host-tab-panel"
              role="tabpanel"
              aria-labelledby={`host-tab-${tab}`}
              className="flex min-h-0 grow flex-col gap-4 overflow-y-auto p-4"
            >
              {tab === 'overview' && <OverviewPane host={h} onAction={onAction} />}
              {tab === 'containers' && <ContainersPane host={h} onAction={onAction} />}
              {tab === 'actions' && !h.reduced && <ActionsPane host={h} onAction={onAction} />}
              {tab === 'history' && <HistoryPane host={h} />}
            </div>
          </>
        )
      )}
    </Drawer>
  );
}

function TabCount({ host: h, tab }: { host: HostDetail; tab: DrawerTab }) {
  const n =
    tab === 'containers'
      ? list(h.containers).length
      : tab === 'actions'
        ? list(h.actions).length
        : tab === 'history'
          ? list(h.leases).length
          : list(h.needs).length;
  if (n === 0) return null;
  return (
    <span aria-hidden="true" className="font-normal text-faint tabular-nums">
      {n}
    </span>
  );
}

function HostHeader({
  host: h,
  row,
  onAction,
}: {
  host: HostDetail;
  row?: HostRow;
  onAction: (intent: ActionIntent) => void;
}) {
  const verbs = list(h.verbs);
  const state = hostState(row ?? { name: h.name, updatePending: false, summary: h });
  const owners = hostOwners(h);
  const kinds = [...new Set(list(h.containers).flatMap(c => (c.managed && c.kind ? [c.kind.toUpperCase()] : [])))];

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="truncate text-sm/6 font-semibold">{h.name}</h2>
        {state ? (
          <Badge tone={state.tone} title={state.title ?? state.detail}>
            {state.label}
          </Badge>
        ) : (
          <Badge tone="success">up to date</Badge>
        )}
        <OfflineBadge mark={h.offline} />
      </div>
      {state?.detail && <p className="text-xs/5 text-muted">{state.detail}</p>}
      <p aria-label="Facts" className="flex flex-wrap gap-x-2 text-xs/5 text-muted">
        <span title="The teams that own a container of this host">{owners.join(', ') || 'no team'}</span>
        <span aria-hidden="true">·</span>
        <span>{kinds.join(' ') || 'no client'}</span>
        <span aria-hidden="true">·</span>
        {!h.reduced && (
          <>
            <span className="font-mono">agent {h.agentVersion || '?'}</span>
            <span aria-hidden="true">·</span>
          </>
        )}
        <span className={h.stale ? 'text-warning' : undefined}>
          seen <RelTime value={h.lastSeen} />
        </span>
      </p>
      <div role="group" aria-label={`Actions for ${h.name}`} className="flex flex-wrap items-center gap-0.5">
        <IntentAction
          icon="roll"
          label="Roll…"
          blocked={verbs.includes('roll') ? '' : 'The agent does not allow the verb roll'}
          onAction={onAction}
          intent={{ kind: 'roll', hosts: [h.name], all: false, containerChoices: rollableContainers([h]) }}
        />
        <IntentAction
          icon="reboot"
          label="Reboot…"
          blocked={verbs.includes('reboot') ? '' : 'The agent does not allow the verb reboot'}
          onAction={onAction}
          intent={{ kind: 'reboot', hosts: [h.name], all: false }}
        />
        <IntentAction icon="hold" label="Hold…" onAction={onAction} intent={holdIntent('hold', [h.name])} />
        <IntentAction
          icon="offline"
          label={h.offline ? 'Lift offline mark…' : 'Mark offline…'}
          onAction={onAction}
          intent={offlineIntent(!h.offline, [h.name])}
        />
        <IntentAction
          icon="forget"
          label="Forget…"
          tone="danger"
          onAction={onAction}
          intent={holdIntent('forget', [h.name])}
        />
        <HostActionsMenu host={h} onAction={onAction} />
      </div>
    </>
  );
}

// ------------------------------------------------------------------ overview

function Group({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section aria-label={title} className="flex flex-col gap-1">
      <h3 className="text-xs/5 font-semibold tracking-wide text-muted uppercase">{title}</h3>
      {children}
    </section>
  );
}

function Facts({ children }: { children: ReactNode }) {
  return <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 text-xs/5">{children}</dl>;
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  );
}

function OverviewPane({ host: h, onAction }: { host: HostDetail; onAction: (intent: ActionIntent) => void }) {
  const needs = list(h.needs);
  const lease = list(h.leases).find(l => l.id === h.plan.leaseId);
  const verbs = list(h.verbs);

  return (
    <>
      <Group title="Now">
        <Facts>
          {lease && (
            <Fact label="lease">
              <span className="inline-flex flex-wrap items-center gap-1.5">
                <ShortId value={lease.id} label="lease ID" />
                <LeaseStateBadge state={lease.state} />
                {lease.operation && <span className="text-muted">{lease.operation.phase}</span>}
              </span>
            </Fact>
          )}
          {lease?.readiness && (
            <Fact label="readiness">
              <Readiness lease={lease} />
            </Fact>
          )}
          <Fact label="hold">
            <HoldFact host={h} onAction={onAction} />
          </Fact>
          {h.broken && Object.keys(h.broken).length > 0 && (
            <Fact label="broken">
              <span className="flex flex-wrap gap-1">
                {Object.entries(h.broken).map(([group, leaseId]) => (
                  <Badge
                    key={group}
                    tone="danger"
                    title={`Lease ${leaseId} failed on it: the host goes first in the next rollout of it`}
                  >
                    {group.replace(/^unit:/, '')}
                  </Badge>
                ))}
              </span>
            </Fact>
          )}
          <Fact label="needs">
            {needs.length === 0 ? (
              'none'
            ) : (
              <span className="flex flex-col gap-0.5">
                <ul aria-label="Open needs" className="flex flex-col">
                  {needs.map(n => (
                    <li key={n.id}>
                      <NeedLine need={n} />
                    </li>
                  ))}
                </ul>
                {isHeld(h) && (
                  <span className="text-warning">
                    {heldBy(h)}: {needs.length === 1 ? 'it waits' : 'they wait'} until the hold is lifted
                  </span>
                )}
              </span>
            )}
          </Fact>
        </Facts>
      </Group>

      {!h.reduced && (
        <Group title="Packages">
          <Facts>
            <Fact label="updates">
              <span className="inline-flex flex-wrap items-center gap-x-1">
                {h.pending.upgradable} upgradable,{' '}
                <span className={h.pending.security > 0 ? 'text-danger' : undefined}>
                  {h.pending.security} security
                </span>
                , {h.pending.blocked} blocked <SecurityUpdates host={h} onAction={onAction} />
              </span>
            </Fact>
            <Fact label="reboot">
              {h.os.rebootRequired ? (
                <span className="text-warning">
                  required by {list(h.os.rebootPackages).join(', ') || 'unknown packages'}
                </span>
              ) : (
                'not required'
              )}
            </Fact>
            <Fact label="inventory">
              {h.inventory ? (
                <>
                  {list(h.inventory.packages).length} packages, collected <RelTime value={h.inventory.collectedAt} />
                </>
              ) : (
                <span className="text-muted">not uploaded yet</span>
              )}
            </Fact>
          </Facts>
          {list(h.inventory?.upgradable).length > 0 && (
            <Disclosure summary={`Upgradable packages (${list(h.inventory?.upgradable).length})`}>
              <UpgradableTable detail={h} />
            </Disclosure>
          )}
        </Group>
      )}

      <Group title="Node">
        <Facts>
          {!h.reduced && (
            <>
              <Fact label="EL">
                {h.node.el ? <ELLine el={h.node.el} /> : <span className="text-muted">no probe</span>}
              </Fact>
              <Fact label="CL">
                {h.node.cl ? <CLLine cl={h.node.cl} /> : <span className="text-muted">no probe</span>}
              </Fact>
            </>
          )}
          <Fact label="attesting">
            {h.plan.weight === 0
              ? 'no validators'
              : h.plan.liveKnown
                ? `${percentOf(h.plan.live, h.plan.weight)} (${h.plan.live}/${h.plan.weight})`
                : 'unknown'}
          </Fact>
          <Fact label="validators">
            <AssignedValidators host={h} />
          </Fact>
        </Facts>
      </Group>

      {h.resources && (
        <Group title="System resources">
          <HostResources resources={h.resources} />
        </Group>
      )}

      <Group title="System">
        <Facts>
          <Fact label="roles">{list(h.roles).join(', ') || <Dash />}</Fact>
          {h.reduced ? (
            <>
              <Fact label="agent">
                <span className="text-muted">allows {verbs.join(', ') || 'nothing'}</span>
              </Fact>
              <Fact label="last sync">
                <RelTime value={h.lastSeen} />
              </Fact>
            </>
          ) : (
            <>
              <Fact label="OS">
                {h.os.prettyName} <span className="text-muted">({h.os.arch})</span>
              </Fact>
              <Fact label="kernel">
                <span className="font-mono">{h.os.runningKernel}</span>
                {h.os.newestKernel !== '' && h.os.newestKernel !== h.os.runningKernel && (
                  <span className="text-warning">
                    {' '}
                    → newest installed <span className="font-mono">{h.os.newestKernel}</span>
                  </span>
                )}
              </Fact>
              {h.inventory && (
                <Fact label="kernels">
                  <span className="font-mono">{list(h.inventory.kernels.installed).join(', ')}</span>
                </Fact>
              )}
              <Fact label="agent">
                <span className="font-mono">{h.agentVersion || '?'}</span>{' '}
                <span className={h.incompatible ? 'text-danger' : 'text-muted'} title={h.incompatible}>
                  protocol {h.agentProtocol}
                </span>{' '}
                <span className="text-muted">allows {verbs.join(', ') || 'nothing'}</span>
              </Fact>
              <Fact label="certificate">
                <ShortId value={h.certHash} label="certificate hash" />
              </Fact>
              <Fact label="first seen">
                <RelTime value={h.firstSeen} />
              </Fact>
              <Fact label="last sync">
                <RelTime value={h.lastSeen} /> <span className="text-muted">from</span>{' '}
                <span className="font-mono">{h.remoteAddr || '?'}</span>
              </Fact>
            </>
          )}
        </Facts>
      </Group>
    </>
  );
}

/**
 * HoldFact lists the holds the agent reports, the host's own first (lifted on the host only), and the hold
 * requests it has not answered yet.
 */
function HoldFact({ host: h, onAction }: { host: HostDetail; onAction: (intent: ActionIntent) => void }) {
  const holds = list(h.hold.holds);
  const requests = list(h.plan.holdRequests);
  if (!isHeld(h) && requests.length === 0) return <>none</>;

  return (
    <span className="flex flex-col gap-1">
      {holds.map(hold => (
        <span key={hold.holder} className="flex flex-wrap items-center gap-1.5">
          <Badge tone="warning">{hold.holder === 'host' ? 'on the host' : hold.holder}</Badge>
          {hold.reason && <span>{hold.reason}</span>}
          <span className="text-muted">
            {hold.by ? `${hold.by}, ` : ''}
            <RelTime value={hold.at} />
          </span>
          {hold.holder !== 'host' && (
            <IntentAction
              icon="unhold"
              label="Lift…"
              onAction={onAction}
              intent={holdIntent('unhold', [h.name], [], [hold.holder])}
            />
          )}
        </span>
      ))}
      {requests.map(r => (
        <span key={`${r.holder} ${r.at}`} className="text-muted">
          {r.release ? 'release' : 'hold'} of {r.holder} requested{r.by ? ` by ${r.by}` : ''}, waiting for the host
        </span>
      ))}
    </span>
  );
}

/** AssignedValidators says how many validators the inventory assigns to the host and how many are active on chain. */
function AssignedValidators({ host: h }: { host: HostDetail }) {
  if (!h.assignedValidators) return <>none</>;

  return (
    <span title="Assigned to this host by the validator ranges inventory; active: with an attestation duty">
      {h.assignedValidators} assigned, {h.validators} active
    </span>
  );
}

function ELLine({ el }: { el: ELHealth }) {
  if (!el.reachable) return <span className="text-danger">unreachable{el.error ? `: ${el.error}` : ''}</span>;
  return (
    <span>
      {el.syncing ? <Badge tone="warning">syncing</Badge> : <Badge tone="success">synced</Badge>}{' '}
      <span className={cx('tabular-nums', el.peers === 0 && 'text-danger')}>{el.peers} peers</span>, head{' '}
      <span className="font-mono">{el.head}</span>
    </span>
  );
}

function CLLine({ cl }: { cl: CLHealth }) {
  if (!cl.reachable) return <span className="text-danger">unreachable{cl.error ? `: ${cl.error}` : ''}</span>;
  return (
    <span>
      {cl.syncing ? (
        <Badge tone="warning">syncing (distance {cl.syncDistance})</Badge>
      ) : (
        <Badge tone="success">synced</Badge>
      )}{' '}
      {cl.optimistic && <Badge tone="warning">optimistic</Badge>}{' '}
      {cl.elOffline && <Badge tone="danger">EL offline</Badge>}{' '}
      <span className={cx('tabular-nums', cl.peers === 0 && 'text-danger')}>{cl.peers} peers</span>, head slot{' '}
      <span className="font-mono">{cl.headSlot}</span>
    </span>
  );
}

function UpgradableTable({ detail }: { detail: HostDetail }) {
  return (
    <Table label="Upgradable packages">
      <thead>
        <tr>
          <Th>Package</Th>
          <Th>Installed</Th>
          <Th>Candidate</Th>
          <Th>Origin</Th>
          <Th>Flags</Th>
        </tr>
      </thead>
      <tbody>
        {list(detail.inventory?.upgradable).map(u => (
          <tr key={`${u.name}:${u.arch}`} className="hover:bg-surface-hover">
            <Td className="font-mono">
              {u.name}
              <span className="text-faint">:{u.arch}</span>
            </Td>
            <Td className="font-mono text-muted">{u.current}</Td>
            <Td className="font-mono">{u.candidate}</Td>
            <Td className="text-muted">{u.origin}</Td>
            <Td>
              <span className="inline-flex gap-1">
                {u.security && <Badge tone="danger">security</Badge>}
                {u.blocked && (
                  <Badge tone="warning" title="Held back from unattended-upgrades; upgraded by upgradoor under a slot">
                    blocked
                  </Badge>
                )}
              </span>
            </Td>
          </tr>
        ))}
      </tbody>
    </Table>
  );
}

// ---------------------------------------------------------------- containers

function ContainersPane({ host: h, onAction }: { host: HostDetail; onAction: (intent: ActionIntent) => void }) {
  const containers = list(h.containers);
  if (containers.length === 0) return <Empty>The agent reports no containers.</Empty>;

  return (
    <ul aria-label="Containers" className="flex flex-col rounded-sm border border-border text-xs/5">
      {containers.map(c => (
        <ContainerRow key={c.id || c.name} host={h} container={c} onAction={onAction} />
      ))}
    </ul>
  );
}

const STATE_DOT: Record<string, string> = { running: 'bg-success', restarting: 'bg-warning' };

/** ContainerRow is one line per container; it opens to its details, its overrides and its command line. */
function ContainerRow({
  host: h,
  container: c,
  onAction,
}: {
  host: HostDetail;
  container: Container;
  onAction: (intent: ActionIntent) => void;
}) {
  const command = list(c.command);
  // A container of another team on a reduced host carries no internals.
  const internals = !h.reduced || c.id !== '';
  const behind = c.registry?.digest !== undefined && c.registry.digest !== '' && c.registry.digest !== c.digest;
  const override = c.override ? overrideState(c.override) : undefined;

  return (
    <li className="border-b border-border last:border-b-0">
      <details className="group">
        <summary
          aria-label={`Container ${c.name}`}
          className="flex cursor-pointer list-none items-center gap-2 px-2 py-1.5 hover:bg-surface-hover [&::-webkit-details-marker]:hidden"
        >
          <span
            aria-hidden="true"
            title={c.state}
            className={cx('size-2 shrink-0 rounded-full', STATE_DOT[c.state] ?? 'bg-danger-solid')}
          />
          <span className="font-medium">{c.name}</span>
          <span className="text-muted">{c.kind ? c.kind.toUpperCase() : c.managed ? 'unlabelled' : 'unmanaged'}</span>
          <span className="min-w-0 truncate font-mono text-muted" title={c.image}>
            {displayImage(c.image)}
          </span>
          <span className="ml-auto flex shrink-0 items-center gap-1">
            {c.stats && (
              <span className="mr-1 text-muted">
                <ContainerUsage stats={c.stats} />
              </span>
            )}
            {c.state !== 'running' && <Badge tone={c.state === 'restarting' ? 'warning' : 'danger'}>{c.state}</Badge>}
            {c.health && c.health !== 'healthy' && (
              <Badge tone={c.health === 'starting' ? 'info' : 'danger'}>{c.health}</Badge>
            )}
            {behind && <Badge tone="warning">newer</Badge>}
            {c.registry?.pullFailed && <Badge tone="danger">pull failed</Badge>}
            {override && (
              <Badge tone={override.tone} title={`Override: ${override.title}`}>
                override
              </Badge>
            )}
          </span>
        </summary>

        <div className="flex flex-col gap-2 border-t border-border bg-background px-2 py-2">
          <Facts>
            <Fact label="image">
              <span className="font-mono break-all">{c.image}</span>
              {c.managed && c.configuredImage && c.configuredImage !== c.image && !c.override && (
                <span
                  className="block text-muted"
                  title="the deployed block of overrides.yaml names another image: the container is rolled onto it"
                >
                  configured: <span className="font-mono break-all">{c.configuredImage}</span>
                </span>
              )}
            </Fact>
            <Fact label="team">
              <span className="inline-flex items-center gap-1.5">
                {c.team || <Dash />}
                {c.notRolled && (
                  <Badge title={`The agent rolls this container to nothing: ${c.notRolled}`}>not rolled</Badge>
                )}
              </span>
            </Fact>
            <Fact label="managed">
              <span className="inline-flex items-center gap-1.5">
                {c.managed ? <Badge tone="accent">managed</Badge> : <span className="text-faint">unmanaged</span>}
                {c.impact && <ImpactBadge impact={c.impact} />}
              </span>
            </Fact>
            <Fact label="state">
              <span className="inline-flex flex-wrap items-center gap-1.5">
                <Badge tone={c.state === 'running' ? 'success' : c.state === 'restarting' ? 'warning' : 'danger'}>
                  {c.state}
                </Badge>
                {c.health && (
                  <Badge tone={c.health === 'healthy' ? 'success' : c.health === 'starting' ? 'info' : 'danger'}>
                    {c.health}
                  </Badge>
                )}
                {internals && (
                  <>
                    <span className={c.restartCount > 0 ? 'font-medium text-warning' : 'text-muted'}>
                      {c.restartCount} restarts
                    </span>
                    <span className="text-muted">
                      started <RelTime value={c.startedAt} />
                    </span>
                  </>
                )}
              </span>
            </Fact>
            <Fact label="digest">
              <Digest value={c.digest} />
            </Fact>
            <Fact label="registry">
              {c.registry ? (
                <span className="inline-flex flex-wrap items-center gap-1.5">
                  <Digest value={c.registry.digest} />
                  {behind && <Badge tone="warning">{c.registry.pulled ? 'newer, pulled' : 'newer'}</Badge>}
                  {c.registry.error && (
                    <Badge
                      tone="danger"
                      title={c.registry.pullFailed ? `image pull failed: ${c.registry.error}` : c.registry.error}
                    >
                      {c.registry.pullFailed ? 'pull failed' : 'check failed'}
                    </Badge>
                  )}
                  <span className="text-faint">
                    <RelTime value={c.registry.checkedAt} />
                  </span>
                </span>
              ) : (
                <Dash />
              )}
            </Fact>
            <Fact label="revision">
              <span className="font-mono" title={[c.revision, c.version].filter(Boolean).join(' / ')}>
                {shortRevision(c.revision) || c.version || <Dash />}
              </span>
            </Fact>
          </Facts>

          {c.managed && c.team && internals && (
            <div className="flex flex-col gap-1">
              <span className="flex items-center gap-2">
                <span className="text-muted">override</span>
                <span role="group" aria-label={`Overrides of ${c.name}`} className="flex items-center gap-1">
                  <OverrideButtons host={h.name} containers={[c]} stale={h.stale} onAction={onAction} />
                </span>
              </span>
              {c.override ? <OverrideSummary override={c.override} /> : <span className="text-faint">no override</span>}
            </div>
          )}

          {internals && (
            <Disclosure summary={`command line (${command.length} ${command.length === 1 ? 'argument' : 'arguments'})`}>
              {command.length === 0 ? (
                <span className="text-muted">The agent reports no arguments.</span>
              ) : (
                <ul aria-label={`Command line of ${c.name}`} className="font-mono">
                  {command.map((arg, i) => (
                    <li key={`${i} ${arg}`} className="break-all">
                      {arg}
                    </li>
                  ))}
                </ul>
              )}
            </Disclosure>
          )}
        </div>
      </details>
    </li>
  );
}

// ------------------------------------------------------------------- actions

/** lastRuns maps each host action to its newest run among the host's leases. */
function lastRuns(leases: readonly Lease[]): Map<string, { lease: Lease; run: ActionRun }> {
  const runs = new Map<string, { lease: Lease; run: ActionRun }>();
  const newest = [...leases].sort((a, b) => Date.parse(b.grantedAt) - Date.parse(a.grantedAt));

  for (const lease of newest) {
    for (const step of list(lease.operation?.steps)) {
      const name = list(lease.needs).find(n => n.id === step.needId)?.action?.name;
      if (step.action && name && !runs.has(name)) runs.set(name, { lease, run: step.action });
    }
  }
  return runs;
}

function ActionsPane({ host: h, onAction }: { host: HostDetail; onAction: (intent: ActionIntent) => void }) {
  const { login } = usePermissions();
  const actions = list(h.actions);
  if (actions.length === 0) return <Empty>The agent reports no host actions.</Empty>;

  const runnable = new Set(runnableActions(login, h).map(a => a.name));
  const runs = lastRuns(list(h.leases));
  const blocked = verbBlocked(h);

  return (
    <ul aria-label="Host actions" className="flex flex-col rounded-sm border border-border text-xs/5">
      {actions.map(a => (
        <ActionRow
          key={a.name}
          host={h.name}
          action={a}
          last={runs.get(a.name)}
          blocked={blocked || (runnable.has(a.name) ? '' : 'You may not start this action on this host')}
          intent={hostActionIntent(login, [h], a.name)}
          onAction={onAction}
        />
      ))}
    </ul>
  );
}

function ActionRow({
  host,
  action: a,
  last,
  blocked,
  intent,
  onAction,
}: {
  host: string;
  action: ActionInfo;
  last: { lease: Lease; run: ActionRun } | undefined;
  blocked: string;
  intent: ActionIntent;
  onAction: (intent: ActionIntent) => void;
}) {
  return (
    <li
      aria-label={`Action ${a.name}`}
      className="flex flex-col gap-1 border-b border-border px-2 py-1.5 last:border-b-0"
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono font-medium">{a.name}</span>
        <ImpactBadge impact={a.impact} />
        {a.due && (
          <Badge tone="warning" title={a.when ? `Its condition ${a.when} holds now` : undefined}>
            due
          </Badge>
        )}
        <span className="ml-auto">
          <IntentAction icon="action" label="Run…" blocked={blocked} intent={intent} onAction={onAction} />
        </span>
      </div>
      {a.description && <p className="text-muted">{a.description}</p>}
      {(a.feature || list(a.teams).length > 0) && (
        <p className="text-faint">
          allowed for {[a.feature, ...list(a.teams).map(t => `team ${t}`)].filter(Boolean).join(', ')}
        </p>
      )}
      {last ? (
        <div className="flex flex-col gap-1">
          <span className="flex flex-wrap items-center gap-1.5">
            <span className="text-muted">last run</span>
            <RelTime value={last.lease.grantedAt} />
            <RunExit run={last.run} />
          </span>
          <RunOutput run={last.run} label={`${a.name} on ${host}`} />
        </div>
      ) : (
        <span className="text-faint">no run in the recent leases</span>
      )}
    </li>
  );
}

// ------------------------------------------------------------------- history

function HistoryPane({ host: h }: { host: HostDetail }) {
  const leases = list(h.leases);
  const history = list(h.history);

  return (
    <>
      <Group title="Leases">
        {leases.length === 0 ? (
          <Empty>This host never held a lease.</Empty>
        ) : (
          <LeaseTable leases={leases} label={`Leases of ${h.name}`} showHost={false} closed />
        )}
      </Group>
      <Group title="Events">
        {history.length === 0 ? (
          <Empty>No history yet.</Empty>
        ) : (
          <AuditTable entries={history} label={`History of ${h.name}`} showTarget={false} />
        )}
      </Group>
    </>
  );
}
