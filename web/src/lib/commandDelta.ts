// The command line change of a soft deploy, as the agent reports it: the
// arguments gained and lost. An argument lost and one gained under the same
// "--name=" read as one changed value; a value that is a comma-separated list
// (bootnodes, peers) reads as the items it gains and loses.

import { COMMAND_DELTA_CUT, list, type CommandDelta } from '../api/types';

/** ArgChange is one line of a command line change. */
export type ArgChange =
  | { kind: 'added' | 'removed'; arg: string }
  | { kind: 'value'; name: string; from: string; to: string }
  | {
      kind: 'list';
      name: string;
      added: string[];
      removed: string[];
      /** The items before and after; a cut list counts what it shows. */
      from: number;
      to: number;
      /** A side was cut short by the agent: its last item is left out. */
      cut: boolean;
    };

/** flagName is the "--name" of a "--name=value" argument, or null. */
function flagName(arg: string): string | null {
  if (!arg.startsWith('-')) return null;
  const at = arg.indexOf('=');
  return at > 0 ? arg.slice(0, at) : null;
}

/** items splits a list value; the item an agent's cut ends in is left out. */
function items(value: string, cut: boolean): string[] {
  const out = value.split(',').filter(item => item !== '');
  if (cut) out.pop();
  return out;
}

/** changeOfPair is how one named argument changes its value. */
function changeOfPair(name: string, from: string, to: string, truncated: boolean): ArgChange {
  const fromValue = from.slice(name.length + 1);
  const toValue = to.slice(name.length + 1);

  if (!fromValue.includes(',') && !toValue.includes(',')) return { kind: 'value', name, from: fromValue, to: toValue };

  const fromCut = truncated && from.endsWith(COMMAND_DELTA_CUT);
  const toCut = truncated && to.endsWith(COMMAND_DELTA_CUT);
  const before = items(fromValue, fromCut);
  const after = items(toValue, toCut);
  const inBefore = new Set(before);
  const inAfter = new Set(after);

  return {
    kind: 'list',
    name,
    added: after.filter(item => !inBefore.has(item)),
    removed: before.filter(item => !inAfter.has(item)),
    from: before.length,
    to: after.length,
    cut: fromCut || toCut,
  };
}

/**
 * argChanges reads a command delta as lines: the changed values of the names
 * that are lost and gained once each, then the arguments gained, then those
 * lost.
 */
export function argChanges(delta: CommandDelta): ArgChange[] {
  const added = list(delta.added);
  const removed = list(delta.removed);
  const truncated = delta.truncated === true;

  const count = (args: readonly string[]): Map<string, number> => {
    const out = new Map<string, number>();
    for (const arg of args) {
      const name = flagName(arg);
      if (name) out.set(name, (out.get(name) ?? 0) + 1);
    }
    return out;
  };
  const addedNames = count(added);
  const removedNames = count(removed);
  const paired = (name: string | null): name is string =>
    name !== null && addedNames.get(name) === 1 && removedNames.get(name) === 1;

  const out: ArgChange[] = [];

  for (const to of added) {
    const name = flagName(to);
    if (!paired(name)) continue;
    const from = removed.find(arg => flagName(arg) === name) ?? '';
    out.push(changeOfPair(name, from, to, truncated));
  }
  for (const arg of added) if (!paired(flagName(arg))) out.push({ kind: 'added', arg });
  for (const arg of removed) if (!paired(flagName(arg))) out.push({ kind: 'removed', arg });

  return out;
}

/** deltaSummary counts a delta's arguments for its one-line summary: "+2 −1". */
export function deltaSummary(delta: CommandDelta): string {
  return `+${list(delta.added).length} −${list(delta.removed).length}`;
}
