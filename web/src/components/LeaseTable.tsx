import { list, type Lease, type Need } from '../api/types';
import { formatDuration, secondsBetween } from '../lib/format';
import { ShortId } from './Digest';
import { RelTime } from './RelTime';
import { Badge, Dash, Table, Td, Th } from './primitives';
import { LeaseStateBadge, StepBadge, needSummary } from './status';

function stepLabel(needs: readonly Need[], needId: string): string {
  const need = needs.find(n => n.id === needId);
  if (!need) return needId;
  if (need.kind === 'image') return need.image?.container ?? 'image';
  return need.kind;
}

/**
 * LeaseTable lists leases with the agent's phase, the per-need steps and the
 * controller's readiness note. `closed` switches the time columns to the
 * history form (ended, duration).
 */
export function LeaseTable({
  leases,
  label,
  showHost = true,
  closed = false,
}: {
  leases: readonly Lease[];
  label: string;
  showHost?: boolean;
  closed?: boolean;
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
        </tr>
      </thead>
      <tbody>
        {leases.map(l => {
          const needs = list(l.needs);
          const steps = list(l.operation?.steps);
          const took = secondsBetween(l.grantedAt, l.endedAt);

          return (
            <tr key={l.id} className="hover:bg-surface-hover">
              <Td>
                <ShortId value={l.id} label="lease" />
              </Td>
              {showHost && <Td className="font-medium">{l.host}</Td>}
              <Td>
                <span className="inline-flex items-center gap-1">
                  <LeaseStateBadge state={l.state} />
                  {l.force && <Badge tone="warning">force</Badge>}
                </span>
              </Td>
              <Td>{l.operation ? <span className="font-mono">{l.operation.phase}</span> : <Dash />}</Td>
              <Td>
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
                          </span>
                        );
                      })
                    : needs.map(n => (
                        <span key={n.id} className="font-mono text-muted" title={needSummary(n)}>
                          {stepLabel(needs, n.id)}
                        </span>
                      ))}
                </span>
              </Td>
              <Td className="max-w-sm whitespace-normal">
                {l.error ? (
                  <span className="text-danger">{l.error}</span>
                ) : l.operation?.error ? (
                  <span className="text-danger">{l.operation.error}</span>
                ) : l.readiness ? (
                  l.readiness
                ) : (
                  <Dash />
                )}
              </Td>
              <Td className="font-mono" title={`weight ${l.weight}, charged ${l.charged}`}>
                {l.charged}/{l.weight}
              </Td>
              <Td>{l.grantedBy}</Td>
              <Td>
                <RelTime value={l.grantedAt} />
              </Td>
              <Td>
                <RelTime value={closed ? l.endedAt : l.expiresAt} />
              </Td>
              {closed && <Td>{took === null ? <Dash /> : formatDuration(took)}</Td>}
            </tr>
          );
        })}
      </tbody>
    </Table>
  );
}
