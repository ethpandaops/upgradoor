import { createContext, useContext, type ReactNode } from 'react';

import { useNow } from '../hooks/useNow';
import { absoluteTime, isZeroTime, relativeTime, timestamp } from '../lib/format';
import { cx } from './primitives';

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

/**
 * FIXED is the box of a relative time that must not move what is around it:
 * wide enough for the longest text ("59m 59s ago", "364d 23h ago"), so the
 * text can tick from "9s ago" to "1m 10s ago" without resizing a column or
 * pushing a line into a wrap.
 */
const FIXED = 'inline-block min-w-20';

/**
 * RelTime shows an instant relative to now, with the absolute time on hover.
 * The text is computed here from the timestamp and ticks with the shared
 * clock; the controller never sends an elapsed time. fixed reserves the width
 * of the longest text: use it in table cells and wherever the time ends a
 * line that must not wrap.
 */
export function RelTime({
  value,
  className,
  fixed = false,
}: {
  value: string | null | undefined;
  className?: string;
  fixed?: boolean;
}) {
  const now = useSharedNow();

  if (isZeroTime(value)) return <span className={cx('text-faint', fixed && FIXED)}>never</span>;

  return (
    <time
      dateTime={value ?? undefined}
      title={absoluteTime(value)}
      className={cx('whitespace-nowrap tabular-nums', fixed && FIXED, className)}
    >
      {relativeTime(value, now)}
    </time>
  );
}

/**
 * Timestamp shows an instant as a local date and time that stands still, with
 * the UTC time on hover: for logs, where rows are compared with each other
 * and with other logs.
 */
export function Timestamp({ value, className }: { value: string | null | undefined; className?: string }) {
  if (isZeroTime(value)) return <span className="text-faint">never</span>;

  return (
    <time
      dateTime={value ?? undefined}
      title={absoluteTime(value)}
      className={cx('whitespace-nowrap tabular-nums', className)}
    >
      {timestamp(value)}
    </time>
  );
}
