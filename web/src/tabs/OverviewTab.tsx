import { useMemo, useState, type ReactNode } from 'react';

import {
  dict,
  list,
  type Gate,
  type Lease,
  type NeedKind,
  type Plan,
  type PublicImage,
  type QueueEntry,
  type Rollout,
} from '../api/types';
import { useApp, useAuthState, usePrivateAccess } from '../appContext';
import { useActions, type ActionIntent } from '../components/ActionDialog';
import { Digest } from '../components/Digest';
import { LeaseProgress } from '../components/LeaseProgress';
import { ActionButton } from '../components/Permissions';
import { RelTime, useSharedNow } from '../components/RelTime';
import { Timeline, TimelineLegend } from '../components/Timeline';
import { Badge, Button, Card, ErrorBanner, Loading, Meter, Segmented, cx } from '../components/primitives';
import { WorkTag } from '../components/status';
import { usePoll } from '../hooks/usePoll';
import { attentionItems, type AttentionItem } from '../lib/activity';
import { formatDuration, formatPercent, percentOf, plural } from '../lib/format';
import { holdIntent, hostsIntent, scopeIntent } from '../lib/intents';
import { buildTimeline, splitQueue, workKind, type StageOf } from '../lib/timeline';

const RANGES = [
  { value: '15m', label: '15m', ms: 900_000 },
  { value: '1h', label: '1h', ms: 3_600_000 },
  { value: '3h', label: '3h', ms: 3 * 3_600_000 },
  { value: '6h', label: '6h', ms: 6 * 3_600_000 },
  { value: '12h', label: '12h', ms: 12 * 3_600_000 },
] as const;
type RangeId = (typeof RANGES)[number]['value'];

const HISTORY_EPOCHS = 256;
const LIST_LIMIT = 8;

/**
 * OverviewTab answers "what is going on": the network's health, how much of
 * the outage budget is in use, a timeline of participation and host outages
 * (past, running and planned), what is in progress, what comes next and what
 * is stuck. Logged out it shows the public part: the gate, the participation
 * history and the images.
 */
