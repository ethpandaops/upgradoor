import { useMemo } from 'react';

import {
  list,
  type Budget,
  type Lease,
  type Plan,
  type Policy,
  type QueueEntry,
  type Rollout,
  type RolloutHost,
  type Settings,
} from '../api/types';
import { useApp } from '../appContext';
import { useActions, type ActionIntent } from '../components/ActionDialog';
import { Digest, ShortId } from '../components/Digest';
import { GateIndicator } from '../components/GateIndicator';
import { LeaseTable } from '../components/LeaseTable';
import { RelTime } from '../components/RelTime';
import { Badge, Button, Dash, Empty, ErrorBanner, Loading, Section, Table, Td, Th, cx } from '../components/primitives';
import { HostStateBadge, RolloutStateBadge, stageLabel } from '../components/status';
import { usePoll } from '../hooks/usePoll';
import { buildRolloutHostsRequest, buildScopeRequest } from '../lib/actionRequests';
import { formatPercent, percentOf, shortRevision } from '../lib/format';

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

/** policyFor mirrors fleet.Settings.PolicyFor; manual rollouts have no policy. */
function policyFor(settings: Settings | undefined, rollout: Rollout): Policy | undefined {
  if (!settings) return undefined;
  switch (rollout.kind) {
    case 'image':
      return settings.policyImage;
    case 'packages':
      return settings.policyPackages;
    case 'reboot':
      return settings.policyReboot;
    case 'manual':
      return undefined;
  }
}

function scopeIntent(action: 'pause' | 'resume' | 'abort', title: string, rollout: string): ActionIntent {
  return {
    kind: 'call',
    title,
    danger: action === 'abort',
    make: reason => ({ action, body: buildScopeRequest(rollout, reason) }),
  };
}

function hostsIntent(action: 'skip' | 'retry', title: string, rollout: string, hosts: readonly string[]): ActionIntent {
  return {
    kind: 'call',
    title,
    make: reason => ({ action, body: buildRolloutHostsRequest(rollout, hosts, reason) }),
  };
}

/**
 * RolloutsTab shows the plan: the global pause, the budget, every open
 * rollout as a queue with the waiting reason per host, the active leases and
 * the recent history.
 */
export function RolloutsTab() {
  const { api } = useApp();

  const rollouts = usePoll(s => api.rollouts(50, s), { key: 'rollouts' });
  const plan = usePoll(s => api.plan(s), { key: 'plan' });
  const settings = usePoll(s => api.settings(s), { key: 'settings' });

  const actions = useActions(() => {
    rollouts.refresh();
    plan.refresh();
  });

  const queue = useMemo(() => new Map(list(plan.data?.queue).map(q => [q.host, q])), [plan.data]);
  const leases = useMemo(
    () => new Map([...list(rollouts.data?.leases), ...list(rollouts.data?.recentLeases)].map(l => [l.id, l])),
    [rollouts.data]
  );

  if ((rollouts.loading && !rollouts.data) || (plan.loading && !plan.data)) return <Loading what="rollouts" />;

  const open = list(rollouts.data?.open);
  const recent = list(rollouts.data?.recent);
  const openLeases = list(rollouts.data?.leases);
  const recentLeases = list(rollouts.data?.recentLeases);

  return (
    <div className="flex flex-col gap-3">
      {rollouts.error !== undefined && (
        <ErrorBanner error={rollouts.error} what="the rollouts" onRetry={rollouts.refresh} />
      )}
      {plan.error !== undefined && <ErrorBanner error={plan.error} what="the plan" onRetry={plan.refresh} />}
      {settings.error !== undefined && (
        <ErrorBanner error={settings.error} what="the policies" onRetry={settings.refresh} />
      )}
      {actions.notice}

      {plan.data && <PlanBar plan={plan.data} onAction={actions.request} />}
      {plan.data && <GateIndicator gate={plan.data.gate} />}

      <Section title={`Open rollouts (${open.length})`}>
        {open.length === 0 ? (
          <Empty>Nothing is rolling out. A rollout opens when a host reports a need.</Empty>
        ) : (
          <div className="flex flex-col gap-2">
            {open.map(r => (
              <RolloutCard
                key={r.id}
                rollout={r}
                policy={policyFor(settings.data?.settings, r)}
                queue={queue}
                leases={leases}
                onAction={actions.request}
              />
            ))}
          </div>
        )}
      </Section>

      <Section title={`Active leases (${openLeases.length})`}>
        {openLeases.length === 0 ? (
          <Empty>No host holds a lease.</Empty>
        ) : (
          <LeaseTable leases={openLeases} label="Active leases" />
        )}
      </Section>

      <Section title={`Queue (${list(plan.data?.queue).length})`}>
        {list(plan.data?.queue).length === 0 ? (
          <Empty>No host is waiting for a slot.</Empty>
        ) : (
          <QueueTable queue={list(plan.data?.queue)} rollouts={open} />
        )}
      </Section>

      <Section title="Recent rollouts">
        {recent.length === 0 ? (
          <Empty>No rollout has finished yet.</Empty>
        ) : (
          <RecentRollouts rollouts={recent} onAction={actions.request} />
        )}
      </Section>

      <Section title="Recent leases">
        {recentLeases.length === 0 ? (
          <Empty>No lease has ended yet.</Empty>
        ) : (
          <LeaseTable leases={recentLeases} label="Recent leases" closed />
        )}
      </Section>

      {actions.dialog}
    </div>
  );
}

