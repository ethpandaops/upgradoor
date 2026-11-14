import { list, type Gate } from '../api/types';
import { percentOf } from '../lib/format';
import { RelTime } from './RelTime';
import { Badge } from './primitives';

/**
 * GateIndicator shows the network gate: ok or failing with every reason,
 * participation and finality lag. It uses public chain data only. When the
 * beacon node does not serve liveness it says what the scheduler uses instead.
 */
export function GateIndicator({ gate }: { gate: Gate }) {
  const n = gate.network;
  const reasons = list(gate.reasons);

  return (
    <div
      aria-label="Network gate"
      className="flex flex-wrap items-center gap-x-4 gap-y-1 rounded-sm border border-border bg-surface px-2 py-1 text-xs/5"
    >
      <span className="inline-flex items-center gap-1.5">
        <span className="font-semibold text-muted uppercase">Network gate</span>
        {gate.ok ? <Badge tone="success">ok</Badge> : <Badge tone="danger">failing</Badge>}
      </span>

      {!gate.ok && (
        <span className="font-medium whitespace-normal text-danger">
          {reasons.length > 0 ? reasons.join(' · ') : 'no reason given'}
        </span>
      )}

      <Fact label="participation">
        {n.livenessKnown ? (
          <span title={`${n.live} of ${n.tracked} tracked validators attested in epoch ${n.livenessEpoch}`}>
            {percentOf(n.live, n.tracked)}{' '}
            <span className="text-muted">
              ({n.live}/{n.tracked})
            </span>
          </span>
        ) : (
          <span
            className="text-warning"
            title={
              n.livenessUnsupported
                ? 'The beacon node does not serve the liveness endpoint'
                : 'No liveness data (yet): the gate stays closed until there is'
            }
          >
            {n.livenessUnsupported ? 'not served' : 'unknown'}
          </span>
        )}
      </Fact>
      <Fact label="finality lag">
        <span title={`current epoch ${n.currentEpoch}, finalized epoch ${n.finalizedEpoch}`}>
          {n.finalityLagEpochs} epochs
        </span>
      </Fact>
      <Fact label="epoch">{n.currentEpoch}</Fact>
      <Fact label="head slot">{n.headSlot}</Fact>
      <Fact label="beacon">
        <RelTime value={n.updatedAt} />
      </Fact>
      {n.livenessUnsupported && (
        <span className="whitespace-normal text-warning">
          liveness not served by the beacon node: readiness and budget use agent probes
        </span>
      )}
      {n.error && (
        <span className="whitespace-normal text-warning" title={n.error}>
          beacon error: {n.error}
        </span>
      )}
    </div>
  );
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-baseline gap-1">
      <span className="text-muted">{label}</span>
      <span className="font-mono">{children}</span>
    </span>
  );
}