export function OverviewTab({ onOpenTab }: { onOpenTab: (tab: 'hosts' | 'rollouts' | 'settings') => void }) {
  const { api, auth } = useApp();
  const priv = usePrivateAccess();
  const authEnabled = useAuthState(auth).enabled;
  const now = useSharedNow();

  const gate = usePoll(s => api.publicNetwork(s), { key: 'public-network' });
  const history = usePoll(s => api.publicHistory(HISTORY_EPOCHS, s), { key: 'public-history', intervalMs: 30_000 });
  const images = usePoll(s => api.publicImages(s), { key: 'public-images' });
  const publicHosts = usePoll(s => api.publicHosts(s), { key: 'public-hosts' });
  const plan = usePoll(s => api.plan(s), { key: 'plan', enabled: priv });
  const rollouts = usePoll(s => api.rollouts(50, s), { key: 'rollouts', enabled: priv });
  const hosts = usePoll(s => api.hosts(s), { key: 'hosts', enabled: priv });

  const [range, setRange] = useState<RangeId>('3h');
  const rangeMs = (RANGES.find(r => r.value === range) ?? RANGES[2]).ms;
  const futureMs = rangeMs / 3;

  const actions = useActions(() => {
    plan.refresh();
    rollouts.refresh();
    hosts.refresh();
  });

  const openLeases = list(rollouts.data?.leases);
  const queue = list(plan.data?.queue);
  const openRollouts = list(rollouts.data?.open);

  // Which queued hosts start by themselves: not the ones that wait for a
  // person or the network, nor the ones behind such a host in their rollout.
  const split = useMemo(() => {
    const byRollout = new Map(
      list(rollouts.data?.open).map(r => [r.id, new Map(list(r.hosts).map(h => [h.host, h.stage]))])
    );
    const stageOf: StageOf = (rollout, host) => byRollout.get(rollout)?.get(host);
    return { ...splitQueue(list(plan.data?.queue), stageOf), stageOf };
  }, [plan.data, rollouts.data]);
  const next = split.next;

  // The model only changes with the data; "now" moves the bars' right edge
  // once per poll, which is often enough.
  const dataNow = Math.max(plan.updatedAt ?? 0, rollouts.updatedAt ?? 0) || now;
  const model = useMemo(() => {
    if (!priv || !rollouts.data) return undefined;

    // What granting a host costs the budget: its live validators.
    const costs = new Map(Object.entries(dict(plan.data?.hosts)).map(([name, h]) => [name, h.live]));
    const needKinds = new Map<string, NeedKind[]>(
      list(hosts.data?.hosts).map(h => [h.name, list(h.needs).map(n => n.kind)])
    );

    return buildTimeline({
      now: dataNow,
      from: dataNow - 12 * 3_600_000,
      horizonMs: 4 * 3_600_000,
      epochSeconds: history.data?.epochSeconds ?? 0,
      leases: [...list(rollouts.data.leases), ...list(rollouts.data.recentLeases)],
      queue: split.next,
      budget: plan.data?.budget,
      costs,
      stageOf: split.stageOf,
      needKinds,
    });
  }, [priv, rollouts.data, plan.data, hosts.data, history.data, dataNow, split]);

  const attention = useMemo(
    () =>
      attentionItems({
        gate: plan.data?.gate ?? gate.data,
        paused: plan.data?.paused ?? false,
        queue: list(plan.data?.queue),
        rollouts: list(rollouts.data?.open),
        hosts: list(hosts.data?.hosts),
      }),
    [plan.data, gate.data, rollouts.data, hosts.data]
  );
  const needKindsOf = (host: string) =>
    list(list(hosts.data?.hosts).find(h => h.name === host)?.needs).map(n => n.kind);

  const pendingHosts = list(publicHosts.data?.hosts).filter(h => h.updatePending).length;
  const totalHosts = list(publicHosts.data?.hosts).length;

  if (gate.loading && !gate.data) return <Loading what="the overview" />;

  return (
    <div className="mx-auto flex w-full max-w-screen-2xl flex-col gap-4">
      {gate.error !== undefined && <ErrorBanner error={gate.error} what="the network gate" onRetry={gate.refresh} />}
      {plan.error !== undefined && <ErrorBanner error={plan.error} what="the plan" onRetry={plan.refresh} />}
      {rollouts.error !== undefined && (
        <ErrorBanner error={rollouts.error} what="the leases" onRetry={rollouts.refresh} />
      )}
      {actions.notice}

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <GateTile gate={plan.data?.gate ?? gate.data} />
        <ParticipationTile gate={plan.data?.gate ?? gate.data} />
        {priv ? (
          <>
            <BudgetTile plan={plan.data} />
            <SchedulerTile
              plan={plan.data}
              running={openLeases.length}
              next={next.length}
              stuck={queue.length - next.length}
              onAction={actions.request}
            />
          </>
        ) : (
          <>
            <Tile label="Hosts" value={String(totalHosts)}>
              {plural(list(images.data?.images).length, 'client image')} in use
            </Tile>
            <Tile label="Updates pending" value={String(pendingHosts)} tone={pendingHosts > 0 ? 'warning' : undefined}>
              {pendingHosts > 0
                ? `of ${plural(totalHosts, 'host')} have an update waiting`
                : 'every host is up to date'}
            </Tile>
          </>
        )}
      </div>

      <Card
        title={priv ? 'Participation and outages' : 'Participation history'}
        aside={
          <>
            <TimelineLegend lanes={priv} threshold={history.data?.minParticipationPercent} />
            <Segmented label="Time range" options={RANGES} value={range} onChange={setRange} />
          </>
        }
      >
        {history.error !== undefined && (
          <ErrorBanner error={history.error} what="the participation history" onRetry={history.refresh} />
        )}
        <Timeline
          history={history.data}
          model={model}
          now={now}
          rangeMs={rangeMs}
          futureMs={futureMs}
          emptyNote={participationNote(plan.data?.gate ?? gate.data)}
        />
        {priv && model && (
          <p className="text-xs/5 text-muted">
            Planned outages are an estimate: the queue replayed against the budget, assuming a host takes{' '}
            {formatDuration(model.typicalMs / 1000)} like the recent ones did. Hosts that wait for a person or for the
            network are not planned.
            {model.unplaced > 0 &&
              ` ${plural(model.unplaced, 'queued host')} cannot be placed: other outages use up the budget.`}
            {model.late > 0 &&
              ` ${plural(model.late, 'more queued host')} would start later than the estimate looks ahead.`}
          </p>
        )}
      </Card>

      {priv ? (
        <div className="grid grid-cols-1 gap-4 xl:grid-cols-3">
          <Card title="In progress" count={openLeases.length}>
            {openLeases.length === 0 ? (
              <Quiet>No host is being updated right now.</Quiet>
            ) : (
              <ul className="flex flex-col gap-4">
                {openLeases.map(l => (
                  <LeaseItem key={l.id} lease={l} rollouts={openRollouts} />
                ))}
              </ul>
            )}
          </Card>

          <Card
            title="Up next"
            count={next.length}
            aside={
              next.length > 0 && (
                <Button
                  dense
                  variant="ghost"
                  onClick={() => {
                    onOpenTab('rollouts');
                  }}
                >
                  Rollouts →
                </Button>
              )
            }
          >
            {next.length === 0 ? (
              <Quiet>
                {queue.length > 0
                  ? 'Nothing will start by itself: see what needs attention.'
                  : 'The queue is empty. A host joins it when it reports an update.'}
              </Quiet>
            ) : (
              <NextList entries={next} eta={model?.eta} now={now} kindsOf={needKindsOf} />
            )}
            {split.behind.length > 0 && (
              <p className="text-xs/5 text-muted" title={split.behind.map(q => q.host).join(', ')}>
                {plural(split.behind.length, 'more host')} {split.behind.length === 1 ? 'waits' : 'wait'} behind a host
                that is stuck: see what needs attention.
              </p>
            )}
          </Card>

          <Card title="Needs attention" count={attention.length}>
            {attention.length === 0 ? (
              <Quiet>Nothing is stuck.</Quiet>
            ) : (
              <ul className="flex flex-col gap-3">
                {attention.map(item => (
                  <AttentionRow key={item.id} item={item} onAction={actions.request} onOpenTab={onOpenTab} />
                ))}
              </ul>
            )}
          </Card>
        </div>
      ) : (
        authEnabled && (
          <p className="text-sm/6 text-muted">
            Log in to see which hosts are being updated, the queue and the actions.
          </p>
        )
      )}

      <Card title="Client images" count={list(images.data?.images).length}>
        {images.error !== undefined && (
          <ErrorBanner error={images.error} what="the image summary" onRetry={images.refresh} />
        )}
        <ImageGrid images={list(images.data?.images)} />
      </Card>

      {actions.dialog}
    </div>
  );
}

