// Resource use: the share bar of the tables, the fleet summary above the host
// table, a host's resources, and a container's use.

import type { ReactNode } from 'react';

import type { ContainerStats, HostListing, Resources } from '../api/types';
import { formatBytes, formatPercent, plural } from '../lib/format';
import {
  CRITICAL_AT,
  USAGE_LABEL,
  USAGE_METRICS,
  WARN_AT,
  fleetUsage,
  memoryPercent,
  usageLevel,
  usageWord,
  type UsageLevel,
  type UsageMetric,
} from '../lib/usage';
import { Dash, cx } from './primitives';

const LEVEL_BAR: Record<UsageLevel, string> = { ok: 'bg-accent', warning: 'bg-warning', danger: 'bg-danger-solid' };
const LEVEL_TEXT: Record<UsageLevel, string> = { ok: '', warning: 'text-warning', danger: 'font-medium text-danger' };

/** UsageBar is a share as a small bar and its number, amber from 80%, red from 90%. */
export function UsageBar({ value, label, detail }: { value: number; label: string; detail?: ReactNode }) {
  const level = usageLevel(value);

  return (
    <span className="flex min-w-0 items-center gap-2">
      <span
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(value)}
        className="block h-1.5 w-12 shrink-0 overflow-hidden rounded-full bg-surface-raised"
      >
        <span
          className={cx('block h-full rounded-full', LEVEL_BAR[level])}
          style={{ width: `${Math.min(100, Math.max(0, value))}%` }}
        />
      </span>
      <span className={cx('min-w-9 text-right tabular-nums', LEVEL_TEXT[level])}>
        {formatPercent(Math.round(value))}
      </span>
      {detail && <span className="min-w-0 truncate text-muted">{detail}</span>}
    </span>
  );
}

/** UsageCell is a share of a table row, or a dash while it is not known. */
export function UsageCell({ value, label }: { value: number | undefined; label: string }) {
  if (value === undefined) return <Dash />;
  return <UsageBar value={value} label={label} />;
}

/**
 * FleetUsage is the fleet summary above the host table: per metric the
 * average, the busiest host and how many hosts are at 80% and 90% or more. A
 * tile narrows the table to the hosts at 80% or more; again, it lifts that.
 */
export function FleetUsage({
  hosts,
  filter,
  onFilter,
}: {
  hosts: readonly HostListing[];
  filter: string;
  onFilter: (filter: string) => void;
}) {
  const fleet = fleetUsage(hosts);
  if (USAGE_METRICS.every(m => fleet[m].known === 0)) return null;

  return (
    <div role="group" aria-label="Fleet resources" className="grid gap-2 sm:grid-cols-2">
      {USAGE_METRICS.map(metric => {
        const m = fleet[metric];
        const word = usageWord(metric, WARN_AT);
        const pressed = filter.trim() === word;

        return (
          <button
            key={metric}
            type="button"
            aria-pressed={pressed}
            aria-label={`${USAGE_LABEL[metric]}: show the hosts at ${WARN_AT}% or more`}
            disabled={m.known === 0}
            onClick={() => {
              onFilter(pressed ? '' : word);
            }}
            className={cx(
              'flex min-w-0 flex-col gap-1 rounded-md border px-3 py-2 text-left text-xs/5',
              'disabled:cursor-default disabled:opacity-60',
              pressed ? 'border-accent bg-surface-selected' : 'border-border bg-surface enabled:hover:bg-surface-hover'
            )}
          >
            <span className="flex items-baseline justify-between gap-2">
              <span className="font-semibold text-muted">{USAGE_LABEL[metric]}</span>
              <span className="text-muted">
                avg{' '}
                <span className="text-sm/6 font-semibold text-foreground tabular-nums">
                  {m.average === undefined ? '–' : formatPercent(Math.round(m.average))}
                </span>
              </span>
            </span>
            {m.worst ? (
              <span className="flex min-w-0 items-center gap-1.5 text-muted">
                worst
                <span className="min-w-0 truncate font-medium text-foreground">{m.worst.host}</span>
                <span className={cx('tabular-nums', LEVEL_TEXT[usageLevel(m.worst.value)])}>
                  {formatPercent(m.worst.value)}
                </span>
              </span>
            ) : (
              <span className="text-faint">no host reports it yet</span>
            )}
            <span className="flex gap-3 tabular-nums">
              <span className={m.warning > 0 ? 'text-warning' : 'text-faint'}>
                {m.warning} ≥{WARN_AT}%
              </span>
              <span className={m.critical > 0 ? 'font-medium text-danger' : 'text-faint'}>
                {m.critical} ≥{CRITICAL_AT}%
              </span>
              <span className="ml-auto text-faint">of {plural(m.known, 'host')}</span>
            </span>
          </button>
        );
      })}
    </div>
  );
}

/** Row is one line of a host's resources: a label, and what there is to say. */
function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  );
}

/** HostResources is a host's processor, memory and swap. */
export function HostResources({ resources: r }: { resources: Resources }) {
  const mem = memoryPercent(r.memory);
  const swapPercent = r.memory.swapTotal > 0 ? (r.memory.swapUsed / r.memory.swapTotal) * 100 : undefined;
  const load = [r.cpu.load1, r.cpu.load5, r.cpu.load15].map(v => v.toFixed(2)).join(' / ');

  return (
    <dl className="grid grid-cols-[max-content_1fr] items-center gap-x-3 gap-y-1 text-xs/5">
      <Row label="CPU">
        <span className="flex flex-wrap items-center gap-x-3">
          {r.cpu.busy === undefined ? (
            <span className="text-muted">busy share after the next sync</span>
          ) : (
            <UsageBar value={r.cpu.busy} label="CPU busy" />
          )}
          <span className="text-muted">
            {plural(r.cpu.cores, 'core')} · load {load}
          </span>
        </span>
      </Row>
      <Row label="memory">
        {mem === undefined ? (
          <Dash />
        ) : (
          <UsageBar
            value={mem}
            label="Memory used"
            detail={`${formatBytes(r.memory.total - r.memory.available)} of ${formatBytes(r.memory.total)}`}
          />
        )}
      </Row>
      <Row label="swap">
        {swapPercent === undefined ? (
          <span className="text-muted">none</span>
        ) : (
          <UsageBar
            value={swapPercent}
            label="Swap used"
            detail={`${formatBytes(r.memory.swapUsed)} of ${formatBytes(r.memory.swapTotal)}`}
          />
        )}
      </Row>
    </dl>
  );
}

/** ContainerUsage is a container's CPU share of the host and its memory, against its limit when it has one. */
export function ContainerUsage({ stats }: { stats: ContainerStats }) {
  const limitShare = stats.memoryLimit ? (stats.memory / stats.memoryLimit) * 100 : undefined;

  return (
    <span className="inline-flex items-center gap-2 tabular-nums" title="CPU: share of the host's capacity">
      <span>{stats.cpu === undefined ? '–' : formatPercent(stats.cpu)} CPU</span>
      <span className={limitShare === undefined ? undefined : LEVEL_TEXT[usageLevel(limitShare)]}>
        {formatBytes(stats.memory)}
        {stats.memoryLimit !== undefined && <span className="text-muted"> / {formatBytes(stats.memoryLimit)}</span>}
      </span>
    </span>
  );
}

/** usageLabel names a metric of a host for a meter. */
export function usageLabel(metric: UsageMetric, host: string): string {
  return `${USAGE_LABEL[metric]} of ${host}`;
}
