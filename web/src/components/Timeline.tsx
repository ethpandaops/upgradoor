import { useMemo, useState, type PointerEvent } from 'react';

import { list, type Outage, type SlotHistory } from '../api/types';
import { useWidth } from '../hooks/useWidth';
import { formatDuration, formatPercent, percentOf, plural } from '../lib/format';
import {
  buildSlotWindow,
  packOutages,
  slotWindowEnd,
  type OutageBlock,
  type OutageGroup,
  type SlotColumn,
  type SlotWindow,
} from '../lib/slotHistory';
import { cx } from './primitives';
import { WORK_LABEL } from './status';

const WORK_FILL: Record<string, string> = {
  image: 'fill-series-image',
  packages: 'fill-series-packages',
  reboot: 'fill-series-reboot',
  mixed: 'fill-series-image',
};

// Geometry, in CSS pixels. Nothing here depends on the data or on the size
// of the fleet: the graph keeps its size while slots arrive and however many
// hosts there are.
const GUTTER = 72;
const GUTTER_PUBLIC = 52;
const PAD_RIGHT = 8;
const CHART_TOP = 8;
const CHART_HEIGHT = 140;
const STRIPS_GAP = 10;
const ROW = 18;
const CELL = 10;
const AXIS = 22;
const LANE = 18;
const BLOCK = 14;
const BAND_GAP = 6;
/** The lanes of the two kinds of outage. They are lanes of blocks, not of hosts. */
const LANES: Record<OutageGroup, number> = { work: 3, off: 3 };
const LABEL_CHAR = 6.9;
const MAX_NAMES = 8;

const TICK_STEPS_MIN = [1, 2, 5, 10, 15, 30, 60, 120, 180, 360, 720];

function clock(ms: number, seconds = false): string {
  return new Date(ms).toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    second: seconds ? '2-digit' : undefined,
  });
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

/** rect is one rectangle of an SVG path. */
function rect(x: number, y: number, w: number, h: number): string {
  return `M${x.toFixed(2)},${y.toFixed(2)}h${w.toFixed(2)}v${h.toFixed(2)}h${(-w).toFixed(2)}z`;
}

/** The percent ticks of the participation scale, besides the gate's threshold. */
const PERCENT_TICKS = [100, 99, 95, 75, 50, 0];

/**
 * scaleY places a participation percentage between 0 (100%) and 1 (0%). The
 * scale is a square root of what is missing: it stretches the top, so a dip
 * of one percent is a tenth of the height and can be seen, while the outage
 * of a quarter of the fleet (half the height) still fits the same panel.
 */
function scaleY(percent: number): number {
  return Math.sqrt(Math.min(100, Math.max(0, 100 - percent)) / 100);
}

function slotsLabel(c: SlotColumn): string {
  return c.slots === 1 ? `Slot ${c.slot}` : `Slots ${c.slot}–${c.slot + c.slots - 1}`;
}

interface Hover {
  x: number;
  y: number;
  column: number;
  block?: OutageBlock;
}

function workLabel(block: OutageBlock): string {
  if (block.group === 'off') return 'Offline';
  const work = block.work;
  return work === 'image' || work === 'packages' || work === 'reboot' ? WORK_LABEL[work] : 'Update';
}

/** blockNames are the names a block can show, and how many hosts it stands for beyond them. */
function blockNames(block: OutageBlock): { names: string[]; more: number } {
  const names = [...new Set(block.outages.flatMap(o => list(o.names)))].sort((a, b) =>
    a.localeCompare(b, undefined, { numeric: true })
  );
  const shown = names.slice(0, MAX_NAMES);
  return { names: shown, more: Math.max(0, block.hosts - shown.length) };
}

/** blockText is what is written in a block: the longest of its forms that fits. */
function blockText(block: OutageBlock, width: number): string {
  const single = block.hosts === 1 ? (block.outages[0]?.names?.[0] ?? '') : '';
  const kind = workLabel(block).toLowerCase();
  const forms =
    block.hosts === 1
      ? [
          `${single} · ${block.failed ? `${kind} failed` : kind}`,
          block.failed ? `✕ ${single}` : single,
          block.failed ? '✕' : '',
        ]
      : block.group === 'off'
        ? [`${block.hosts} hosts offline`, `${block.hosts} offline`, String(block.hosts)]
        : [
            `${kind} · ${block.hosts} hosts${block.failed ? ' · some failed' : ''}`,
            `${kind} · ${block.hosts}`,
            `${block.hosts} hosts`,
            String(block.hosts),
          ];

  return forms.find(f => f.length * LABEL_CHAR + 8 <= width) ?? '';
}

