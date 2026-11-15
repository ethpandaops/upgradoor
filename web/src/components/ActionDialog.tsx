import { useId, useMemo, useState, type ReactNode } from 'react';

import { errorText, type ActionCall } from '../api/client';
import type { ActionResult } from '../api/types';
import { useApp } from '../appContext';
import {
  buildRebootRequest,
  buildRollRequest,
  describeCall,
  describeReboot,
  describeRoll,
  type Timing,
} from '../lib/actionRequests';
import { hostList, plural } from '../lib/format';
import { Dialog } from './Dialog';
import { usePermissions } from './Permissions';
import { Button, INPUT_CLASS, cx } from './primitives';

/** What the operator asked for; the dialog turns it into one API call. */
export type ActionIntent = ActionIntentKind & {
  /**
   * hiddenHosts are targets of the action that the caller's view does not
   * show right now (selected, then filtered away). The dialog warns about them.
   */
  hiddenHosts?: readonly string[];
};

type ActionIntentKind =
  | {
      kind: 'roll';
      hosts: readonly string[];
      /** all selects the whole fleet; hosts is then ignored. */
      all: boolean;
      /** containers the operator may limit the roll to. */
      containerChoices: readonly string[];
    }
  | { kind: 'reboot'; hosts: readonly string[]; all: boolean }
  | {
      kind: 'call';
      title: string;
      /** make builds the call from the reason typed in the dialog. */
      make: (reason: string) => ActionCall;
      danger?: boolean;
      /** leaseHost is the host of the lease an end-lease call is about; the permission to end it follows the host. */
      leaseHost?: string;
      /** rolloutState is the state of the rollout a retry is about: retrying an aborted one is no recovery of hosts. */
      rolloutState?: string;
    };

function intentTitle(intent: ActionIntent): string {
  switch (intent.kind) {
    case 'roll':
      return intent.all ? 'Roll the whole fleet' : `Roll ${plural(intent.hosts.length, 'host')}`;
    case 'reboot':
      return intent.all ? 'Reboot the whole fleet' : `Reboot ${plural(intent.hosts.length, 'host')}`;
    case 'call':
      return intent.title;
  }
}

/**
 * ActionDialog confirms one action. It states exactly what will happen, takes
 * an optional reason, and for roll / reboot the timing, force and the
 * explicit "ignore budget" switch.
 */
