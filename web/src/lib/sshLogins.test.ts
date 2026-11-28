import { describe, expect, it } from 'vitest';

import type { SSHLoginCluster } from '../api/types';
import {
  DEFAULT_SSH_FILTER,
  clusterQuery,
  durationText,
  githubName,
  sshApiQuery,
  sshFilterFromParams,
  sshFilterToParams,
} from './sshLogins';

describe('sshLogins', () => {
  it('keeps the filter in the URL without the defaults', () => {
    expect(sshFilterToParams(DEFAULT_SSH_FILTER).toString()).toBe('');
    const f = { ...DEFAULT_SSH_FILTER, view: 'groups' as const, host: 'node-1', unknown: true, open: true };
    expect(sshFilterToParams(f).toString()).toBe('view=groups&host=node-1&unknown=1&open=1');
    expect(sshFilterFromParams(sshFilterToParams(f))).toEqual(f);
    expect(sshFilterFromParams(new URLSearchParams('view=nope&unknown=yes'))).toEqual(DEFAULT_SSH_FILTER);
  });

  it('asks the controller with instants', () => {
    const q = sshApiQuery({ ...DEFAULT_SSH_FILTER, from: '2026-11-28T10:00', identity: ' github:pk910 ' });
    expect(q.from).toBe(new Date('2026-11-28T10:00').toISOString());
    expect(q.identity).toBe('github:pk910');
    expect(q.view).toBe('fleet');
  });

  it('unfolds a cluster by its identity, or its key without one, over its starts', () => {
    const c: SSHLoginCluster = {
      source: 'unknown',
      fingerprint: 'SHA256:abc',
      user: 'devops',
      from: ['10.0.0.1'],
      firstLogin: '2026-11-28T10:00:00.000Z',
      lastStart: '2026-11-28T10:15:00.000Z',
      lastLogout: '2026-11-28T10:20:00.000Z',
      hosts: 3,
      hostNames: ['a', 'b', 'c'],
      groups: 3,
      sessions: 9,
      open: 0,
      longestMs: 1000,
      totalMs: 5000,
    };
    expect(clusterQuery(c)).toEqual({
      view: 'groups',
      identity: '',
      fingerprint: 'SHA256:abc',
      user: 'devops',
      from: '2026-11-28T10:00:00.000Z',
      to: '2026-11-28T10:15:00.001Z',
    });
    expect(clusterQuery({ ...c, identity: 'github:pk910' }).fingerprint).toBe('');
  });

  it('reads identities and durations', () => {
    expect(githubName('github:pk910')).toBe('pk910');
    expect(githubName('ops@laptop')).toBeUndefined();
    expect(githubName('github:bad/name')).toBeUndefined();
    expect(durationText({ totalMs: 125_000, longestMs: 61_000, sessions: 3 })).toBe('2m 5s / 1m 1s');
    expect(durationText({ totalMs: 7_000, longestMs: 7_000, sessions: 1 })).toBe('7s');
  });
});