/** participationNote says why the participation panel can be empty. */
function participationNote(gate: Gate | undefined): string {
  const n = gate?.network;
  if (n?.livenessUnsupported) return 'The beacon node does not serve liveness data';
  if (n && !n.livenessKnown && n.tracked === 0) return 'No validators to watch on this fleet';
  return 'No participation data in this window yet';
}

function Quiet({ children }: { children: ReactNode }) {
  return <p className="text-sm/6 text-muted">{children}</p>;
}

function Tile({
  label,
  value,
  tone,
  aside,
  children,
}: {
  label: string;
  value: ReactNode;
  tone?: 'success' | 'warning' | 'danger';
  aside?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <section aria-label={label} className="flex flex-col gap-1 rounded-lg border border-border bg-surface p-4">
      <div className="flex items-center gap-2">
        <h2 className="text-xs/5 font-medium text-muted">{label}</h2>
        {aside && <span className="ml-auto">{aside}</span>}
      </div>
      <p
        className={cx(
          'text-2xl/8 font-semibold',
          tone === 'success' && 'text-success',
          tone === 'warning' && 'text-warning',
          tone === 'danger' && 'text-danger'
        )}
      >
        {value}
      </p>
      <div className="flex flex-col gap-1.5 text-xs/5 text-muted">{children}</div>
    </section>
  );
}

