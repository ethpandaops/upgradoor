import { useEffect, useState } from 'react';

import { shortDigest } from '../lib/format';
import { Dash, cx } from './primitives';

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

/** useCopy copies a text and says, for a moment, how that went. */
function useCopy(): ['idle' | 'copied' | 'failed', (text: string) => void] {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');

  useEffect(() => {
    if (state === 'idle') return;
    const timer = setTimeout(() => {
      setState('idle');
    }, 1200);
    return () => {
      clearTimeout(timer);
    };
  }, [state]);

  const copy = (text: string) => {
    void copyText(text).then(ok => {
      setState(ok ? 'copied' : 'failed');
    });
  };

  return [state, copy];
}

/**
 * CopyValue shows a shortened identifier in monospace. The full value is the
 * tooltip; activating it copies the full value.
 */
export function CopyValue({ value, short, label }: { value: string; short: string; label: string }) {
  const [state, copy] = useCopy();

  return (
    <button
      type="button"
      title={`${value}\n(click to copy)`}
      aria-label={`${label} ${value}, click to copy`}
      onClick={e => {
        e.stopPropagation();
        copy(value);
      }}
      className={cx(
        'rounded-xs font-mono hover:bg-surface-hover hover:text-foreground',
        state === 'copied' ? 'text-success' : state === 'failed' ? 'text-danger' : 'text-muted'
      )}
    >
      {state === 'copied' ? 'copied' : state === 'failed' ? 'copy failed' : short}
    </button>
  );
}

/** CopyButton is a small button that copies a text, such as a whole list of flags; its width does not change. */
export function CopyButton({ value, label }: { value: string; label: string }) {
  const [state, copy] = useCopy();

  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      onClick={() => {
        copy(value);
      }}
      className={cx(
        'min-w-16 rounded-xs px-1.5 text-xs/5 font-medium hover:bg-surface-hover',
        state === 'copied' ? 'text-success' : state === 'failed' ? 'text-danger' : 'text-accent'
      )}
    >
      {state === 'copied' ? 'Copied' : state === 'failed' ? 'Copy failed' : 'Copy'}
    </button>
  );
}

/** ShortId shows an identifier (lease, rollout, certificate hash) cut to 12 characters. */
export function ShortId({ value, label }: { value: string | null | undefined; label: string }) {
  if (!value) return <Dash />;
  return <CopyValue value={value} short={value.length > 14 ? value.slice(0, 12) : value} label={label} />;
}

/** Digest shows the first 12 hex characters of a digest; full value on hover, click to copy. */
export function Digest({ value }: { value: string | null | undefined }) {
  if (!value) return <Dash />;
  return <CopyValue value={value} short={shortDigest(value)} label="digest" />;
}