function BudgetMeter({ budget }: { budget: Budget }) {
  const used = budget.totalWeight > 0 ? (budget.unavailableWeight / budget.totalWeight) * 100 : 0;
  const allowed = budget.totalWeight > 0 ? (budget.allowedWeight / budget.totalWeight) * 100 : 0;
  const over = budget.unavailableWeight > budget.allowedWeight;
  // The bar spans twice the allowance, so the limit sits in the middle.
  const scale = allowed > 0 ? Math.min(100, (used / (allowed * 2)) * 100) : used > 0 ? 100 : 0;

  return (
    <span className="inline-flex items-center gap-2">
      <span className="text-muted">unavailable</span>
      <span className={cx('font-mono font-medium', over ? 'text-danger' : 'text-foreground')}>
        {budget.unavailableWeight}
      </span>
      <span className="text-muted">of allowed</span>
      <span className="font-mono font-medium">{Math.round(budget.allowedWeight * 10) / 10}</span>
      <span className="text-muted">
        weight ({formatPercent(used)} of {formatPercent(allowed)}; total {budget.totalWeight})
      </span>
      <span
        role="meter"
        aria-label="Unavailable weight against the allowance"
        aria-valuemin={0}
        aria-valuemax={Math.round(budget.allowedWeight)}
        aria-valuenow={budget.unavailableWeight}
        className="relative h-2 w-24 overflow-hidden rounded-xs bg-surface-raised"
      >
        <span
          className={cx('absolute inset-y-0 left-0', over ? 'bg-danger' : 'bg-accent')}
          style={{ width: `${scale}%` }}
        />
        <span className="absolute inset-y-0 left-1/2 w-px bg-border-strong" />
      </span>
    </span>
  );
}

function PlanBar({ plan, onAction }: { plan: Plan; onAction: (intent: ActionIntent) => void }) {
  const b = plan.budget;

  return (
    <div
      aria-label="Scheduler"
      className={cx(
        'flex flex-wrap items-center gap-x-4 gap-y-1 rounded-sm border bg-surface px-2 py-1 text-xs/5',
        plan.paused ? 'border-warning' : 'border-border'
      )}
    >
      <span className="inline-flex items-center gap-1.5">
        <span className="font-semibold text-muted uppercase">Scheduler</span>
        {plan.paused ? <Badge tone="warning">paused</Badge> : <Badge tone="success">running</Badge>}
        {plan.paused ? (
          <Button
            dense
            variant="primary"
            onClick={() => {
              onAction(scopeIntent('resume', 'Resume everything', ''));
            }}
          >
            Resume all…
          </Button>
        ) : (
          <Button
            dense
            onClick={() => {
              onAction(scopeIntent('pause', 'Pause everything', ''));
            }}
          >
            Pause all…
          </Button>
        )}
      </span>

      <BudgetMeter budget={b} />

      <span className="inline-flex items-baseline gap-1">
        <span className="text-muted">leased hosts</span>
        <span className={cx('font-mono font-medium', b.leasedHosts >= b.maxLeasedHosts && 'text-warning')}>
          {b.leasedHosts}
        </span>
        <span className="text-muted">of limit</span>
        <span className="font-mono font-medium">{b.maxLeasedHosts}</span>
      </span>

      <span className="inline-flex items-baseline gap-1 text-muted">
        planned <RelTime value={plan.computedAt} />
      </span>
    </div>
  );
}

function hostProgress(h: RolloutHost, queue: ReadonlyMap<string, QueueEntry>, leases: ReadonlyMap<string, Lease>) {
  switch (h.state) {
    case 'pending': {
      const q = queue.get(h.host);
      if (!q) return <span className="text-muted">not in the queue</span>;
      return (
        <span>
          <span className="text-faint">#{q.position}</span> {q.reason}
        </span>
      );
    }
    case 'leased': {
      const lease = h.leaseId ? leases.get(h.leaseId) : undefined;
      if (!lease) return <span className="text-muted">lease open</span>;
      return (
        <span>
          lease {lease.state}
          {lease.operation && (
            <>
              , agent phase <span className="font-mono">{lease.operation.phase}</span>
            </>
          )}
          {lease.readiness && <span className="text-muted"> — {lease.readiness}</span>}
        </span>
      );
    }
    case 'failed':
      return <span className="text-danger">{h.error || 'failed'}</span>;
    case 'expected':
      return <span className="text-muted">has not reported the need yet</span>;
    case 'done':
    case 'skipped':
      return <Dash />;
  }
}