function GateTile({ gate }: { gate: Gate | undefined }) {
  if (!gate) return <Tile label="Network gate" value="–" />;
  const n = gate.network;

  return (
    <Tile label="Network gate" value={gate.ok ? '✓ Open' : '✕ Closed'} tone={gate.ok ? 'success' : 'danger'}>
      {!gate.ok && (
        <span className="font-medium text-danger">{list(gate.reasons).join(' · ') || 'no reason given'}</span>
      )}
      <span title={`current epoch ${n.currentEpoch}, finalized epoch ${n.finalizedEpoch}, head slot ${n.headSlot}`}>
        Finality lag {plural(n.finalityLagEpochs, 'epoch')} · epoch {n.currentEpoch} · beacon{' '}
        <RelTime value={n.updatedAt} />
      </span>
      {n.error && <span className="text-warning">beacon error: {n.error}</span>}
    </Tile>
  );
}

function ParticipationTile({ gate }: { gate: Gate | undefined }) {
  if (!gate) return <Tile label="Participation" value="–" />;
  const n = gate.network;

  if (!n.livenessKnown && !n.livenessUnsupported && n.tracked === 0) {
    return (
      <Tile label="Participation" value="–">
        No host of this fleet has validators, so there is no participation to watch. The gate judges finality only.
      </Tile>
    );
  }

  if (!n.livenessKnown) {
    return (
      <Tile label="Participation" value={n.livenessUnsupported ? 'Not served' : 'Unknown'} tone="warning">
        {n.livenessUnsupported
          ? 'The beacon node does not serve liveness: readiness and the budget use the agents’ probes.'
          : 'No liveness data yet; the gate stays closed until there is.'}
      </Tile>
    );
  }

  return (
    <Tile label="Participation" value={percentOf(n.live, n.tracked)}>
      <span>
        {n.live} of {n.tracked} validators attested in epoch {n.livenessEpoch}
      </span>
    </Tile>
  );
}

function BudgetTile({ plan }: { plan: Plan | undefined }) {
  if (!plan) return <Tile label="Outage budget" value="–" />;
  const b = plan.budget;

  if (b.totalWeight === 0) {
    return (
      <Tile
        label="Outage budget"
        value={
          <>
            {b.leasedHosts}
            <span className="text-sm/6 font-normal text-muted"> of {plural(b.maxLeasedHosts, 'host')} down</span>
          </>
        }
      >
        <Meter value={b.leasedHosts} max={b.maxLeasedHosts} progress label="Hosts down against the host limit" />
        <span>No host has validators: only the host limit applies.</span>
      </Tile>
    );
  }

  const used = b.totalWeight > 0 ? (b.unavailableWeight / b.totalWeight) * 100 : 0;
  const allowed = b.totalWeight > 0 ? (b.allowedWeight / b.totalWeight) * 100 : 0;
  const over = b.unavailableWeight > b.allowedWeight + 1e-9;

  return (
    <Tile
      label="Outage budget"
      value={
        <>
          {formatPercent(used)}
          <span className="text-sm/6 font-normal text-muted"> of {formatPercent(allowed)} validators offline</span>
        </>
      }
      tone={over ? 'danger' : undefined}
    >
      <Meter value={b.unavailableWeight} max={b.allowedWeight} label="Validators offline against the allowance" />
      <span title={`${b.unavailableWeight} of ${b.totalWeight} validators are offline or on a host being updated`}>
        {plural(b.leasedHosts, 'host')} down for updates · at most {b.maxLeasedHosts} at once
      </span>
    </Tile>
  );
}

