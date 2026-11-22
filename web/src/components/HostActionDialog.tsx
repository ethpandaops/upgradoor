import { useMemo, useState } from 'react';

import type { ActionCall } from '../api/client';
import type { ActionResult, HostActionRequest } from '../api/types';
import { useApp } from '../appContext';
import { describeCall } from '../lib/actionRequests';
import { plural } from '../lib/format';
import type { SelectionAction } from '../lib/hostActions';
import type { Verdict } from '../lib/permissions';
import { ActionShell, Choice, Field } from './ActionShell';
import { INPUT_CLASS, cx } from './primitives';

/** HostActionIntent starts one configured host action on hosts. */
export interface HostActionIntent {
  kind: 'host-action';
  hosts: readonly string[];
  /** choices are the actions the login may start on some of the hosts. */
  choices: readonly SelectionAction[];
  /** action is the choice the dialog starts with; the first one without it. */
  action?: string;
  hiddenHosts?: readonly string[];
}

/** intentVerdict says whether the dialog can start anything: some host offers the login an action. */
export function intentVerdict(intent: HostActionIntent, choice?: SelectionAction): Verdict {
  const ok = choice ? choice.hosts.length > 0 : intent.choices.some(c => c.hosts.length > 0);
  if (ok) return { allowed: true, reason: '', by: 'action', staggered: false };
  return {
    allowed: false,
    reason:
      intent.choices.length === 0
        ? 'No selected host reports an action you may start'
        : 'You may start this action on none of the selected hosts',
    by: '',
    staggered: false,
  };
}

/**
 * HostActionDialog starts a host action: the action, chosen among those the
 * hosts offer, on the hosts as one staggered rollout, optionally paused after
 * its canary. Hosts that do not offer it come back rejected.
 */
export function HostActionDialog({
  intent,
  onClose,
  onDone,
}: {
  intent: HostActionIntent;
  onClose: () => void;
  onDone: (result: ActionResult) => void;
}) {
  const { api } = useApp();
  const [action, setAction] = useState(intent.action ?? intent.choices[0]?.name ?? '');
  const [pauseAfterCanary, setPauseAfterCanary] = useState(false);
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const choice = intent.choices.find(c => c.name === action);
  const many = intent.hosts.length > 1;

  const call = useMemo<ActionCall>(() => {
    const body: HostActionRequest = { action, hosts: [...intent.hosts], reason: reason.trim() };
    if (many) body.pauseAfterCanary = pauseAfterCanary;
    return { action: 'host-action', body };
  }, [action, intent.hosts, many, pauseAfterCanary, reason]);

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

  const left = choice ? intent.hosts.length - choice.hosts.length : 0;
  const where = many ? 'these hosts' : 'this host';
  const title = intent.action
    ? `Run ${intent.action}${many ? '' : ` on ${intent.hosts[0] ?? ''}`}`
    : `Run an action${many ? '' : ` on ${intent.hosts[0] ?? ''}`}`;

  return (
    <ActionShell
      title={title}
      lead={
        <>
          Run <span className="font-mono">{action || '…'}</span> on {where}
          {many ? ', staggered: one canary first, then the others once it is back.' : ' under a lease.'}
        </>
      }
      targets={intent.hosts}
      hidden={intent.hiddenHosts}
      details={describeCall(call)}
      reason={{ value: reason, onChange: setReason, required: false, onEnter: () => void submit() }}
      verdict={intentVerdict(intent, choice)}
      error={error}
      busy={busy}
      onClose={onClose}
      primary={{ label: 'Start', disabled: action === '', onClick: () => void submit() }}
      advanced={
        many && (
          <Choice
            type="checkbox"
            checked={pauseAfterCanary}
            onChange={() => {
              setPauseAfterCanary(v => !v);
            }}
            label="Pause after the canary"
            hint="The rollout stops when its first host is done, until a person resumes it."
          />
        )
      }
    >
      {intent.choices.length > 1 && (
        <Field label="Action">
          <select
            value={action}
            disabled={busy}
            onChange={e => {
              setAction(e.target.value);
            }}
            className={cx(INPUT_CLASS, 'font-mono')}
          >
            {intent.choices.map(c => (
              <option key={c.name} value={c.name}>
                {c.name} ({c.hosts.length} of {intent.hosts.length})
              </option>
            ))}
          </select>
        </Field>
      )}
      {choice?.description && <p className="text-xs/5 text-muted">{choice.description}</p>}
      {left > 0 && (
        <p role="note" className="text-xs/5 text-warning">
          {plural(left, 'selected host does', 'selected hosts do')} not offer {action} to you: the controller rejects{' '}
          {left === 1 ? 'it' : 'them'}.
        </p>
      )}
    </ActionShell>
  );
}
