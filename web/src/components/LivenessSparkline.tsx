import type { Liveness } from '../api/types';
import { percentOf } from '../lib/format';

const BAR_WIDTH = 6;
const BAR_GAP = 2;
const HEIGHT = 24;

function barTone(live: number, total: number): string {
  if (total === 0) return 'fill-border-strong';
  const share = live / total;
  if (share >= 0.9) return 'fill-success';
  if (share > 0) return 'fill-warning';
  return 'fill-danger';
}

/**
 * LivenessSparkline draws one bar per epoch, oldest on the left. The height
 * is the share of the host's validators that attested; an epoch with nothing
 * live shows a short red stub so it stays visible.
 */
export function LivenessSparkline({ rows }: { rows: readonly Liveness[] }) {
  if (rows.length === 0) return <span className="text-faint">no liveness data</span>;

  const ordered = [...rows].sort((a, b) => a.epoch - b.epoch);
  const first = ordered[0] as Liveness;
  const last = ordered[ordered.length - 1] as Liveness;
  const width = ordered.length * (BAR_WIDTH + BAR_GAP) - BAR_GAP;

  return (
    <svg
      role="img"
      aria-label={`Attestation liveness, epochs ${first.epoch} to ${last.epoch}; latest ${last.live} of ${last.total}`}
      width={width}
      height={HEIGHT}
      viewBox={`0 0 ${width} ${HEIGHT}`}
      className="shrink-0"
    >
      {ordered.map((row, i) => {
        const share = row.total > 0 ? row.live / row.total : 0;
        const h = Math.max(2, Math.round(share * HEIGHT));
        return (
          <rect
            key={row.epoch}
            x={i * (BAR_WIDTH + BAR_GAP)}
            y={HEIGHT - h}
            width={BAR_WIDTH}
            height={h}
            className={barTone(row.live, row.total)}
          >
            <title>{`epoch ${row.epoch}: ${row.live}/${row.total} (${percentOf(row.live, row.total)})`}</title>
          </rect>
        );
      })}
    </svg>
  );
}
