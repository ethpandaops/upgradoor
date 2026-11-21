import { describe, expect, it } from 'vitest';

import { MAX_FLAGS, applyFlags, parseFlags, validateFlag, validateFlags } from './flags';

const CMD = ['--network=devnet', '--datadir', '/data', '--http', '--port', '8545', '--verbosity=3', '--port', '9000'];

describe('applyFlags', () => {
  const cases: [string, string[], string[], string[]][] = [
    ['removes the exact argument', [], ['--verbosity=3'], CMD.filter(a => a !== '--verbosity=3')],
    ['leaves an argument with another value', [], ['--verbosity=4'], CMD],
    ['removes by name the form with =', [], ['--verbosity'], CMD.filter(a => a !== '--verbosity=3')],
    [
      'removes by name the form of two arguments, every one of them',
      [],
      ['--port'],
      ['--network=devnet', '--datadir', '/data', '--http', '--verbosity=3'],
    ],
    ['removes a switch and keeps the flag after it', [], ['--http'], CMD.filter(a => a !== '--http')],
    [
      'removes a pair',
      [],
      ['--port 8545'],
      ['--network=devnet', '--datadir', '/data', '--http', '--verbosity=3', '--port', '9000'],
    ],
    ['leaves a pair with another value', [], ['--port 1'], CMD],
    ['appends what is added', ['--metrics', '--ws.port 8546'], [], [...CMD, '--metrics', '--ws.port', '8546']],
    ['adds nothing that is there', ['--http', '--verbosity=3', '--port 8545'], [], CMD],
    [
      'changes a value: remove the name, add the new one',
      ['--verbosity=5'],
      ['--verbosity'],
      [...CMD.filter(a => a !== '--verbosity=3'), '--verbosity=5'],
    ],
  ];

  it.each(cases)('%s', (_name, add, remove, want) => {
    const before = [...CMD];
    expect(applyFlags(CMD, add, remove)).toEqual(want);
    expect(CMD).toEqual(before);
  });

  it('removes first and adds then, whatever the order of the lists', () => {
    expect(applyFlags(['--a=1'], ['--a=1'], ['--a'])).toEqual(['--a=1']);
    expect(applyFlags([], ['--x'], ['--x'])).toEqual(['--x']);
  });
});

describe('the flags of a text field', () => {
  it('are one per line, without the empty lines', () => {
    expect(parseFlags('--a=1\n\n--b 2\n  \n')).toEqual(['--a=1', '--b 2']);
  });

  it('are refused when they are not a flag the agent takes', () => {
    for (const ok of ['--a', '--a=1', '--a 1', '-v', 'positional', '--a=x=y']) expect(validateFlag(ok), ok).toBeNull();
    for (const bad of ['', ' --a', '--a ', '--a 1 2', 'a b', '--a=1 b', '--a\n--b', 'x'.repeat(2049)]) {
      expect(validateFlag(bad), bad).not.toBeNull();
    }

    expect(validateFlags(['--a'], ['--b c'])).toBeNull();
    expect(validateFlags(['--a'], ['b c'])).toMatch(/^remove "b c"/);
    const many = Array.from({ length: MAX_FLAGS / 2 }, (_, i) => `--f${i}`);
    expect(validateFlags(many, many)).toBeNull();
    expect(validateFlags([...many, '--one-more'], many)).toMatch(/more than 64/);
  });
});
