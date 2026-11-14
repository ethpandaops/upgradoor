import { createContext, useContext, type ReactNode } from 'react';

import { useNow } from '../hooks/useNow';
import { absoluteTime, isZeroTime, relativeTime } from '../lib/format';

const NowContext = createContext<number | null>(null);

/** NowProvider ticks once a second for every RelTime below it. */
export function NowProvider({ children }: { children: ReactNode }) {
  const now = useNow(1000);
  return <NowContext value={now}>{children}</NowContext>;
}

/** useSharedNow returns the shared clock (or the current time outside a provider). */
export function useSharedNow(): number {
  return useContext(NowContext) ?? Date.now();
}

/** RelTime shows an instant relative to now, with the absolute time on hover. */
export function RelTime({ value, className }: { value: string | null | undefined; className?: string }) {
  const now = useSharedNow();

  if (isZeroTime(value)) return <span className="text-faint">never</span>;

  return (
    <time dateTime={value ?? undefined} title={absoluteTime(value)} className={className}>
      {relativeTime(value, now)}
    </time>
  );
}
