import { useMemo, useState, type PointerEvent } from 'react';

import { list, type ParticipationPoint, type PublicHistory } from '../api/types';
import { useWidth } from '../hooks/useWidth';
import { formatDuration, formatPercent } from '../lib/format';
import type { BarState, TimelineBar, TimelineModel, WorkKind } from '../lib/timeline';
import { cx } from './primitives';

const KIND_LABEL: Record<WorkKind, string> = {
  image: 'Image roll',
  packages: 'Package upgrade',
  reboot: 'Reboot',
};

const KIND_FILL: Record<WorkKind, string> = {
  image: 'fill-series-image',
  packages: 'fill-series-packages',
  reboot: 'fill-series-reboot',
};

const KIND_STROKE: Record<WorkKind, string> = {
  image: 'stroke-series-image',
  packages: 'stroke-series-packages',
  reboot: 'stroke-series-reboot',
};

const KIND_BG: Record<WorkKind, string> = {
  image: 'bg-series-image',
  packages: 'bg-series-packages',
  reboot: 'bg-series-reboot',
};

const STATE_LABEL: Record<BarState, string> = {
  done: 'finished',
  failed: 'failed',
  active: 'in progress',
  planned: 'planned (estimate)',
};

const KINDS: readonly WorkKind[] = ['image', 'packages', 'reboot'];

// Geometry, in CSS pixels.
const GUTTER = 172;
// Without lanes the gutter only holds the percent ticks.
const PUBLIC_GUTTER = 48;
const PAD_RIGHT = 14;
const CHART_TOP = 22;
const CHART_HEIGHT = 112;
const LANES_GAP = 20;
const ROW = 22;
const BAR = 12;
const AXIS = 22;
const MAX_ROWS = 14;

const TICK_STEPS_MIN = [1, 2, 5, 10, 15, 30, 60, 120, 180, 360, 720];

