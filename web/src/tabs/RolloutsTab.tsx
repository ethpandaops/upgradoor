import { useMemo, useState } from 'react';

import {
  list,
  type Lease,
  type Plan,
  type Policy,
  type QueueEntry,
  type Rollout,
  type RolloutHost,
  type RolloutTarget,
  type Settings,
} from '../api/types';
import { useActions, type ActionIntent } from '../components/ActionDialog';
import { Digest, ShortId } from '../components/Digest';
import { LeaseTable, PeerChecks } from '../components/LeaseTable';
import { useLive } from '../components/Live';
import { ActionIcon } from '../components/icons';
import { IntentAction } from '../components/SelectionBar';
import { RelTime } from '../components/RelTime';
import {
  Badge,
  Dash,
  Empty,
  ErrorBanner,
  Loading,
  Section,
  Segmented,
  Table,
  Td,
  Th,
  cx,
} from '../components/primitives';
import { HostStateBadge, RolloutKindBadge, RolloutStateBadge, stageLabel } from '../components/status';
import { pauseLabel, percentOf, plural, shortRevision } from '../lib/format';
import { endLeaseIntent, hostsIntent, scopeIntent } from '../lib/intents';

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

/**
 * policyFor mirrors the scheduler's rolloutPolicy: the policy of the kind,
 * and for a group rollout the image policy of its client where one is set,
 * or the default group's policy. Manual rollouts have no policy.
 */
export function policyFor(settings: Settings | undefined, rollout: Rollout): Policy | undefined {
  if (!settings) return undefined;
  switch (rollout.kind) {
    case 'group':
      if (rollout.group === 'default') return settings.policyDefaultGroup;
      return (rollout.client ? settings.policyClients[rollout.client] : undefined) ?? settings.policyImage;
    case 'packages':
      return settings.policyPackages;
    case 'reboot':
      return settings.policyReboot;
    case 'manual':
    case 'local':
      // Asked for by a person, or on the host itself: no policy, no approval.
      return undefined;
  }
}

/** waitsForApproval says whether a rollout waits for a person to approve it: its policy is manual. */
export function waitsForApproval(rollout: Rollout, policy: Policy | undefined): boolean {
  return policy === 'manual' && !rollout.approved;
}

/**
 * useRolloutData is what a rollout card needs from the stream: the rollouts,
 * the plan's queue by host, every lease the page knows by id, and the settings
 * that say the policy of a rollout.
 */
export function useRolloutData() {
  const rollouts = useLive('rollouts');
  const plan = useLive('plan');
  const settings = useLive('settings');

  const queue = useMemo(() => new Map(list(plan.data?.queue).map(q => [q.host, q])), [plan.data]);
  const leases = useMemo(
    () => new Map([...list(rollouts.data?.leases), ...list(rollouts.data?.recentLeases)].map(l => [l.id, l])),
    [rollouts.data]
  );

  return { rollouts, plan, settings: settings.data?.settings, queue, leases };
}

/**
 * RolloutsTab shows the work in detail: every open rollout with the state and
 * waiting reason of each of its hosts, the leases, and what has finished.
 * The Overview tab has the summary: timeline, in progress, up next.
 */
