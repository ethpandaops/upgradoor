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

/**
 * Table is the table shell; it scrolls horizontally on narrow screens. roomy
 * is the main table of a view: taller rows, cells centred on the row.
 */
export function Table({ children, label, roomy = false }: { children: ReactNode; label: string; roomy?: boolean }) {
  return (
    <div className="overflow-x-auto rounded-lg border border-border bg-surface">
      <table
        aria-label={label}
        className={cx(
          'w-full border-collapse text-left',
          'text-xs/5',
          roomy && '[&_td]:py-2 [&_td]:align-middle [&_th]:py-2'
        )}
      >
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
        'border-b border-border bg-surface-raised py-1.5 font-medium whitespace-nowrap text-muted',
        tight ? 'px-0.5' : 'px-2.5',
        className
      )}
      {...rest}
    />
  );
}

export function Td({ className, tight = false, ...rest }: TdHTMLAttributes<HTMLTableCellElement> & CellProps) {
  return (
    <td
      className={cx(
        'border-b border-border py-1.5 align-top whitespace-nowrap',
        tight ? 'px-0.5' : 'px-2.5',
        className
      )}
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
export function SortTh({ columns, className }: { columns: readonly SortColumn[]; className?: string }) {
  const active = columns.find(c => c.active);

  return (
    <Th className={className} aria-sort={active ? (active.direction === 'asc' ? 'ascending' : 'descending') : 'none'}>
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
    <section className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-3">
        <h2 className="text-sm/6 font-semibold">{title}</h2>
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

/** Card is a titled panel on the page background. */
export function Card({
  title,
  count,
  aside,
  label,
  className,
  children,
}: {
  title: ReactNode;
  /** count is shown muted after the title. */
  count?: number;
  aside?: ReactNode;
  /** label names the region for assistive tech when the title is not plain text. */
  label?: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <section
      aria-label={label ?? (typeof title === 'string' ? title : undefined)}
      className={cx('flex min-w-0 flex-col gap-3 rounded-lg border border-border bg-surface p-4', className)}
    >
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <h2 className="text-sm/6 font-semibold">
          {title}
          {count !== undefined && <span className="font-normal text-faint"> {count}</span>}
        </h2>
        {aside && <div className="ml-auto flex flex-wrap items-center gap-2">{aside}</div>}
      </div>
      {children}
    </section>
  );
}

/** Segmented is a small one-of-n switch. */
export function Segmented<T extends string>({
  label,
  options,
  value,
  onChange,
}: {
  label: string;
  options: readonly { value: T; label: string }[];
  value: T;
  onChange: (value: T) => void;
}) {
  return (
    <div role="group" aria-label={label} className="inline-flex rounded-sm border border-border-strong">
      {options.map(o => (
        <button
          key={o.value}
          type="button"
          aria-pressed={o.value === value}
          onClick={() => {
            onChange(o.value);
          }}
          className={cx(
            'h-6 px-2 text-xs/5 font-medium first:rounded-l-xs last:rounded-r-xs',
            o.value === value ? 'bg-surface-selected text-foreground' : 'text-muted hover:bg-surface-hover'
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

/** Meter is a thin bar: how much of an allowance is used. Past the allowance it turns red. */
export function Meter({
  value,
  max,
  label,
  progress = false,
}: {
  value: number;
  max: number;
  label: string;
  /** progress: the bar fills towards a goal instead of using up an allowance. */
  progress?: boolean;
}) {
  const over = !progress && value > max + 1e-9;
  const share = max > 0 ? Math.min(1, value / max) : value > 0 ? 1 : 0;

  return (
    <span
      role="meter"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={Math.round(max * 10) / 10}
      aria-valuenow={Math.round(value * 10) / 10}
      className="block h-1.5 w-full overflow-hidden rounded-full bg-surface-raised"
    >
      <span
        className={cx(
          'block h-full rounded-full',
          over ? 'bg-danger-solid' : !progress && share > 0.85 ? 'bg-warning' : 'bg-accent'
        )}
        style={{ width: `${share * 100}%` }}
      />
    </span>
  );
}
