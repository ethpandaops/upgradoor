// Small building blocks shared by every view: buttons, badges, table cells
// and the loading / error / empty states.

import { type ButtonHTMLAttributes, type ReactNode, type TdHTMLAttributes, type ThHTMLAttributes } from 'react';

import { errorText } from '../api/client';

/** cx joins class names, dropping falsy entries. */
export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(' ');
}

export type Tone = 'neutral' | 'success' | 'warning' | 'danger' | 'info' | 'accent';

const TONE_FILL: Record<Tone, string> = {
  neutral: 'border-border bg-surface-raised text-muted',
  success: 'border-transparent bg-success-subtle text-success',
  warning: 'border-transparent bg-warning-subtle text-warning',
  danger: 'border-transparent bg-danger-subtle text-danger',
  info: 'border-transparent bg-info-subtle text-info',
  accent: 'border-transparent bg-surface-selected text-accent',
};

/** Badge is a small state label. */
export function Badge({ tone = 'neutral', title, children }: { tone?: Tone; title?: string; children: ReactNode }) {
  return (
    <span
      title={title}
      className={cx(
        'inline-flex items-center rounded-sm border px-1.5 text-xs/5 font-medium whitespace-nowrap',
        TONE_FILL[tone]
      )}
    >
      {children}
    </span>
  );
}

type ButtonVariant = 'default' | 'primary' | 'danger' | 'ghost';

const BUTTON_VARIANT: Record<ButtonVariant, string> = {
  default: 'border-border-strong bg-surface text-foreground hover:bg-surface-hover',
  primary: 'border-transparent bg-accent text-on-accent hover:bg-accent-hover',
  danger: 'border-danger bg-danger-subtle text-danger hover:bg-surface-hover',
  ghost: 'border-transparent text-muted hover:bg-surface-hover hover:text-foreground',
};

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  /** dense is the in-table size. */
  dense?: boolean;
}

/** Button is the one button style; type defaults to "button". */
export function Button({ variant = 'default', dense = false, className, type, ...rest }: ButtonProps) {
  return (
    <button
      type={type ?? 'button'}
      className={cx(
        'inline-flex items-center justify-center gap-1 rounded-sm border font-medium whitespace-nowrap',
        'disabled:cursor-not-allowed disabled:opacity-50',
        dense ? 'h-6 px-1.5 text-xs/5' : 'h-7 px-2.5 text-xs/5',
        BUTTON_VARIANT[variant],
        className
      )}
      {...rest}
    />
  );
}

/** Table is the dense table shell; it scrolls horizontally on narrow screens. */
export function Table({ children, label }: { children: ReactNode; label: string }) {
  return (
    <div className="overflow-x-auto rounded-sm border border-border bg-surface">
      <table aria-label={label} className="w-full border-collapse text-left text-xs/5">
        {children}
      </table>
    </div>
  );
}

interface CellProps {
  /** tight is the padding of a cell that holds only a checkbox or an icon button. */
  tight?: boolean;
}

export function Th({ className, tight = false, ...rest }: ThHTMLAttributes<HTMLTableCellElement> & CellProps) {
  return (
    <th
      scope="col"
      className={cx(
        'border-b border-border bg-surface-raised py-1 font-medium whitespace-nowrap text-muted',
        tight ? 'px-0.5' : 'px-1.5',
        className
      )}
      {...rest}
    />
  );
}

export function Td({ className, tight = false, ...rest }: TdHTMLAttributes<HTMLTableCellElement> & CellProps) {
  return (
    <td
      className={cx('border-b border-border py-1 align-top whitespace-nowrap', tight ? 'px-0.5' : 'px-1.5', className)}
      {...rest}
    />
  );
}

export type SortDirection = 'asc' | 'desc';

export interface SortColumn {
  label: string;
  active: boolean;
  direction: SortDirection;
  onSort: () => void;
  title?: string;
}

/**
 * SortTh is a column header that sorts when activated. A cell that stacks two
 * values takes two sort columns, stacked the same way.
 */
export function SortTh({ columns }: { columns: readonly SortColumn[] }) {
  const active = columns.find(c => c.active);

  return (
    <Th aria-sort={active ? (active.direction === 'asc' ? 'ascending' : 'descending') : 'none'}>
      <span className="flex flex-col items-start">
        {columns.map(c => (
          <button
            key={c.label}
            type="button"
            title={c.title ?? `Sort by ${c.label}`}
            onClick={c.onSort}
            className={cx(
              'inline-flex items-center gap-0.5 font-medium hover:text-foreground',
              c.active && 'text-foreground'
            )}
          >
            {c.label}
            {c.active && (
              <span aria-hidden="true" className="text-accent">
                {c.direction === 'asc' ? '▲' : '▼'}
              </span>
            )}
          </button>
        ))}
      </span>
    </Th>
  );
}

/** Loading is the first-load placeholder. */
export function Loading({ what }: { what: string }) {
  return (
    <div role="status" className="flex items-center gap-2 px-2 py-6 text-sm/6 text-muted">
      <span
        aria-hidden="true"
        className="size-3 animate-spin rounded-full border-2 border-border-strong border-t-accent"
      />
      Loading {what}…
    </div>
  );
}

/** ErrorBanner shows a failed request; stale data may still be displayed below it. */
export function ErrorBanner({ error, what, onRetry }: { error: unknown; what: string; onRetry?: () => void }) {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-2 rounded-sm border border-danger bg-danger-subtle px-2 py-1 text-xs/5 text-danger"
    >
      <span className="font-medium">Could not load {what}:</span>
      <span className="whitespace-normal">{errorText(error)}</span>
      {onRetry && (
        <Button dense onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}

/** Empty is the "nothing here" state of a list. */
export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-sm border border-dashed border-border px-3 py-6 text-center text-sm/6 text-muted">
      {children}
    </div>
  );
}

/** Section is a titled block inside a tab. */
export function Section({ title, aside, children }: { title: ReactNode; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <h2 className="text-xs/5 font-semibold tracking-wide text-muted uppercase">{title}</h2>
        {aside}
      </div>
      {children}
    </section>
  );
}

/** Mono renders versions and identifiers in the monospace face. */
export function Mono({ children, title }: { children: ReactNode; title?: string }) {
  return (
    <span title={title} className="font-mono">
      {children}
    </span>
  );
}

/** Dash is the placeholder of an empty cell. */
export function Dash() {
  return <span className="text-faint">–</span>;
}

/** TextInput is the one text field style. */
export const INPUT_CLASS =
  'h-7 rounded-sm border border-border-strong bg-surface px-2 text-xs/5 text-foreground placeholder:text-faint disabled:opacity-50';
