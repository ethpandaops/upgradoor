// Resource use: the levels a share is shown in, the brief of a host
// (api.HostUsage), the fleet summary of the host table, and the filter words
// that narrow the table to the busy hosts.

import type { HostListing, HostUsage, Resources } from '../api/types';

export type UsageMetric = 'cpu' | 'memory';

export const USAGE_METRICS: readonly UsageMetric[] = ['cpu', 'memory'];

export const USAGE_LABEL: Record<UsageMetric, string> = { cpu: 'CPU', memory: 'Memory' };

/** A share from WARN_AT on is shown amber, from CRITICAL_AT on red. */
export const WARN_AT = 80;
export const CRITICAL_AT = 90;

export type UsageLevel = 'ok' | 'warning' | 'danger';

export function usageLevel(percent: number): UsageLevel {
  return percent >= CRITICAL_AT ? 'danger' : percent >= WARN_AT ? 'warning' : 'ok';
}

/** memoryPercent is the share of memory not available; undefined when unknown. */
export function memoryPercent(m: Resources['memory']): number | undefined {
  if (m.total <= 0) return undefined;
  return ((m.total - Math.min(m.available, m.total)) / m.total) * 100;
}

/** hostUsage is the brief of a host's resource use, as the controller makes it (views.NewHostUsage). */
export function hostUsage(r: Resources | undefined): HostUsage | undefined {
  if (!r) return undefined;
  const out: HostUsage = {};
  if (r.cpu.busy !== undefined) out.cpu = Math.round(r.cpu.busy);
  const mem = memoryPercent(r.memory);
  if (mem !== undefined) out.memory = Math.round(mem);
  return Object.keys(out).length > 0 ? out : undefined;
}

export function usageValue(u: HostUsage | undefined, metric: UsageMetric): number | undefined {
  return u?.[metric];
}

/** FleetMetric is one metric over the hosts that report it. */
export interface FleetMetric {
  known: number;
  average?: number;
  worst?: { host: string; value: number };
  /** Hosts at WARN_AT or more (critical ones included), and at CRITICAL_AT or more. */
  warning: number;
  critical: number;
}

export function fleetUsage(hosts: readonly Pick<HostListing, 'name' | 'usage'>[]): Record<UsageMetric, FleetMetric> {
  const out = {} as Record<UsageMetric, FleetMetric>;

  for (const metric of USAGE_METRICS) {
    let sum = 0;
    const m: FleetMetric = { known: 0, warning: 0, critical: 0 };

    for (const h of hosts) {
      const v = usageValue(h.usage, metric);
      if (v === undefined) continue;
      m.known++;
      sum += v;
      if (v >= WARN_AT) m.warning++;
      if (v >= CRITICAL_AT) m.critical++;
      if (!m.worst || v > m.worst.value) m.worst = { host: h.name, value: v };
    }

    if (m.known > 0) m.average = sum / m.known;
    out[metric] = m;
  }

  return out;
}

const USAGE_WORD = /^(cpu|memory)>=(\d+)$/;

/** usageWord is the filter word that keeps the hosts with metric at threshold or more. */
export function usageWord(metric: UsageMetric, threshold: number): string {
  return `${metric}>=${threshold}`;
}

/**
 * matchesUsageWord reports whether a host's use passes a filter word such as
 * "memory>=90"; undefined when the word is not one of those.
 */
export function matchesUsageWord(word: string, usage: HostUsage | undefined): boolean | undefined {
  const m = USAGE_WORD.exec(word);
  if (!m) return undefined;
  const v = usageValue(usage, m[1] as UsageMetric);
  return v !== undefined && v >= Number(m[2]);
}
