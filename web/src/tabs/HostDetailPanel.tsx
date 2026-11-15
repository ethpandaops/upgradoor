import type { ReactNode } from 'react';

import { list, type CLHealth, type Container, type ELHealth, type HostDetail } from '../api/types';
import { useApp } from '../appContext';
import type { ActionIntent } from '../components/ActionDialog';
import { AuditTable } from '../components/AuditTable';
import { Digest, ShortId } from '../components/Digest';
import { LeaseTable } from '../components/LeaseTable';
import { LivenessSparkline } from '../components/LivenessSparkline';
import { RelTime } from '../components/RelTime';
import { Badge, Button, Dash, Empty, ErrorBanner, Loading, Section, Table, Td, Th } from '../components/primitives';
import { NeedLine } from '../components/status';
import { usePoll } from '../hooks/usePoll';
import { buildHoldRequest } from '../lib/actionRequests';
import { percentOf, shortRevision } from '../lib/format';
import { rollableContainers } from './hostRows';

/**
 * HostDetailPanel is the expanded row of the Hosts table: all containers,
 * open needs, pending packages, liveness, leases and history of one host.
 */
export function HostDetailPanel({ name, onAction }: { name: string; onAction: (intent: ActionIntent) => void }) {
  const { api } = useApp();
  const detail = usePoll(s => api.host(name, s), { key: `host/${name}` });

  if (detail.loading && !detail.data) return <Loading what={name} />;
  if (!detail.data) return <ErrorBanner error={detail.error} what={name} onRetry={detail.refresh} />;

  const h = detail.data;
  const containers = list(h.containers);
  const needs = list(h.needs);
  const liveness = list(h.liveness);
  const leases = list(h.leases);
  const history = list(h.history);
  const verbs = list(h.verbs);
  const held = h.holdWanted || h.hold.controller;

  return (
    <div className="flex flex-col gap-3">
      {detail.error !== undefined && <ErrorBanner error={detail.error} what={name} onRetry={detail.refresh} />}

      <div role="group" aria-label={`Actions for ${name}`} className="flex flex-wrap items-center gap-1.5">
        <span className="text-sm/6 font-semibold">{name}</span>
        <Button
          dense
          disabled={!verbs.includes('roll')}
          onClick={() => {
            onAction({ kind: 'roll', hosts: [name], all: false, containerChoices: rollableContainers([h]) });
          }}
        >
          Roll…
        </Button>
        <Button
          dense
          disabled={!verbs.includes('reboot')}
          onClick={() => {
            onAction({ kind: 'reboot', hosts: [name], all: false });
          }}
        >
          Reboot…
        </Button>
        <Button
          dense
          onClick={() => {
            const action = held ? 'unhold' : 'hold';
            onAction({
              kind: 'call',
              title: `${held ? 'Unhold' : 'Hold'} ${name}`,
              make: reason => ({ action, body: buildHoldRequest([name], reason) }),
            });
          }}
        >
          {held ? 'Unhold…' : 'Hold…'}
        </Button>
        <Button
          dense
          variant="danger"
          onClick={() => {
            onAction({
              kind: 'call',
              title: `Forget ${name}`,
              danger: true,
              make: reason => ({ action: 'forget', body: buildHoldRequest([name], reason) }),
            });
          }}
        >
          Forget…
        </Button>
      </div>

      <div className="grid grid-cols-1 gap-3 xl:grid-cols-3">
        <Section title="Identity">
          <Facts>
            <Fact label="roles">{list(h.roles).join(', ') || <Dash />}</Fact>
            <Fact label="validators">
              {h.validatorStart !== undefined && h.validatorEnd !== undefined ? (
                <span className="font-mono">
                  [{h.validatorStart}, {h.validatorEnd}) = {h.validators}
                </span>
              ) : (
                'none'
              )}
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
                <Badge tone="warning">controller hold{h.holdWanted && h.holdReason ? `: ${h.holdReason}` : ''}</Badge>
              )}{' '}
              {h.holdWanted !== h.hold.controller && (
                <span className="text-muted">
                  ({h.holdWanted ? `hold requested${h.holdReason ? `: ${h.holdReason}` : ''}` : 'release requested'},
                  waiting for the agent)
                </span>
              )}
            </Fact>
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
            <Fact label="per epoch">
              <LivenessSparkline rows={liveness} />
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
      <span className={el.peers === 0 ? 'text-danger' : undefined}>{el.peers} peers</span>, head{' '}
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
      <span className={cl.peers === 0 ? 'text-danger' : undefined}>{cl.peers} peers</span>, head slot{' '}
      <span className="font-mono">{cl.headSlot}</span>
    </span>
  );
}

function ContainerTable({ containers }: { containers: readonly Container[] }) {
  return (
    <Table label="Containers">
      <thead>
        <tr>
          <Th>Name</Th>
          <Th>Kind</Th>
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
              <Td>{c.kind ? c.kind.toUpperCase() : <Dash />}</Td>
              <Td>
                <span className="font-mono">{c.image}</span>
                {c.custom && c.configuredImage && (
                  <span className="text-muted">
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
                <RelTime value={c.startedAt} />
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
                      <RelTime value={c.registry.checkedAt} />
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
                {c.custom ? (
                  <Badge tone="warning" title="Image swapped by hand; never rolled">
                    custom
                  </Badge>
                ) : c.managed ? (
                  <Badge tone="accent">managed</Badge>
                ) : (
                  <span className="text-faint">unmanaged</span>
                )}
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
