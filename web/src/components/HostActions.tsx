import { hostActionIntent, runnableActions, securityAction, verbBlocked, type ActionHost } from '../lib/hostActions';
import type { ActionIntent } from './ActionDialog';
import { ActionMenu, Icon } from './icons';
import { usePermissions } from './Permissions';
import { cx } from './primitives';

/**
 * HostActionsMenu lists the host actions of one host that the login may
 * start; each opens the dialog that starts it. Nothing is shown when there
 * is none.
 */
export function HostActionsMenu({ host, onAction }: { host: ActionHost; onAction: (intent: ActionIntent) => void }) {
  const { login } = usePermissions();
  const actions = runnableActions(login, host);
  if (actions.length === 0) return null;

  const blocked = verbBlocked(host);

  return (
    <ActionMenu
      label={`Host actions of ${host.name}`}
      text="Actions"
      onAction={onAction}
      items={actions.map(a => ({
        icon: 'action',
        label: a.due ? `${a.name} (due)` : a.name,
        hint: a.description,
        blocked,
        intent: hostActionIntent(login, [host], a.name),
      }))}
    />
  );
}

/**
 * SecurityUpdates is the shortcut to the host's security updates action, next
 * to its pending package counts: highlighted while the action is due.
 */
export function SecurityUpdates({ host, onAction }: { host: ActionHost; onAction: (intent: ActionIntent) => void }) {
  const { login } = usePermissions();
  const action = securityAction(login, host);
  if (!action) return null;

  const blocked = verbBlocked(host);
  const label = `Run ${action.name} on ${host.name}`;
  const title = blocked || `${label}${action.due ? ': security updates are due' : ''}`;

  return (
    <span title={title} className="inline-flex">
      <button
        type="button"
        aria-label={label}
        title={title}
        disabled={blocked !== ''}
        onClick={() => {
          onAction(hostActionIntent(login, [host], action.name));
        }}
        className={cx(
          'inline-flex h-6 items-center gap-1 rounded-sm border px-1.5 text-xs/5 font-medium whitespace-nowrap',
          'disabled:cursor-not-allowed disabled:opacity-40',
          action.due
            ? 'border-warning bg-warning-subtle text-warning enabled:hover:bg-surface-hover'
            : 'border-transparent text-muted enabled:hover:border-border-strong enabled:hover:text-foreground'
        )}
      >
        <Icon name="action" className="size-3.5" />
        {action.due ? 'Update now' : 'Update'}
      </button>
    </span>
  );
}