function RolloutCard({
  rollout: r,
  policy,
  queue,
  leases,
  onAction,
}: {
  rollout: Rollout;
  policy: Policy | undefined;
  queue: ReadonlyMap<string, QueueEntry>;
  leases: ReadonlyMap<string, Lease>;
  onAction: (intent: ActionIntent) => void;
}) {
  const hosts = useMemo(
    () => [...list(r.hosts)].sort((a, b) => a.stage - b.stage || collator.compare(a.host, b.host)),
    [r.hosts]
  );
  const settled = hosts.filter(h => h.state === 'done' || h.state === 'skipped').length;
  const failed = hosts.filter(h => h.state === 'failed');
  const waitsForApproval = policy === 'manual' && !r.approved;

  return (
    <article
      aria-label={r.title}
      className={cx(
        'flex flex-col gap-1.5 rounded-sm border bg-surface p-2',
        r.state === 'halted' ? 'border-danger' : 'border-border'
      )}
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs/5">
        <Badge>{r.kind}</Badge>
        <h3 className="text-sm/6 font-semibold">{r.title}</h3>
        <RolloutStateBadge state={r.state} />
        {r.paused && <Badge tone="warning">paused</Badge>}
        {waitsForApproval && <Badge tone="warning">waiting for approval</Badge>}
        {policy === 'paused' && <Badge tone="warning">policy paused</Badge>}
        {r.force && <Badge tone="warning">force</Badge>}
        {r.ignoreBudget && <Badge tone="danger">ignores budget</Badge>}
        <span className="text-muted">
          {settled}/{hosts.length} hosts settled
        </span>

        <span className="ml-auto inline-flex flex-wrap gap-1">
          {waitsForApproval && !r.paused && (
            <Button
              dense
              variant="primary"
              onClick={() => {
                onAction(scopeIntent('resume', `Approve rollout: ${r.title}`, r.id));
              }}
            >
              Approve…
            </Button>
          )}
          {r.paused ? (
            <Button
              dense
              variant="primary"
              onClick={() => {
                onAction(scopeIntent('resume', `Resume rollout: ${r.title}`, r.id));
              }}
            >
              Resume…
            </Button>
          ) : (
            <Button
              dense
              onClick={() => {
                onAction(scopeIntent('pause', `Pause rollout: ${r.title}`, r.id));
              }}
            >
              Pause…
            </Button>
          )}
          {(r.state === 'halted' || failed.length > 0) && (
            <Button
              dense
              variant="primary"
              onClick={() => {
                onAction(hostsIntent('retry', `Retry rollout: ${r.title}`, r.id, []));
              }}
            >
              Retry…
            </Button>
          )}
          <Button
            dense
            variant="danger"
            onClick={() => {
              onAction(scopeIntent('abort', `Abort rollout: ${r.title}`, r.id));
            }}
          >
            Abort…
          </Button>
        </span>
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-0.5 text-xs/5 text-muted">
        <span>
          id <ShortId value={r.id} label="rollout" />
        </span>
        {r.image && (
          <span>
            image <span className="font-mono text-foreground">{r.image}</span>
          </span>
        )}
        {r.digest && (
          <span>
            to <Digest value={r.digest} />
          </span>
        )}
        {r.revision && (
          <span>
            revision <span className="font-mono">{shortRevision(r.revision)}</span>
          </span>
        )}
        {list(r.packages).length > 0 && (
          <span className="font-mono whitespace-normal">
            {list(r.packages)
              .map(p => `${p.name} ${p.from} → ${p.to}`)
              .join(', ')}
          </span>
        )}
        {r.canary && (
          <span>
            canary <span className="font-medium text-foreground">{r.canary}</span>
          </span>
        )}
        <span>
          opened by {r.createdBy} <RelTime value={r.createdAt} />
        </span>
      </div>

      {r.reason && (
        <p
          className={cx(
            'text-xs/5 font-medium whitespace-normal',
            r.state === 'halted' ? 'text-danger' : 'text-warning'
          )}
        >
          {r.state}: {r.reason}
        </p>
      )}

      <Table label={`Hosts of ${r.title}`}>
        <thead>
          <tr>
            <Th>Host</Th>
            <Th>Stage</Th>
            <Th>State</Th>
            <Th className="w-full">Waiting for / progress</Th>
            <Th>Updated</Th>
            <Th>Actions</Th>
          </tr>
        </thead>
        <tbody>
          {hosts.map(h => (
            <tr key={h.host} className="hover:bg-surface-hover">
              <Td className="font-medium">
                {h.host}
                {r.canary === h.host && (
                  <>
                    {' '}
                    <Badge tone="info">canary</Badge>
                  </>
                )}
              </Td>
              <Td>{stageLabel(h.stage)}</Td>
              <Td>
                <HostStateBadge state={h.state} />
              </Td>
              <Td className="whitespace-normal">{hostProgress(h, queue, leases)}</Td>
              <Td>
                <RelTime value={h.updatedAt} />
              </Td>
              <Td>
                <span className="inline-flex gap-1">
                  {(h.state === 'pending' || h.state === 'expected' || h.state === 'failed') && (
                    <Button
                      dense
                      aria-label={`Skip ${h.host} in ${r.title}`}
                      onClick={() => {
                        onAction(hostsIntent('skip', `Skip ${h.host}`, r.id, [h.host]));
                      }}
                    >
                      Skip
                    </Button>
                  )}
                  {h.state === 'failed' && (
                    <Button
                      dense
                      aria-label={`Retry ${h.host} in ${r.title}`}
                      onClick={() => {
                        onAction(hostsIntent('retry', `Retry ${h.host}`, r.id, [h.host]));
                      }}
                    >
                      Retry
                    </Button>
                  )}
                </span>
              </Td>
            </tr>
          ))}
        </tbody>
      </Table>
    </article>
  );
}

