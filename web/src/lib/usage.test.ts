import { describe, expect, it } from 'vitest';

import type { Resources } from '../api/types';
import { fleetUsage, hostUsage, matchesUsageWord, usageLevel, usageWord } from './usage';

const GIB = 2 ** 30;

describe('hostUsage', () => {
  it('rounds to whole percents', () => {
    const r: Resources = {
      cpu: { cores: 8, load1: 1, load5: 1, load15: 1, busy: 37.6 },
      memory: { total: 64 * GIB, available: 16 * GIB, swapTotal: 0, swapUsed: 0 },
    };
    expect(hostUsage(r)).toEqual({ cpu: 38, memory: 75 });
  });

  it('leaves out what the agent has not read yet', () => {
    const r: Resources = {
      cpu: { cores: 8, load1: 0, load5: 0, load15: 0 },
      memory: { total: 0, available: 0, swapTotal: 0, swapUsed: 0 },
    };
    expect(hostUsage(r)).toBeUndefined();
    expect(hostUsage(undefined)).toBeUndefined();
  });
});

describe('fleetUsage', () => {
  it('averages, finds the worst host and counts the busy ones', () => {
    const got = fleetUsage([
      { name: 'a', usage: { cpu: 20, memory: 95 } },
      { name: 'b', usage: { cpu: 85, memory: 50 } },
      { name: 'c', usage: { cpu: 92 } },
      { name: 'd' },
    ]);

    expect(got.cpu).toEqual({ known: 3, average: 197 / 3, worst: { host: 'c', value: 92 }, warning: 2, critical: 1 });
    expect(got.memory).toEqual({ known: 2, average: 72.5, worst: { host: 'a', value: 95 }, warning: 1, critical: 1 });
  });
});

describe('usage filter words', () => {
  it('keep the hosts at the threshold or more', () => {
    expect(usageWord('memory', 90)).toBe('memory>=90');
    expect(matchesUsageWord('memory>=90', { memory: 90 })).toBe(true);
    expect(matchesUsageWord('memory>=90', { memory: 89 })).toBe(false);
    expect(matchesUsageWord('cpu>=80', undefined)).toBe(false);
    expect(matchesUsageWord('geth', { cpu: 99 })).toBeUndefined();
  });

  it('levels', () => {
    expect([79, 80, 89, 90].map(usageLevel)).toEqual(['ok', 'warning', 'warning', 'danger']);
  });
});
