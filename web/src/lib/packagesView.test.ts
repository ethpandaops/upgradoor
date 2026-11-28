import { describe, expect, it } from 'vitest';

import type { PackageMatch } from '../api/types';
import { START_PLACE, compareDebianVersions, placeFromHash, placeToHash, summarize } from './packagesView';

describe('compareDebianVersions', () => {
  it.each([
    ['1.0', '1.0', 0],
    ['1.0', '1.1', -1],
    ['1.10', '1.9', 1],
    ['1:0.9', '2.0', 1],
    ['1.0~rc1', '1.0', -1],
    ['1.0', '1.0+deb13u1', -1],
    ['3.5.1-1', '3.5.1-1+deb13u1', -1],
    ['3.5a-3', '3.5a-3+deb13u1', -1],
    ['2.41-12', '2.41-9', 1],
    ['5:29.0.0-1~debian.13~trixie', '5:28.5.1-1~debian.13~trixie', 1],
    ['1.0a', '1.0', 1],
    ['1.0~', '1.0', -1],
  ])('%s against %s', (a, b, want) => {
    expect(Math.sign(compareDebianVersions(a, b))).toBe(want);
    expect(Math.sign(compareDebianVersions(b, a))).toBe(-want || 0);
  });
});

describe('summarize', () => {
  it('reduces a package to its most common version, the rest and its upgrades', () => {
    const match: PackageMatch = {
      name: 'openssl',
      versions: [
        { version: '3.5.1-1', count: 2, hosts: ['a', 'b'] },
        { version: '3.5.4-1', count: 20, hosts: [] },
        { version: '3.5.0-1', count: 1, hosts: ['c'] },
      ],
      upgrades: [
        { current: '3.5.1-1', candidate: '3.5.4-1', security: true, blocked: false, count: 2, hosts: [] },
        { current: '3.5.0-1', candidate: '3.5.3-1', security: false, blocked: true, count: 1, hosts: [] },
      ],
    };

    const s = summarize(match);
    expect(s.hosts).toBe(23);
    expect(s.top?.version).toBe('3.5.4-1');
    expect(s.others.map(v => v.version)).toEqual(['3.5.1-1', '3.5.0-1']);
    expect(s.outdated).toBe(3);
    expect(s.newest).toBe('3.5.4-1');
    expect(s.security).toBe(true);
    expect(s.blocked).toBe(true);
    expect(s.behind.get('3.5.1-1')).toBe(2);
  });

  it('has no newest version for a package without upgrades', () => {
    const s = summarize({ name: 'bash', versions: [{ version: '5.2', count: 3, hosts: [] }], upgrades: [] });
    expect(s.newest).toBeUndefined();
    expect(s.outdated).toBe(0);
    expect(s.others).toEqual([]);
  });
});

describe('place in the hash', () => {
  it('leaves out the defaults', () => {
    const { path, params } = placeToHash(START_PLACE);
    expect(path).toBe('');
    expect(params.toString()).toBe('');
  });

  it('round-trips a place', () => {
    const place = {
      ...START_PLACE,
      q: 'lib',
      size: 250,
      show: 'outdated' as const,
      sort: 'hosts' as const,
      cursors: ['', 'a', '0lib-x'],
    };
    const { path, params } = placeToHash(place);
    expect(params.toString()).toBe('q=lib&show=outdated&sort=hosts&size=250&page=2&after=0lib-x');

    // The pages before a linked one are not known.
    expect(placeFromHash(path, params)).toEqual({ ...place, cursors: ['', null, '0lib-x'] });
  });

  it('ignores what it does not know', () => {
    const place = placeFromHash('images', new URLSearchParams('size=7&show=differing&sort=x&page=-3'));
    expect(place).toEqual({ ...START_PLACE, kind: 'docker' });
  });
});
