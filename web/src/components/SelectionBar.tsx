import type { ReactNode } from 'react';

import { plural } from '../lib/format';
import type { ActionIntent } from './ActionDialog';
import { Icon, IconButton, type IconName } from './icons';
import { usePermissions } from './Permissions';
import { cx } from './primitives';

/**
 * SelectionBar is the one bar of the actions on selected hosts, on Hosts and
 * Clients. It is always there, with one height, so selecting a row moves
 * nothing; without a selection its actions are disabled and say why.
 */
export function SelectionBar({
  count,
  matched,
  wholeFilter = false,
  onSelectWholeFilter,
  onClear,
  note,
  children,
}: {
  /** count is the number of selected hosts. */
  count: number;
  /** matched is how many hosts the current filter selects, for "select all"; absent where the list is not paged. */
  matched?: number;
  /** wholeFilter: the selection is every host of the filter, not only rows ticked by hand. */
  wholeFilter?: boolean;
  onSelectWholeFilter?: () => void;
  onClear: () => void;
  note?: ReactNode;
  children: ReactNode;
}) {
  const offerWhole = onSelectWholeFilter && matched !== undefined && !wholeFilter && count > 0 && count < matched;

  return (
    <div
      role="group"
      aria-label="Actions on the selected hosts"
      className={cx(
        'flex min-h-10 flex-wrap items-center gap-x-3 gap-y-1 rounded-md border px-2 py-1',
        count > 0 ? 'border-accent bg-surface-selected' : 'border-border bg-surface'
      )}
    >
      <span className="flex min-w-36 items-center gap-1.5 text-xs/5" aria-live="polite">
        <span className={cx('font-semibold tabular-nums', count === 0 && 'text-muted')}>
          {count === 0 ? 'No host selected' : `${plural(count, 'host')} selected`}
        </span>
        {count > 0 && <IconButton icon="close" label="Clear the selection" onClick={onClear} className="size-6" />}
      </span>

      {offerWhole && (
        <button
          type="button"
          onClick={onSelectWholeFilter}
          className="text-xs/5 font-medium text-accent hover:underline"
        >
          Select all {plural(matched, 'matching host')}
        </button>
      )}
      {wholeFilter && <span className="text-xs/5 text-muted">every host of the current filter</span>}
      {note}

      <span className="ml-auto flex flex-wrap items-center gap-1">{children}</span>
    </div>
  );
}

/**
 * BulkAction is one action of the selection bar: an icon with a short label.
 * check is an intent the permission is judged by; run opens the real one
 * (after fetching the names of a whole-filter selection).
 */
export function BulkAction({
  icon,
  label,
  check,
  run,
  blocked,
  tone = 'default',
}: {
  icon: IconName;
  label: string;
  check: ActionIntent | undefined;
  run: () => void;
  blocked?: string;
  tone?: 'default' | 'danger';
}) {
  const permissions = usePermissions();
  if (check && !permissions.offers(check)) return null;

  const verdict = check ? permissions.canIntent(check) : undefined;
  const reason = blocked || (verdict && !verdict.allowed ? verdict.reason : '');

  return (
    <span title={reason ? `${label}: ${reason}` : label} className="inline-flex">
      <button
        type="button"
        title={reason || undefined}
        disabled={reason !== ''}
        onClick={run}
        className={cx(
          'inline-flex h-7 items-center gap-1.5 rounded-sm border px-2 text-xs/5 font-medium whitespace-nowrap max-sm:h-10',
          'disabled:cursor-not-allowed disabled:opacity-40',
          tone === 'danger'
            ? 'border-transparent text-danger enabled:hover:border-danger enabled:hover:bg-danger-subtle'
            : 'border-transparent text-foreground enabled:hover:border-border-strong enabled:hover:bg-surface'
        )}
      >
        <Icon name={icon} />
        {label}
      </button>
    </span>
  );
}

/** IntentAction is a labelled icon action that opens the dialog of one intent: the toolbar form of ActionIcon. */
export function IntentAction({
  intent,
  onAction,
  ...rest
}: {
  icon: IconName;
  label: string;
  intent: ActionIntent;
  onAction: (intent: ActionIntent) => void;
  blocked?: string;
  tone?: 'default' | 'danger';
}) {
  return (
    <BulkAction
      {...rest}
      check={intent}
      run={() => {
        onAction(intent);
      }}
    />
  );
}