function SchedulerTile({
  plan,
  running,
  next,
  stuck,
  onAction,
}: {
  plan: Plan | undefined;
  running: number;
  next: number;
  stuck: number;
  onAction: (intent: ActionIntent) => void;
}) {
  if (!plan) return <Tile label="Scheduler" value="–" />;

  return (
    <Tile
      label="Scheduler"
      value={plan.paused ? 'Paused' : 'Running'}
      tone={plan.paused ? 'warning' : undefined}
      aside={
        plan.paused ? (
          <ActionButton
            dense
            variant="primary"
            onAction={onAction}
            intent={scopeIntent('resume', 'Resume everything', '')}
          >
            Resume all…
          </ActionButton>
        ) : (
          <ActionButton dense onAction={onAction} intent={scopeIntent('pause', 'Pause everything', '')}>
            Pause all…
          </ActionButton>
        )
      }
    >
      <span>
        {running} in progress · {next} up next ·{' '}
        {stuck > 0 ? <span className="text-warning">{stuck} stuck</span> : '0 stuck'}
      </span>
    </Tile>
  );
}

function rolloutTitles(ids: readonly string[], rollouts: readonly Rollout[]): string {
  const titles = new Map(rollouts.map(r => [r.id, r.title]));
  return [...new Set(ids)].map(id => titles.get(id) ?? id).join(' · ');
}

function LeaseItem({ lease, rollouts }: { lease: Lease; rollouts: readonly Rollout[] }) {
  const title = rolloutTitles(Object.values(dict(lease.rollouts)), rollouts);
  const note =
    lease.error ??
    lease.operation?.error ??
    lease.readiness ??
    (lease.operation ? undefined : 'Waiting for the agent to pick up the lease.');

  return (
    <li className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-baseline gap-x-2">
        <span className="text-sm/6 font-semibold">{lease.host}</span>
        <WorkTag kind={workKind(list(lease.needs).map(n => n.kind))} />
        {lease.force && <Badge tone="warning">force</Badge>}
        <span className="ml-auto text-xs/5 text-muted">
          started <RelTime value={lease.grantedAt} />
        </span>
      </div>
      <LeaseProgress lease={lease} />
      {title && <p className="truncate text-xs/5 text-muted">{title}</p>}
      {note && (
        <p className={cx('text-xs/5', lease.error || lease.operation?.error ? 'text-danger' : 'text-muted')}>{note}</p>
      )}
    </li>
  );
}

function NextList({
  entries,
  eta,
  now,
  kindsOf,
}: {
  entries: readonly QueueEntry[];
  eta: ReadonlyMap<string, number> | undefined;
  now: number;
  kindsOf: (host: string) => NeedKind[];
}) {
  const [all, setAll] = useState(false);
  const shown = all ? entries : entries.slice(0, LIST_LIMIT);

  return (
    <div className="flex flex-col gap-2">
      <ol className="flex flex-col">
        {shown.map((q, i) => {
          const start = eta?.get(q.host);
          return (
            <li key={q.host} className="flex items-baseline gap-2 border-b border-border py-1.5 last:border-b-0">
              <span className="w-5 shrink-0 text-right text-xs/5 text-faint tabular-nums">{i + 1}</span>
              <span className="flex min-w-0 grow flex-col">
                <span className="flex flex-wrap items-baseline gap-x-2">
                  <span className="text-sm/6 font-medium">{q.host}</span>
                  <WorkTag kind={workKind(kindsOf(q.host))} />
                </span>
                <span className="truncate text-xs/5 text-muted" title={q.reason}>
                  {q.wait === 'next' ? 'starts with the next plan' : `waits: ${q.reason}`}
                </span>
              </span>
              <span className="shrink-0 text-xs/5 text-muted tabular-nums" title="Estimated start">
                {start === undefined
                  ? ''
                  : start - now < 60_000
                    ? '<1m'
                    : `~${formatDuration(Math.round((start - now) / 60_000) * 60)}`}
              </span>
            </li>
          );
        })}
      </ol>
      {entries.length > LIST_LIMIT && (
        <Button
          dense
          variant="ghost"
          className="self-start"
          onClick={() => {
            setAll(v => !v);
          }}
        >
          {all ? 'Show fewer' : `Show all ${entries.length}`}
        </Button>
      )}
    </div>
  );
}

