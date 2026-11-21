import type { ReactNode } from 'react';

import { list, type CLHealth, type Container, type ELHealth, type HostDetail } from '../api/types';
import type { ActionIntent } from '../components/ActionDialog';
import { AuditTable } from '../components/AuditTable';
import { Digest, ShortId } from '../components/Digest';
import { LeaseTable } from '../components/LeaseTable';
import { useLiveHost } from '../components/Live';
import { OverrideButtons, OverrideSummary } from '../components/Overrides';
import { IntentAction } from '../components/SelectionBar';
import { RelTime } from '../components/RelTime';
import { Badge, Dash, Empty, ErrorBanner, Loading, Section, Table, Td, Th, cx } from '../components/primitives';
import { ImpactBadge, NeedLine, PullBadge } from '../components/status';
import { percentOf, shortRevision } from '../lib/format';
import { holdIntent, offlineIntent } from '../lib/intents';
import { hostOwners, rollableContainers } from './hostRows';

/**
 * HostDetailPanel is the expanded row of the Hosts table: all containers,
 * open needs, pending packages, liveness, leases and history of one host.
 */
export function HostDetailPanel({ name, onAction }: { name: string; onAction: (intent: ActionIntent) => void }) {
  const detail = useLiveHost(name);

  if (detail.loading) return <Loading what={name} />;
  if (detail.gone) return <Empty>The controller does not know {name} any more.</Empty>;
  if (detail.crowded) {
    return <Empty>Too many host details are open at once. Close another host to load {name}.</Empty>;
  }
  if (!detail.data) return <ErrorBanner error={detail.error} what={name} />;

  const h = detail.data;
  const containers = list(h.containers);
  const needs = list(h.needs);
  const leases = list(h.leases);
  const history = list(h.history);
  const verbs = list(h.verbs);
  const held = h.holdWanted || h.hold.controller;
  const owners = hostOwners(h);

  return (
    <div className="flex flex-col gap-3">
      <div role="group" aria-label={`Actions for ${name}`} className="flex flex-wrap items-center gap-1.5">
        <span className="text-sm/6 font-semibold">{name}</span>
        <IntentAction
          icon="roll"
          label="Roll…"
          blocked={verbs.includes('roll') ? '' : 'The agent does not allow the verb roll'}
          onAction={onAction}
          intent={{ kind: 'roll', hosts: [name], all: false, containerChoices: rollableContainers([h]) }}
        />
        <IntentAction
          icon="reboot"
          label="Reboot…"
          blocked={verbs.includes('reboot') ? '' : 'The agent does not allow the verb reboot'}
          onAction={onAction}
          intent={{ kind: 'reboot', hosts: [name], all: false }}
        />
        <IntentAction
          icon={held ? 'unhold' : 'hold'}
          label={held ? 'Unhold…' : 'Hold…'}
          onAction={onAction}
          intent={holdIntent(held ? 'unhold' : 'hold', [name])}
        />
        <IntentAction
          icon="offline"
          label={h.offline ? 'Lift offline mark…' : 'Mark offline…'}
          onAction={onAction}
          intent={offlineIntent(!h.offline, [name])}
        />
        <IntentAction
          icon="forget"
          label="Forget…"
          tone="danger"
          onAction={onAction}
          intent={holdIntent('forget', [name])}
        />
        {owners.length > 0 && (
          <span className="text-xs/5 text-muted" title="The teams that own a container of this host">
            owners: {owners.join(', ')}
          </span>
        )}
      </div>

      <div className="grid grid-cols-1 gap-3 xl:grid-cols-3">
        <Section title="Identity">
          <Facts>
            <Fact label="roles">{list(h.roles).join(', ') || <Dash />}</Fact>
            <Fact label="validators">
              <ValidatorRange host={h} />
            </Fact>
            <Fact label="certificate">
              <ShortId value={h.certHash} label="certificate hash" />{' '}
              <span className="text-muted">generation {h.generation}</span>
            </Fact>
            <Fact label="first seen">
              <RelTime value={h.firstSeen} />
            </Fact>
            <Fact label="last sync">
              <RelTime value={h.lastSeen} /> <span className="text-muted">from</span>{' '}
              <span className="font-mono">{h.remoteAddr || '?'}</span>
            </Fact>
            <Fact label="agent">
              <span className="font-mono">{h.agentVersion || '?'}</span>{' '}
              <span className="text-muted">allows {verbs.join(', ') || 'nothing'}</span>
            </Fact>
            <Fact label="hold">
              {!h.hold.local && !h.hold.controller && !h.holdWanted && 'none'}
              {h.hold.local && (
                <Badge tone="warning">local hold file{h.hold.localReason ? `: ${h.hold.localReason}` : ''}</Badge>
              )}{' '}
              {h.hold.controller && (
                <Badge
                  tone="warning"
                  title={
                    h.holdBy === 'team'
                      ? 'Set through a team feature: a member of a team that owns a container of this host can lift it'
                      : h.holdWanted
                        ? 'Set by staff: a team feature does not lift it'
                        : undefined
                  }
                >
                  controller hold{h.holdWanted && h.holdBy ? ` (${h.holdBy})` : ''}
                  {h.holdWanted && h.holdReason ? `: ${h.holdReason}` : ''}
                </Badge>
              )}{' '}
              {h.holdWanted !== h.hold.controller && (
                <span className="text-muted">
                  ({h.holdWanted ? `hold requested${h.holdReason ? `: ${h.holdReason}` : ''}` : 'release requested'},
                  waiting for the agent)
                </span>
              )}
            </Fact>
            {h.plan.pull && (
              <Fact label="images">
                <PullBadge pull={h.plan.pull} />
              </Fact>
            )}
            {h.broken && Object.keys(h.broken).length > 0 && (
              <Fact label="broken">
                <span className="flex flex-wrap gap-1">
                  {Object.entries(h.broken).map(([group, lease]) => (
                    <Badge
                      key={group}
                      tone="danger"
                      title={`Lease ${lease} failed on it: the host goes first in the next rollout of it`}
                    >
                      {group.replace(/^group:/, '')}
                    </Badge>
                  ))}
                </span>
              </Fact>
            )}
          </Facts>
        </Section>

        <Section title="System">
          <Facts>
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
                <span className="font-mono whitespace-normal">{list(h.inventory.kernels.installed).join(', ')}</span>
              </Fact>
            )}
            <Fact label="reboot">
              {h.os.rebootRequired ? (
                <span className="whitespace-normal text-warning">
                  required by {list(h.os.rebootPackages).join(', ') || 'unknown packages'}
                </span>
              ) : (
                'not required'
              )}
            </Fact>
            <Fact label="updates">
              {h.pending.upgradable} upgradable,{' '}
              <span className={h.pending.security > 0 ? 'text-danger' : undefined}>{h.pending.security} security</span>,{' '}
              {h.pending.blocked} blocked
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
        </Section>

        <Section title="Node and liveness">
          <Facts>
            <Fact label="EL">
              {h.node.el ? <ELLine el={h.node.el} /> : <span className="text-muted">no probe</span>}
            </Fact>
            <Fact label="CL">
              {h.node.cl ? <CLLine cl={h.node.cl} /> : <span className="text-muted">no probe</span>}
            </Fact>
            <Fact label="attesting">
              {h.plan.weight === 0
                ? 'no validators'
                : h.plan.liveKnown
                  ? `${percentOf(h.plan.live, h.plan.weight)} (${h.plan.live}/${h.plan.weight})`
                  : 'unknown'}
            </Fact>
          </Facts>
        </Section>
      </div>

      <Section title={`Containers (${containers.length})`}>
        {containers.length === 0 ? (
          <Empty>The agent reports no containers.</Empty>
        ) : (
          <ContainerTable containers={containers} />
        )}
      </Section>

      {containers.some(c => c.managed && c.client) && (
        <Section title="Clients, overrides and command lines">
          <ul className="flex flex-col rounded-sm border border-border bg-surface text-xs/5">
            {containers
              .filter(c => c.managed && c.client)
              .map(c => (
                <ClientContainer key={c.name} host={name} container={c} stale={h.stale} onAction={onAction} />
              ))}
          </ul>
        </Section>
      )}

      <div className="grid grid-cols-1 gap-3 xl:grid-cols-2">
        <Section title={`Open needs (${needs.length})`}>
          {needs.length === 0 ? (
            <Empty>No open need.</Empty>
          ) : (
            <ul className="flex flex-col gap-1 rounded-sm border border-border bg-surface px-2 py-1 text-xs/5">
              {needs.map(n => (
                <li key={n.id}>
                  <NeedLine need={n} />
                </li>
              ))}
            </ul>
          )}
        </Section>

        <Section title={`Upgradable packages (${list(h.inventory?.upgradable).length})`}>
          <UpgradableTable detail={h} />
        </Section>
      </div>

      <Section title="Leases">
        {leases.length === 0 ? (
          <Empty>This host never held a lease.</Empty>
        ) : (
          <LeaseTable leases={leases} label={`Leases of ${name}`} showHost={false} closed />
        )}
      </Section>

      <Section title="History">
        {history.length === 0 ? (
          <Empty>No history yet.</Empty>
        ) : (
          <AuditTable entries={history} label={`History of ${name}`} showTarget={false} />
        )}
      </Section>
    </div>
  );
}