function clock(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

function timeTicks(from: number, to: number, width: number): number[] {
  const want = Math.max(2, Math.floor(width / 90));
  const step = (TICK_STEPS_MIN.find(m => (to - from) / (m * 60_000) <= want) ?? 720) * 60_000;
  // Align to the local clock, so ticks sit on round times.
  const offset = new Date(from).getTimezoneOffset() * 60_000;
  const first = Math.ceil((from - offset) / step) * step + offset;
  const ticks: number[] = [];
  for (let t = first; t <= to; t += step) ticks.push(t);
  return ticks;
}

function percentTicks(min: number): number[] {
  const span = 100 - min;
  const step = span <= 10 ? 2.5 : span <= 20 ? 5 : span <= 50 ? 10 : 25;
  const ticks: number[] = [];
  for (let v = 100; v >= min - 1e-9; v -= step) ticks.push(v);
  return ticks;
}

function share(p: ParticipationPoint): number {
  return p.total > 0 ? (p.live / p.total) * 100 : 100;
}

/** pickRows keeps the rows that matter most when there are too many: running, then planned, then the latest past. */
function pickRows(model: TimelineModel, from: number, to: number) {
  const visible = model.rows
    .map(r => ({ host: r.host, bars: r.bars.filter(b => b.end >= from && b.start <= to) }))
    .filter(r => r.bars.length > 0);

  if (visible.length <= MAX_ROWS) return { rows: visible, hidden: 0 };

  const rank = (bars: readonly TimelineBar[]) => {
    if (bars.some(b => b.state === 'active')) return 0;
    const planned = bars.find(b => b.state === 'planned');
    if (planned) return 1 + planned.start / 1e14;
    return 3 - (bars[bars.length - 1] as TimelineBar).end / 1e14;
  };
  const keep = new Set(
    [...visible]
      .sort((a, b) => rank(a.bars) - rank(b.bars))
      .slice(0, MAX_ROWS)
      .map(r => r.host)
  );

  return { rows: visible.filter(r => keep.has(r.host)), hidden: visible.length - keep.size };
}

interface Hover {
  x: number;
  y: number;
  time: number;
  point?: ParticipationPoint;
  bar?: TimelineBar;
}

/**
 * Timeline draws two panels over one time axis: the network's attestation
 * participation per epoch against the gate's threshold, and below it one lane
 * per host with its leases: finished, running and (right of "now") planned.
 * Without a model (logged out) only the participation panel is drawn.
 */
export function Timeline({
  history,
  model,
  now,
  rangeMs,
  futureMs,
  emptyNote = 'No participation data in this window yet',
}: {
  history: PublicHistory | undefined;
  model: TimelineModel | undefined;
  now: number;
  rangeMs: number;
  futureMs: number;
  /** emptyNote is shown in the participation panel when it has no point to draw. */
  emptyNote?: string;
}) {
  const [ref, width] = useWidth<HTMLDivElement>(960);
  const [hover, setHover] = useState<Hover | null>(null);

  const from = now - rangeMs;
  const to = now + (model ? futureMs : 0);
  const plotLeft = model ? GUTTER : PUBLIC_GUTTER;
  const plotRight = Math.max(plotLeft + 80, width - PAD_RIGHT);
  const x = (t: number) => plotLeft + ((t - from) / (to - from)) * (plotRight - plotLeft);

  const points = useMemo(
    () =>
      list(history?.epochs)
        .map(p => ({ p, t: Date.parse(p.time) }))
        .filter(d => Number.isFinite(d.t) && d.t >= from && d.t <= now),
    [history, from, now]
  );
  const threshold = history?.minParticipationPercent;

  const lowest = Math.min(threshold ?? 100, ...points.map(d => share(d.p)));
  const yMin = Math.max(0, Math.floor((lowest - 2) / 5) * 5);
  const y = (v: number) => CHART_TOP + ((100 - v) / (100 - yMin || 1)) * CHART_HEIGHT;
  const chartBottom = CHART_TOP + CHART_HEIGHT;

  const picked = useMemo(() => (model ? pickRows(model, from, to) : undefined), [model, from, to]);
  const rows = picked?.rows ?? [];
  const lanesTop = chartBottom + LANES_GAP;
  const lanesHeight = model ? Math.max(rows.length, 1) * ROW : 0;
  const axisTop = model ? lanesTop + lanesHeight : chartBottom;
  const height = axisTop + AXIS;

  const line = points.map((d, i) => `${i === 0 ? 'M' : 'L'}${x(d.t).toFixed(1)},${y(share(d.p)).toFixed(1)}`).join(' ');
  const first = points[0];
  const last = points[points.length - 1];
  const area =
    first && last ? `${line} L${x(last.t).toFixed(1)},${chartBottom} L${x(first.t).toFixed(1)},${chartBottom} Z` : '';

  const onMove = (e: PointerEvent<SVGSVGElement>) => {
    const box = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - box.left;
    const py = e.clientY - box.top;
    if (px < plotLeft || px > plotRight) {
      setHover(null);
      return;
    }

    const time = from + ((px - plotLeft) / (plotRight - plotLeft)) * (to - from);
    let point: ParticipationPoint | undefined;
    let best = Infinity;
    for (const d of points) {
      const dist = Math.abs(d.t - time);
      if (dist < best) {
        best = dist;
        point = d.p;
      }
    }
    // A point further away than one epoch and a half is not "at" the pointer.
    if (history && best > history.epochSeconds * 1500) point = undefined;

    let bar: TimelineBar | undefined;
    const row = rows[Math.floor((py - lanesTop) / ROW)];
    if (row && py >= lanesTop) {
      const slack = ((to - from) / (plotRight - plotLeft)) * 4;
      bar = row.bars.find(b => time >= b.start - slack && time <= b.end + slack);
    }

    setHover({ x: px, y: py, time, point, bar });
  };

  const summary = last
    ? `Participation ${formatPercent(share(last.p))} in epoch ${last.p.epoch}` +
      (model ? `; ${rows.length} hosts with work in the window` : '')
    : 'No participation data yet';

  return (
    <div ref={ref} className="relative">
      <svg
        role="img"
        aria-label={summary}
        width={width}
        height={height}
        className="block touch-pan-y select-none"
        onPointerMove={onMove}
        onPointerLeave={() => {
          setHover(null);
        }}
      >
        {/* The future: where the bars are an estimate. */}
        {model && (
          <rect
            x={x(now)}
            y={CHART_TOP - 8}
            width={Math.max(0, plotRight - x(now))}
            height={axisTop - CHART_TOP + 8}
            className="fill-surface-raised"
          />
        )}

        {/* Participation panel */}
        {(!model || x(now) - plotLeft > 190) && (
          <text x={plotLeft} y={10} className="fill-muted text-xs font-medium">
            Attestation participation
          </text>
        )}
        {percentTicks(yMin).map(v => (
          <g key={v}>
            <line x1={plotLeft} x2={plotRight} y1={y(v)} y2={y(v)} className="stroke-border" />
            <text x={plotLeft - 8} y={y(v) + 4} textAnchor="end" className="fill-faint text-xs tabular-nums">
              {formatPercent(v)}
            </text>
          </g>
        ))}
        {threshold !== undefined && points.length > 0 && (
          <g>
            <line x1={plotLeft} x2={x(now)} y1={y(threshold)} y2={y(threshold)} className="stroke-danger" />
            <title>{`Gate threshold ${formatPercent(threshold)}`}</title>
          </g>
        )}
        {area && <path d={area} className="fill-accent/10" />}
        {line && (
          <path
            d={line}
            fill="none"
            strokeWidth={2}
            strokeLinejoin="round"
            strokeLinecap="round"
            className="stroke-accent"
          />
        )}
        {last && (
          <>
            <circle cx={x(last.t)} cy={y(share(last.p))} r={4} strokeWidth={2} className="fill-accent stroke-surface" />
            <text
              x={model ? x(last.t) + 9 : x(last.t)}
              y={model ? y(share(last.p)) + 4 : y(share(last.p)) + (y(share(last.p)) - CHART_TOP < 18 ? 18 : -9)}
              textAnchor={model ? 'start' : 'end'}
              className="fill-foreground text-xs font-semibold tabular-nums"
            >
              {formatPercent(share(last.p))}
            </text>
          </>
        )}
        {points.length === 0 && (
          <text
            x={(plotLeft + plotRight) / 2}
            y={CHART_TOP + CHART_HEIGHT / 2}
            textAnchor="middle"
            className="fill-muted text-xs"
          >
            {emptyNote}
          </text>
        )}

        {/* Lanes */}
        {model && (
          <g>
            <text x={plotLeft} y={lanesTop - 6} className="fill-muted text-xs font-medium">
              Host outages
            </text>
            <text x={plotRight} y={lanesTop - 6} textAnchor="end" className="fill-faint text-xs">
              planned →
            </text>
            {rows.length === 0 && (
              <text x={plotLeft + 4} y={lanesTop + ROW / 2 + 4} className="fill-muted text-xs">
                No host was taken down in this window and none is planned.
              </text>
            )}
            {rows.map((row, i) => {
              const top = lanesTop + i * ROW;
              return (
                <g key={row.host}>
                  <line x1={plotLeft} x2={plotRight} y1={top + ROW} y2={top + ROW} className="stroke-border" />
                  <text
                    x={plotLeft - 8}
                    y={top + ROW / 2 + 4}
                    textAnchor="end"
                    className={cx('text-xs', hover?.bar?.host === row.host ? 'fill-foreground' : 'fill-muted')}
                  >
                    {row.host.length > 25 ? `${row.host.slice(0, 24)}…` : row.host}
                  </text>
                  {row.bars.map(b => {
                    const x0 = Math.max(plotLeft, x(b.start));
                    const x1 = Math.min(plotRight, x(b.end));
                    const w = Math.max(3, x1 - x0);
                    const hovered = hover?.bar === b;
                    const key = `${b.state}/${b.start}`;
                    const yBar = top + (ROW - BAR) / 2;
                    const name = `${row.host}: ${KIND_LABEL[b.kind]}, ${STATE_LABEL[b.state]}, ${clock(b.start)} to ${
                      b.state === 'active' ? 'now' : clock(b.end)
                    }`;
                    // The label of a failed bar needs room before the next bar (a retry).
                    const following = row.bars[row.bars.indexOf(b) + 1];
                    const room = (following ? x(following.start) : plotRight) - (x0 + w);

                    if (b.state === 'planned') {
                      return (
                        <rect
                          key={key}
                          x={x0 + 0.75}
                          y={yBar + 0.75}
                          width={Math.max(2, w - 1.5)}
                          height={BAR - 1.5}
                          rx={3}
                          strokeWidth={1.5}
                          className={cx(KIND_FILL[b.kind], KIND_STROKE[b.kind])}
                          fillOpacity={hovered ? 0.4 : 0.15}
                        >
                          <title>{name}</title>
                        </rect>
                      );
                    }

                    return (
                      <g key={key}>
                        <rect
                          x={x0}
                          y={yBar}
                          width={w}
                          height={BAR}
                          rx={3}
                          opacity={b.state === 'done' && !hovered ? 0.75 : 1}
                          className={b.state === 'failed' ? 'fill-danger-solid' : KIND_FILL[b.kind]}
                        >
                          <title>{name}</title>
                        </rect>
                        {b.state === 'failed' && room >= 64 && (
                          <text x={x0 + w + 4} y={yBar + BAR - 2} className="fill-danger text-xs font-semibold">
                            ✕ failed
                          </text>
                        )}
                        {b.state === 'active' && (
                          <circle
                            cx={x0 + w}
                            cy={yBar + BAR / 2}
                            r={4}
                            strokeWidth={2}
                            className={cx(KIND_FILL[b.kind], 'animate-pulse stroke-surface')}
                          />
                        )}
                      </g>
                    );
                  })}
                </g>
              );
            })}
          </g>
        )}

        {/* Now */}
        {model && (
          <g>
            <line x1={x(now)} x2={x(now)} y1={CHART_TOP - 8} y2={axisTop} className="stroke-border-strong" />
            <text x={x(now)} y={10} textAnchor="middle" className="fill-foreground text-xs font-semibold">
              now
            </text>
          </g>
        )}

        {/* Time axis */}
        <line x1={plotLeft} x2={plotRight} y1={axisTop} y2={axisTop} className="stroke-border-strong" />
        {timeTicks(from, to, plotRight - plotLeft).map(t => (
          <g key={t}>
            <line x1={x(t)} x2={x(t)} y1={axisTop} y2={axisTop + 4} className="stroke-border-strong" />
            <text x={x(t)} y={axisTop + 16} textAnchor="middle" className="fill-faint text-xs tabular-nums">
              {clock(t)}
            </text>
          </g>
        ))}

        {/* Crosshair */}
        {hover && (
          <g pointerEvents="none">
            <line x1={hover.x} x2={hover.x} y1={CHART_TOP - 8} y2={axisTop} className="stroke-muted" />
            {hover.point && (
              <circle
                cx={x(Date.parse(hover.point.time))}
                cy={y(share(hover.point))}
                r={4}
                strokeWidth={2}
                className="fill-accent stroke-surface"
              />
            )}
          </g>
        )}
      </svg>

      {picked !== undefined && picked.hidden > 0 && (
        <p className="pl-43 text-xs/5 text-muted">
          {picked.hidden} more {picked.hidden === 1 ? 'host' : 'hosts'} not shown.
        </p>
      )}

      {hover && (hover.point || hover.bar) && (
        <div
          role="tooltip"
          className="pointer-events-none absolute z-10 flex w-60 flex-col gap-1 rounded-md border border-border-strong bg-surface p-2 text-xs/5 shadow-lg"
          style={{
            left: Math.max(0, hover.x + 256 > width ? hover.x - 252 : hover.x + 12),
            top: Math.max(0, Math.min(hover.y - 12, height - 96)),
          }}
        >
          <span className="text-muted">
            {clock(hover.time)}
            {hover.time > now && ` · in ${formatDuration((hover.time - now) / 1000)}`}
          </span>
          {hover.point && (
            <span className="flex items-baseline gap-1.5">
              <span aria-hidden="true" className="h-0.5 w-3 self-center rounded-full bg-accent" />
              <span className="font-semibold">{formatPercent(share(hover.point))}</span>
              <span className="text-muted">
                attesting, epoch {hover.point.epoch} ({hover.point.live}/{hover.point.total})
              </span>
            </span>
          )}
          {hover.bar && (
            <span className="flex flex-col border-t border-border pt-1">
              <span className="flex items-center gap-1.5">
                <span
                  aria-hidden="true"
                  className={cx(
                    'size-2 rounded-xs',
                    hover.bar.state === 'failed' ? 'bg-danger-solid' : KIND_BG[hover.bar.kind]
                  )}
                />
                <span className="font-semibold">{hover.bar.host}</span>
              </span>
              <span>
                {KIND_LABEL[hover.bar.kind]}, {STATE_LABEL[hover.bar.state]}
              </span>
              <span className="text-muted">
                {hover.bar.state === 'planned'
                  ? `starts about ${clock(hover.bar.start)} · now: ${hover.bar.detail}`
                  : `${clock(hover.bar.start)} – ${hover.bar.state === 'active' ? 'now' : clock(hover.bar.end)} (${formatDuration((hover.bar.end - hover.bar.start) / 1000)})`}
              </span>
              {hover.bar.state !== 'planned' && (
                <span className="whitespace-normal text-muted">{hover.bar.detail}</span>
              )}
            </span>
          )}
        </div>
      )}
    </div>
  );
}

/**
 * TimelineLegend keys the chart: the gate's threshold and, with lanes, their
 * colours (what is done to a host) and bar styles (when).
 */
export function TimelineLegend({ lanes, threshold }: { lanes: boolean; threshold: number | undefined }) {
  return (
    <ul aria-label="Legend" className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs/5 text-muted">
      {threshold !== undefined && (
        <li
          className="inline-flex items-center gap-1.5"
          title="The gate closes when participation falls below this line. The gate leaves the validators of hosts that are being updated out of its count, so a planned outage can dip the line below the threshold without closing it."
        >
          <span aria-hidden="true" className="h-px w-4 bg-danger" />
          Gate threshold {formatPercent(threshold)}, not counting hosts being updated
        </li>
      )}
      {lanes &&
        KINDS.map(k => (
          <li key={k} className="inline-flex items-center gap-1.5">
            <span aria-hidden="true" className={cx('h-2.5 w-4 rounded-xs', KIND_BG[k])} />
            {KIND_LABEL[k]}
          </li>
        ))}
      {lanes && (
        <>
          <li className="inline-flex items-center gap-1.5">
            <span aria-hidden="true" className="h-2.5 w-4 rounded-xs border-2 border-muted" />
            Planned (estimate)
          </li>
          <li className="inline-flex items-center gap-1.5">
            <span aria-hidden="true" className="h-2.5 w-4 rounded-xs bg-danger-solid" />
            Failed
          </li>
        </>
      )}
    </ul>
  );
}
