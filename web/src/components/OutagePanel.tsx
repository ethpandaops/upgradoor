import { list, type Lease, type Outage, type Rollout, type Rollouts, type SlotRow } from '../api/types';
import { usePrivateAccess } from '../appContext';
import { formatDuration, percentOf, plural, shortRevision } from '../lib/format';
import { leasePlace } from '../lib/rolloutsView';
import { Digest, ShortId } from './Digest';
import { Drawer } from './Drawer';
import { useLive } from './Live';
import { clock, workLabel, type TimelineSelection } from './Timeline';
import { Badge, Button, Section, cx } from './primitives';
import { LeaseStateBadge, NeedLine, RolloutStateBadge } from './status';

/** HostMiss is what one host's validators did in the slots of a span in which they missed attestations. */
export interface HostMiss {
  host: string;
  /** The slots of the span in which the host missed. */
  slots: number;
  assigned: number;
  included: number;
}

/**
 * hostMisses sums, per host, the slots of a span in which the host missed
 * attestations, from the slots an event stream carries. Only the newest
 * epochs are there: covered says how many slots of the span the rows hold.
 */
export function hostMisses(
  rows: readonly SlotRow[],
  slot: number,
  slots: number
): { hosts: HostMiss[]; covered: number } {
  const byHost = new Map<string, HostMiss>();
  let covered = 0;

  for (const row of rows) {
    if (row.slot < slot || row.slot >= slot + slots) continue;
    covered++;

    for (const h of list(row.hosts)) {
      const miss = byHost.get(h.host) ?? { host: h.host, slots: 0, assigned: 0, included: 0 };
      miss.slots++;
      miss.assigned += h.assigned;
      miss.included += h.included;
      byHost.set(h.host, miss);
    }
  }

  const hosts = [...byHost.values()].sort(
    (a, b) =>
      b.assigned - b.included - (a.assigned - a.included) || a.host.localeCompare(b.host, undefined, { numeric: true })
  );

  return { hosts, covered };
}

/** outagesIn are the spans that overlap the time from fromMs to toMs: work by upgradoor first, then by when they began. */
export function outagesIn(outages: readonly Outage[], fromMs: number, toMs: number, now: number): Outage[] {
  return outages
    .filter(o => Date.parse(o.from) < toMs && (o.open ? now : Date.parse(o.to)) > fromMs)
    .sort((a, b) => Number(!a.work) - Number(!b.work) || Date.parse(a.from) - Date.parse(b.from));
}

/** outageKey names a span across updates of the window, which replace its object. */
function outageKey(o: Outage): string {
  return `${o.lease ?? ''}/${o.from}/${list(o.names).join(',')}`;
}

/**
 * outageCauses are the spans of the block that was asked about, then whatever
 * else was off in its time, each once and as the window has it now.
 */
export function outageCauses(
  block: readonly Outage[],
  outages: readonly Outage[],
  fromMs: number,
  toMs: number,
  now: number
): Outage[] {
  const causes = new Map<string, Outage>();
  for (const o of [...block, ...outagesIn(outages, fromMs, toMs, now)]) causes.set(outageKey(o), o);
  return [...causes.values()];
}

function slotsTitle(s: TimelineSelection): string {
  return s.slots === 1 ? `Slot ${s.slot}` : `Slots ${s.slot}–${s.slot + s.slots - 1}`;
}

/**
 * OutagePanel answers "what happened here" for a span of the participation
 * graph: what the fleet's validators did in it, which hosts missed, and what
 * upgradoor was doing to them at that time, down to the images of the roll.
 * Without a login it has the public numbers only.
 */
