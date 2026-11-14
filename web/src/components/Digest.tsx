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

/**
 * CopyValue shows a shortened identifier in monospace. The full value is the
 * tooltip; activating it copies the full value.
 */
export function CopyValue({ value, short, label }: { value: string; short: string; label: string }) {
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

  return (
    <button
      type="button"
      title={`${value}\n(click to copy)`}
      aria-label={`${label} ${value}, click to copy`}
      onClick={e => {
        e.stopPropagation();
        void copyText(value).then(ok => {
          setState(ok ? 'copied' : 'failed');
        });
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
