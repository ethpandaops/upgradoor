import { describe, expect, it } from 'vitest';

import {
  absoluteTime,
  displayImage,
  formatDuration,
  formatPercent,
  hostList,
  isZeroTime,
  percentOf,
  plural,
  relativeTime,
  secondsBetween,
  shortDigest,
  shortRevision,
  splitImage,
} from './format';
import { formatGoDuration, normalizeGoDuration, parseGoDuration } from './goDuration';

describe('shortDigest', () => {
  it.each([
    ['sha256:a6d0c01328bc7da7cff9aa97225b792a769a38ea2e04cc191c56febafa87848b', 'a6d0c01328bc'],
    ['a6d0c01328bc7da7cff9aa97225b792a', 'a6d0c01328bc'],
    ['sha256:abc', 'abc'],
    ['', ''],
    [undefined, ''],
    [null, ''],
  ])('%s -> %s', (input, want) => {
    expect(shortDigest(input)).toBe(want);
  });
});

describe('shortRevision', () => {
  it.each([
    ['d22f83e6c0ffee00d22f83e6c0ffee00d22f83e6', 'd22f83e6'],
    ['v1.2.3', 'v1.2.3'],
    ['abcdef0', 'abcdef0'],
    [undefined, ''],
  ])('%s -> %s', (input, want) => {
    expect(shortRevision(input)).toBe(want);
  });
});

describe('displayImage', () => {
  it.each([
    ['ethpandaops/geth:master', 'geth:master'],
    ['gcr.io/offchainlabs/prysm/beacon-chain:latest', 'beacon-chain:latest'],
    ['localhost:5000/app:dev', 'app:dev'],
    ['nginx', 'nginx'],
    ['sigp/lighthouse:unstable@sha256:abcdef', 'lighthouse:unstable'],
  ])('%s -> %s', (ref, want) => {
    expect(displayImage(ref)).toBe(want);
  });
});

describe('splitImage', () => {
  it.each([
    ['sigp/lighthouse:unstable', 'sigp/lighthouse', 'unstable'],
    ['registry.local:5000/geth', 'registry.local:5000/geth', ''],
    ['registry.local:5000/geth:master', 'registry.local:5000/geth', 'master'],
    ['geth@sha256:abc', 'geth', ''],
  ])('%s', (ref, repository, tag) => {
    expect(splitImage(ref)).toEqual({ repository, tag });
  });
});

describe('formatDuration', () => {
  it.each([
    [0, '0s'],
    [0.4, '0s'],
    [42, '42s'],
    [60, '1m'],
    [125, '2m 5s'],
    [3600, '1h'],
    [3725, '1h 2m'],
    [86400, '1d'],
    [90061, '1d 1h'],
    [41 * 86400 + 6 * 3600 + 59, '41d 6h'],
    [-1, '–'],
    [Number.NaN, '–'],
  ])('%s s -> %s', (seconds, want) => {
    expect(formatDuration(seconds)).toBe(want);
  });
});

describe('relativeTime', () => {
  const now = Date.parse('2026-11-14T12:00:00Z');

  it.each([
    ['2026-11-14T11:59:48Z', '12s ago'],
    ['2026-11-14T11:57:00Z', '3m ago'],
    ['2026-11-14T09:30:00Z', '2h 30m ago'],
    ['2026-11-09T12:00:00Z', '5d ago'],
    ['2026-11-14T12:03:00Z', 'in 3m'],
    ['2026-11-14T12:00:01Z', 'just now'],
    ['0001-01-01T00:00:00Z', 'never'],
    [undefined, 'never'],
    ['not a time', '–'],
  ])('%s -> %s', (iso, want) => {
    expect(relativeTime(iso, now)).toBe(want);
  });
});

describe('absoluteTime and zero times', () => {
  it('carries the UTC ISO form for the tooltip', () => {
    expect(absoluteTime('2026-11-14T11:59:48Z')).toContain('2026-11-14T11:59:48.000Z');
  });

  it('treats the Go zero time as unset', () => {
    expect(isZeroTime('0001-01-01T00:00:00Z')).toBe(true);
    expect(isZeroTime('')).toBe(true);
    expect(isZeroTime('2026-11-14T11:59:48Z')).toBe(false);
    expect(absoluteTime('0001-01-01T00:00:00Z')).toBe('never');
  });

  it('measures the seconds between two instants', () => {
    expect(secondsBetween('2026-11-14T11:00:00Z', '2026-11-14T11:15:00Z')).toBe(900);
    expect(secondsBetween('2026-11-14T11:00:00Z', undefined)).toBeNull();
  });
});

describe('percent helpers', () => {
  it.each([
    [100, '100%'],
    [95.12, '95.1%'],
    [9.96, '10%'],
    [0, '0%'],
    [Number.NaN, '–'],
  ])('formatPercent(%s) = %s', (value, want) => {
    expect(formatPercent(value)).toBe(want);
  });

  it.each([
    [126, 128, '98.4%'],
    [128, 128, '100%'],
    [0, 128, '0%'],
    [0, 0, '–'],
  ])('percentOf(%s, %s) = %s', (part, total, want) => {
    expect(percentOf(part, total)).toBe(want);
  });
});

describe('plural and hostList', () => {
  it('pluralises', () => {
    expect(plural(1, 'host')).toBe('1 host');
    expect(plural(3, 'host')).toBe('3 hosts');
    expect(plural(2, 'entry', 'entries')).toBe('2 entries');
  });

  it('truncates long host lists', () => {
    expect(hostList(['a', 'b'])).toBe('a, b');
    expect(hostList(['a', 'b', 'c', 'd'], 2)).toBe('a, b and 2 more');
  });
});

describe('Go durations', () => {
  it.each([
    ['30m0s', 1_800_000],
    ['30m', 1_800_000],
    ['2m0s', 120_000],
    ['1h30m', 5_400_000],
    ['90s', 90_000],
    ['1.5h', 5_400_000],
    ['500ms', 500],
    ['0', 0],
    ['0s', 0],
    ['-1m', -60_000],
  ])('parses %s', (input, want) => {
    expect(parseGoDuration(input)).toBe(want);
  });

  it.each(['', 'abc', '30', '5 m', '1d', 'm', '30m ago'])('rejects %j', input => {
    expect(parseGoDuration(input)).toBeNull();
  });

  it.each([
    [1_800_000, '30m'],
    [5_400_000, '1h30m'],
    [90_000, '1m30s'],
    [500, '500ms'],
    [0, '0s'],
    [-60_000, '-1m'],
  ])('formats %s ms as %s', (ms, want) => {
    expect(formatGoDuration(ms)).toBe(want);
  });

  it('writes what Go reads back', () => {
    for (const s of ['30m0s', '2m0s', '1h0m0s', '45s', '1h1m1s']) {
      expect(parseGoDuration(normalizeGoDuration(s))).toBe(parseGoDuration(s));
    }
    expect(normalizeGoDuration('30m0s')).toBe('30m');
    expect(normalizeGoDuration('nonsense')).toBe('nonsense');
  });
});
