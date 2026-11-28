import { describe, expect, it } from 'vitest';

import golden from '../../api/wire.golden.json';
import type { AuditEntry } from '../../api/types';
import { FALLBACK, FALLBACK_ACTIONS, auditRenderers, humanize } from './auditRenderers';

const renderers = auditRenderers(new Map([['paused', 'Global pause']]));

const entry = (action: string, detail?: Record<string, unknown>, target = 'node-1'): AuditEntry => ({
  id: 1,
  time: '2026-11-28T10:00:00Z',
  actor: 'alice@example.org',
  action,
  target,
  detail,
});

describe('auditRenderers', () => {
  it('has a renderer for every action the controller records, or falls back on purpose', () => {
    const actions = golden.auditActions.map(a => a.action);
    const missing = actions.filter(a => !(a in renderers) && !FALLBACK_ACTIONS.has(a));
    expect(missing).toEqual([]);
    // No renderer for an action the controller does not record, and none both ways.
    expect(Object.keys(renderers).filter(a => !actions.includes(a))).toEqual([]);
    expect([...FALLBACK_ACTIONS].filter(a => a in renderers || !actions.includes(a))).toEqual([]);
  });

  it('reads each entry as a sentence', () => {
    const r = (e: AuditEntry) => renderers[e.action] ?? FALLBACK;
    const pause = entry('action.pause', { reason: 'upgrade' }, 'settings');
    expect(r(pause).title(pause)).toBe('Paused the scheduler');
    expect(r(pause).summary?.(pause)).toBe('upgrade');

    const roll = entry('action.roll', { hosts: ['a', 'b'], staggered: true, reason: 'jwt' }, 'ro-1');
    expect(r(roll).title(roll)).toBe('Started a roll of 2 hosts');
    expect(r(roll).summary?.(roll)).toBe('staggered · jwt');

    const settings = entry('settings.update', { before: { paused: false }, after: { paused: true } }, 'settings');
    expect(r(settings).summary?.(settings)).toBe('Global pause');

    const clear = entry('action.image-override', { clear: true, container: 'geth' });
    expect(r(clear).title(clear)).toBe('Cleared the image override');
    expect(r(clear).tone).toBeUndefined();

    const failed = entry('lease.failed', { reason: 'crash loop' });
    expect(r(failed).tone?.(failed)).toBe('danger');

    const refused = entry('auth.refused', { reason: 'lacks admin' }, '/api/v1/settings');
    expect(FALLBACK.title(refused)).toBe('Refused');
    expect(FALLBACK.summary?.(refused)).toBe('lacks admin');
  });

  it('reads a profile download', () => {
    const r = renderers['debug.pprof'];
    if (!r) throw new Error('no renderer for debug.pprof');

    const ok = entry('debug.pprof', { profile: 'heap', query: 'gc=1', status: 200 }, '/debug/pprof/heap');
    expect(r.title(ok)).toBe('Downloaded profile heap');
    expect(r.tone?.(ok)).toBeUndefined();
    expect(r.summary?.(ok)).toBe('gc=1');
    expect(r.fields?.map(f => [f.key, f.kind])).toEqual([
      ['profile', 'mono'],
      ['query', 'mono'],
      ['status', 'number'],
    ]);

    const failed = entry('debug.pprof', { profile: 'nope', query: '', status: 404 }, '/debug/pprof/nope');
    expect(r.tone?.(failed)).toBe('danger');
    expect(r.summary?.(failed)).toBe('status 404');
  });

  it('humanizes names', () => {
    expect(humanize('debug.pprof')).toBe('Pprof');
    expect(humanize('action.mark-offline')).toBe('Mark offline');
    expect(humanize('ignoredGate')).toBe('Ignored gate');
  });
});
