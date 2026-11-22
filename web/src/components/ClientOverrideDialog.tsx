import { useState } from 'react';

import type { ActionCall } from '../api/client';
import { list, type ActionResult, type ClientOverrideRequest, type ClientView, type ContainerKind } from '../api/types';
import { useApp } from '../appContext';
import { applyFlags, parseFlags, validateFlags } from '../lib/flags';
import { absoluteTime, plural } from '../lib/format';
import { validateImageRef } from '../lib/imageRef';
import { ActionShell, Choice, Field } from './ActionShell';
import { DURATIONS, durationMs, type Duration } from './Overrides';
import { usePermissions } from './Permissions';
import { INPUT_CLASS, Segmented, cx } from './primitives';

/** An override of one client's containers on many hosts: its image, its flags, or clearing both. */
export interface ClientOverrideIntent {
  kind: 'client-override';
  part: 'image' | 'flags' | 'clear';
  client: ClientView;
  /** hosts are the targets: every host of the client, or the selection. */
  hosts: readonly string[];
}

/** FOR says each end in words, for the lead sentence. */
const FOR: Record<Duration, string> = {
  '1h': 'an hour',
  '6h': '6 hours',
  '24h': '24 hours',
  '3d': '3 days',
  '7d': '7 days',
};

const KIND_ORDER: readonly ContainerKind[] = ['el', 'cl', 'vc'];

export const KIND_LABEL: Record<string, string> = { el: 'execution', cl: 'beacon', vc: 'validator' };

/** clientKinds are the container kinds of a client, in the page's order. */
export function clientKinds(client: ClientView): ContainerKind[] {
  const kinds = new Set(list(client.images).map(i => i.kind));
  return KIND_ORDER.filter(k => kinds.has(k));
}

/** overrideCall is the request of one part for the intent's hosts; the permission check uses it with empty values. */
function overrideCall(
  intent: ClientOverrideIntent,
  kind: ContainerKind,
  part: 'image' | 'flags',
  fields: Partial<ClientOverrideRequest> = {}
): ActionCall {
  return {
    action: 'client-override',
    body: { client: intent.client.name, kind, hosts: [...intent.hosts], part, reason: '', ...fields },
  };
}

/** merge folds the answers of the image and the flags part of a clear into one. */
function merge(results: readonly ActionResult[]): ActionResult {
  if (results.length === 1 && results[0]) return results[0];
  return {
    message: results.map(r => r.message).join(' · '),
    hosts: [...new Set(results.flatMap(r => list(r.hosts)))],
    rejected: Object.assign({}, ...results.map(r => r.rejected ?? {})) as Record<string, string>,
  };
}

/**
 * ClientOverrideDialog asks for a temporary override of a client's
 * containers on many hosts: another image, changed flags, or the way back to
 * the configured ones. It is one request; the controller sends each host its
 * own directive and lists the hosts that cannot take it.
 */