function blockSummary(block: OutageBlock): string {
  const { names, more } = blockNames(block);
  const who = block.hosts === 1 ? (names[0] ?? 'a host') : plural(block.hosts, 'host');
  const what = block.group === 'off' ? 'offline' : `${workLabel(block).toLowerCase()}${block.failed ? ', failed' : ''}`;
  return `${who}: ${what}${block.hosts > 1 ? ` (${names.join(', ')}${more > 0 ? ` and ${more} more` : ''})` : ''}`;
}

/**
 * Timeline is the participation graph. One time axis, one column per slot
 * (or per bucket of slots in a long window), the newest at the right edge:
 *
 *   - on top, how many of the fleet's validators that had to attest in the
 *     slot got their attestation into a block, as a full-height column split
 *     into attested and missed, against the gate's threshold;
 *   - below it a strip for the blocks (a mark where a proposal was missed);
 *   - with outages (a login), what was off, as blocks on the same axis in
 *     two bands: hosts upgradoor was working on, and hosts that were offline
 *     without it. A block is a span of time, not a host: blocks are packed
 *     into a few lanes, and blocks of one kind that overlap more than fit
 *     are one block that counts its hosts. So the graph has one height for
 *     four hosts and for a thousand.
 */
export function Timeline({
  history,
  outages,
  now,
  rangeMs,
  emptyNote = 'No participation data in this window yet',
  onOpenHost,
}: {
  history: SlotHistory | undefined;
  /** outages are the spans during which hosts were off; undefined without a login, which hides the bands. */
  outages: readonly Outage[] | undefined;
  now: number;
  rangeMs: number;
  /** emptyNote is shown in the participation panel when it has nothing to draw. */
  emptyNote?: string;
  /** onOpenHost is called for a click on a block of one host. */
  onOpenHost?: (host: string) => void;
}) {
  const [ref, width] = useWidth<HTMLDivElement>(960);
  const [hover, setHover] = useState<Hover | null>(null);

  // The window moves on by whole columns: it is rebuilt when the clock
  // enters the next one or an answer arrives, not every second.
  const end = slotWindowEnd(history, now);
  const win = useMemo<SlotWindow | undefined>(
    () => (history && end !== undefined ? buildSlotWindow(history, end, rangeMs) : undefined),
    [history, end, rangeMs]
  );

  const bands = outages !== undefined;
  const plotLeft = bands ? GUTTER : GUTTER_PUBLIC;
  const plotRight = Math.max(plotLeft + 80, width - PAD_RIGHT);
  const plotWidth = plotRight - plotLeft;
  const count = win?.columns.length ?? 1;
  const colWidth = plotWidth / count;
  const startMs = win?.startMs ?? now - rangeMs;
  const endMs = win?.endMs ?? now;
  const x = (t: number) => plotLeft + ((t - startMs) / (endMs - startMs)) * plotWidth;

  const chartBottom = CHART_TOP + CHART_HEIGHT;
  const y = (percent: number) => CHART_TOP + scaleY(percent) * CHART_HEIGHT;
  const blocksTop = chartBottom + STRIPS_GAP;
  const workTop = blocksTop + ROW + BAND_GAP;
  const offTop = workTop + LANES.work * LANE + BAND_GAP;
  const bandTop = (group: OutageGroup) => (group === 'work' ? workTop : offTop);
  const axisTop = bands ? offTop + LANES.off * LANE + 4 : blocksTop + ROW + 4;
  const height = axisTop + AXIS;

  const layout = useMemo(
    () => (outages ? packOutages(outages, { startMs, endMs, width: plotWidth, lanes: LANES }) : undefined),
    [outages, startMs, endMs, plotWidth]
  );
  const blocks = layout?.blocks ?? [];

  // Everything drawn per column is a handful of paths, built when the data
  // or the width changes: a pointer that moves redraws the overlay only.
  const marks = useMemo(() => {
    const attested: string[] = [];
    const missed: string[] = [];
    const worst: string[] = [];
    const quiet: string[] = [];
    const noBlock: string[] = [];
    if (!win) return { attested: '', missed: '', worst: '', quiet: '', noBlock: '', any: false };

    const gap = colWidth >= 3 ? 1 : 0;
    const w = Math.max(colWidth - gap, 0.5);
    const cellTop = blocksTop + (ROW - CELL) / 2;
    let any = false;

    win.columns.forEach((c, i) => {
      const x0 = plotLeft + i * colWidth;

      if (c.recorded > 0) (c.missed > 0 ? noBlock : quiet).push(rect(x0, cellTop, w, CELL));
      if (c.recorded === 0 || c.assigned === 0) return;
      any = true;

      // From the top: missed on average, then down to the worst slot of the
      // column, then attested. A miss is at least two pixels high.
      let yMean = y((c.included / c.assigned) * 100);
      if (c.included < c.assigned) yMean = Math.max(yMean, CHART_TOP + 2);
      let yWorst = c.worstAssigned > 0 ? y((c.worstIncluded / c.worstAssigned) * 100) : yMean;
      yWorst = Math.min(Math.max(yWorst, yMean), chartBottom);

      if (yMean > CHART_TOP) missed.push(rect(x0, CHART_TOP, w, yMean - CHART_TOP));
      if (yWorst > yMean) worst.push(rect(x0, yMean, w, yWorst - yMean));
      if (chartBottom > yWorst) attested.push(rect(x0, yWorst, w, chartBottom - yWorst));
    });

    return {
      attested: attested.join(''),
      missed: missed.join(''),
      worst: worst.join(''),
      quiet: quiet.join(''),
      noBlock: noBlock.join(''),
      any,
    };
    // y and blocksTop are constants of the geometry.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [win, plotLeft, colWidth]);

  const threshold = history?.minParticipationPercent;
  const hoverColumn = hover ? win?.columns[hover.column] : undefined;

  const blockAt = (px: number, py: number): OutageBlock | undefined =>
    blocks.find(b => {
      const top = bandTop(b.group) + b.lane * LANE;
      return py >= top && py < top + LANE && px >= plotLeft + b.x - 2 && px <= plotLeft + b.x + b.width + 2;
    });

  const onMove = (e: PointerEvent<SVGSVGElement>) => {
    const box = e.currentTarget.getBoundingClientRect();
    const px = e.clientX - box.left;
    const py = e.clientY - box.top;
    if (!win || px < plotLeft || px >= plotRight || py > axisTop) {
      setHover(null);
      return;
    }

    const column = Math.min(count - 1, Math.floor((px - plotLeft) / colWidth));
    setHover({ x: px, y: py, column, block: blockAt(px, py) });
  };

  const latest = win?.latest;
  const summary = latest
    ? `Attestations: ${latest.included} of ${latest.assigned} included in ${slotsLabel(latest).toLowerCase()} (${percentOf(
        latest.included,
        latest.assigned
      )})` + (bands ? `; ${blocks.length === 0 ? 'nothing is off' : plural(blocks.length, 'outage block')}` : '')
    : 'No participation data yet';

  const offSlots = history?.offSlots ?? 0;
  const offHosts = history?.outageHosts ?? 0;

  return (
    // No width of its own: the graph takes the width it is given and never
    // widens what it stands in, at any window width.
    <div ref={ref} className="relative w-0 min-w-full">
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
        {/* Participation: percent of the attestations due in the slot. */}
        {PERCENT_TICKS.map(v => (
          <g key={v}>
            <line x1={plotLeft} x2={plotRight} y1={y(v)} y2={y(v)} className="stroke-border" />
            {/* The threshold's label takes the place of a tick it would sit on. */}
            {(threshold === undefined || Math.abs(y(threshold) - y(v)) > 12) && (
              <text x={plotLeft - 8} y={y(v) + 4} textAnchor="end" className="fill-faint text-xs tabular-nums">
                {formatPercent(v)}
              </text>
            )}
          </g>
        ))}
        <path d={marks.attested} className="fill-accent/30" />
        <path d={marks.worst} className="fill-danger-solid/40" />
        <path d={marks.missed} className="fill-danger-solid" />
        {threshold !== undefined && (
          <g>
            <line
              x1={plotLeft}
              x2={plotRight}
              y1={y(threshold)}
              y2={y(threshold)}
              strokeDasharray="5 3"
              className="stroke-foreground"
            />
            <text
              x={plotLeft - 8}
              y={y(threshold) + 4}
              textAnchor="end"
              className="fill-foreground text-xs font-semibold tabular-nums"
            >
              {formatPercent(threshold)}
            </text>
          </g>
        )}
        {!marks.any && (
          <text
            x={(plotLeft + plotRight) / 2}
            y={CHART_TOP + CHART_HEIGHT / 2}
            textAnchor="middle"
            className="fill-muted text-xs"
          >
            {emptyNote}
          </text>
        )}

        {/* Blocks: a mark where a proposal was missed. */}
        <text x={plotLeft - 8} y={blocksTop + ROW / 2 + 4} textAnchor="end" className="fill-muted text-xs">
          Blocks
        </text>
        <path d={marks.quiet} className="fill-accent/30" />
        <path d={marks.noBlock} className="fill-danger-solid" />

        {/* What was off: updates by upgradoor, and hosts that were offline without it. */}
        {bands && (
          <>
            {(['work', 'off'] as const).map(group => (
              <g key={group}>
                <rect
                  x={plotLeft}
                  y={bandTop(group)}
                  width={plotWidth}
                  height={LANES[group] * LANE}
                  rx={3}
                  className="fill-surface-raised/60"
                />
                <text
                  x={plotLeft - 8}
                  y={bandTop(group) + (LANES[group] * LANE) / 2 + 4}
                  textAnchor="end"
                  className="fill-muted text-xs"
                >
                  {group === 'work' ? 'Updates' : 'Offline'}
                </text>
              </g>
            ))}
            {/* A band without a block is good news, and says so. */}
            {win &&
              (['work', 'off'] as const)
                .filter(group => !blocks.some(b => b.group === group))
                .map(group => (
                  <text
                    key={group}
                    x={(plotLeft + plotRight) / 2}
                    y={bandTop(group) + (LANES[group] * LANE) / 2 + 4}
                    textAnchor="middle"
                    className="fill-success text-xs font-medium"
                  >
                    {group === 'work'
                      ? '✓ Nothing is off: no host was updated in this window'
                      : '✓ Nothing is off: no host was offline in this window'}
                  </text>
                ))}
            {blocks.map(b => {
              const top = bandTop(b.group) + b.lane * LANE + (LANE - BLOCK) / 2;
              const left = plotLeft + b.x;
              const text = blockText(b, b.width);
              const single = b.hosts === 1 ? b.outages[0]?.names?.[0] : undefined;
              const open = single !== undefined && onOpenHost !== undefined;
              const fill =
                b.group === 'off'
                  ? 'fill-foreground/55'
                  : b.failed
                    ? 'fill-danger-solid'
                    : WORK_FILL[b.work ?? 'mixed'];

              return (
                <g
                  key={`${b.group}/${b.lane}/${b.x.toFixed(1)}`}
                  role={open ? 'button' : 'img'}
                  tabIndex={0}
                  aria-label={blockSummary(b)}
                  className={cx('outline-hidden', open && 'cursor-pointer')}
                  onFocus={() => {
                    const column = Math.min(count - 1, Math.max(0, Math.floor((b.x + b.width / 2) / colWidth)));
                    setHover({ x: left + b.width / 2, y: top, column, block: b });
                  }}
                  onBlur={() => {
                    setHover(null);
                  }}
                  onClick={() => {
                    if (single && onOpenHost) onOpenHost(single);
                  }}
                  onKeyDown={e => {
                    if (e.key === 'Enter' && single && onOpenHost) onOpenHost(single);
                  }}
                >
                  <rect
                    x={left}
                    y={top}
                    width={b.width}
                    height={BLOCK}
                    rx={3}
                    className={cx(fill, hover?.block === b && 'stroke-foreground')}
                    strokeWidth={hover?.block === b ? 1.5 : 0}
                  />
                  {text && (
                    <text
                      x={left + 5}
                      y={top + BLOCK - 3.5}
                      pointerEvents="none"
                      className={cx('text-xs font-medium', b.group === 'off' ? 'fill-background' : 'fill-white')}
                    >
                      {text}
                    </text>
                  )}
                </g>
              );
            })}
          </>
        )}

        {/* Time axis */}
        <line x1={plotLeft} x2={plotRight} y1={axisTop} y2={axisTop} className="stroke-border-strong" />
        {timeTicks(startMs, endMs, plotWidth).map(t => (
          <g key={t}>
            <line x1={x(t)} x2={x(t)} y1={axisTop} y2={axisTop + 4} className="stroke-border-strong" />
            {x(t) > plotLeft + 20 && x(t) < plotRight - 64 && (
              <text x={x(t)} y={axisTop + 16} textAnchor="middle" className="fill-faint text-xs tabular-nums">
                {clock(t)}
              </text>
            )}
          </g>
        ))}
        <text x={plotRight} y={axisTop + 16} textAnchor="end" className="fill-muted text-xs font-medium">
          now
        </text>

        {/* The column under the pointer. */}
        {hover && !hover.block && (
          <rect
            pointerEvents="none"
            x={plotLeft + hover.column * colWidth}
            y={CHART_TOP}
            width={Math.max(colWidth, 1)}
            height={axisTop - CHART_TOP}
            className="fill-foreground/15"
          />
        )}
      </svg>

      {win && (
        <p className="text-xs/5 text-muted">
          {win.bucketSlots === 1
            ? `One column is one slot (${formatDuration(win.slotMs / 1000)}).`
            : `One column is ${win.bucketSlots} slots (${formatDuration(
                (win.bucketSlots * win.slotMs) / 1000
              )}): solid red is the average missed, light red reaches its worst slot.`}{' '}
          The scale is stretched towards 100%, so a small dip shows.
          {bands &&
            ` A block is a span of time, not a host: blocks of one kind that overlap more than fit are one block that counts its hosts.${
              offSlots > 0
                ? ` Offline: a host whose validators missed attestations in ${offSlots} or more slots within an epoch of each other while upgradoor was not working on it.`
                : ''
            }${offHosts > 0 ? ` ${plural(offHosts, 'host')} ${offHosts === 1 ? 'was' : 'were'} off in this window.` : ''}`}
        </p>
      )}

      {hover && win && (hover.block || hoverColumn) && (
        <div
          role="tooltip"
          className="pointer-events-none absolute z-10 flex w-66 flex-col gap-1 rounded-md border border-border-strong bg-surface p-2 text-xs/5 shadow-lg"
          style={{
            left: Math.max(0, hover.x + 284 > width ? hover.x - 276 : hover.x + 14),
            top: Math.max(0, Math.min(hover.y - 12, height - 150)),
          }}
        >
          {hover.block ? (
            <BlockTip block={hover.block} now={now} canOpen={onOpenHost !== undefined} />
          ) : (
            hoverColumn && <ColumnTip column={hoverColumn} win={win} now={now} />
          )}
        </div>
      )}
    </div>
  );
}

/** BlockTip says what a block stands for: who, what, from when to when, and what it took out. */
function BlockTip({ block, now, canOpen }: { block: OutageBlock; now: number; canOpen: boolean }) {
  const { names, more } = blockNames(block);
  const from = Math.min(...block.outages.map(o => Date.parse(o.from)));
  const to = Math.max(...block.outages.map(o => Date.parse(o.to)));
  const validators = block.outages.reduce((n, o) => n + o.validators, 0);
  const missed = block.outages.reduce((n, o) => n + o.missed, 0);
  const assigned = block.outages.reduce((n, o) => n + o.assigned, 0);
  const failed = block.outages.filter(o => o.failed).reduce((n, o) => n + o.hosts, 0);
  const before = block.outages.some(o => o.fromStart);

  return (
    <>
      <span className="flex items-center gap-1.5">
        <span
          aria-hidden="true"
          className={cx(
            'size-2.5 shrink-0 rounded-xs',
            block.group === 'off'
              ? 'bg-foreground/55'
              : block.failed
                ? 'bg-danger-solid'
                : (WORK_FILL[block.work ?? 'mixed'] ?? '').replace('fill-', 'bg-')
          )}
        />
        <span className="font-semibold">
          {workLabel(block)} · {block.hosts === 1 ? names[0] : plural(block.hosts, 'host')}
        </span>
      </span>
      <span className="text-muted">
        {block.group === 'off'
          ? 'Not attesting, and upgradoor was not working on it.'
          : block.open
            ? 'In progress.'
            : failed > 0
              ? block.hosts === 1
                ? 'Failed.'
                : `${failed} of ${block.hosts} failed.`
              : 'Finished.'}
      </span>
      <span className="tabular-nums">
        {before ? 'before ' : ''}
        {clock(from)} – {block.open ? 'now' : clock(to)}{' '}
        <span className="text-muted">({formatDuration(((block.open ? now : to) - from) / 1000)})</span>
      </span>
      {(validators > 0 || assigned > 0) && (
        <span>
          {validators > 0 && `${plural(validators, 'validator')} out`}
          {validators > 0 && assigned > 0 && ' · '}
          {assigned > 0 && (
            <span className={missed > 0 ? 'text-danger' : 'text-muted'}>
              {missed} of {assigned} attestations missed
            </span>
          )}
        </span>
      )}
      {block.hosts > 1 && (
        <span className="border-t border-border pt-1 whitespace-normal text-muted">
          {names.join(', ')}
          {more > 0 && ` and ${more} more`}
        </span>
      )}
      {block.hosts === 1 && canOpen && <span className="text-faint">Click to find the host in the Hosts tab.</span>}
    </>
  );
}

function ColumnTip({ column, win, now }: { column: SlotColumn; win: SlotWindow; now: number }) {
  const epoch = Math.floor(column.slot / Math.max(1, win.slotsPerEpoch));
  const lastEpoch = Math.floor((column.slot + column.slots - 1) / Math.max(1, win.slotsPerEpoch));
  const bucketed = column.slots > 1;

  return (
    <>
      <span className="flex flex-wrap items-baseline gap-x-1.5">
        <span className="font-semibold">{slotsLabel(column)}</span>
        <span className="text-muted">
          epoch {epoch === lastEpoch ? epoch : `${epoch}–${lastEpoch}`} · {clock(column.time, true)}
          {bucketed && `–${clock(column.time + column.slots * win.slotMs, true)}`}
        </span>
      </span>

      {column.recorded === 0 ? (
        <span className="text-muted">
          {column.time + column.slots * win.slotMs > now - 3 * win.slotMs
            ? 'Not settled yet: a slot is counted two slots after it began.'
            : 'No data: the controller was not watching the chain.'}
        </span>
      ) : (
        <>
          {column.assigned > 0 ? (
            <span className="flex items-baseline gap-1.5">
              <span
                aria-hidden="true"
                className={cx(
                  'size-2 shrink-0 rounded-xs',
                  column.included < column.assigned ? 'bg-danger-solid' : 'bg-accent/30'
                )}
              />
              <span>
                <span className="font-semibold tabular-nums">
                  {column.included} of {column.assigned}
                </span>{' '}
                attested ({percentOf(column.included, column.assigned)})
                {bucketed && column.worstAssigned > 0 && (
                  <span className="text-muted">
                    {' '}
                    · worst slot {column.worstIncluded} of {column.worstAssigned}
                  </span>
                )}
              </span>
            </span>
          ) : (
            <span className="text-muted">No attestation was due from this fleet.</span>
          )}
          <span className={column.missed > 0 ? 'font-medium text-danger' : 'text-muted'}>
            {bucketed
              ? column.missed > 0
                ? `${column.missed} of ${column.recorded} blocks missed`
                : `all ${column.recorded} blocks proposed`
              : column.missed > 0
                ? 'Block missed'
                : 'Block proposed'}
            {bucketed && column.recorded < column.slots && ` · ${column.slots - column.recorded} slots without data`}
          </span>
        </>
      )}
    </>
  );
}

/**
 * TimelineLegend keys the graph: what the two colours of a column mean, the
 * gate's threshold and, with a login, the kinds of block below the columns.
 */
export function TimelineLegend({ lanes, threshold }: { lanes: boolean; threshold: number | undefined }) {
  const swatch = (className: string, label: string) => (
    <li key={label} className="inline-flex items-center gap-1.5">
      <span aria-hidden="true" className={cx('h-2.5 w-4 rounded-xs', className)} />
      {label}
    </li>
  );

  return (
    <ul aria-label="Legend" className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs/5 text-muted">
      <li className="inline-flex items-center gap-1.5">
        <span aria-hidden="true" className="size-2.5 rounded-xs bg-accent/30" />
        Attested
      </li>
      <li className="inline-flex items-center gap-1.5">
        <span aria-hidden="true" className="size-2.5 rounded-xs bg-danger-solid" />
        Missed
      </li>
      {threshold !== undefined && (
        <li
          className="inline-flex items-center gap-1.5"
          title="The gate closes when the participation of an epoch falls below this line. It leaves the validators of hosts that are being updated out of its count, so a planned outage can take single slots below the line without closing it."
        >
          <span aria-hidden="true" className="w-4 border-t border-dashed border-foreground" />
          Gate threshold {formatPercent(threshold)}
        </li>
      )}
      {lanes && (
        <>
          {swatch('bg-series-image', 'Image roll')}
          {swatch('bg-series-packages', 'Packages')}
          {swatch('bg-series-reboot', 'Reboot')}
          {swatch('bg-danger-solid', 'Failed update')}
          {swatch('bg-foreground/55', 'Offline')}
        </>
      )}
    </ul>
  );
}
