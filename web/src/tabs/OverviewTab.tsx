import { useMemo, useState, type ReactNode } from 'react';

import {
  dict,
  type Budget,
  list,
  type Gate,
  type Lease,
  type NeedKind,
  type Plan,
  type PublicImage,
  type QueueEntry,
  type Rollout,
} from '../api/types';
import { beaconSeen, chainAt } from '../api/wire';
import { useApp, useAuthState, usePrivateAccess } from '../appContext';
import { useActions, type ActionIntent } from '../components/ActionDialog';
import { Digest } from '../components/Digest';
import { useLive } from '../components/Live';
import { LeaseProgress } from '../components/LeaseProgress';
import { useReadinessNote } from '../components/Readiness';
import { IntentAction } from '../components/SelectionBar';
import { RelTime, useSharedNow } from '../components/RelTime';
import { Timeline, TimelineLegend } from '../components/Timeline';
import { Badge, Button, Card, ErrorBanner, Loading, Meter, Segmented, cx } from '../components/primitives';
import { WorkTag } from '../components/status';
import { setKept } from '../hooks/useKept';
import { useSlotHistory } from '../hooks/useSlotHistory';
import { attentionItems, type AttentionItem } from '../lib/activity';
import { formatPercent, percentOf, plural } from '../lib/format';
import { rolloutActionOf } from '../lib/hostActions';
import { holdIntent, hostsIntent, scopeIntent } from '../lib/intents';
import { splitQueue, workKind, type StageOf } from '../lib/timeline';

const RANGES = [
  { value: '15m', label: '15m', ms: 900_000 },
  { value: '1h', label: '1h', ms: 3_600_000 },
  { value: '3h', label: '3h', ms: 3 * 3_600_000 },
  { value: '6h', label: '6h', ms: 6 * 3_600_000 },
  { value: '12h', label: '12h', ms: 12 * 3_600_000 },
] as const;
type RangeId = (typeof RANGES)[number]['value'];

const LIST_LIMIT = 8;

/** IN_PROGRESS_SHOWN is how many open leases the In progress card lists; the rest is a count. */
const IN_PROGRESS_SHOWN = 8;

/**
 * OverviewTab answers "what is going on": the network's health, how much of
 * the outage budget is in use, the participation slot by slot with what
 * every host's validators did and what upgradoor did to the host, what is in
 * progress, what comes next and what is stuck. Logged out it shows the
 * public part: the gate, the fleet's participation per slot and the images.
 */
export function OverviewTab({ onOpenTab }: { onOpenTab: (tab: 'hosts' | 'rollouts' | 'settings') => void }) {
  const { auth } = useApp();
  const priv = usePrivateAccess();
  const authEnabled = useAuthState(auth).enabled;
  const now = useSharedNow();

  const [range, setRange] = useState<RangeId>('3h');
  const rangeMs = (RANGES.find(r => r.value === range) ?? RANGES[2]).ms;

  const gate = useLive('public-network');
  const history = useSlotHistory(rangeMs / 1000);
  const images = useLive('public-images');
  const publicHosts = useLive('public-hosts');
  const plan = useLive('plan', priv);
  const rollouts = useLive('rollouts', priv);
  const hosts = useLive('hosts', priv);

  const actions = useActions();

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
    return splitQueue(list(plan.data?.queue), stageOf);
  }, [plan.data, rollouts.data]);
  const next = split.next;

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

  if (gate.loading) return <Loading what="the overview" />;

  return (
    <div className="mx-auto flex w-full max-w-screen-2xl flex-col gap-4">
      {gate.error !== undefined && <ErrorBanner error={gate.error} what="the overview" />}
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
        title={priv ? 'Participation and outages' : 'Participation per slot'}
        aside={
          <>
            <TimelineLegend lanes={priv} threshold={history.data?.minParticipationPercent} />
            <Segmented label="Time range" options={RANGES} value={range} onChange={setRange} />
          </>
        }
      >
        <Timeline
          history={history.data}
          outages={history.outages}
          onOpenHost={host => {
            setKept('hosts.filter', host);
            onOpenTab('hosts');
          }}
          now={now}
          rangeMs={rangeMs}
          emptyNote={participationNote(plan.data?.gate ?? gate.data, history.loading)}
        />
      </Card>

      {priv ? (
        <div className="grid grid-cols-1 gap-4 xl:grid-cols-3">
          <Card title="In progress" count={openLeases.length}>
            {openLeases.length === 0 ? (
              <Quiet>No host is being updated right now.</Quiet>
            ) : (
              <ul className="flex flex-col gap-4">
                {openLeases.slice(0, IN_PROGRESS_SHOWN).map(l => (
                  <LeaseItem key={l.id} lease={l} rollouts={openRollouts} />
                ))}
                {openLeases.length > IN_PROGRESS_SHOWN && (
                  <li className="text-xs/5 text-muted">
                    and {openLeases.length - IN_PROGRESS_SHOWN} more:{' '}
                    <button
                      type="button"
                      className="font-medium text-accent hover:underline"
                      onClick={() => {
                        onOpenTab('rollouts');
                      }}
                    >
                      see the rollouts
                    </button>
                  </li>
                )}
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
              <NextList entries={next} kindsOf={needKindsOf} />
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
                  <AttentionRow key={item.id} item={item} onAction={actions.request} />
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
        <ImageGrid images={list(images.data?.images)} />
      </Card>

      {actions.dialog}
    </div>
  );
}

/**
 * participationNote says why the participation panel can be empty.
 */
function participationNote(gate: Gate | undefined, loading: boolean): string {
  const n = gate?.network;
  if (loading) return 'Loading the participation…';
  if (n && n.livenessKnown && n.tracked === 0) return 'No validators to watch on this fleet';
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
          'truncate text-2xl/8 font-semibold tabular-nums',
          tone === 'success' && 'text-success',
          tone === 'warning' && 'text-warning',
          tone === 'danger' && 'text-danger'
        )}
      >
        {value}
      </p>
      <div className="flex flex-col gap-1.5 text-xs/5 text-muted tabular-nums">{children}</div>
    </section>
  );
}

