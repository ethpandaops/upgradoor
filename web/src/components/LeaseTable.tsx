import { list, type Lease, type LeaseBaseline, type Need, type PeerCheck } from '../api/types';
import { formatDuration, secondsBetween } from '../lib/format';
import { endLeaseIntent } from '../lib/intents';
import { RunExit, RunOutput } from './ActionRun';
import { ShortId } from './Digest';
import { ActionIcon } from './icons';
import { Readiness } from './Readiness';
import { RelTime } from './RelTime';
import { Badge, Dash, Table, Td, Th } from './primitives';
import { PullNote } from './PullNote';
import { LeaseOutcomeBadge, LeaseStateBadge, StepBadge, needSummary } from './status';

function stepLabel(needs: readonly Need[], needId: string): string {
  const need = needs.find(n => n.id === needId);
  if (!need) return needId;
  if (need.kind === 'image') return need.image?.container ?? 'image';
  if (need.kind === 'override') return need.override?.container ?? 'override';
  if (need.kind === 'spec') return need.spec?.container ?? 'spec';
  if (need.kind === 'action') return need.action?.name ?? 'action';
  return need.kind;
}

/**
 * PeerChecks shows how a host's clients compared with the fleet in the last
 * readiness check of its lease: "EL 28 peers, fleet median 31, needs 16". A
 * client below what it needs is marked.
 */
export function PeerChecks({ checks }: { checks: readonly PeerCheck[] | undefined }) {
  if (!checks || checks.length === 0) return null;

  return (
    <span className="block text-muted" aria-label="Peers compared with the fleet">
      {checks.map((c, i) => (
        <span key={c.layer} className={c.peers < c.needs ? 'text-danger' : undefined}>
          {i > 0 && ' · '}
          {c.layer} {c.peers} peers, fleet median {c.median}, needs {c.needs}
        </span>
      ))}
    </span>
  );
}

/**
 * baselineFacts sums up the node of a lease's baseline in short parts: each
 * client (reachable, syncing, head and peers), the unhealthy probes, the
 * validators that attested, the containers that were not running or had
 * restarted, and a client below the fleet's peer count.
 */
export function baselineFacts(b: LeaseBaseline): string[] {
  const out: string[] = [];
  const { el, cl } = b.node;

  if (el) {
    if (!el.reachable) out.push('EL not reachable');
    else out.push(`EL ${el.syncing ? 'syncing, ' : ''}head ${el.head}, ${el.peers} peers`);
  }
  if (cl) {
    if (!cl.reachable) out.push('CL not reachable');
    else if (cl.elOffline) out.push('CL reports its EL offline');
    else out.push(`CL ${cl.syncing ? 'syncing, ' : ''}slot ${cl.headSlot}, ${cl.peers} peers`);
  }
  for (const p of list(b.node.probes)) {
    if (!p.healthy) out.push(`${p.kind} not healthy`);
  }
  if (b.duty > 0 && b.liveKnown) out.push(`validators ${b.live} of ${b.duty} attesting`);
  for (const c of list(b.containers)) {
    if (c.state === 'running' && c.restartCount === 0) continue;
    out.push(`${c.name} ${c.state}${c.restartCount > 0 ? `, ${c.restartCount} restarts` : ''}`);
  }
  if (b.belowFleet) out.push(b.belowFleet);

  return out;
}

/** BaselineState says whether the host was ready before its lease; for one that was not, why and how its node was. */
export function BaselineState({ baseline: b }: { baseline: LeaseBaseline }) {
  if (!b.unready) return <span>ready</span>;

  const facts = baselineFacts(b);
  return (
    <span>
      <span className="text-warning">not ready — {b.unready}</span>
      {b.root && <span className="text-muted"> (root {b.root})</span>}
      {facts.length > 0 && <span className="text-muted"> · {facts.join(' · ')}</span>}
    </span>
  );
}

/** BeforeLease is the line under a lease's readiness that says how the host was when the lease was granted. */
export function BeforeLease({ lease }: { lease: Pick<Lease, 'baseline'> }) {
  if (!lease.baseline) return null;

  return (
    <span className="block text-muted" aria-label="Before the lease">
      Before the lease: <BaselineState baseline={lease.baseline} />
    </span>
  );
}

/**
 * LeaseTable lists leases with the agent's phase, the per-need steps and the
 * controller's readiness note. `closed` switches the time columns to the
 * history form (ended, duration). With `onEnd` every open lease gets a
 * button to end it. An open lease that a person has ended already says so
 * ("ending (cancelled by <actor>)") and its button is disabled: it stays open
 * until the agent reports or the lease expires.
 */