export function ActionDialog({
  intent,
  onClose,
  onDone,
}: {
  intent: ActionIntent;
  onClose: () => void;
  onDone: (result: ActionResult) => void;
}) {
  const { api } = useApp();
  const permissions = usePermissions();
  const id = useId();

  // Through a team feature alone the controller runs a roll or reboot
  // staggered, whatever is asked: "now" is not offered then.
  const mustStagger = intent.kind !== 'call' && permissions.canIntent(intent).staggered;
  const [timing, setTiming] = useState<Timing>(
    intent.kind !== 'call' && (mustStagger || intent.all || intent.hosts.length > 1) ? 'staggered' : 'now'
  );
  const [force, setForce] = useState(false);
  const [ignoreBudget, setIgnoreBudget] = useState(false);
  // A login that may roll only some of the containers starts with those.
  const [containers, setContainers] = useState<readonly string[]>(() => permissions.ownContainers(intent));
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const call = useMemo<ActionCall>(() => {
    switch (intent.kind) {
      case 'roll':
        return {
          action: 'roll',
          body: buildRollRequest({
            hosts: intent.hosts,
            all: intent.all,
            containers,
            timing,
            force,
            ignoreBudget,
            reason,
          }),
        };
      case 'reboot':
        return {
          action: 'reboot',
          body: buildRebootRequest({ hosts: intent.hosts, all: intent.all, timing, force, ignoreBudget, reason }),
        };
      case 'call':
        return intent.make(reason.trim());
    }
  }, [intent, containers, timing, force, ignoreBudget, reason]);

  const description = useMemo<string[]>(() => {
    const opts = {
      hosts: intent.kind === 'call' ? [] : intent.hosts,
      all: intent.kind === 'call' ? false : intent.all,
      timing,
      force,
      ignoreBudget,
      reason,
    };
    switch (intent.kind) {
      case 'roll':
        return describeRoll({ ...opts, containers });
      case 'reboot':
        return describeReboot(opts);
      case 'call':
        return describeCall(call);
    }
  }, [intent, call, containers, timing, force, ignoreBudget, reason]);

  const disruptive = intent.kind !== 'call';
  const hiddenHosts = intent.hiddenHosts ?? [];
  const danger = disruptive ? ignoreBudget : intent.danger === true;

  // The same rule the controller applies to the call as it stands now.
  const verdict = permissions.can(
    call,
    intent.kind === 'call' ? intent.leaseHost : undefined,
    intent.kind === 'call' ? intent.rolloutState : undefined
  );
  const adminHint = permissions.admin ? '' : `; needs ${permissions.login.service}:admin`;

  const submit = async () => {
    setBusy(true);
    setError(undefined);
    try {
      onDone(await api.action(call));
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={intentTitle(intent)}
      onClose={busy ? () => undefined : onClose}
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            variant={danger ? 'danger' : 'primary'}
            onClick={() => void submit()}
            disabled={busy || !verdict.allowed}
            title={verdict.allowed ? undefined : verdict.reason}
          >
            {busy ? 'Sending…' : danger && disruptive ? 'Confirm, ignoring the budget' : 'Confirm'}
          </Button>
        </>
      }
    >
      {hiddenHosts.length > 0 && (
        <p role="alert" className="rounded-sm border border-warning bg-warning-subtle px-2 py-1 text-xs/5">
          <strong>Includes {plural(hiddenHosts.length, 'selected host')} hidden by the current filter:</strong>{' '}
          <span title={hiddenHosts.join(', ')}>{hostList(hiddenHosts, 12)}</span>
        </p>
      )}

      {disruptive && !intent.all && intent.hosts.length > 1 && (
        <p className="text-xs/5 text-muted" title={intent.hosts.join(', ')}>
          Selected: {hostList(intent.hosts, 12)}
        </p>
      )}

      {disruptive && (
        <fieldset className="flex flex-col gap-1">
          <legend className="text-xs/5 font-semibold text-muted uppercase">When</legend>
          <Choice
            type="radio"
            name={`${id}-timing`}
            checked={timing === 'now'}
            disabled={mustStagger}
            onChange={() => {
              setTiming('now');
            }}
            label="Now"
            hint={`as fast as budget and gate allow; no canary, no ready check between hosts${mustStagger ? adminHint : ''}`}
          />
          <Choice
            type="radio"
            name={`${id}-timing`}
            checked={timing === 'staggered'}
            onChange={() => {
              setTiming('staggered');
            }}
            label="Staggered"
            hint="a manual rollout: one canary, then everybody else, each with a ready check"
          />
          <Choice
            type="checkbox"
            checked={timing === 'staggered' && force}
            disabled={timing !== 'staggered' || !permissions.admin}
            onChange={() => {
              setForce(f => !f);
            }}
            label="Force"
            hint={`skip the order and the ready check (never the budget)${adminHint}`}
          />
        </fieldset>
      )}

      {intent.kind === 'roll' && intent.containerChoices.length > 0 && (
        <fieldset className="flex flex-col gap-1">
          <legend className="text-xs/5 font-semibold text-muted uppercase">
            Containers <span className="font-normal normal-case">(none ticked = every managed container)</span>
          </legend>
          <div className="flex flex-wrap gap-x-4 gap-y-1">
            {intent.containerChoices.map(name => (
              <Choice
                key={name}
                type="checkbox"
                checked={containers.includes(name)}
                onChange={() => {
                  setContainers(prev => (prev.includes(name) ? prev.filter(c => c !== name) : [...prev, name]));
                }}
                label={<span className="font-mono">{name}</span>}
              />
            ))}
          </div>
        </fieldset>
      )}

      {disruptive && (
        <div
          className={cx(
            'rounded-sm border px-2 py-1.5',
            ignoreBudget ? 'border-danger bg-danger-subtle' : 'border-border-strong'
          )}
        >
          <Choice
            type="checkbox"
            checked={ignoreBudget}
            disabled={!permissions.admin}
            onChange={() => {
              setIgnoreBudget(v => !v);
            }}
            label={<span className="font-semibold text-danger">Ignore budget</span>}
            hint={`DANGEROUS: also skips the host limit and the network gate; audited${adminHint}`}
          />
        </div>
      )}

      <div
        aria-label="What will happen"
        className={cx(
          'flex flex-col gap-1.5 rounded-sm border px-2 py-1.5 text-xs/5',
          danger ? 'border-danger' : 'border-border'
        )}
      >
        <span className="font-semibold text-muted uppercase">What will happen</span>
        {description.map(line => (
          <p key={line} className={line.startsWith('IGNORE BUDGET') ? 'font-semibold text-danger' : undefined}>
            {line}
          </p>
        ))}
      </div>

      <label className="flex flex-col gap-1 text-xs/5">
        <span className="font-semibold text-muted uppercase">Reason (optional, recorded in the audit log)</span>
        <input
          data-autofocus
          type="text"
          value={reason}
          maxLength={500}
          onChange={e => {
            setReason(e.target.value);
          }}
          onKeyDown={e => {
            if (e.key === 'Enter' && !busy && !danger && verdict.allowed) void submit();
          }}
          className={INPUT_CLASS}
          placeholder="why"
        />
      </label>

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

function Choice({
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
      <span>
        <span className="font-medium">{label}</span>
        {hint && <span className="text-muted"> — {hint}</span>}
      </span>
    </label>
  );
}

/** ActionNotice shows the answer of the last action until dismissed. */
export function ActionNotice({ result, onDismiss }: { result: ActionResult; onDismiss: () => void }) {
  const rejected = Object.entries(result.rejected ?? {});

  return (
    <div
      role="status"
      className={cx(
        'flex flex-wrap items-start gap-2 rounded-sm border px-2 py-1 text-xs/5',
        rejected.length > 0 ? 'border-warning bg-warning-subtle' : 'border-success bg-success-subtle'
      )}
    >
      <div className="flex min-w-0 grow flex-col">
        <span className="font-medium whitespace-normal">{result.message}</span>
        {result.rollout && (
          <span className="text-muted">
            rollout <span className="font-mono">{result.rollout}</span>
          </span>
        )}
        {rejected.map(([host, why]) => (
          <span key={host} className="whitespace-normal text-warning">
            <span className="font-mono">{host}</span>: {why}
          </span>
        ))}
      </div>
      <Button dense variant="ghost" onClick={onDismiss} aria-label="Dismiss">
        ✕
      </Button>
    </div>
  );
}

/**
 * useActions holds the open action dialog and the last result for one view.
 * afterAction runs once an action went through (to refresh the view).
 */
export function useActions(afterAction: () => void): {
  request: (intent: ActionIntent) => void;
  notice: ReactNode;
  dialog: ReactNode;
} {
  const [intent, setIntent] = useState<ActionIntent | null>(null);
  const [result, setResult] = useState<ActionResult | null>(null);

  return {
    request: setIntent,
    notice: result && (
      <ActionNotice
        result={result}
        onDismiss={() => {
          setResult(null);
        }}
      />
    ),
    dialog: intent && (
      <ActionDialog
        intent={intent}
        onClose={() => {
          setIntent(null);
        }}
        onDone={r => {
          setIntent(null);
          setResult(r);
          afterAction();
        }}
      />
    ),
  };
}
