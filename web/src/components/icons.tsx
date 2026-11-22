import { useEffect, useId, useRef, useState, type CSSProperties, type ReactNode } from 'react';

import type { ActionIntent } from './ActionDialog';
import { usePermissions } from './Permissions';
import { cx } from './primitives';

/**
 * The page's icons: one small set of inline SVG strokes, drawn in the text
 * colour, used by every action on Hosts, Clients and Rollouts.
 */
const PATHS = {
  roll: 'M21 12a9 9 0 1 1-2.64-6.36M21 4v5h-5',
  retry: 'M3 12a9 9 0 1 0 2.64-6.36M3 4v5h5',
  reboot: 'M12 3v8M6.34 6.34a8 8 0 1 0 11.32 0',
  hold: 'M9 5v14M15 5v14',
  unhold: 'M7 5l12 7-12 7z',
  offline: 'M4 4l16 16M8.5 8.5a5 5 0 0 0 7 7M12 3v3M3 12h3M18 12h3M12 18v3',
  pause: 'M9 5v14M15 5v14',
  resume: 'M7 5l12 7-12 7z',
  abort: 'M6 6h12v12H6z',
  skip: 'M5 5l10 7-10 7zM19 5v14',
  forget: 'M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3',
  image: 'M12 3l8 4.5v9L12 21l-8-4.5v-9zM4 7.5l8 4.5 8-4.5M12 12v9',
  flags: 'M4 6h9M17 6h3M4 12h3M11 12h9M4 18h11M19 18h1M15 4v4M9 10v4M17 16v4',
  clear: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM9 9l6 6M15 9l-6 6',
  unlock: 'M7 11V7a5 5 0 0 1 9.9-1M5 11h14v10H5z',
  more: 'M5 12h.01M12 12h.01M19 12h.01',
  open: 'M7 17L17 7M8 7h9v9',
  close: 'M6 6l12 12M18 6L6 18',
  chevron: 'M6 9l6 6 6-6',
  check: 'M5 12l5 5 9-10',
  policy: 'M12 3l7 3v6c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z',
  action: 'M13 2L4 14h7l-1 8 9-12h-7z',
} as const;

export type IconName = keyof typeof PATHS;