/**
 * ValidatorRange shows the range the controller counts for the host and
 * where the host's agent read it. A host that does not know its range
 * (missing, invalid) and a range that another host reports too are warnings:
 * the budget is then computed from a range that may be wrong.
 */
function ValidatorRange({ host: h }: { host: HostDetail }) {
  const report = h.validatorReport;
  const overlaps = list(h.validatorOverlaps);
  const counted = h.validatorStart !== undefined && h.validatorEnd !== undefined;

  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
      {counted ? (
        <span className="font-mono" title="The range the controller counts for this host">
          [{h.validatorStart}, {h.validatorEnd}) = {h.validators}
        </span>
      ) : (
        'none'
      )}
      {report && report.state === 'known' && (
        <span className="text-muted" title="Where the agent read the range it reports">
          from the{' '}
          {report.source === 'marker'
            ? 'marker file'
            : report.source === 'config'
              ? 'agent configuration'
              : report.source}
        </span>
      )}
      {report && report.state !== 'known' && (
        <Badge
          tone="warning"
          title={
            report.state === 'missing'
              ? 'The marker file of the validator keys is not on the host. The controller keeps counting the last range it knew.'
              : `The host reports a range that cannot be used${report.error ? `: ${report.error}` : ''}. The controller keeps counting the last range it knew.`
          }
        >
          range {report.state}
          {report.error ? `: ${report.error}` : ''}
        </Badge>
      )}
      {overlaps.length > 0 && (
        <Badge
          tone="danger"
          title="These hosts report validators this host reports too: keys are being moved, or one of them is wrong"
        >
          overlaps with {overlaps.join(', ')}
        </Badge>
      )}
    </span>
  );
}

