// Formatting helpers for digests, times, durations and shares.

import { list, type Hold } from '../api/types';

const ZERO_TIME_PREFIX = '0001-01-01';

/** isZeroTime reports whether an RFC 3339 string is absent or the Go zero time. */
export function isZeroTime(iso: string | null | undefined): boolean {
  return !iso || iso.startsWith(ZERO_TIME_PREFIX);
}

/**
 * shortDigest returns the first 12 hex characters of a digest, without its
 * algorithm prefix ("sha256:abcdef..." -> "abcdef012345").
 */
export function shortDigest(digest: string | null | undefined): string {
  if (!digest) return '';
  const hex = digest.includes(':') ? digest.slice(digest.indexOf(':') + 1) : digest;
  return hex.slice(0, 12);
}

/** shortRevision shortens a git revision to 8 characters; other values pass. */
export function shortRevision(revision: string | null | undefined): string {
  if (!revision) return '';
  return /^[0-9a-f]{12,}$/i.test(revision) ? revision.slice(0, 8) : revision;
}

/**
 * displayImage shortens an image reference to its last path segment
 * ("ghcr.io/org/app:tag" -> "app:tag") for narrow cells; the full reference
 * belongs in the tooltip.
 */
export function displayImage(ref: string): string {
  const at = ref.indexOf('@');
  const base = at >= 0 ? ref.slice(0, at) : ref;
  return base.slice(base.lastIndexOf('/') + 1);
}

/** formatDuration renders seconds with its two largest units: "3d 4h", "5m 2s". */
export function formatDuration(totalSeconds: number): string {
  if (!Number.isFinite(totalSeconds) || totalSeconds < 0) return '–';
  const s = Math.floor(totalSeconds);
  const units: [string, number][] = [
    ['d', 86400],
    ['h', 3600],
    ['m', 60],
    ['s', 1],
  ];
  const first = units.findIndex(([, size]) => s >= size);
  if (first < 0) return '0s';

  const [label, size] = units[first] as [string, number];
  const n = Math.floor(s / size);
  const next = units[first + 1];
  if (!next) return `${n}${label}`;

  const m = Math.floor((s - n * size) / next[1]);
  return m > 0 ? `${n}${label} ${m}${next[0]}` : `${n}${label}`;
}

/** relativeTime renders an instant relative to now: "12s ago", "in 3m", "never". */
export function relativeTime(iso: string | null | undefined, now: number = Date.now()): string {
  if (isZeroTime(iso)) return 'never';
  const t = Date.parse(iso as string);
  if (Number.isNaN(t)) return '–';
  const diff = Math.round((now - t) / 1000);
  if (Math.abs(diff) < 2) return 'just now';
  return diff > 0 ? `${formatDuration(diff)} ago` : `in ${formatDuration(-diff)}`;
}

/** absoluteTime renders an instant for a tooltip: local time plus the UTC ISO form. */
export function absoluteTime(iso: string | null | undefined): string {
  if (isZeroTime(iso)) return 'never';
  const d = new Date(iso as string);
  if (Number.isNaN(d.getTime())) return String(iso);
  return `${d.toLocaleString()} (${d.toISOString()})`;
}

/** secondsBetween returns b - a in seconds, or null when either is unset. */
export function secondsBetween(a: string | null | undefined, b: string | null | undefined): number | null {
  if (isZeroTime(a) || isZeroTime(b)) return null;
  const d = (Date.parse(b as string) - Date.parse(a as string)) / 1000;
  return Number.isNaN(d) ? null : d;
}

/** QUEUE_PLACE_TITLE explains queuePlace. */
export const QUEUE_PLACE_TITLE = "position in the scheduler's queue";

/** queuePlace names a position in the plan queue: "next" or "#3 in line". */
export function queuePlace(position: number): string {
  return position <= 1 ? 'next' : `#${position} in line`;
}

/** formatPercent renders a 0-100 value: whole numbers bare, others with one decimal. */
export function formatPercent(value: number): string {
  if (!Number.isFinite(value)) return '–';
  const rounded = Math.round(value * 10) / 10;
  return `${Number.isInteger(rounded) ? rounded.toFixed(0) : rounded.toFixed(1)}%`;
}

/** percentOf renders part / total as a percentage; "–" when total is 0. */
export function percentOf(part: number, total: number): string {
  if (!total) return '–';
  return formatPercent((part / total) * 100);
}

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

/** formatBytes renders a size in binary units: "512 MiB", "3.1 GiB", "12 GiB". */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '–';
  let unit = 0;
  let v = bytes;
  while (v >= 1024 && unit < BYTE_UNITS.length - 1) {
    v /= 1024;
    unit++;
  }
  const shown = unit > 0 && v < 10 ? (Math.round(v * 10) / 10).toString() : Math.round(v).toString();
  return `${shown} ${BYTE_UNITS[unit] ?? 'B'}`;
}

/** plural renders "1 host" / "3 hosts". */
export function plural(n: number, singular: string, pluralForm: string = `${singular}s`): string {
  return `${n} ${n === 1 ? singular : pluralForm}`;
}

/**
 * pauseLabel says in a few words that a rollout is paused and why, from its
 * pause reason: "paused" when a person paused it, "paused after the canary",
 * or "paused (aborted: <reason>)" for a rollout of an observed cause that a
 * person aborted. The reason's call to resume is left out.
 */
export function pauseLabel(pauseReason: string | null | undefined): string {
  const why = (pauseReason ?? '').replace(/; resume to continue$/, '');
  if (why === '') return 'paused';
  return why.startsWith('paused') ? why : `paused (${why})`;
}

/** hostList renders host names for a sentence, truncated after `max` names. */
export function hostList(hosts: readonly string[], max = 8): string {
  if (hosts.length <= max) return hosts.join(', ');
  return `${hosts.slice(0, max).join(', ')} and ${hosts.length - max} more`;
}

/** isHeld reports whether a hold is in effect: the agent reports one. */
export function isHeld(s: { hold: Hold }): boolean {
  return s.hold.local || list(s.hold.holds).length > 0;
}

/**
 * heldBy says what holds the host as the scheduler's waiting reason does: "held
 * by the host, staff, lighthouse". Undefined when nothing holds it.
 */
export function heldBy(s: { hold: Hold }): string | undefined {
  const by = list(s.hold.holds).map(h => (h.holder === 'host' ? 'the host' : h.holder));
  if (by.length > 0) return `held by ${by.join(', ')}`;
  return s.hold.local ? 'held by the host' : undefined;
}