export function ClientOverrideDialog({
  intent,
  onClose,
  onDone,
}: {
  intent: ClientOverrideIntent;
  onClose: () => void;
  onDone: (result: ActionResult) => void;
}) {
  const { api } = useApp();
  const permissions = usePermissions();
  const kinds = clientKinds(intent.client);
  const [kind, setKind] = useState<ContainerKind>(kinds[0] ?? 'cl');
  const [duration, setDuration] = useState<Duration>('24h');
  const [reason, setReason] = useState('');
  const [image, setImage] = useState('');
  const [add, setAdd] = useState('');
  const [remove, setRemove] = useState('');
  const [parts, setParts] = useState<readonly ('image' | 'flags')[]>(() =>
    intent.client.flagOverrides > 0 && intent.client.imageOverrides === 0
      ? ['flags']
      : intent.client.imageOverrides > 0 && intent.client.flagOverrides === 0
        ? ['image']
        : ['image', 'flags']
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(undefined);
  const [opened] = useState(() => Date.now());

  const configured = list(intent.client.images).find(i => i.kind === kind);
  const base = list(configured?.command);
  const addFlags = parseFlags(add);
  const removeFlags = parseFlags(remove);
  const ref = image.trim();
  const name = intent.client.name;
  const what = `${name} ${KIND_LABEL[kind] ?? kind}`;
  const count = plural(intent.hosts.length, 'host');
  const until = new Date(opened + durationMs(duration)).toISOString();

  // What the primary button sends, and what is wrong with the input.
  let calls: ActionCall[];
  let problem: string | null = null;
  let ready: boolean;
  if (intent.part === 'image') {
    calls = [overrideCall(intent, kind, 'image', { image: ref, until, reason: reason.trim() })];
    const invalid = ref === '' ? null : validateImageRef(ref);
    problem = invalid === null ? null : `The image ${invalid}.`;
    ready = ref !== '';
  } else if (intent.part === 'flags') {
    calls = [overrideCall(intent, kind, 'flags', { addFlags, removeFlags, until, reason: reason.trim() })];
    const invalid = validateFlags(addFlags, removeFlags);
    problem = invalid === null ? null : `Not a flag list the agent takes: ${invalid}.`;
    ready = addFlags.length + removeFlags.length > 0;
  } else {
    calls = parts.map(p => overrideCall(intent, kind, p, { reason: reason.trim() }));
    ready = parts.length > 0;
  }

  const verdicts = calls.map(c => permissions.can(c));
  const verdict = verdicts.find(v => !v.allowed) ??
    verdicts[0] ?? { allowed: false, reason: 'Pick what to clear', by: '', staggered: false };

  const submit = async () => {
    setBusy(true);
    setError(undefined);
    try {
      // The end is counted from when the request is sent.
      const sentUntil = new Date(Date.now() + durationMs(duration)).toISOString();
      const results: ActionResult[] = [];
      for (const call of calls) {
        if (call.action !== 'client-override') continue;
        const body = intent.part === 'clear' ? call.body : { ...call.body, until: sentUntil };
        results.push(await api.action({ action: 'client-override', body }));
      }
      onDone(merge(results));
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };

  const title =
    intent.part === 'image'
      ? `Change the image of ${name}`
      : intent.part === 'flags'
        ? `Change the flags of ${name}`
        : `Clear overrides of ${name}`;

  const lead =
    intent.part === 'image'
      ? `Run the ${what} containers on ${count} from another image for ${FOR[duration]}.`
      : intent.part === 'flags'
        ? `Change the command line of the ${what} containers on ${count} for ${FOR[duration]}.`
        : `Put the ${what} containers on ${count} back to their configured ${parts.join(' and ') || 'image and flags'}.`;

  return (
    <ActionShell
      title={title}
      lead={lead}
      targets={intent.hosts}
      verdict={verdict}
      error={error}
      busy={busy}
      onClose={onClose}
      reason={{ value: reason, onChange: setReason, required: intent.part !== 'clear' }}
      primary={{
        label: intent.part === 'image' ? 'Override image' : intent.part === 'flags' ? 'Override flags' : 'Clear',
        disabled: !ready || problem !== null,
        onClick: () => void submit(),
      }}
      details={[
        'The controller sends each host its own override instruction. The agent of the host checks it against what its configuration allows and may refuse it; nothing changes at once: the container is recreated under the host’s next lease, through the budget and the network gate.',
        'Hosts this login may not act on, or that cannot take the override (silent, not allowed, no such container), are listed after sending; the others go ahead.',
        ...(intent.part === 'clear'
          ? []
          : ['When the override ends, each host puts the configured value back, again under a lease.']),
      ]}
    >
      {kinds.length > 1 && (
        <Field label="Container">
          <span className="flex">
            <Segmented<ContainerKind>
              label="Container"
              value={kind}
              options={kinds.map(k => ({ value: k, label: KIND_LABEL[k] ?? k }))}
              onChange={setKind}
              disabled={busy}
            />
          </span>
        </Field>
      )}

      {intent.part === 'image' && (
        <Field
          label="New image"
          hint={
            <>
              Configured: <span className="font-mono break-all">{configured?.image ?? 'unknown'}</span>. Each host
              accepts only images its agent configuration allows.
            </>
          }
        >
          <input
            data-autofocus
            type="text"
            aria-label="Image"
            value={image}
            disabled={busy}
            spellCheck={false}
            onChange={e => {
              setImage(e.target.value);
            }}
            className={cx(INPUT_CLASS, 'font-mono')}
            placeholder={configured ? configured.image.replace(/:[^:/]*$/, ':tag') : 'repository:tag'}
          />
        </Field>
      )}

      {intent.part === 'flags' && (
        <FlagFields
          base={base}
          add={add}
          remove={remove}
          removeFlags={removeFlags}
          addFlags={addFlags}
          onAdd={setAdd}
          onRemove={setRemove}
          busy={busy}
        />
      )}

      {intent.part === 'clear' && (
        <fieldset className="flex flex-col gap-1.5">
          <legend className="mb-1 text-xs/5 font-medium">What goes back to the configuration</legend>
          {(['image', 'flags'] as const).map(p => (
            <Choice
              key={p}
              type="checkbox"
              checked={parts.includes(p)}
              disabled={busy}
              onChange={() => {
                setParts(prev => (prev.includes(p) ? prev.filter(x => x !== p) : [...prev, p]));
              }}
              label={p === 'image' ? 'Image override' : 'Flag override'}
              hint={`${plural(p === 'image' ? intent.client.imageOverrides : intent.client.flagOverrides, 'host')} of ${name} have one now.`}
            />
          ))}
        </fieldset>
      )}

      {intent.part !== 'clear' && (
        <Field label="Ends after" hint="A temporary override: at most 7 days. Then the configured value comes back.">
          <span className="flex flex-wrap items-center gap-2">
            <Segmented<Duration>
              label="Ends after"
              value={duration}
              options={DURATIONS}
              onChange={setDuration}
              disabled={busy}
            />
            <span className="text-muted tabular-nums">{absoluteTime(until)}</span>
          </span>
        </Field>
      )}

      {problem !== null && (
        <p role="alert" className="text-xs/5 text-danger">
          {problem}
        </p>
      )}
    </ActionShell>
  );
}

/** FlagFields: the configured flags (click one to remove it), the flags to add, and what the command line becomes. */
function FlagFields({
  base,
  add,
  remove,
  addFlags,
  removeFlags,
  onAdd,
  onRemove,
  busy,
}: {
  base: readonly string[];
  add: string;
  remove: string;
  addFlags: readonly string[];
  removeFlags: readonly string[];
  onAdd: (v: string) => void;
  onRemove: (v: string) => void;
  busy: boolean;
}) {
  const after = validateFlags(addFlags, removeFlags) === null ? applyFlags(base, addFlags, removeFlags) : base;

  return (
    <>
      <div className="flex flex-col gap-1 text-xs/5">
        <span className="font-medium">
          Configured flags <span className="font-normal text-muted">(click one to remove it)</span>
        </span>
        {base.length === 0 ? (
          <span className="text-muted">The hosts report no arguments.</span>
        ) : (
          <ul aria-label="Configured flags" className="flex max-h-40 flex-wrap gap-1 overflow-y-auto">
            {base.map((arg, i) => (
              <li key={`${i} ${arg}`}>
                <button
                  type="button"
                  disabled={busy}
                  title={removeFlags.includes(arg) ? 'Marked for removal' : 'Remove this flag'}
                  onClick={() => {
                    onRemove(
                      removeFlags.includes(arg)
                        ? removeFlags.filter(f => f !== arg).join('\n')
                        : [...removeFlags, arg].join('\n')
                    );
                  }}
                  className={cx(
                    'rounded-sm border px-1.5 font-mono break-all',
                    removeFlags.includes(arg)
                      ? 'border-danger bg-danger-subtle text-danger line-through'
                      : 'border-border bg-surface-raised hover:border-border-strong'
                  )}
                >
                  {arg}
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <Field label="Add (one per line)">
          <textarea
            data-autofocus
            aria-label="Flags to add"
            rows={3}
            value={add}
            disabled={busy}
            spellCheck={false}
            onChange={e => {
              onAdd(e.target.value);
            }}
            className={cx(INPUT_CLASS, 'h-auto py-1 font-mono')}
            placeholder="--name=value"
          />
        </Field>
        <Field label="Remove (one per line)">
          <textarea
            aria-label="Flags to remove"
            rows={3}
            value={remove}
            disabled={busy}
            spellCheck={false}
            onChange={e => {
              onRemove(e.target.value);
            }}
            className={cx(INPUT_CLASS, 'h-auto py-1 font-mono')}
            placeholder="--name"
          />
        </Field>
      </div>

      {addFlags.length + removeFlags.length > 0 && (
        <div className="flex flex-col gap-1 text-xs/5">
          <span className="font-medium">
            Command line after{' '}
            <span className="font-normal text-muted">(on hosts with the configured flags; new flags marked)</span>
          </span>
          <ul aria-label="Command line after" className="flex max-h-32 flex-wrap gap-1 overflow-y-auto">
            {after.map((arg, i) => (
              <li
                key={`${i} ${arg}`}
                className={cx(
                  'rounded-sm border px-1.5 font-mono break-all',
                  base.includes(arg) ? 'border-border' : 'border-success bg-success-subtle text-success'
                )}
              >
                {arg}
              </li>
            ))}
          </ul>
        </div>
      )}
    </>
  );
}
