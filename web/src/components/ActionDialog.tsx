import { useId, useMemo, useState, type ReactNode } from 'react';

import type { ActionCall } from '../api/client';
import type { ActionResult, Container } from '../api/types';
import { useApp } from '../appContext';
import {
  buildRebootRequest,
  buildRollRequest,
  describeCall,
  describeReboot,
  describeRoll,
  type Timing,
} from '../lib/actionRequests';
import { plural } from '../lib/format';
import { ActionShell, Choice, Field } from './ActionShell';
import { ClientOverrideDialog, type ClientOverrideIntent } from './ClientOverrideDialog';
import { Disclosure, IconButton } from './icons';
import { DURATIONS, OverrideDialog, durationMs, type Duration } from './Overrides';
import { usePermissions } from './Permissions';
import { Segmented, cx } from './primitives';

/** EndChoice is when what a call sets ends: when a person lifts it, or after a duration. */
type EndChoice = 'never' | Duration;

const END_CHOICES: readonly { value: EndChoice; label: string }[] = [
  { value: 'never', label: 'until lifted' },
  ...DURATIONS,
];

/** What the operator asked for; the dialog turns it into one API call. */
export type ActionIntent = ActionIntentKind & {
  /**
   * hiddenHosts are targets of the action that the caller's view does not
   * show right now (selected, then filtered away). The dialog warns about them.
   */
  hiddenHosts?: readonly string[];
};

/** An override of one container on one host: its image, or its command line. It has a dialog of its own. */
interface OverrideOf<K extends string> {
  kind: K;
  host: string;
  /** containers the override can be for: the dialog lets the user pick among them. */
  containers: readonly Container[];
}

type ActionIntentKind =
  | {
      kind: 'roll';
      hosts: readonly string[];
      /** all selects the whole fleet; hosts is then ignored. */
      all: boolean;
      /** containers the operator may limit the roll to. */
      containerChoices: readonly string[];
      /** containers the roll is about: the dialog starts with them ticked. Without them it starts as a roll of the whole host. */
      containers?: readonly string[];
    }
  | { kind: 'reboot'; hosts: readonly string[]; all: boolean }
  | OverrideOf<'image-override'>
  | OverrideOf<'flag-override'>
  | ClientOverrideIntent
  | {
      kind: 'call';
      title: string;
      /** verb is the label of the primary button ("Hold", "Abort"); "Confirm" when absent. */
      verb?: string;
      /** make builds the call from the reason typed in the dialog and the end picked, if offered (ends). */
      make: (reason: string, until?: string) => ActionCall;
      /** reasonRequired: the call is refused without a reason. */
      reasonRequired?: boolean;
      /** ends offers an end for what the call sets: until lifted, or after one of the durations. */
      ends?: boolean;
      danger?: boolean;
      /** leaseHost is the host of the lease an end-lease call is about; the permission to end it follows the host. */
      leaseHost?: string;
      /** rolloutState is the state of the rollout a retry is about: retrying an aborted one is no recovery of hosts. */
      rolloutState?: string;
    };

/** The intents this dialog confirms itself; an override has its own. */
type PlainIntent = Exclude<ActionIntent, { kind: 'image-override' | 'flag-override' | 'client-override' }>;

function intentTitle(intent: PlainIntent): string {
  switch (intent.kind) {
    case 'roll':
      return intent.all
        ? 'Roll the whole fleet'
        : intent.hosts.length === 1
          ? `Roll ${intent.hosts[0] ?? ''}`
          : 'Roll hosts';
    case 'reboot':
      return intent.all
        ? 'Reboot the whole fleet'
        : intent.hosts.length === 1
          ? `Reboot ${intent.hosts[0] ?? ''}`
          : 'Reboot hosts';
    case 'call':
      return intent.title;
  }
}

/** callHosts are the hosts a plain call names, for the target summary. */
function callHosts(call: ActionCall): readonly string[] | undefined {
  return 'hosts' in call.body && Array.isArray(call.body.hosts) ? call.body.hosts : undefined;
}

