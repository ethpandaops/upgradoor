import { describe, expect, it } from 'vitest';

import golden from '../api/wire.golden.json';
import type { AuditEntry } from '../api/types';
import {
  AUDIT_ACTIONS,
  AUDIT_GROUPS,
  DEFAULT_FILTER,
  TARGET_KINDS,
  actorKindOf,
  apiQuery,
  auditHash,
  filterFromParams,
  filterToParams,
  matchesFilter,
} from './auditCatalog';

const entry = (actor: string, action: string, target: string, time = '2026-11-28T10:00:00Z'): AuditEntry => ({
  id: 1,
  time,
  actor,
  action,
  target,
});

describe('auditCatalog', () => {
  it('is fleet.AuditActions, as the controller writes it into the wire golden', () => {
    expect(AUDIT_ACTIONS).toEqual(golden.auditActions);
    expect(new Set(golden.auditActions.map(a => a.group))).toEqual(new Set(AUDIT_GROUPS));
    expect(new Set(golden.auditActions.map(a => a.target))).toEqual(new Set(TARGET_KINDS));
  });

  it('sorts actors as the controller does', () => {
    expect(actorKindOf('scheduler')).toBe('scheduler');
    expect(actorKindOf('user:scheduler')).toBe('people');
    expect(actorKindOf('agent:node-1')).toBe('agents');
    expect(actorKindOf('anonymous')).toBe('people');
  });

  it('keeps the filter in the URL without the defaults', () => {
    expect(filterToParams(DEFAULT_FILTER).toString()).toBe('');
    const f = { ...DEFAULT_FILTER, who: 'all' as const, target: 'node-1', group: 'leases' as const, q: 'x y' };
    expect(filterFromParams(filterToParams(f))).toEqual(f);
    expect(filterFromParams(new URLSearchParams('who=robots&kind=nope&group=nope'))).toEqual(DEFAULT_FILTER);
    expect(auditHash({ target: 'ro-1' })).toBe('#audit/?who=all&target=ro-1');
    expect(auditHash({ who: 'people' })).toBe('#audit/');
  });

  it('asks the controller with instants and without "all"', () => {
    const q = apiQuery({ ...DEFAULT_FILTER, who: 'all', from: '2026-11-28T10:00', q: ' geth ' });
    expect(q.who).toBe('');
    expect(q.q).toBe('geth');
    expect(q.from).toBe(new Date('2026-11-28T10:00').toISOString());
  });

  it('puts every entry about the settings in the settings group, a global pause too', () => {
    const globalPause = entry('alice', 'action.pause', 'settings');
    const rolloutPause = entry('alice', 'action.pause', 'ro-1');
    expect(matchesFilter(globalPause, { group: 'settings' })).toBe(true);
    expect(matchesFilter(globalPause, { group: 'rollouts' })).toBe(false);
    expect(matchesFilter(rolloutPause, { group: 'rollouts' })).toBe(true);
    expect(matchesFilter(rolloutPause, { kind: 'rollout', who: 'people' })).toBe(true);
    expect(matchesFilter(rolloutPause, { who: 'scheduler' })).toBe(false);
    expect(matchesFilter(rolloutPause, { from: '2026-11-28T10:00:00Z', to: '2026-11-28T10:00:01Z' })).toBe(true);
    expect(matchesFilter(rolloutPause, { to: '2026-11-28T10:00:00Z' })).toBe(false);
    expect(matchesFilter(rolloutPause, { q: 'RO-1' })).toBe(true);
  });
});
