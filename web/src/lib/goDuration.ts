// Go duration strings ("30m0s", "1h30m", "90s") as used by fleet.Duration.

const UNIT_MS: Record<string, number> = {
  ns: 1e-6,
  us: 1e-3,
  µs: 1e-3,
  μs: 1e-3,
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
};

const TOKEN = /(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)/gy;

/**
 * parseGoDuration parses what Go's time.ParseDuration accepts and returns
 * milliseconds, or null for an invalid string.
 */
export function parseGoDuration(input: string): number | null {
  let s = input.trim();
  if (s === '') return null;

  let sign = 1;
  if (s.startsWith('-') || s.startsWith('+')) {
    sign = s.startsWith('-') ? -1 : 1;
    s = s.slice(1);
  }

  if (s === '0') return 0;

  let total = 0;
  let pos = 0;
  TOKEN.lastIndex = 0;

  while (pos < s.length) {
    TOKEN.lastIndex = pos;
    const m = TOKEN.exec(s);
    if (!m) return null;
    total += parseFloat(m[1] as string) * (UNIT_MS[m[2] as string] as number);
    pos = TOKEN.lastIndex;
  }

  return sign * total;
}

/**
 * formatGoDuration renders milliseconds as a compact Go duration string that
 * time.ParseDuration reads back ("30m", "1h30m", "1m30s", "500ms").
 */
export function formatGoDuration(ms: number): string {
  if (!Number.isFinite(ms)) return '';
  if (ms === 0) return '0s';

  const sign = ms < 0 ? '-' : '';
  let rest = Math.abs(ms);
  let out = '';

  const h = Math.floor(rest / 3_600_000);
  rest -= h * 3_600_000;
  const m = Math.floor(rest / 60_000);
  rest -= m * 60_000;
  const s = Math.floor(rest / 1000);
  rest -= s * 1000;

  if (h > 0) out += `${h}h`;
  if (m > 0) out += `${m}m`;
  if (s > 0) out += `${s}s`;
  if (rest > 0) out += `${Math.round(rest * 1000) / 1000}ms`;

  return sign + out;
}

/** normalizeGoDuration turns "30m0s" into "30m"; invalid input is returned as is. */
export function normalizeGoDuration(input: string): string {
  const ms = parseGoDuration(input);
  return ms === null ? input : formatGoDuration(ms);
}
