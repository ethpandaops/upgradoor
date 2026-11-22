// A flag override changes the command line of a container: the list of its
// arguments. The agent applies it; this is the same rule, for the preview of
// the dialog and for the mock (the agent's side is pkg/agent).
//
// First the removals, then the additions:
//   remove "--name=value"   every argument equal to it
//   remove "--name"         also every "--name=<anything>", and "--name"
//                           together with the argument after it when that
//                           one does not start with "-"
//   remove "--name value"   exactly that adjacent pair
//   add "--name=value"      appended, unless it is there already
//   add "--name value"      appended as two arguments, unless the pair is there

/** The most flags an override may carry, additions and removals together. */
export const MAX_FLAGS = 64;
/** The most characters of one flag. */
const MAX_FLAG_LENGTH = 2048;

/** FLAG_HELP is the rule in short, for the dialog. */
export const FLAG_HELP: readonly string[] = [
  'Removals are applied first, then additions.',
  'Remove --name=value: every argument equal to it. Remove --name: also every --name=<anything>, and --name with the argument after it when that one does not start with "-". Remove "--name value": exactly that pair.',
  'Add --name=value or --switch: appended at the end unless it is there already. Add "--name value": appended as two arguments, unless the pair is there already.',
  'To change a value: remove --name, add --name=new.',
  `At most ${MAX_FLAGS} flags in total, one per line; at most one blank in a flag, and only in the form "--name value".`,
];

const isPair = (flag: string): boolean => flag.includes(' ');

function removeFlag(args: readonly string[], flag: string): string[] {
  const out: string[] = [];

  if (isPair(flag)) {
    const [name = '', value = ''] = flag.split(' ');
    for (let i = 0; i < args.length; i++) {
      if (args[i] === name && args[i + 1] === value) i++;
      else out.push(args[i] as string);
    }
    return out;
  }

  const byName = flag.startsWith('-') && !flag.includes('=');

  for (let i = 0; i < args.length; i++) {
    const arg = args[i] as string;

    if (arg === flag) {
      // "--name value": the value goes with its name; a flag after a switch stays.
      const next = args[i + 1];
      if (byName && next !== undefined && !next.startsWith('-')) i++;
      continue;
    }
    if (byName && arg.startsWith(`${flag}=`)) continue;

    out.push(arg);
  }

  return out;
}

/** applyFlags returns the command line after an override: the removals, then the additions. */
export function applyFlags(command: readonly string[], add: readonly string[], remove: readonly string[]): string[] {
  let args = [...command];
  for (const flag of remove) args = removeFlag(args, flag);

  for (const flag of add) {
    if (isPair(flag)) {
      // A pair that is there already, next to each other, is not added again.
      const [name = '', value = ''] = flag.split(' ');
      if (!args.some((arg, i) => arg === name && args[i + 1] === value)) args.push(name, value);
    } else if (!args.includes(flag)) args.push(flag);
  }

  return args;
}

/** parseFlags reads the flags of a text field: one per line, empty lines left out. */
export function parseFlags(text: string): string[] {
  return text.split('\n').filter(line => line.trim() !== '');
}

/** validateFlag returns what is wrong with one flag, or null. */
export function validateFlag(flag: string): string | null {
  if (flag === '') return 'is empty';
  if (flag.length > MAX_FLAG_LENGTH) return `is longer than ${MAX_FLAG_LENGTH} characters`;
  if (/[\r\n]/.test(flag)) return 'has a line break';
  if (flag !== flag.trim()) return 'starts or ends with a blank';

  const blanks = flag.split(/\s/).length - 1;
  if (blanks > 1) return 'has more than one blank';
  if (blanks === 1 && !/^-[^\s=]* \S+$/.test(flag)) return 'has a blank, which only the form "--name value" may have';

  return null;
}

/** validateFlags returns the first problem of an override's two lists, or null. */
export function validateFlags(add: readonly string[], remove: readonly string[]): string | null {
  if (add.length + remove.length > MAX_FLAGS) return `more than ${MAX_FLAGS} flags`;

  for (const [list, what] of [
    [remove, 'remove'],
    [add, 'add'],
  ] as const) {
    for (const flag of list) {
      const problem = validateFlag(flag);
      if (problem) return `${what} "${flag.trim().slice(0, 40)}": ${problem}`;
    }
  }

  return null;
}

/**
 * commandDiff sets a command line against the arguments most hosts run:
 * each argument, marked when the base lacks it, then the arguments of the
 * base this command line lacks.
 */
export function commandDiff(
  command: readonly string[],
  base: readonly string[]
): { args: { arg: string; extra: boolean }[]; missing: string[] } {
  const inBase = new Set(base);
  const inCommand = new Set(command);
  return {
    args: command.map(arg => ({ arg, extra: !inBase.has(arg) })),
    missing: base.filter(arg => !inCommand.has(arg)),
  };
}
