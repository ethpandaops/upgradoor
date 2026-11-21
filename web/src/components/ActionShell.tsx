import type { ReactNode } from 'react';

import { errorText } from '../api/client';
import type { Verdict } from '../lib/permissions';
import { hostList, plural } from '../lib/format';
import { Dialog } from './Dialog';
import { Disclosure } from './icons';
import { Button, INPUT_CLASS, cx } from './primitives';

/** The most names a target list renders; the rest is counted. */
const MAX_LISTED = 300;

/**
 * TargetSummary names what an action acts on without a wall of names: one
 * host by name, several as a count with the names folded away (at most
 * MAX_LISTED of them rendered).
 */
function TargetSummary({
  hosts,
  hidden = [],
  noun = 'host',
}: {
  hosts: readonly string[];
  /** hidden are targets the view behind the dialog does not show right now. */
  hidden?: readonly string[];
  noun?: string;
}) {
  if (hosts.length === 0) return null;

  return (
    <div className="flex flex-col gap-1 rounded-sm bg-surface-raised px-3 py-2 text-xs/5">
      {hosts.length === 1 ? (
        <span>
          <span className="text-muted">On </span>
          <span className="font-mono font-medium">{hosts[0]}</span>
        </span>
      ) : (
        <Disclosure summary={<span className="text-foreground">{plural(hosts.length, noun)}</span>}>
          <ul
            aria-label={`Selected ${noun}s`}
            className="grid max-h-36 grid-cols-[repeat(auto-fill,minmax(10rem,1fr))] gap-x-3 overflow-y-auto font-mono"
          >
            {hosts.slice(0, MAX_LISTED).map(h => (
              <li key={h} className="truncate" title={h}>
                {h}
              </li>
            ))}
          </ul>
          {hosts.length > MAX_LISTED && <span className="text-muted">and {hosts.length - MAX_LISTED} more</span>}
        </Disclosure>
      )}
      {hidden.length > 0 && (
        <span role="alert" className="text-warning">
          {plural(hidden.length, 'of them is', 'of them are')} hidden by the current filter: {hostList(hidden)}
        </span>
      )}
    </div>
  );
}

/** Choice is one radio or checkbox line with a short hint. */
export function Choice({
  type,
  name,
  checked,
  disabled,
  onChange,
  label,
  hint,
}: {
  type: 'radio' | 'checkbox';
  name?: string;
  checked: boolean;
  disabled?: boolean;
  onChange: () => void;
  label: ReactNode;
  hint?: string;
}) {
  return (
    <label className={cx('flex items-baseline gap-2 text-xs/5', disabled && 'opacity-50')}>
      <input
        type={type}
        name={name}
        checked={checked}
        disabled={disabled}
        onChange={onChange}
        className="accent-accent"
      />
      <span className="flex flex-col">
        <span className="font-medium">{label}</span>
        {hint && <span className="text-muted">{hint}</span>}
      </span>
    </label>
  );
}

/** Field is a labelled input of an action dialog. */
export function Field({ label, hint, children }: { label: ReactNode; hint?: ReactNode; children: ReactNode }) {
  return (
    <label className="flex flex-col gap-1 text-xs/5">
      <span className="font-medium text-foreground">{label}</span>
      {children}
      {hint && <span className="text-muted">{hint}</span>}
    </label>
  );
}

/**
 * ActionShell is the one structure of every action dialog: a title, one
 * sentence of what will happen, the targets, the fields the action needs,
 * the details and the advanced options folded away, the reason, and a single
 * primary button.
 */
export function ActionShell({
  title,
  lead,
  targets,
  hidden,
  children,
  details,
  advanced,
  reason,
  verdict,
  error,
  busy,
  primary,
  secondary,
  onClose,
}: {
  title: string;
  /** lead is the one sentence of what will happen. */
  lead: ReactNode;
  targets?: readonly string[];
  hidden?: readonly string[];
  children?: ReactNode;
  /** details are the precise sentences, folded away under "What exactly happens". */
  details?: readonly string[];
  advanced?: ReactNode;
  reason: { value: string; onChange: (v: string) => void; required: boolean; onEnter?: () => void };
  verdict: Verdict;
  error: unknown;
  busy: boolean;
  primary: { label: string; disabled?: boolean; danger?: boolean; onClick: () => void };
  /** secondary is a quieter second way out of the dialog (clear an override). */
  secondary?: { label: string; disabled?: boolean; title?: string; onClick: () => void };
  onClose: () => void;
}) {
  const missingReason = reason.required && reason.value.trim() === '';
  const disabled = busy || !verdict.allowed || primary.disabled === true || missingReason;

  return (
    <Dialog
      title={title}
      onClose={busy ? () => undefined : onClose}
      footer={
        <>
          {secondary && (
            <Button
              variant="ghost"
              className="mr-auto"
              disabled={busy || secondary.disabled}
              title={secondary.title}
              onClick={secondary.onClick}
            >
              {secondary.label}
            </Button>
          )}
          <Button onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant={primary.danger ? 'danger' : 'primary'}
            onClick={primary.onClick}
            disabled={disabled}
            title={
              !verdict.allowed
                ? verdict.reason
                : missingReason
                  ? 'Give a reason: it is recorded with the action'
                  : undefined
            }
          >
            {busy ? 'Sending…' : primary.label}
          </Button>
        </>
      }
    >
      <p className="text-sm/6">{lead}</p>

      {targets && <TargetSummary hosts={targets} hidden={hidden} />}

      {children}

      <Field label={reason.required ? 'Reason' : 'Reason (optional)'}>
        <input
          type="text"
          value={reason.value}
          maxLength={500}
          disabled={busy}
          onChange={e => {
            reason.onChange(e.target.value);
          }}
          onKeyDown={e => {
            if (e.key === 'Enter' && !disabled && !primary.danger) reason.onEnter?.();
          }}
          className={INPUT_CLASS}
          placeholder="Recorded in the audit log"
        />
      </Field>

      {(details?.length || advanced) && (
        <div className="flex flex-col gap-2 border-t border-border pt-3">
          {advanced && <Disclosure summary="Advanced options">{advanced}</Disclosure>}
          {details && details.length > 0 && (
            <Disclosure summary="What exactly happens">
              <div aria-label="What will happen" className="flex flex-col gap-1.5">
                {details.map(line => (
                  <p
                    key={line}
                    className={line.startsWith('IGNORE BUDGET') ? 'font-semibold text-danger' : 'text-muted'}
                  >
                    {line}
                  </p>
                ))}
              </div>
            </Disclosure>
          )}
        </div>
      )}

      {!verdict.allowed && (
        <p role="note" className="rounded-sm border border-warning bg-warning-subtle px-2 py-1 text-xs/5">
          Not permitted: {verdict.reason}
        </p>
      )}

      {error !== undefined && (
        <p role="alert" className="rounded-sm border border-danger bg-danger-subtle px-2 py-1 text-xs/5 text-danger">
          Action failed: {errorText(error)}
        </p>
      )}
    </Dialog>
  );
}