export function RolloutsTab() {
  const [leaseView, setLeaseView] = useState<'active' | 'recent'>('active');

  const { rollouts, plan, settings, queue, leases } = useRolloutData();
  const actions = useActions();

  if (rollouts.loading || plan.loading) return <Loading what="rollouts" />;

  const open = list(rollouts.data?.open);
  const recent = list(rollouts.data?.recent);
  const openLeases = list(rollouts.data?.leases);
  const recentLeases = list(rollouts.data?.recentLeases);

  return (
    <div className="mx-auto flex w-full max-w-screen-2xl flex-col gap-6">
      {rollouts.error !== undefined && <ErrorBanner error={rollouts.error} what="the rollouts" />}
      {actions.notice}

      {plan.data && <PlanBar plan={plan.data} onAction={actions.request} />}

      <Section title={`Open rollouts (${open.length})`}>
        {open.length === 0 ? (
          <Empty>Nothing is rolling out. A rollout opens when a host reports a need.</Empty>
        ) : (
          <div className="flex flex-col gap-3">
            {open.map(r => (
              <RolloutCard
                key={r.id}
                rollout={r}
                policy={policyFor(settings, r)}
                queue={queue}
                leases={leases}
                onAction={actions.request}
              />
            ))}
          </div>
        )}
      </Section>

      <Section
        title="Leases"
        aside={
          <Segmented
            label="Leases shown"
            value={leaseView}
            onChange={setLeaseView}
            options={[
              { value: 'active', label: `Active (${openLeases.length})` },
              { value: 'recent', label: `Recent (${recentLeases.length})` },
            ]}
          />
        }
      >
        {leaseView === 'active' ? (
          openLeases.length === 0 ? (
            <Empty>No host holds a lease.</Empty>
          ) : (
            <LeaseTable
              leases={openLeases}
              label="Active leases"
              onEnd={l => {
                actions.request(endLeaseIntent(l.id, l.host));
              }}
            />
          )
        ) : recentLeases.length === 0 ? (
          <Empty>No lease has ended yet.</Empty>
        ) : (
          <LeaseTable leases={recentLeases} label="Recent leases" closed />
        )}
      </Section>

      <Section title="Finished rollouts">
        {recent.length === 0 ? (
          <Empty>No rollout has finished yet.</Empty>
        ) : (
          <RecentRollouts rollouts={recent} leases={leases} />
        )}
      </Section>

      {actions.dialog}
    </div>
  );
}

/**
 * PlanBar is the scheduler's state and its one control; the Overview has the
 * gate, the budget and the queue in full.
 */
function PlanBar({ plan, onAction }: { plan: Plan; onAction: (intent: ActionIntent) => void }) {
  return (
    <div role="group" aria-label="Scheduler" className="flex flex-wrap items-center gap-x-6 gap-y-2 text-xs/5">
      <span className="inline-flex items-center gap-2">
        <span className="text-sm/6 font-semibold">Scheduler</span>
        {plan.paused ? <Badge tone="warning">paused</Badge> : <Badge tone="success">running</Badge>}
      </span>
      <span className="inline-flex items-center gap-2" title={list(plan.gate.reasons).join(' · ') || undefined}>
        <span className="text-muted">Network gate</span>
        {plan.gate.ok ? <Badge tone="success">open</Badge> : <Badge tone="danger">closed</Badge>}
      </span>
      <span className="ml-auto">
        {plan.paused ? (
          <IntentAction
            icon="resume"
            label="Resume all…"
            onAction={onAction}
            intent={scopeIntent('resume', 'Resume everything', '')}
          />
        ) : (
          <IntentAction
            icon="pause"
            label="Pause all…"
            onAction={onAction}
            intent={scopeIntent('pause', 'Pause everything', '')}
          />
        )}
      </span>
    </div>
  );
}

const PIP: Record<RolloutHost['state'], string> = {
  done: 'bg-success',
  leased: 'animate-pulse bg-accent',
  failed: 'bg-danger-solid',
  pending: 'bg-border-strong',
  expected: 'bg-border',
  skipped: 'bg-border',
};

/** HostPips draws one mark per host of a rollout, in serving order, coloured by its state. */
function HostPips({ hosts }: { hosts: readonly RolloutHost[] }) {
  const count = (state: RolloutHost['state']) => hosts.filter(h => h.state === state).length;
  const summary = [
    `${count('done')} done`,
    count('leased') > 0 && `${count('leased')} in progress`,
    count('pending') + count('expected') > 0 && `${count('pending') + count('expected')} waiting`,
    count('failed') > 0 && `${count('failed')} failed`,
    count('skipped') > 0 && `${count('skipped')} skipped`,
  ]
    .filter(Boolean)
    .join(', ');

  return (
    <span className="inline-flex items-center gap-2 text-xs/5 text-muted">
      <span role="img" aria-label={`${plural(hosts.length, 'host')}: ${summary}`} className="inline-flex gap-0.5">
        {hosts.map(h => (
          <span key={h.host} title={`${h.host}: ${h.state}`} className={cx('h-3 w-1.5 rounded-xs', PIP[h.state])} />
        ))}
      </span>
      {summary}
    </span>
  );
}