/**
 * ActionDialog confirms one action. Every dialog has the same structure
 * (ActionShell): what will happen in a sentence, the targets, the fields the
 * action needs, the advanced options folded away, a single primary button.
 * Overrides have dialogs of their own on the same shell.
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
  switch (intent.kind) {
    case 'image-override':
    case 'flag-override':
      return <OverrideDialog intent={intent} onClose={onClose} onDone={onDone} />;
    case 'client-override':
      return <ClientOverrideDialog intent={intent} onClose={onClose} onDone={onDone} />;
    default:
      return <PlainActionDialog intent={intent} onClose={onClose} onDone={onDone} />;
  }
}

function PlainActionDialog({
  intent,
  onClose,
  onDone,
}: {
  intent: PlainIntent;
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
  const [ends, setEnds] = useState<EndChoice>('never');
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
        return intent.make(
          reason.trim(),
          ends === 'never' ? undefined : new Date(Date.now() + durationMs(ends)).toISOString()
        );
    }
  }, [intent, containers, timing, force, ignoreBudget, reason, ends]);

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
  const danger = disruptive ? ignoreBudget : intent.danger === true;

  // The same rule the controller applies to the call as it stands now.
  const verdict = permissions.can(
    call,
    intent.kind === 'call' ? intent.leaseHost : undefined,
    intent.kind === 'call' ? intent.rolloutState : undefined
  );
  const adminHint = permissions.admin ? '' : ` Needs ${permissions.login.service}:admin.`;

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

  let lead: string;
  if (intent.kind === 'call') {
    lead = description[0] ?? '';
  } else {
    const verb = intent.kind === 'roll' ? 'Recreate the containers' : 'Reboot';
    const where = intent.all ? 'every host of the fleet' : intent.hosts.length === 1 ? 'this host' : 'these hosts';
    const how =
      timing === 'now'
        ? 'as fast as the budget and the gate allow'
        : 'staggered: one canary first, then the others once it is back';
    lead =
      intent.kind === 'roll'
        ? `${verb} on ${where} from the tag they already use, ${how}.`
        : `${verb} ${where}, ${how}.`;
  }

  const targets = intent.kind === 'call' ? callHosts(call) : intent.all ? undefined : intent.hosts;

  return (
    <ActionShell
      title={intentTitle(intent)}
      lead={lead}
      targets={targets}
      hidden={intent.hiddenHosts}
      details={description}
      reason={{
        value: reason,
        onChange: setReason,
        required: intent.kind === 'call' && intent.reasonRequired === true,
        onEnter: () => void submit(),
      }}
      verdict={verdict}
      error={error}
      busy={busy}
      onClose={onClose}
      primary={{
        label: danger && disruptive ? 'Confirm, ignoring the budget' : intentVerb(intent),
        danger,
        onClick: () => void submit(),
      }}
      advanced={
        disruptive && (
          <>
            <fieldset className="flex flex-col gap-1.5">
              <legend className="sr-only">When</legend>
              <Choice
                type="radio"
                name={`${id}-timing`}
                checked={timing === 'staggered'}
                onChange={() => {
                  setTiming('staggered');
                }}
                label="Staggered"
                hint="One canary, then everybody else, each with a ready check."
              />
              <Choice
                type="radio"
                name={`${id}-timing`}
                checked={timing === 'now'}
                disabled={mustStagger}
                onChange={() => {
                  setTiming('now');
                }}
                label="Now"
                hint={`As fast as budget and gate allow; no canary, no ready check.${mustStagger ? adminHint : ''}`}
              />
            </fieldset>
            <Choice
              type="checkbox"
              checked={timing === 'staggered' && force}
              disabled={timing !== 'staggered' || !permissions.admin}
              onChange={() => {
                setForce(f => !f);
              }}
              label="Force"
              hint={`Skip the order and the ready check (never the budget).${adminHint}`}
            />
            <div
              className={cx(
                'rounded-sm border px-2 py-1.5',
                ignoreBudget ? 'border-danger bg-danger-subtle' : 'border-border'
              )}
            >
              <Choice
                type="checkbox"
                checked={ignoreBudget}
                disabled={!permissions.admin}
                onChange={() => {
                  setIgnoreBudget(v => !v);
                }}
                label={<span className="text-danger">Ignore budget</span>}
                hint={`Dangerous: also skips the host limit and the network gate; audited.${adminHint}`}
              />
            </div>
          </>
        )
      }
    >
      {intent.kind === 'call' && intent.ends === true && (
        <Field label="Ends">
          <Segmented<EndChoice> label="Ends" value={ends} options={END_CHOICES} onChange={setEnds} disabled={busy} />
        </Field>
      )}
      {intent.kind === 'roll' && intent.containerChoices.length > 1 && (
        <fieldset className="flex flex-col gap-1">
          <legend className="text-xs/5 font-medium">
            Containers <span className="font-normal text-muted">(none ticked: every managed container)</span>
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
    </ActionShell>
  );
}

/** intentVerb is the label of the primary button: what the action does, in a word or two. */
function intentVerb(intent: PlainIntent): string {
  switch (intent.kind) {
    case 'roll':
      return 'Roll';
    case 'reboot':
      return 'Reboot';
    case 'call':
      return intent.verb ?? 'Confirm';
  }
}

/** The most rejected hosts an answer lists; the rest is counted. */
const MAX_REJECTED = 200;

/** ActionNotice shows the answer of the last action until dismissed: what was sent, and every host that was not. */
function ActionNotice({ result, onDismiss }: { result: ActionResult; onDismiss: () => void }) {
  const rejected = Object.entries(result.rejected ?? {}).sort(([a], [b]) => a.localeCompare(b));

  return (
    <div
      role="status"
      className={cx(
        'flex items-start gap-2 rounded-sm border px-3 py-2 text-xs/5',
        rejected.length > 0 ? 'border-warning bg-warning-subtle' : 'border-success bg-success-subtle'
      )}
    >
      <div className="flex min-w-0 grow flex-col gap-1">
        <span className="font-medium whitespace-normal">{result.message}</span>
        {result.rollout && (
          <span className="text-muted">
            rollout <span className="font-mono">{result.rollout}</span>
          </span>
        )}
        {rejected.length > 0 && (
          <Disclosure
            defaultOpen={rejected.length <= 5}
            summary={<span className="text-warning">{plural(rejected.length, 'host')} not sent</span>}
          >
            <ul aria-label="Hosts not sent" className="flex max-h-48 flex-col overflow-y-auto">
              {rejected.slice(0, MAX_REJECTED).map(([host, why]) => (
                <li key={host} className="whitespace-normal">
                  <span className="font-mono font-medium">{host}</span>: <span className="text-muted">{why}</span>
                </li>
              ))}
            </ul>
            {rejected.length > MAX_REJECTED && (
              <span className="text-muted">and {rejected.length - MAX_REJECTED} more</span>
            )}
          </Disclosure>
        )}
      </div>
      <IconButton icon="close" label="Dismiss" onClick={onDismiss} />
    </div>
  );
}

/**
 * useActions holds the open action dialog and the last result for one view.
 * What an action changed arrives over the event stream.
 */
export function useActions(): {
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
        }}
      />
    ),
  };
}