export function Icon({ name, className }: { name: IconName; className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={name === 'more' ? 3 : 1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      className={cx('size-4 shrink-0', className)}
    >
      <path d={PATHS[name]} />
    </svg>
  );
}

/**
 * IconButton is an action as an icon: its label is the accessible name and
 * the tooltip; a disabled one says why in the tooltip. The tooltip sits on a
 * wrapper, so it shows on a disabled button too.
 */
export function IconButton({
  icon,
  label,
  onClick,
  disabled = false,
  reason,
  tone = 'default',
  pressed,
  className,
}: {
  icon: IconName;
  label: string;
  onClick: () => void;
  disabled?: boolean;
  /** reason is why the button is disabled; it replaces the label in the tooltip. */
  reason?: string;
  tone?: 'default' | 'danger';
  pressed?: boolean;
  className?: string;
}) {
  return (
    <span title={disabled && reason ? `${label}: ${reason}` : label} className="inline-flex">
      <button
        type="button"
        aria-label={label}
        title={disabled && reason ? reason : label}
        aria-pressed={pressed}
        disabled={disabled}
        onClick={onClick}
        className={cx(
          'inline-flex size-7 items-center justify-center rounded-sm border border-transparent',
          'disabled:cursor-not-allowed disabled:opacity-35',
          tone === 'danger'
            ? 'text-danger hover:enabled:border-danger hover:enabled:bg-danger-subtle'
            : 'text-muted hover:enabled:border-border-strong hover:enabled:bg-surface-hover hover:enabled:text-foreground',
          className
        )}
      >
        <Icon name={icon} />
      </button>
    </span>
  );
}

/**
 * ActionIcon opens the dialog of an intent from an icon. It is disabled, and
 * says why, when the login may not make the call the dialog would send or
 * when blocked names another reason.
 */
export function ActionIcon({
  icon,
  label,
  intent,
  onAction,
  blocked,
  tone,
}: {
  icon: IconName;
  label: string;
  intent: ActionIntent;
  onAction: (intent: ActionIntent) => void;
  /** blocked is why the action does not apply here; empty or undefined when it does. */
  blocked?: string;
  tone?: 'default' | 'danger';
}) {
  const verdict = usePermissions().canIntent(intent);
  const reason = blocked || (verdict.allowed ? '' : verdict.reason);

  return (
    <IconButton
      icon={icon}
      label={label}
      tone={tone}
      disabled={reason !== ''}
      reason={reason}
      onClick={() => {
        onAction(intent);
      }}
    />
  );
}

/** One entry of an ActionMenu: an intent (checked against the permissions) or a plain callback. */
interface MenuItem {
  icon: IconName;
  label: string;
  /** hint is a line under the label of an entry that is not blocked. */
  hint?: string;
  intent?: ActionIntent;
  onSelect?: () => void;
  blocked?: string;
  tone?: 'default' | 'danger';
}

/**
 * ActionMenu holds the less frequent actions of a row behind one icon. It
 * opens below the icon, closes on Escape, on a click elsewhere and after a
 * choice; a disabled entry says why under its label.
 */
export function ActionMenu({
  label,
  text,
  items,
  onAction,
}: {
  label: string;
  /** text makes the trigger a labelled button instead of the "more" icon. */
  text?: string;
  items: readonly MenuItem[];
  onAction: (intent: ActionIntent) => void;
}) {
  // The menu is placed on the viewport, so a table that scrolls does not cut it off: below the icon, or above it near the bottom.
  const [place, setPlace] = useState<CSSProperties | null>(null);
  const open = place !== null;
  const ref = useRef<HTMLSpanElement>(null);
  const id = useId();

  const toggle = () => {
    const r = ref.current?.getBoundingClientRect();
    if (open || !r) {
      setPlace(null);
      return;
    }
    const right = Math.max(8, window.innerWidth - r.right);
    setPlace(
      window.innerHeight - r.bottom < 260
        ? { right, bottom: window.innerHeight - r.top + 4 }
        : { right, top: r.bottom + 4 }
    );
  };

  useEffect(() => {
    if (!open) return;
    const close = (e: Event) => {
      if (!ref.current?.contains(e.target as Node)) setPlace(null);
    };
    document.addEventListener('mousedown', close);
    window.addEventListener('scroll', close, true);
    window.addEventListener('resize', close);
    return () => {
      document.removeEventListener('mousedown', close);
      window.removeEventListener('scroll', close, true);
      window.removeEventListener('resize', close);
    };
  }, [open]);

  return (
    <span
      ref={ref}
      className="inline-flex"
      onKeyDown={e => {
        if (e.key === 'Escape' && open) {
          e.stopPropagation();
          setPlace(null);
        }
      }}
    >
      <span title={label} className="inline-flex">
        <button
          type="button"
          aria-label={label}
          aria-haspopup="menu"
          aria-expanded={open}
          aria-controls={open ? id : undefined}
          onClick={toggle}
          className={cx(
            'inline-flex items-center justify-center rounded-sm border',
            text ? 'h-7 gap-1.5 px-2 text-xs/5 font-medium text-foreground' : 'size-7 text-muted',
            open
              ? 'border-border-strong bg-surface-hover text-foreground'
              : 'border-transparent hover:border-border-strong hover:bg-surface-hover hover:text-foreground'
          )}
        >
          {text ? (
            <>
              <Icon name="action" />
              {text}
              <Icon name="chevron" className="size-3.5" />
            </>
          ) : (
            <Icon name="more" />
          )}
        </button>
      </span>
      {open && (
        <span
          id={id}
          role="menu"
          aria-label={label}
          style={place}
          className="fixed z-40 flex w-64 flex-col rounded-md border border-border-strong bg-surface py-1 shadow-lg"
        >
          {items.map(item => (
            <MenuEntry
              key={item.label}
              item={item}
              onChoose={() => {
                setPlace(null);
                if (item.intent) onAction(item.intent);
                item.onSelect?.();
              }}
            />
          ))}
        </span>
      )}
    </span>
  );
}

function MenuEntry({ item, onChoose }: { item: MenuItem; onChoose: () => void }) {
  const permissions = usePermissions();
  const verdict = item.intent ? permissions.canIntent(item.intent) : undefined;
  const reason = item.blocked || (verdict && !verdict.allowed ? verdict.reason : '');

  return (
    <button
      type="button"
      role="menuitem"
      disabled={reason !== ''}
      onClick={onChoose}
      className={cx(
        'flex items-start gap-2.5 px-3 py-1.5 text-left text-xs/5',
        'enabled:hover:bg-surface-hover disabled:cursor-not-allowed',
        item.tone === 'danger' ? 'text-danger' : 'text-foreground'
      )}
    >
      <Icon name={item.icon} className={cx('mt-0.5', reason !== '' && 'opacity-40')} />
      <span className="flex min-w-0 flex-col">
        <span className={cx('font-medium', reason !== '' && 'opacity-50')}>{item.label}</span>
        {reason !== '' ? (
          <span className="whitespace-normal text-muted">{reason}</span>
        ) : (
          item.hint && <span className="whitespace-normal text-muted">{item.hint}</span>
        )}
      </span>
    </button>
  );
}

/** Disclosure is a calm "more" section: a summary line that folds its content away until asked for. */
export function Disclosure({
  summary,
  children,
  defaultOpen = false,
}: {
  summary: ReactNode;
  children: ReactNode;
  defaultOpen?: boolean;
}) {
  return (
    <details open={defaultOpen} className="group text-xs/5">
      <summary className="flex cursor-pointer list-none items-center gap-1 font-medium text-muted select-none hover:text-foreground [&::-webkit-details-marker]:hidden">
        <Icon name="chevron" className="size-3.5 -rotate-90 transition-transform group-open:rotate-0" />
        {summary}
      </summary>
      <div className="mt-2 flex flex-col gap-2 pl-4.5">{children}</div>
    </details>
  );
}