const LEASE_ENDED: ReadonlySet<Lease['state']> = new Set(['released', 'failed', 'expired']);

/**
 * memberState names the state of a member of a closed rollout. A member that
 * held a lease when its rollout closed stays leased: it is named by how that
 * lease ended, when the page knows the lease.
 */
function memberState(h: RolloutHost, leases: ReadonlyMap<string, Lease>): string {
  if (h.state !== 'leased') return h.state;

  const lease = h.leaseId ? leases.get(h.leaseId) : undefined;
  if (!lease) return 'leased when the rollout closed';
  return LEASE_ENDED.has(lease.state) ? `lease ${lease.state}` : 'leased';
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
          <PeerChecks checks={lease.peerChecks} />
        </span>
      );
    }
    case 'failed':
      return <span className="text-danger">{h.error || 'failed'}</span>;
    case 'expected':
      return <span className="text-muted">has not reported the need yet</span>;
    case 'skipped':
      // A person's skip names the person and the reason given; the scheduler's only its reason.
      if (!h.skippedBy && !h.error) return <Dash />;
      return (
        <span className="text-muted">{[h.skippedBy && `by ${h.skippedBy}`, h.error].filter(Boolean).join(': ')}</span>
      );
    case 'done':
      return <Dash />;
  }
}

export function RolloutCard({
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
  const failed = hosts.filter(h => h.state === 'failed');
  const waiting = waitsForApproval(r, policy);

  return (
    <article
      aria-label={r.title}
      className={cx(
        'flex flex-col gap-2 rounded-lg border bg-surface p-4',
        r.state === 'halted' ? 'border-danger' : 'border-border'
      )}
    >
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs/5">
        <RolloutKindBadge kind={r.kind} />
        <h3 className="text-sm/6 font-semibold">{r.title}</h3>
        <RolloutStateBadge state={r.state} />
        {r.paused && <Badge tone="warning">{pauseLabel(r.pauseReason)}</Badge>}
        {waiting && <Badge tone="warning">waiting for approval</Badge>}
        {r.force && <Badge tone="warning">force</Badge>}
        {r.ignoreBudget && <Badge tone="danger">ignores budget</Badge>}
        <HostPips hosts={hosts} />

        <span className="ml-auto inline-flex flex-wrap gap-1">
          {waiting && !r.paused && (
            <IntentAction
              icon="check"
              label="Approve…"
              onAction={onAction}
              intent={scopeIntent('resume', `Approve rollout: ${r.title}`, r.id)}
            />
          )}
          {r.paused ? (
            <IntentAction
              icon="resume"
              label="Resume…"
              onAction={onAction}
              intent={scopeIntent('resume', `Resume rollout: ${r.title}`, r.id)}
            />
          ) : (
            <IntentAction
              icon="pause"
              label="Pause…"
              onAction={onAction}
              intent={scopeIntent('pause', `Pause rollout: ${r.title}`, r.id)}
            />
          )}
          {(r.state === 'halted' || failed.length > 0) && (
            <IntentAction
              icon="retry"
              label="Retry…"
              onAction={onAction}
              intent={hostsIntent('retry', `Retry rollout: ${r.title}`, r.id, [])}
            />
          )}
          <IntentAction
            icon="abort"
            label="Abort…"
            tone="danger"
            onAction={onAction}
            intent={scopeIntent('abort', `Abort rollout: ${r.title}`, r.id)}
          />
        </span>
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-0.5 text-xs/5 text-muted">
        <span>
          id <ShortId value={r.id} label="rollout" />
        </span>
        {r.group && (
          <span>
            group <span className="font-medium text-foreground">{r.group}</span>
          </span>
        )}
        {r.client && (
          <span>
            client <span className="font-medium text-foreground">{r.client}</span>
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
          opened by {r.createdBy} <RelTime fixed value={r.createdAt} />
        </span>
      </div>

      {list(r.targets).length > 0 && <RolloutTargets targets={list(r.targets)} label={`Targets of ${r.title}`} />}

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
            <Th className="w-full">Waiting for / doing</Th>
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
              {/* A host's own request is one host: there is no order to name. */}
              <Td>{r.kind === 'local' ? <Dash /> : stageLabel(h.stage)}</Td>
              {/* State and actions keep their width from one state to the next. */}
              <Td className="min-w-24">
                <HostStateBadge state={h.state} />
              </Td>
              <Td className="whitespace-normal">{hostProgress(h, queue, leases)}</Td>
              <Td>
                <RelTime fixed value={h.updatedAt} />
              </Td>
              <Td className="min-w-28">
                <span className="inline-flex gap-1">
                  {(h.state === 'pending' || h.state === 'expected' || h.state === 'failed') && (
                    <ActionIcon
                      icon="skip"
                      label={`Skip ${h.host} in ${r.title}`}
                      onAction={onAction}
                      intent={hostsIntent('skip', `Skip ${h.host}`, r.id, [h.host])}
                    />
                  )}
                  {h.state === 'failed' && (
                    <ActionIcon
                      icon="retry"
                      label={`Retry ${h.host} in ${r.title}`}
                      onAction={onAction}
                      intent={hostsIntent('retry', `Retry ${h.host}`, r.id, [h.host], r.state)}
                    />
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

/**
 * RolloutTargets lists what a group rollout moves its group to: each tag at
 * the digest it moves to, and for a soft deploy the container it was reported
 * for and whether its command line changes.
 */
export function RolloutTargets({ targets, label }: { targets: readonly RolloutTarget[]; label: string }) {
  return (
    <ul aria-label={label} className="flex flex-col text-xs/5">
      {targets.map(t => (
        <li key={`${t.spec ?? ''} ${t.image}`} className="flex flex-wrap items-center gap-x-1.5">
          {t.spec && (
            <>
              <Badge
                title={`Ansible's soft deploy ${t.spec}: the deployed block of overrides.yaml names another image or command line`}
              >
                spec
              </Badge>
              {t.container && <span className="font-medium">{t.container}</span>}
              <span className="text-muted" aria-hidden="true">
                →
              </span>
            </>
          )}
          <span className="font-mono">{t.image}</span>
          <span className="text-muted">@</span>
          <Digest value={t.digest} />
          {t.revision && (
            <span className="text-muted">
              revision <span className="font-mono">{shortRevision(t.revision)}</span>
            </span>
          )}
          {t.flags && <span className="text-warning">command line changed</span>}
        </li>
      ))}
    </ul>
  );
}

export function RecentRollouts({
  rollouts,
  leases,
  label = 'Recent rollouts',
}: {
  rollouts: readonly Rollout[];
  leases: ReadonlyMap<string, Lease>;
  label?: string;
}) {
  return (
    <Table label={label}>
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
              <Td>
                {r.kind === 'local' ? 'host request' : r.kind === 'group' && r.group ? `group ${r.group}` : r.kind}
              </Td>
              <Td>
                <RolloutStateBadge state={r.state} />
              </Td>
              <Td title={hosts.map(h => `${h.host}: ${memberState(h, leases)}`).join('\n')}>
                <span className="font-mono">
                  {done}/{hosts.length}
                </span>{' '}
                <span className="text-muted">({percentOf(done, hosts.length)})</span>
                {failed > 0 && <span className="text-danger"> {failed} failed</span>}
              </Td>
              <Td className="whitespace-normal text-muted">{r.reason || <Dash />}</Td>
              <Td>{r.createdBy}</Td>
              <Td>
                <RelTime fixed value={r.createdAt} />
              </Td>
              <Td>
                <RelTime fixed value={r.endedAt} />
              </Td>
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}
