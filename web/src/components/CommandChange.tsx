import { useState } from 'react';

import type { CommandDelta } from '../api/types';
import { argChanges, deltaSummary, type ArgChange } from '../lib/commandDelta';
import { Disclosure } from './icons';
import { cx } from './primitives';

/** CLIP is how many characters of a long argument or list item show until it is expanded. */
const CLIP = 48;

/** Clipped shows a long value by its start; the whole is the tooltip and one click away. */
export function Clipped({ value, className }: { value: string; className?: string }) {
  const [open, setOpen] = useState(false);

  if (value.length <= CLIP) return <span className={cx('font-mono wrap-anywhere', className)}>{value}</span>;

  return (
    <span className={cx('font-mono wrap-anywhere', className)} title={open ? undefined : value}>
      {open ? value : `${value.slice(0, CLIP)}…`}{' '}
      <button
        type="button"
        onClick={() => {
          setOpen(!open);
        }}
        className="font-sans text-muted underline hover:text-foreground"
      >
        {open ? 'less' : `all ${value.length} characters`}
      </button>
    </span>
  );
}

const SIGN = { added: '+', removed: '−' } as const;
const TONE = { added: 'text-success', removed: 'text-danger' } as const;

function Arg({ kind, value }: { kind: 'added' | 'removed'; value: string }) {
  return (
    <li className="flex gap-1.5">
      <span className={cx('shrink-0 font-mono', TONE[kind])} aria-label={kind}>
        {SIGN[kind]}
      </span>
      <Clipped value={value} />
    </li>
  );
}

function ChangeLine({ change }: { change: ArgChange }) {
  switch (change.kind) {
    case 'added':
    case 'removed':
      return <Arg kind={change.kind} value={change.arg} />;
    case 'value':
      return (
        <li className="flex flex-wrap items-baseline gap-x-1.5">
          <span className="font-mono">{change.name}</span>
          <Clipped value={change.from} className="text-danger line-through" />
          <span className="text-muted" aria-hidden="true">
            →
          </span>
          <Clipped value={change.to} className="text-success" />
        </li>
      );
    case 'list':
      return (
        <li className="flex flex-col">
          <span className="flex flex-wrap items-baseline gap-x-1.5">
            <span className="font-mono">{change.name}</span>
            <span className="text-muted">
              {change.from} → {change.to} items
              {change.cut ? ' (cut short by the agent; the last item is left out)' : ''}
            </span>
          </span>
          <ul aria-label={`Items of ${change.name}`} className="flex flex-col pl-3">
            {change.removed.map(item => (
              <Arg key={`-${item}`} kind="removed" value={item} />
            ))}
            {change.added.map(item => (
              <Arg key={`+${item}`} kind="added" value={item} />
            ))}
            {change.added.length + change.removed.length === 0 && <li className="text-muted">same items</li>}
          </ul>
        </li>
      );
  }
}

/**
 * CommandChange says how a soft deploy changes a container: the image it ran
 * before, and, folded away, the arguments its command line gains and loses.
 * Without a delta (an older agent) it says only that the command line changes.
 */
export function CommandChange({
  fromImage,
  flags,
  command,
  label,
}: {
  fromImage?: string;
  flags?: boolean;
  command?: CommandDelta;
  label: string;
}) {
  return (
    <>
      {fromImage && (
        <span className="text-muted">
          from <span className="font-mono wrap-anywhere">{fromImage}</span>
        </span>
      )}
      {command ? (
        <div className="basis-full">
          <Disclosure
            summary={
              <span className="text-warning">
                command line changed <span className="font-mono">{deltaSummary(command)}</span>
              </span>
            }
          >
            <ul aria-label={label} className="flex flex-col gap-0.5 font-normal">
              {argChanges(command).map(c => (
                <ChangeLine key={`${c.kind} ${'arg' in c ? c.arg : c.name}`} change={c} />
              ))}
            </ul>
            {command.truncated && (
              <span className="text-muted">
                The agent reports a bounded change: some arguments are cut or left out.
              </span>
            )}
          </Disclosure>
        </div>
      ) : (
        flags && <span className="text-warning">command line changed</span>
      )}
    </>
  );
}
