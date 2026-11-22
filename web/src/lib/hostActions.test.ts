import { describe, expect, it } from 'vitest';

import type { ActionInfo, RolloutHost } from '../api/types';
import { actionBasis, approvalBasis, hostActionVerdict, rolloutActionOf, selectionActions } from './hostActions';
import type { Login } from './permissions';

const login = (features: string[], teams: string[] = []): Login => ({
  open: false,
  service: 'upgradoor',
  hasFeature: f => features.includes(f),
  teams,
});

const info = (p: Partial<ActionInfo>): ActionInfo => ({ name: 'security-upgrades', impact: 'node', ...p });

describe('actionBasis', () => {
  it('allows open mode, admin, the action feature (manage when it names none) and a team the action names', () => {
    expect(actionBasis({ ...login([]), open: true }, info({}))).toBe('open');
    expect(actionBasis(login(['admin']), info({ feature: 'bogus' }))).toBe('admin');
    expect(actionBasis(login(['details']), info({ feature: 'details' }))).toBe('feature');
    expect(actionBasis(login(['manage']), info({}))).toBe('feature');
    expect(actionBasis(login(['manage_flags']), info({ feature: 'manage_flags' }))).toBe('feature');
    expect(actionBasis(login([], ['lighthouse']), info({ teams: ['lighthouse'] }))).toBe('team');

    // A twin, another feature, an unknown one, another team or nothing at all is not enough.
    expect(actionBasis(login(['team_manage'], ['lighthouse']), info({}))).toBe('');
    expect(actionBasis(login(['manage']), info({ feature: 'details' }))).toBe('');
    expect(actionBasis(login(['manage']), info({ feature: 'admin' }))).toBe('');
    expect(actionBasis(login(['bogus']), info({ feature: 'bogus' }))).toBe('');
    expect(actionBasis(login([], ['prysm']), info({ teams: ['lighthouse'] }))).toBe('');
    expect(actionBasis(login([]), info({}))).toBe('');
  });
});

describe('selectionActions and hostActionVerdict', () => {
  const hosts = [
    { name: 'a', actions: [info({ teams: ['lighthouse'] }), info({ name: 'prune', teams: ['lighthouse'] })] },
    { name: 'b', actions: [info({ feature: 'details' })] },
    { name: 'c', actions: null },
  ];

  it('offers each action on the hosts that let the login start it', () => {
    expect(selectionActions(login([], ['lighthouse']), hosts).map(a => [a.name, a.hosts])).toEqual([
      ['prune', ['a']],
      ['security-upgrades', ['a']],
    ]);
    expect(selectionActions(login(['admin']), hosts).map(a => [a.name, a.hosts])).toEqual([
      ['prune', ['a']],
      ['security-upgrades', ['a', 'b']],
    ]);
  });

  it('allows a request that one selected host permits; the controller rejects the others', () => {
    const team = login([], ['lighthouse']);
    expect(
      hostActionVerdict(team, { action: 'security-upgrades', hosts: ['a', 'b'], reason: '' }, hosts)
    ).toMatchObject({
      allowed: true,
      by: 'team',
    });
    expect(hostActionVerdict(team, { action: 'security-upgrades', hosts: ['b', 'c'], reason: '' }, hosts).allowed).toBe(
      false
    );
  });
});

describe('approvalBasis and rolloutActionOf', () => {
  const hosts = [
    { name: 'a', actions: [info({ teams: ['lighthouse'] })] },
    { name: 'b', actions: [info({ teams: ['lighthouse', 'teku'] })] },
    { name: 'c', actions: [info({ teams: ['teku'] })] },
    { name: 'd', actions: null },
  ];
  const ra = (...members: string[]) => ({ action: 'security-upgrades', hosts: members });

  it('takes the action on every member host', () => {
    const team = login([], ['lighthouse']);
    expect(approvalBasis(team, ra('a', 'b'), hosts)).toBe('team');
    expect(approvalBasis(team, ra('a', 'c'), hosts)).toBe('');
    expect(approvalBasis(team, ra('a', 'd'), hosts)).toBe('');
    expect(approvalBasis(team, ra('a', 'ghost'), hosts)).toBe('');
    expect(approvalBasis(team, ra(), hosts)).toBe('');
    expect(approvalBasis(login(['admin']), ra('a', 'c'), hosts)).toBe('admin');
  });

  it('is only for an action rollout', () => {
    const members: RolloutHost[] = ['a', 'b'].map(host => ({ host, state: 'pending', stage: 3, updatedAt: '' }));
    expect(rolloutActionOf({ kind: 'action', action: 'security-upgrades', hosts: members })).toEqual(ra('a', 'b'));
    expect(rolloutActionOf({ kind: 'packages', action: 'security-upgrades', hosts: members })).toBeUndefined();
    expect(rolloutActionOf({ kind: 'action', hosts: members })).toBeUndefined();
  });
});