export function OutagePanel({
  selection,
  outages,
  now,
  onClose,
  onOpenHost,
  onOpenRollouts,
}: {
  selection: TimelineSelection;
  /** outages are the spans of the window; undefined without a login. */
  outages: readonly Outage[] | undefined;
  now: number;
  onClose: () => void;
  onOpenHost?: (host: string) => void;
  /** onOpenRollouts opens the Rollouts tab at a place: the list that holds the leases of the span. */
  onOpenRollouts?: (place: string) => void;
}) {
  const priv = usePrivateAccess();
  const rollouts = useLive('rollouts', priv).data;
  const rows = useLive('slots').data?.rows;

  const s = selection;
  const block = s.block;
  const epoch = Math.floor(s.slot / Math.max(1, s.slotsPerEpoch));
  const lastEpoch = Math.floor((s.slot + Math.max(1, s.slots) - 1) / Math.max(1, s.slotsPerEpoch));

  const causes = outages ? outageCauses(block?.outages ?? [], outages, s.fromMs, s.toMs, now) : undefined;
  const misses = hostMisses(rows ?? [], s.slot, s.slots);
  const explained = new Set(list(causes).flatMap(o => list(o.names)));

  const title = block
    ? `${workLabel(block)} · ${block.hosts === 1 ? (block.outages[0]?.names?.[0] ?? 'a host') : plural(block.hosts, 'host')}`
    : slotsTitle(s);

  return (
    <Drawer
      label={`What happened: ${title}`}
      onClose={onClose}
      header={
        <>
          <h2 className="text-sm/6 font-semibold">{title}</h2>
          <p className="text-xs/5 text-muted tabular-nums">
            {block && `${slotsTitle(s).toLowerCase()} · `}
            epoch {epoch === lastEpoch ? epoch : `${epoch}–${lastEpoch}`} · {clock(s.fromMs, true)}–
            {clock(s.toMs, true)}
          </p>
        </>
      }
    >
      <div className="flex min-h-0 grow flex-col gap-5 overflow-y-auto p-4 text-xs/5">
        <Section title="Participation">
          {s.recorded === 0 ? (
            <p className="text-muted">
              {s.toMs > now - 3 * s.slotMs
                ? 'Not settled yet: a slot is counted two slots after it began.'
                : 'No data: the controller was not watching the chain.'}
            </p>
          ) : (
            <ul className="flex flex-col gap-0.5">
              <li>
                {s.assigned > 0 ? (
                  <>
                    <span className={cx('font-semibold tabular-nums', s.included < s.assigned && 'text-danger')}>
                      {s.assigned - s.included} of {s.assigned}
                    </span>{' '}
                    attestations missed ({percentOf(s.included, s.assigned)} attested)
                  </>
                ) : (
                  <span className="text-muted">No attestation was due from this fleet.</span>
                )}
              </li>
              <li className={s.missed > 0 ? 'font-medium text-danger' : 'text-muted'}>
                {s.slots === 1
                  ? s.missed > 0
                    ? 'Block missed'
                    : 'Block proposed'
                  : s.missed > 0
                    ? `${s.missed} of ${s.recorded} blocks missed`
                    : `all ${s.recorded} blocks proposed`}
                {s.recorded < s.slots && ` · ${plural(s.slots - s.recorded, 'slot')} without data`}
              </li>
            </ul>
          )}
        </Section>

        {!causes ? (
          <p className="text-muted">Log in to see which hosts missed and what was being rolled on them.</p>
        ) : (
          <>
            <Section title="Hosts that missed attestations">
              {misses.hosts.length > 0 ? (
                <ul className="flex flex-col">
                  {misses.hosts.map(m => (
                    <li key={m.host} className="flex items-baseline gap-2 border-b border-border py-1 last:border-b-0">
                      <HostName name={m.host} onOpenHost={onOpenHost} />
                      {!explained.has(m.host) && (
                        <span
                          className="text-faint"
                          title="No update by upgradoor and no offline span covers this host here."
                        >
                          nothing below explains it
                        </span>
                      )}
                      <span className="ml-auto text-danger tabular-nums">
                        {m.assigned - m.included} of {m.assigned} missed
                        {s.slots > 1 && <span className="text-muted"> in {plural(m.slots, 'slot')}</span>}
                      </span>
                    </li>
                  ))}
                </ul>
              ) : misses.covered >= s.slots && s.slots > 0 ? (
                <p className="text-muted">No host missed an attestation in this span.</p>
              ) : null}
              {misses.covered < s.slots && (
                <p className="text-muted">
                  {misses.covered === 0
                    ? 'The split per host and slot is only kept for the newest epochs, and this span is older: the hosts are those of the spans below, with the misses of each whole span.'
                    : `The split per host and slot is only kept for the newest epochs: it covers ${misses.covered} of the ${s.slots} slots here. The spans below count the rest.`}
                </p>
              )}
            </Section>

            <Section title="What was off, and why">
              {causes.length === 0 ? (
                <p className="text-muted">
                  No host was being updated by upgradoor and none counted as offline in this span. A host counts as
                  offline once its validators miss in several slots close together; a single miss is usually a late
                  attestation or a missed block.
                </p>
              ) : (
                <ul className="flex flex-col gap-3">
                  {causes.map(o => (
                    <Cause key={outageKey(o)} outage={o} rollouts={rollouts} now={now} onOpenHost={onOpenHost} />
                  ))}
                </ul>
              )}
            </Section>

            {onOpenRollouts && (
              <Button
                className="self-start"
                onClick={() => {
                  onOpenRollouts(
                    leasePlace(
                      list(causes).flatMap(o => (o.lease ? [o.lease] : [])),
                      rollouts
                    )
                  );
                }}
              >
                Rollouts →
              </Button>
            )}
          </>
        )}
      </div>
    </Drawer>
  );
}

function HostName({ name, onOpenHost }: { name: string; onOpenHost: ((host: string) => void) | undefined }) {
  if (!onOpenHost) return <span className="font-medium">{name}</span>;

  return (
    <button
      type="button"
      title={`Find ${name} in the Hosts tab`}
      className="font-medium text-accent hover:underline"
      onClick={() => {
        onOpenHost(name);
      }}
    >
      {name}
    </button>
  );
}