function GateTile({ gate }: { gate: Gate | undefined }) {
  const now = useSharedNow();
  if (!gate) return <Tile label="Network gate" value="–" />;
  // The stream does not carry what the clock says: the epoch is computed here.
  const n = chainAt(gate.network, now);
  const seen = beaconSeen(gate.network);

  return (
    <Tile label="Network gate" value={gate.ok ? '✓ Open' : '✕ Closed'} tone={gate.ok ? 'success' : 'danger'}>
      {!gate.ok && (
        <span className="font-medium text-danger">{list(gate.reasons).join(' · ') || 'no reason given'}</span>
      )}
      {/* One line each, whatever the numbers: a wrap would change the height of the whole row of tiles. */}
      <span
        className="truncate"
        title={`current epoch ${n.currentEpoch}, finalized epoch ${n.finalizedEpoch}, head slot ${n.headSlot}`}
      >
        Finality lag {plural(n.finalityLagEpochs, 'epoch')} · epoch {n.currentEpoch}
      </span>
      {/* While the beacon node answers, when it last did is "now": that time is not sent. */}
      {seen.at === undefined ? (
        <span className="truncate">beacon node answers</span>
      ) : (
        <span className={cx('truncate', seen.silent && 'text-warning')}>
          beacon node last answered <RelTime fixed value={seen.at} />
        </span>
      )}
      {n.error && <span className="text-warning">beacon error: {n.error}</span>}
      {n.validatorRangesError && <span className="text-warning">validator ranges: {n.validatorRangesError}</span>}
    </Tile>
  );
}

function ParticipationTile({ gate }: { gate: Gate | undefined }) {
  const now = useSharedNow();
  if (!gate) return <Tile label="Participation" value="–" />;
  const n = chainAt(gate.network, now);

  if (n.livenessKnown && n.tracked === 0) {
    return (
      <Tile label="Participation" value="–">
        No host of this fleet has validators, so there is no participation to watch. The gate judges finality only.
      </Tile>
    );
  }

  if (!n.livenessKnown) {
    return (
      <Tile label="Participation" value="Unknown" tone="warning">
        {n.livenessError ?? 'No settled epoch of attestation data yet; the gate stays closed until there is.'}
      </Tile>
    );
  }

  return (
    <Tile label="Participation" value={percentOf(n.live, n.tracked)}>
      <span className="truncate">
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
      <span
        className="truncate"
        title={`${b.unavailableWeight} of ${b.totalWeight} validators are offline or on a host being updated`}
      >
        {plural(b.leasedHosts, 'host')} down for updates · at most {b.maxLeasedHosts} at once
      </span>
      <OfflineNote budget={b} />
    </Tile>
  );
}