function Facts({ children }: { children: ReactNode }) {
  return (
    <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 rounded-sm border border-border bg-surface px-2 py-1 text-xs/5">
      {children}
    </dl>
  );
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
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

/**
 * ClientContainer is one managed container of a client: the client, the
 * override the host reports for it, the two override actions and, on demand,
 * its command line.
 */
function ClientContainer({
  host,
  container: c,
  stale,
  onAction,
}: {
  host: string;
  container: Container;
  stale: boolean;
  onAction: (intent: ActionIntent) => void;
}) {
  const command = list(c.command);

  return (
    <li className="flex flex-col gap-1 border-b border-border px-2 py-1.5 last:border-b-0">
      {/* The actions stand at the left edge: the panel can be wider than the window. */}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <span className="font-medium">{c.name}</span>
        <span className="text-muted" title="The client software the container runs">
          {c.client}
          {c.kind ? ` · ${c.kind.toUpperCase()}` : ''}
        </span>
        {c.group && (
          <span className="text-muted" title="The roll group: its containers are rolled together, by one lease">
            group <span className="font-medium text-foreground">{c.group}</span>
          </span>
        )}
        {c.impact && <ImpactBadge impact={c.impact} />}
        <span role="group" aria-label={`Overrides of ${c.name}`} className="flex shrink-0 items-center gap-1.5">
          <OverrideButtons host={host} containers={[c]} stale={stale} onAction={onAction} />
        </span>
      </div>
      {c.override ? <OverrideSummary override={c.override} /> : <span className="text-faint">no override</span>}
      <details>
        <summary className="cursor-pointer text-muted hover:text-foreground">
          command line ({command.length} {command.length === 1 ? 'argument' : 'arguments'})
        </summary>
        {command.length === 0 ? (
          <span className="text-muted">The agent reports no arguments.</span>
        ) : (
          <ul aria-label={`Command line of ${c.name}`} className="pt-1 pl-4 font-mono">
            {command.map((arg, i) => (
              <li key={`${i} ${arg}`} className="break-all whitespace-normal">
                {arg}
              </li>
            ))}
          </ul>
        )}
      </details>
    </li>
  );
}

function ContainerTable({ containers }: { containers: readonly Container[] }) {
  return (
    <Table label="Containers">
      <thead>
        <tr>
          <Th>Name</Th>
          <Th>Kind</Th>
          <Th title="The roll group of a managed container: its containers are rolled together, by one lease">Group</Th>
          <Th title="The teams that own the container: their members may roll it">Owners</Th>
          <Th>Image</Th>
          <Th>State</Th>
          <Th>Health</Th>
          <Th>Restarts</Th>
          <Th>Started</Th>
          <Th>Digest</Th>
          <Th title="The digest the tag points at in the registry, as last checked by the agent">Registry digest</Th>
          <Th>Revision</Th>
          <Th>Managed</Th>
        </tr>
      </thead>
      <tbody>
        {containers.map(c => {
          const behind = c.registry?.digest !== undefined && c.registry.digest !== '' && c.registry.digest !== c.digest;
          return (
            <tr key={c.id || c.name} className="hover:bg-surface-hover">
              <Td className="font-medium">{c.name}</Td>
              <Td>
                {c.kind ? c.kind.toUpperCase() : <Dash />}
                {c.impact && (
                  <span className="ml-1.5">
                    <ImpactBadge impact={c.impact} />
                  </span>
                )}
              </Td>
              <Td>
                <span className="inline-flex items-center gap-1.5">
                  {c.group || <Dash />}
                  {c.notRolled && (
                    <Badge title={`The agent rolls this container to nothing: ${c.notRolled}`}>not rolled</Badge>
                  )}
                </span>
              </Td>
              <Td>{list(c.owners).length > 0 ? list(c.owners).join(', ') : <Dash />}</Td>
              <Td>
                <span className="font-mono">{c.image}</span>
                {c.managed && c.configuredImage && c.configuredImage !== c.image && !c.override && (
                  <span
                    className="text-muted"
                    title="the deployed block of overrides.yaml names another image: the container is rolled onto it"
                  >
                    {' '}
                    (configured: <span className="font-mono">{c.configuredImage}</span>)
                  </span>
                )}
              </Td>
              <Td>
                <Badge tone={c.state === 'running' ? 'success' : c.state === 'restarting' ? 'warning' : 'danger'}>
                  {c.state}
                </Badge>
              </Td>
              <Td>
                {c.health ? (
                  <Badge tone={c.health === 'healthy' ? 'success' : c.health === 'starting' ? 'info' : 'danger'}>
                    {c.health}
                  </Badge>
                ) : (
                  <Dash />
                )}
              </Td>
              <Td className={c.restartCount > 0 ? 'font-medium text-warning' : 'text-faint'}>{c.restartCount}</Td>
              <Td>
                <RelTime fixed value={c.startedAt} />
              </Td>
              <Td>
                <Digest value={c.digest} />
              </Td>
              <Td>
                {c.registry ? (
                  <span className="inline-flex items-center gap-1.5">
                    <Digest value={c.registry.digest} />
                    {behind && <Badge tone="warning">{c.registry.pulled ? 'newer, pulled' : 'newer'}</Badge>}
                    {c.registry.error && (
                      <Badge tone="danger" title={c.registry.error}>
                        check failed
                      </Badge>
                    )}
                    <span className="text-faint">
                      <RelTime fixed value={c.registry.checkedAt} />
                    </span>
                  </span>
                ) : (
                  <Dash />
                )}
              </Td>
              <Td className="font-mono" title={[c.revision, c.version].filter(Boolean).join(' / ')}>
                {shortRevision(c.revision) || c.version || <Dash />}
              </Td>
              <Td>
                {c.managed ? <Badge tone="accent">managed</Badge> : <span className="text-faint">unmanaged</span>}
              </Td>
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}

function UpgradableTable({ detail }: { detail: HostDetail }) {
  if (!detail.inventory) return <Empty>The agent has not uploaded its inventory yet.</Empty>;

  const upgradable = list(detail.inventory.upgradable);
  if (upgradable.length === 0) return <Empty>Every package is up to date.</Empty>;

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
        {upgradable.map(u => (
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