/** Cause is one span during which hosts were off: who, what was done to them, and how it went. */
function Cause({
  outage: o,
  rollouts,
  now,
  onOpenHost,
}: {
  outage: Outage;
  rollouts: Rollouts | undefined;
  now: number;
  onOpenHost: ((host: string) => void) | undefined;
}) {
  const names = list(o.names);
  const from = Date.parse(o.from);
  const to = o.open ? now : Date.parse(o.to);
  const off = !o.work;

  const lease: Lease | undefined = o.lease
    ? [...list(rollouts?.leases), ...list(rollouts?.recentLeases)].find(l => l.id === o.lease)
    : undefined;
  const rollout: Rollout | undefined = o.rollout
    ? [...list(rollouts?.open), ...list(rollouts?.recent)].find(r => r.id === o.rollout)
    : undefined;

  return (
    <li className="flex flex-col gap-1.5 rounded-md border border-border p-2.5">
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="flex flex-wrap gap-x-1.5">
          {names.map(n => (
            <HostName key={n} name={n} onOpenHost={onOpenHost} />
          ))}
          {o.hosts > names.length && <span className="text-muted">and {o.hosts - names.length} more</span>}
        </span>
        <Badge tone={off ? 'neutral' : 'info'}>{workLabel({ group: off ? 'off' : 'work', work: o.work })}</Badge>
        {off ? (
          <Badge tone={o.open ? 'danger' : 'neutral'}>{o.open ? 'still off' : 'back'}</Badge>
        ) : (
          <Badge tone={o.failed ? 'danger' : o.open ? 'accent' : 'success'}>
            {o.failed ? 'failed' : o.open ? 'in progress' : 'finished'}
          </Badge>
        )}
        {o.deployed && (
          <Badge
            tone="accent"
            title="The lease applies what a deploy on the host set (upgradoor deploy, e.g. from Ansible)."
          >
            deployed on the host
          </Badge>
        )}
      </div>

      <p className="text-muted tabular-nums">
        {o.fromStart ? 'before ' : ''}
        {clock(from, true)} – {o.open ? 'now' : clock(to, true)} ({formatDuration((to - from) / 1000)})
        {o.validators > 0 && ` · ${plural(o.validators, 'validator')}`}
        {o.assigned > 0 && (
          <span className={o.missed > 0 ? 'text-danger' : undefined}>
            {' '}
            · {o.missed} of {o.assigned} attestations missed
          </span>
        )}
      </p>

      {off && (
        <p className="text-muted">
          Not attesting, and upgradoor was not working on {o.hosts === 1 ? 'it' : 'them'}: no lease explains this span.
        </p>
      )}

      {rollout ? (
        <div className="flex flex-col gap-1">
          <p className="flex flex-wrap items-baseline gap-x-2">
            <span className="text-muted">Rollout</span>
            <span className="font-medium wrap-anywhere">{rollout.title}</span>
            <RolloutStateBadge state={rollout.state} />
          </p>
          {list(rollout.targets).length > 0 && (
            <ul className="flex flex-col gap-0.5">
              {list(rollout.targets).map(t => (
                <li key={`${t.image}/${t.spec ?? ''}`} className="flex flex-wrap items-baseline gap-x-1.5">
                  <span className="text-muted">to</span>
                  <span className="font-mono wrap-anywhere">{t.image}</span>
                  <Digest value={t.digest} />
                  {t.revision && (
                    <span className="font-mono text-muted" title={`revision ${t.revision}`}>
                      {shortRevision(t.revision)}
                    </span>
                  )}
                  {t.spec && <span className="text-muted">deploy{t.flags ? ', command line changes' : ''}</span>}
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : (
        o.rollout && (
          <p className="flex flex-wrap items-baseline gap-x-2 text-muted">
            Rollout <ShortId value={o.rollout} label="rollout" />: it is no longer among the recent ones; the Rollouts
            tab and the audit log have it.
          </p>
        )
      )}

      {lease ? (
        <div className="flex flex-col gap-1">
          <p className="flex flex-wrap items-baseline gap-x-2">
            <span className="text-muted">Lease</span>
            <ShortId value={lease.id} label="lease" />
            <LeaseStateBadge state={lease.state} />
            {lease.force && <Badge tone="warning">force</Badge>}
            <span className="text-muted">by {lease.grantedBy}</span>
          </p>
          <ul className="flex flex-col gap-0.5">
            {list(lease.needs).map(n => (
              <li key={n.id}>
                <NeedLine need={n} />
              </li>
            ))}
          </ul>
          {(lease.error ?? lease.operation?.error) && (
            <p className="text-danger wrap-anywhere">{lease.error ?? lease.operation?.error}</p>
          )}
        </div>
      ) : (
        o.lease && (
          <p className="flex flex-wrap items-baseline gap-x-2 text-muted">
            Lease <ShortId value={o.lease} label="lease" />: what it changed is no longer among the recent leases; the
            host's history has it.
          </p>
        )
      )}

      {!off && !o.lease && o.hosts > 1 && (
        <p className="text-muted">
          Several hosts of one kind of work, drawn as one span. Choose a shorter time range to see each host's lease.
        </p>
      )}
    </li>
  );
}
