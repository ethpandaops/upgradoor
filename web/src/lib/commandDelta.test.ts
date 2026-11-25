import { describe, expect, it } from 'vitest';

import { COMMAND_DELTA_CUT } from '../api/types';
import { argChanges, deltaSummary } from './commandDelta';

describe('argChanges', () => {
  it('reads arguments gained and lost', () => {
    expect(argChanges({ added: ['--metrics'], removed: ['--http'] })).toEqual([
      { kind: 'added', arg: '--metrics' },
      { kind: 'removed', arg: '--http' },
    ]);
  });

  it('reads a name lost and gained once as a changed value', () => {
    expect(argChanges({ added: ['--verbosity=4', '--new'], removed: ['--verbosity=3'] })).toEqual([
      { kind: 'value', name: '--verbosity', from: '3', to: '4' },
      { kind: 'added', arg: '--new' },
    ]);
  });

  it('reads a list value as the items it gains and loses', () => {
    expect(
      argChanges({ added: ['--bootnodes=enr:a,enr:c,enr:d'], removed: ['--bootnodes=enr:a,enr:b,enr:c'] })
    ).toEqual([
      { kind: 'list', name: '--bootnodes', added: ['enr:d'], removed: ['enr:b'], from: 3, to: 3, cut: false },
    ]);
  });

  it('leaves out the item a cut list ends in', () => {
    const changes = argChanges({
      added: [`--bootnodes=enr:a,enr:b,enr:x${COMMAND_DELTA_CUT}`],
      removed: ['--bootnodes=enr:a'],
      truncated: true,
    });
    expect(changes).toEqual([
      { kind: 'list', name: '--bootnodes', added: ['enr:b'], removed: [], from: 1, to: 2, cut: true },
    ]);
  });

  it('pairs nothing when a name is gained or lost more than once', () => {
    expect(argChanges({ added: ['--peer=a', '--peer=b'], removed: ['--peer=c'] }).map(c => c.kind)).toEqual([
      'added',
      'added',
      'removed',
    ]);
  });

  it('counts the arguments for the summary', () => {
    expect(deltaSummary({ added: ['--a', '--b'], removed: null })).toBe('+2 −0');
  });
});