function AttentionRow({
  item,
  onAction,
  onOpenTab,
}: {
  item: AttentionItem;
  onAction: (intent: ActionIntent) => void;
  onOpenTab: (tab: 'hosts' | 'rollouts' | 'settings') => void;
}) {
  const r = item.rollout;
  let buttons: ReactNode = null;

  switch (item.kind) {
    case 'paused':
      buttons = (
        <ActionButton
          dense
          variant="primary"
          onAction={onAction}
          intent={scopeIntent('resume', 'Resume everything', '')}
        >
          Resume all…
        </ActionButton>
      );
      break;
    case 'approval':
      buttons = r && (
        <ActionButton
          dense
          variant="primary"
          onAction={onAction}
          intent={scopeIntent('resume', `Approve rollout: ${r.title}`, r.id)}
        >
          Approve…
        </ActionButton>
      );
      break;
    case 'rollout-paused':
      buttons = r && (
        <ActionButton
          dense
          variant="primary"
          onAction={onAction}
          intent={scopeIntent('resume', `Resume rollout: ${r.title}`, r.id)}
        >
          Resume…
        </ActionButton>
      );
      break;
    case 'halted':
      buttons = r && (
        <>
          <ActionButton
            dense
            variant="primary"
            onAction={onAction}
            intent={hostsIntent('retry', `Retry rollout: ${r.title}`, r.id, [])}
          >
            Retry…
          </ActionButton>
          <ActionButton
            dense
            variant="danger"
            onAction={onAction}
            intent={scopeIntent('abort', `Abort rollout: ${r.title}`, r.id)}
          >
            Abort…
          </ActionButton>
        </>
      );
      break;
    case 'held':
      buttons = (
        <ActionButton dense onAction={onAction} intent={holdIntent('unhold', item.hosts)}>
          Unhold…
        </ActionButton>
      );
      break;
    case 'policy-paused':
      buttons = (
        <Button
          dense
          onClick={() => {
            onOpenTab('settings');
          }}
        >
          Settings →
        </Button>
      );
      break;
    case 'gate':
    case 'silent':
    case 'verb':
      break;
  }

  return (
    <li className="flex gap-2.5">
      <span
        aria-hidden="true"
        className={cx('mt-1.5 size-2 shrink-0 rounded-full', item.tone === 'danger' ? 'bg-danger-solid' : 'bg-warning')}
      />
      <div className="flex min-w-0 grow flex-col gap-1">
        <p className="text-sm/6 font-medium">{item.title}</p>
        <p className="text-xs/5 text-muted">{item.detail}</p>
        {item.hosts.length > 0 && item.kind !== 'held' && item.hosts.length <= 6 && (
          <p className="text-xs/5 text-faint">{item.hosts.join(', ')}</p>
        )}
        {buttons && <div className="flex flex-wrap gap-1.5">{buttons}</div>}
      </div>
    </li>
  );
}

function ImageGrid({ images }: { images: readonly PublicImage[] }) {
  if (images.length === 0) return <Quiet>No host has reported a client image yet.</Quiet>;

  return (
    <ul className="grid grid-cols-1 gap-x-8 lg:grid-cols-2">
      {images.map(img => {
        const on = list(img.hostsOn).length;
        const behind = list(img.hostsBehind);
        const total = on + behind.length;

        return (
          <li
            key={`${img.kind}/${img.image}`}
            className="flex items-center gap-3 border-b border-border py-1.5 text-xs/5"
            title={behind.length > 0 ? `behind: ${behind.join(', ')}` : undefined}
          >
            <span className="w-6 shrink-0 font-medium text-faint uppercase">{img.kind}</span>
            <span className="min-w-0 grow truncate font-mono text-sm/6" title={img.image}>
              {img.image}
            </span>
            <Digest value={img.digest} />
            <span className="flex w-40 shrink-0 items-center gap-2">
              <span className="w-16">
                <Meter progress value={on} max={total} label={`Hosts on the newest digest of ${img.image}`} />
              </span>
              <span className={cx('tabular-nums', behind.length > 0 ? 'font-medium text-warning' : 'text-muted')}>
                {on}/{total}
                {behind.length > 0 && ` · ${behind.length} behind`}
              </span>
            </span>
          </li>
        );
      })}
    </ul>
  );
}
