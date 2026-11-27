import { describe, expect, it } from 'vitest';

import type { Hold } from '../api/types';

import {
  absoluteTime,
  displayImage,
  formatBytes,
  formatDuration,
  formatPercent,
  heldBy,
  isHeld,
  hostList,
  isZeroTime,
  percentOf,
  plural,
  queuePlace,
  relativeTime,
  timestamp,
  secondsBetween,
  shortDigest,
  shortRevision,
} from './format';

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

  it('renders a timestamp that stands still, in local time', () => {
    const d = new Date(2026, 10, 4, 9, 5, 7);
    expect(timestamp(d.toISOString())).toBe('2026-11-04 09:05:07');
    expect(timestamp('0001-01-01T00:00:00Z')).toBe('never');
    expect(timestamp('nonsense')).toBe('nonsense');
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

describe('queuePlace', () => {
  it.each([
    [1, 'next'],
    [3, '#3 in line'],
  ])('queuePlace(%s) = %s', (position, want) => {
    expect(queuePlace(position)).toBe(want);
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

describe('formatBytes', () => {
  it.each([
    [0, '0 B'],
    [1000, '1000 B'],
    [512 * 2 ** 20, '512 MiB'],
    [3.14 * 2 ** 30, '3.1 GiB'],
    [12.6 * 2 ** 30, '13 GiB'],
    [1.8 * 2 ** 40, '1.8 TiB'],
  ])('formatBytes(%s) = %s', (bytes, want) => {
    expect(formatBytes(bytes)).toBe(want);
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

describe('heldBy and isHeld', () => {
  const host = (hold: Hold) => ({ hold });
  const at = '2026-11-22T10:00:00Z';

  it('names the holders as the scheduler does, the host first', () => {
    const held = host({
      local: true,
      holds: [
        { holder: 'host', at },
        { holder: 'staff', at },
        { holder: 'lighthouse', at },
      ],
    });
    expect(heldBy(held)).toBe('held by the host, staff, lighthouse');
    expect(isHeld(held)).toBe(true);
    expect(heldBy(host({ local: false, holds: [{ holder: 'geth', at }] }))).toBe('held by geth');
    expect(heldBy(host({ local: false }))).toBeUndefined();
    expect(isHeld(host({ local: false }))).toBe(false);
  });
});
