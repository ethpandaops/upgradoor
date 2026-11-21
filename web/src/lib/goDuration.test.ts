import { describe, expect, it } from 'vitest';

import { formatGoDuration, normalizeGoDuration, parseGoDuration } from './goDuration';

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
