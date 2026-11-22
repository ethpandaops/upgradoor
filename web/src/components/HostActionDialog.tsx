import { useMemo, useState } from 'react';

import type { ActionCall } from '../api/client';
import { list, type ActionResult, type HostActionRequest } from '../api/types';
import { useApp, usePrivateAccess } from '../appContext';
import { describeCall } from '../lib/actionRequests';
import { plural } from '../lib/format';
import type { SelectionAction } from '../lib/hostActions';
import type { Verdict } from '../lib/permissions';
import { ActionShell, Choice } from './ActionShell';
import { useLive } from './Live';
import { cx } from './primitives';
import { ImpactBadge } from './status';

/** HostActionIntent starts one configured host action on hosts. */
export interface HostActionIntent {
  kind: 'host-action';
  hosts: readonly string[];
  /** choices are the actions the login may start on some of the hosts. */
  choices: readonly SelectionAction[];
  /** action fixes the action; without it the dialog offers every choice and picks none. */
  action?: string;
  hiddenHosts?: readonly string[];
}

/** intentVerdict says whether the dialog can start anything: some host offers the login an action. */
export function intentVerdict(intent: HostActionIntent, choice?: SelectionAction): Verdict {
  const ok = choice ? choice.hosts.length > 0 : intent.choices.some(c => c.hosts.length > 0);
  if (ok) return { allowed: true, reason: '', by: 'action' };
  return {
    allowed: false,
    reason:
      intent.choices.length === 0
        ? 'No selected host reports an action you may start'
        : 'You may start this action on none of the selected hosts',
    by: '',
  };
}

/**
 * HostActionDialog starts a host action: the action, picked among those the
 * hosts offer, on the hosts as one staggered rollout, optionally paused after
 * its canary. Hosts that do not offer it come back rejected. While the
 * network gate is closed it can be told to run anyway.
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
  const priv = usePrivateAccess();
  const plan = useLive('plan', priv);
  const network = useLive('public-network');
  const gate = plan.data?.gate ?? network.data;

  const choices = intent.action ? intent.choices.filter(c => c.name === intent.action) : intent.choices;
  const [action, setAction] = useState(intent.action ?? '');
  const [pauseAfterCanary, setPauseAfterCanary] = useState(false);
  const [ignoreGate, setIgnoreGate] = useState(false);
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const choice = intent.choices.find(c => c.name === action);
  const many = intent.hosts.length > 1;
  const gateClosed = gate !== undefined && !gate.ok;
  const skipGate = gateClosed && ignoreGate;

  const call = useMemo<ActionCall>(() => {
    const body: HostActionRequest = { action, hosts: [...intent.hosts], reason: reason.trim() };
    if (many) body.pauseAfterCanary = pauseAfterCanary;
    if (skipGate) body.ignoreGate = true;
    return { action: 'host-action', body };
  }, [action, intent.hosts, many, pauseAfterCanary, skipGate, reason]);

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
  const details = action
    ? [
        ...describeCall(call),
        ...(skipGate
          ? [
              'Despite the network state: the hosts pass the closed network gate; the budget, the host limit and the order still apply.',
            ]
          : []),
      ]
    : undefined;

  return (
    <ActionShell
      title={`Run ${intent.action ?? 'a host action'}${many ? '' : ` on ${intent.hosts[0] ?? ''}`}`}
      lead={
        <>
          Run <span className="font-mono">{action || 'the action you pick'}</span> on {where}
          {many ? ', staggered: one canary first, then the others once it is back.' : ' under a lease.'}
        </>
      }
      targets={intent.hosts}
      hidden={intent.hiddenHosts}
      details={details}
      reason={{ value: reason, onChange: setReason, required: false, onEnter: () => void submit() }}
      verdict={intentVerdict(intent, choice)}
      error={error}
      busy={busy}
      onClose={onClose}
      primary={{
        label: `Run ${action || '…'} on ${plural(intent.hosts.length, 'host')}`,
        disabled: action === '',
        onClick: () => void submit(),
      }}
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
      <div role="radiogroup" aria-label="Action" className="flex flex-col gap-1.5">
        {choices.map(c => (
          <label
            key={c.name}
            className={cx(
              'flex cursor-pointer items-start gap-2 rounded-md border px-3 py-2 text-xs/5',
              action === c.name ? 'border-accent bg-surface-selected' : 'border-border hover:bg-surface-hover',
              busy && 'cursor-not-allowed opacity-60'
            )}
          >
            <input
              type="radio"
              name="host-action"
              value={c.name}
              checked={action === c.name}
              disabled={busy}
              onChange={() => {
                setAction(c.name);
              }}
              className="mt-1 accent-accent"
            />
            <span className="flex min-w-0 grow flex-col gap-0.5">
              <span className="flex flex-wrap items-center gap-2">
                <span className="font-mono font-medium">{c.name}</span>
                <ImpactBadge impact={c.impact} />
                <span className={cx('ml-auto', c.hosts.length < intent.hosts.length ? 'text-warning' : 'text-muted')}>
                  available on {c.hosts.length} of {intent.hosts.length} selected
                </span>
              </span>
              {c.description && <span className="text-muted">{c.description}</span>}
            </span>
          </label>
        ))}
      </div>
      {left > 0 && (
        <p role="note" className="text-xs/5 text-warning">
          {plural(left, 'selected host does', 'selected hosts do')} not offer {action} to you: the controller rejects{' '}
          {left === 1 ? 'it' : 'them'}.
        </p>
      )}
      {gateClosed && (
        <div className="rounded-sm border border-warning bg-warning-subtle px-2 py-1.5">
          <Choice
            type="checkbox"
            checked={ignoreGate}
            disabled={busy}
            onChange={() => {
              setIgnoreGate(v => !v);
            }}
            label="Roll despite network state"
            hint={`The network gate is closed: ${list(gate.reasons).join(' · ') || 'no reason given'}.`}
          />
        </div>
      )}
    </ActionShell>
  );
}