export function LeaseTable({
  leases,
  label,
  showHost = true,
  closed = false,
  onEnd,
}: {
  leases: readonly Lease[];
  label: string;
  showHost?: boolean;
  closed?: boolean;
  onEnd?: (lease: Lease) => void;
}) {
  return (
    <Table label={label}>
      <thead>
        <tr>
          <Th>Lease</Th>
          {showHost && <Th>Host</Th>}
          <Th>State</Th>
          <Th>Phase</Th>
          <Th>Steps</Th>
          <Th>{closed ? 'Outcome' : 'Readiness'}</Th>
          <Th title="Validator weight at grant time / the part that was live, i.e. what the grant cost the budget">
            Weight
          </Th>
          <Th>Granted by</Th>
          <Th>Granted</Th>
          <Th>{closed ? 'Ended' : 'Expires'}</Th>
          {closed && <Th>Took</Th>}
          {onEnd && <Th>Actions</Th>}
        </tr>
      </thead>
      <tbody>
        {leases.map(l => {
          const needs = list(l.needs);
          const steps = list(l.operation?.steps);
          const took = secondsBetween(l.grantedAt, l.endedAt);
          const ending = l.cancelled !== undefined && ['granted', 'running', 'verifying'].includes(l.state);

          return (
            <tr key={l.id} className="hover:bg-surface-hover">
              <Td>
                <ShortId value={l.id} label="lease" />
              </Td>
              {showHost && <Td className="font-medium">{l.host}</Td>}
              <Td className="min-w-28">
                <span className="inline-flex items-center gap-1">
                  <LeaseStateBadge state={l.state} />
                  <LeaseOutcomeBadge outcome={l.outcome} />
                  {l.force && <Badge tone="warning">force</Badge>}
                  {ending && <span className="text-muted">ending (cancelled by {l.cancelled?.by})</span>}
                </span>
              </Td>
              {/* The phase, the note and the times change while a lease runs: their columns keep their width. */}
              <Td className="min-w-24">
                {l.operation ? <span className="font-mono">{l.operation.phase}</span> : <Dash />}
              </Td>
              <Td className="min-w-48">
                <span className="inline-flex flex-wrap gap-1">
                  {steps.length > 0
                    ? steps.map(s => {
                        const need = needs.find(n => n.id === s.needId);
                        return (
                          <span key={s.needId} className="inline-flex items-center gap-1">
                            <span className="font-mono text-muted">{stepLabel(needs, s.needId)}</span>
                            <StepBadge
                              status={s.status}
                              title={[need ? needSummary(need) : s.needId, s.error].filter(Boolean).join('\n')}
                            />
                            {s.action && <RunExit run={s.action} />}
                          </span>
                        );
                      })
                    : needs.map(n => (
                        <span key={n.id} className="font-mono text-muted" title={needSummary(n)}>
                          {stepLabel(needs, n.id)}
                        </span>
                      ))}
                </span>
                {steps.map(
                  s =>
                    s.action && (
                      <RunOutput key={s.needId} run={s.action} label={`${stepLabel(needs, s.needId)} on ${l.host}`} />
                    )
                )}
              </Td>
              {/* The one column that takes the room that is left: the others stay as wide as their content. */}
              <Td className="w-full min-w-72 whitespace-normal">
                {l.error ? (
                  <span className="text-danger">{l.error}</span>
                ) : l.operation?.error ? (
                  <span className="text-danger">{l.operation.error}</span>
                ) : l.readiness ? (
                  <Readiness lease={l} />
                ) : l.operation?.phase === 'pulling' ? (
                  <PullNote operation={l.operation} />
                ) : (
                  <Dash />
                )}
                <PeerChecks checks={l.peerChecks} />
                <BeforeLease lease={l} />
              </Td>
              <Td className="font-mono" title={`weight ${l.weight}, charged ${l.charged}`}>
                {l.charged}/{l.weight}
              </Td>
              <Td>{l.grantedBy}</Td>
              <Td>
                <RelTime fixed value={l.grantedAt} />
              </Td>
              <Td>
                <RelTime fixed value={closed ? l.endedAt : l.expiresAt} />
              </Td>
              {closed && <Td>{took === null ? <Dash /> : formatDuration(took)}</Td>}
              {onEnd && (
                <Td>
                  <ActionIcon
                    icon="abort"
                    tone="danger"
                    label={`End the lease of ${l.host}`}
                    blocked={
                      ending ? 'This lease is ending: it stays open until the agent reports or the lease runs out' : ''
                    }
                    intent={endLeaseIntent(l)}
                    onAction={() => {
                      onEnd(l);
                    }}
                  />
                </Td>
              )}
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}