/** OfflineNote says what the budget and the gate leave out for hosts marked known offline. */
function OfflineNote({ budget }: { budget: Budget }) {
  const hosts = budget.offlineHosts ?? 0;
  if (hosts === 0) return null;
  return (
    <span className="truncate text-muted">
      {plural(budget.offlineWeight ?? 0, 'validator')} of {plural(hosts, 'known-offline host')} left out
    </span>
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
        )
      }
    >
      <span className="truncate">
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
  const readiness = useReadinessNote(lease);
  const note =
    lease.error ??
    lease.operation?.error ??
    readiness ??
    (lease.operation ? undefined : 'Waiting for the agent to pick up the lease.');

  return (
    // Every line is there in every phase and stays one line: the item keeps
    // its height while the lease moves on, so nothing below it jumps.
    <li className="flex flex-col gap-1.5">
      <div className="flex items-baseline gap-x-2">
        <span className="min-w-0 truncate text-sm/6 font-semibold">{lease.host}</span>
        <WorkTag kind={workKind(list(lease.needs).map(n => n.kind))} />
        {lease.force && <Badge tone="warning">force</Badge>}
        <span className="ml-auto shrink-0 text-right text-xs/5 whitespace-nowrap text-muted">
          started <RelTime value={lease.grantedAt} />
        </span>
      </div>
      <LeaseProgress lease={lease} />
      {title && <p className="truncate text-xs/5 text-muted">{title}</p>}
      <p
        title={note}
        className={cx('truncate text-xs/5', lease.error || lease.operation?.error ? 'text-danger' : 'text-muted')}
      >
        {note ?? '\u00a0'}
      </p>
    </li>
  );
}

function NextList({ entries, kindsOf }: { entries: readonly QueueEntry[]; kindsOf: (host: string) => NeedKind[] }) {
  const [all, setAll] = useState(false);
  const shown = all ? entries : entries.slice(0, LIST_LIMIT);

  return (
    <div className="flex flex-col gap-2">
      <ol className="flex flex-col">
        {shown.map((q, i) => (
          <li key={q.host} className="flex items-baseline gap-2 border-b border-border py-1.5 last:border-b-0">
            <span className="w-5 shrink-0 text-right text-xs/5 text-faint tabular-nums">{i + 1}</span>
            <span className="flex min-w-0 grow flex-col">
              <span className="flex flex-wrap items-baseline gap-x-2">
                <span className="text-sm/6 font-medium">{q.host}</span>
                <WorkTag kind={workKind(kindsOf(q.host))} />
              </span>
              <span className="truncate text-xs/5 text-muted" title={q.reason}>
                waits: {q.reason}
              </span>
            </span>
          </li>
        ))}
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

function AttentionRow({ item, onAction }: { item: AttentionItem; onAction: (intent: ActionIntent) => void }) {
  const r = item.rollout;
  let buttons: ReactNode = null;

  switch (item.kind) {
    case 'approval':
      buttons = r && (
        <IntentAction
          icon="check"
          label="Approve…"
          onAction={onAction}
          intent={scopeIntent('resume', `Approve rollout: ${r.title}`, r.id, r.team, rolloutActionOf(r))}
        />
      );
      break;
    case 'rollout-paused':
      buttons = r && (
        <IntentAction
          icon="resume"
          label="Resume…"
          onAction={onAction}
          intent={scopeIntent('resume', `Resume rollout: ${r.title}`, r.id, r.team, rolloutActionOf(r))}
        />
      );
      break;
    case 'halted':
      buttons = r && (
        <>
          <IntentAction
            icon="retry"
            label="Retry…"
            onAction={onAction}
            intent={hostsIntent('retry', `Retry rollout: ${r.title}`, r.id, [])}
          />
          <IntentAction
            icon="abort"
            label="Abort…"
            tone="danger"
            onAction={onAction}
            intent={scopeIntent('abort', `Abort rollout: ${r.title}`, r.id)}
          />
        </>
      );
      break;
    case 'held':
      buttons = (
        <IntentAction icon="unhold" label="Unhold…" onAction={onAction} intent={holdIntent('unhold', item.hosts)} />
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