function QueueTable({ queue, rollouts }: { queue: readonly QueueEntry[]; rollouts: readonly Rollout[] }) {
  const titles = new Map(rollouts.map(r => [r.id, r.title]));

  return (
    <Table label="Queue">
      <thead>
        <tr>
          <Th>#</Th>
          <Th>Host</Th>
          <Th>Waiting for</Th>
          <Th className="w-full">Rollouts</Th>
        </tr>
      </thead>
      <tbody>
        {[...queue]
          .sort((a, b) => a.position - b.position)
          .map(q => (
            <tr key={q.host} className="hover:bg-surface-hover">
              <Td className="text-faint">{q.position}</Td>
              <Td className="font-medium">{q.host}</Td>
              <Td>{q.reason}</Td>
              <Td className="whitespace-normal text-muted">
                {list(q.rollouts)
                  .map(id => titles.get(id) ?? id)
                  .join(' · ')}
              </Td>
            </tr>
          ))}
      </tbody>
    </Table>
  );
}

function RecentRollouts({
  rollouts,
  onAction,
}: {
  rollouts: readonly Rollout[];
  onAction: (intent: ActionIntent) => void;
}) {
  return (
    <Table label="Recent rollouts">
      <thead>
        <tr>
          <Th>Rollout</Th>
          <Th>Kind</Th>
          <Th>State</Th>
          <Th>Hosts done</Th>
          <Th className="w-full">Reason</Th>
          <Th>Opened by</Th>
          <Th>Opened</Th>
          <Th>Ended</Th>
          <Th>Actions</Th>
        </tr>
      </thead>
      <tbody>
        {rollouts.map(r => {
          const hosts = list(r.hosts);
          const done = hosts.filter(h => h.state === 'done').length;
          const failed = hosts.filter(h => h.state === 'failed').length;
          return (
            <tr key={r.id} className="hover:bg-surface-hover">
              <Td className="font-medium">{r.title}</Td>
              <Td>{r.kind}</Td>
              <Td>
                <RolloutStateBadge state={r.state} />
              </Td>
              <Td title={hosts.map(h => `${h.host}: ${h.state}`).join('\n')}>
                <span className="font-mono">
                  {done}/{hosts.length}
                </span>{' '}
                <span className="text-muted">({percentOf(done, hosts.length)})</span>
                {failed > 0 && <span className="text-danger"> {failed} failed</span>}
              </Td>
              <Td className="whitespace-normal text-muted">{r.reason || <Dash />}</Td>
              <Td>{r.createdBy}</Td>
              <Td>
                <RelTime value={r.createdAt} />
              </Td>
              <Td>
                <RelTime value={r.endedAt} />
              </Td>
              <Td>
                {r.state === 'aborted' && (
                  <Button
                    dense
                    aria-label={`Retry ${r.title}`}
                    onClick={() => {
                      onAction(hostsIntent('retry', `Retry rollout: ${r.title}`, r.id, []));
                    }}
                  >
                    Retry
                  </Button>
                )}
              </Td>
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}
