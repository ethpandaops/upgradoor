// Overrides of a container on one host: another image, or a changed command
// line, for a while. The host's own file is their source: the page shows what
// the agent reports (container.override) and sends an instruction that the
// agent may refuse and applies under its next lease.

import { useMemo, useState, type ReactNode } from 'react';

import type { ActionCall } from '../api/client';
import type { ActionResult, Container, ContainerOverride } from '../api/types';
import { list } from '../api/types';
import { useApp } from '../appContext';
import { describeCall } from '../lib/actionRequests';
import { FLAG_HELP, applyFlags, parseFlags, validateFlags } from '../lib/flags';
import { absoluteTime } from '../lib/format';
import { imageAllowed, validateImageRef } from '../lib/imageRef';
import type { ActionIntent } from './ActionDialog';
import { ActionShell, Field } from './ActionShell';
import { Digest } from './Digest';
import { ActionIcon, Disclosure } from './icons';
import { usePermissions } from './Permissions';
import { RelTime } from './RelTime';
import { Badge, INPUT_CLASS, Segmented, cx, type Tone } from './primitives';

type OverrideKind = 'image-override' | 'flag-override';
type OverrideIntent = Extract<ActionIntent, { kind: OverrideKind }>;

const KIND_ORDER = ['el', 'cl', 'vc'];

/** byKind orders containers the way the dialogs offer them: execution, consensus, validator client. */
function byKind(containers: readonly Container[]): Container[] {
  const rank = (c: Container) => {
    const i = KIND_ORDER.indexOf(c.kind ?? '');
    return i < 0 ? KIND_ORDER.length : i;
  };
  return [...containers].sort((a, b) => rank(a) - rank(b) || (a.name < b.name ? -1 : 1));
}

/** allows reports whether the agent configuration of the host lets the controller override this on the container. */
export function allows(c: Container, kind: OverrideKind): boolean {
  return kind === 'image-override' ? list(c.overridable?.images).length > 0 : c.overridable?.flags === true;
}

const WHAT: Record<OverrideKind, string> = { 'image-override': 'image', 'flag-override': 'flag' };

/** notAllowed is why a host cannot take an override of these containers; empty when one of them can. */
export function notAllowed(containers: readonly Container[], kind: OverrideKind): string {
  if (containers.some(c => allows(c, kind))) return '';
  const names = containers.map(c => c.name).join(', ') || 'its containers';
  return `This host does not allow ${WHAT[kind]} overrides of ${names} (agent.yaml)`;
}

/** hasFlags reports whether an override changes the command line. */
function hasFlags(o: ContainerOverride | undefined): boolean {
  return list(o?.addFlags).length + list(o?.removeFlags).length > 0;
}

/** overrideState is where an override stands: applied, waiting for its lease, being put back, or in error. */
export function overrideState(o: ContainerOverride): { label: string; tone: Tone; title: string } {
  if (o.error) return { label: 'override error', tone: 'warning', title: o.error };
  if (o.restoring) {
    return {
      label: 'restoring',
      tone: 'info',
      title:
        'The override is gone (cleared or ended) and the container still runs it: it waits for a lease to be put back',
    };
  }
  if (!o.applied) {
    return {
      label: 'waiting for a lease',
      tone: 'neutral',
      title: 'The container does not run what the override asks for yet: the agent applies it under its next lease',
    };
  }
  return { label: 'applied', tone: 'info', title: 'The container runs what the override asks for' };
}

/** OverrideSummary says what an override of a container is: what it changes, where it stands, until when, by whom and why. */
export function OverrideSummary({ override: o }: { override: ContainerOverride }) {
  const state = overrideState(o);

  return (
    <span className="flex flex-col gap-0.5 text-xs/5">
      <span className="flex flex-wrap items-center gap-x-2">
        <Badge tone={state.tone} title={state.title}>
          {state.label}
        </Badge>
        {o.until && !o.restoring && (
          <span className="text-muted" title={absoluteTime(o.until)}>
            until <RelTime value={o.until} />
          </span>
        )}
        {o.by && <span className="text-muted">by {o.by}</span>}
        {o.reason && <span className="whitespace-normal text-muted">“{o.reason}”</span>}
      </span>
      {o.image && (
        <span className="whitespace-normal">
          <span className="text-muted">image</span> <span className="font-mono break-all">{o.image}</span>
        </span>
      )}
      {list(o.removeFlags).length > 0 && (
        <span className="whitespace-normal">
          <span className="text-muted">remove</span>{' '}
          <span className="font-mono break-all">{list(o.removeFlags).join('  ')}</span>
        </span>
      )}
      {list(o.addFlags).length > 0 && (
        <span className="whitespace-normal">
          <span className="text-muted">add</span>{' '}
          <span className="font-mono break-all">{list(o.addFlags).join('  ')}</span>
        </span>
      )}
      {o.error && <span className="whitespace-normal text-warning">{o.error}</span>}
    </span>
  );
}

/**
 * OverrideButtons are the two actions on the containers of a host: Image…
 * and Flags…. Each is disabled, with the reason as its tooltip, while the
 * host is silent, while its agent configuration allows no such override of
 * these containers, and for a login that may not ask for it.
 */
export function OverrideButtons({
  host,
  containers,
  stale,
  onAction,
}: {
  host: string;
  /** containers are the candidates: the dialog lets the user pick among them. */
  containers: readonly Container[];
  stale: boolean;
  onAction: (intent: ActionIntent) => void;
}) {
  return (
    <>
      {(['image-override', 'flag-override'] as const).map(kind => {
        const blocked = stale
          ? 'The host is silent: its agent has to sync before it can take an override'
          : notAllowed(containers, kind);

        return (
          <ActionIcon
            key={kind}
            icon={kind === 'image-override' ? 'image' : 'flags'}
            label={kind === 'image-override' ? 'Override the image on this host' : 'Override the flags on this host'}
            blocked={blocked}
            onAction={onAction}
            intent={{ kind, host, containers }}
          />
        );
      })}
    </>
  );
}

// ----------------------------------------------------------------- dialogs

export const DURATIONS = [
  { value: '1h', label: '1h', ms: 3_600_000 },
  { value: '6h', label: '6h', ms: 6 * 3_600_000 },
  { value: '24h', label: '24h', ms: 24 * 3_600_000 },
  { value: '3d', label: '3d', ms: 3 * 86_400_000 },
  // A minute short of the seven days the controller allows: its clock and the request's way are not this page's.
  { value: '7d', label: '7d', ms: 7 * 86_400_000 - 60_000 },
] as const;

export type Duration = (typeof DURATIONS)[number]['value'];

export const durationMs = (d: Duration): number => DURATIONS.find(x => x.value === d)?.ms ?? 0;

/** What both dialogs keep: the container, the end, the reason, and the request in flight. */
function useOverrideForm(intent: OverrideIntent, onDone: (result: ActionResult) => void) {
  const { api } = useApp();
  const permissions = usePermissions();
  const candidates = useMemo(() => byKind(intent.containers), [intent.containers]);
  // The first container the host lets be overridden and the login may override; failing that, the first the host allows.
  const [name, setName] = useState(() => {
    const open = candidates.filter(c => allows(c, intent.kind));
    const mine = open.find(c => permissions.canIntent({ ...intent, containers: [c] }).allowed);
    return (mine ?? open[0] ?? candidates[0])?.name ?? '';
  });
  const [duration, setDuration] = useState<Duration>('24h');
  const [reason, setReason] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(undefined);
  // The end that is shown is counted from when the dialog opened; the one that is sent, from when it is sent.
  const [opened] = useState(() => Date.now());

  const container = candidates.find(c => c.name === name);

  const send = async (make: (until: string) => ActionCall) => {
    setBusy(true);
    setError(undefined);
    try {
      onDone(await api.action(make(new Date(Date.now() + durationMs(duration)).toISOString())));
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };

  return {
    permissions,
    candidates,
    container,
    name,
    setName,
    duration,
    setDuration,
    reason,
    setReason,
    busy,
    error,
    until: new Date(opened + durationMs(duration)).toISOString(),
    send,
  };
}

type Form = ReturnType<typeof useOverrideForm>;

/** Shell is what both dialogs share around their own fields. */
function Shell({
  intent,
  form,
  title,
  call,
  clear,
  problem,
  ready,
  onClose,
  children,
}: {
  intent: OverrideIntent;
  form: Form;
  title: string;
  /** call is the request as it stands, with the end the dialog shows. */
  call: (until: string, reason: string) => ActionCall;
  /** clear is the request that takes this part of the override away; undefined when there is nothing to clear. */
  clear: { label: string; call: (reason: string) => ActionCall } | undefined;
  /** problem is what is wrong with the input; ready whether there is an input to send. */
  problem: string | null;
  ready: boolean;
  onClose: () => void;
  children: ReactNode;
}) {
  const { permissions, container: c, busy } = form;
  const current = call(form.until, form.reason.trim());
  const verdict = permissions.can(current);
  const blocked = c ? notAllowed([c], intent.kind) : 'This host has no such container';
  const other = intent.kind === 'image-override' ? hasFlags(c?.override) : Boolean(c?.override?.image);
  const mayConfirm =
    !busy && verdict.allowed && blocked === '' && ready && problem === null && form.reason.trim() !== '';

  const description = describeCall(current);

  return (
    <ActionShell
      title={title}
      lead={description[0] ?? ''}
      targets={[intent.host]}
      details={description}
      reason={{ value: form.reason, onChange: form.setReason, required: true }}
      verdict={verdict}
      error={form.error}
      busy={busy}
      onClose={onClose}
      primary={{
        label: intent.kind === 'image-override' ? 'Override image' : 'Override flags',
        disabled: !mayConfirm,
        onClick: () => void form.send(until => call(until, form.reason.trim())),
      }}
      secondary={
        clear && {
          label: clear.label,
          disabled: !verdict.allowed,
          title: verdict.allowed
            ? 'The container goes back to what is configured, under its next lease'
            : verdict.reason,
          onClick: () => void form.send(() => clear.call(form.reason.trim())),
        }
      }
    >
      {form.candidates.length > 1 ? (
        <Field label="Container">
          <select
            value={form.name}
            disabled={busy}
            onChange={e => {
              form.setName(e.target.value);
            }}
            className={INPUT_CLASS}
          >
            {form.candidates.map(x => (
              <option key={x.name} value={x.name}>
                {x.name}
                {x.kind ? ` (${x.kind})` : ''}
              </option>
            ))}
          </select>
        </Field>
      ) : (
        <Field label="Container">
          <span className="font-mono text-sm/6">{form.name}</span>
        </Field>
      )}

      {c?.override && (
        <div aria-label="Current override" className="rounded-sm bg-surface-raised px-3 py-2">
          <span className="text-xs/5 font-medium">Current override</span>
          <OverrideSummary override={c.override} />
        </div>
      )}

      {blocked !== '' && (
        <p role="note" className="rounded-sm border border-warning bg-warning-subtle px-2 py-1 text-xs/5">
          {blocked}.
        </p>
      )}

      {children}

      {problem !== null && (
        <p role="alert" className="text-xs/5 text-danger">
          {problem}
        </p>
      )}

      <Field
        label="Ends after"
        hint={
          other
            ? `An override has one end for its image and its flags: this also moves the end of the ${intent.kind === 'image-override' ? 'flags' : 'image'}.`
            : 'A temporary override: at most 7 days. Then the configured value comes back.'
        }
      >
        <span className="flex flex-wrap items-center gap-2">
          <Segmented<Duration>
            label="Ends after"
            value={form.duration}
            options={DURATIONS}
            onChange={form.setDuration}
            disabled={busy}
          />
          <span className="text-muted tabular-nums">{absoluteTime(form.until)}</span>
        </span>
      </Field>
    </ActionShell>
  );
}

/**
 * OverrideDialog asks for an override of one container on one host: another
 * image, or a changed command line. It says what the host allows, what is
 * there already and what will happen, and sends one request.
 */
export function OverrideDialog({
  intent,
  onClose,
  onDone,
}: {
  intent: OverrideIntent;
  onClose: () => void;
  onDone: (result: ActionResult) => void;
}) {
  return intent.kind === 'image-override' ? (
    <ImageOverrideDialog intent={intent} onClose={onClose} onDone={onDone} />
  ) : (
    <FlagOverrideDialog intent={intent} onClose={onClose} onDone={onDone} />
  );
}

interface DialogProps {
  intent: OverrideIntent;
  onClose: () => void;
  onDone: (result: ActionResult) => void;
}

function ImageOverrideDialog({ intent, onClose, onDone }: DialogProps) {
  const form = useOverrideForm(intent, onDone);
  const c = form.container;
  const patterns = list(c?.overridable?.images);
  const [image, setImage] = useState('');
  const ref = image.trim();

  const problem =
    ref === ''
      ? null
      : validateImageRef(ref) !== null
        ? `The image ${validateImageRef(ref) ?? ''}.`
        : patterns.length > 0 && !imageAllowed(ref, patterns)
          ? `The image is not in a repository this host allows for ${form.name}.`
          : null;

  const body = { host: intent.host, container: form.name };

  return (
    <Shell
      intent={intent}
      form={form}
      title={`Image override on ${intent.host}`}
      call={(until, reason) => ({ action: 'image-override', body: { ...body, image: ref, until, reason } })}
      clear={
        c?.override?.image && !c.override.restoring
          ? {
              label: 'Clear image override',
              call: reason => ({ action: 'image-override', body: { ...body, image: '', reason } }),
            }
          : undefined
      }
      problem={problem}
      ready={ref !== ''}
      onClose={onClose}
    >
      {c && (
        <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-0.5 text-xs/5">
          <dt className="text-muted">configured</dt>
          <dd className="font-mono break-all">{c.configuredImage || c.image}</dd>
          <dt className="text-muted">running</dt>
          <dd className="flex flex-wrap items-center gap-x-2">
            <span className="font-mono break-all">{c.image}</span>
            <Digest value={c.digest} />
          </dd>
        </dl>
      )}

      <label className="flex flex-col gap-1 text-xs/5">
        <span className="font-semibold text-muted uppercase">Image to run instead</span>
        <input
          data-autofocus
          type="text"
          aria-label="Image"
          value={image}
          disabled={form.busy}
          spellCheck={false}
          onChange={e => {
            setImage(e.target.value);
          }}
          className={cx(INPUT_CLASS, 'font-mono')}
          placeholder={patterns[0] ? `${patterns[0].replace(/\*/g, 'name')}:tag` : 'repository:tag'}
        />
        {patterns.length > 0 && (
          <span className="text-muted">
            A tag of an allowed repository: <span className="font-mono break-all">{patterns.join(', ')}</span>. The
            agent of the host decides.
          </span>
        )}
      </label>
    </Shell>
  );
}

function FlagOverrideDialog({ intent, onClose, onDone }: DialogProps) {
  const form = useOverrideForm(intent, onDone);
  const c = form.container;
  const command = list(c?.command);
  const existing = c?.override && !c.override.restoring ? c.override : undefined;

  // The lists start with what the override says now: a new request replaces both.
  const [text, setText] = useState<{ of: string; add: string; remove: string }>({ of: '', add: '', remove: '' });
  const shown =
    text.of === form.name
      ? text
      : { of: form.name, add: list(existing?.addFlags).join('\n'), remove: list(existing?.removeFlags).join('\n') };
  const set = (part: Partial<typeof shown>) => {
    setText({ ...shown, ...part });
  };

  const add = parseFlags(shown.add);
  const remove = parseFlags(shown.remove);
  const invalid = validateFlags(add, remove);
  const result = invalid === null ? applyFlags(command, add, remove) : command;
  const body = { host: intent.host, container: form.name };

  return (
    <Shell
      intent={intent}
      form={form}
      title={`Flag override on ${intent.host}`}
      call={(until, reason) => ({
        action: 'flag-override',
        body: { ...body, addFlags: add, removeFlags: remove, until, reason },
      })}
      clear={
        hasFlags(existing)
          ? {
              label: 'Clear flag override',
              call: reason => ({ action: 'flag-override', body: { ...body, addFlags: [], removeFlags: [], reason } }),
            }
          : undefined
      }
      problem={invalid === null ? null : `Not a flag list the agent takes: ${invalid}.`}
      ready={add.length + remove.length > 0}
      onClose={onClose}
    >
      <div className="flex flex-col gap-1 text-xs/5">
        <span className="font-semibold text-muted uppercase">
          Command line now <span className="font-normal normal-case">(click an argument to remove it)</span>
        </span>
        {command.length === 0 ? (
          <span className="text-muted">The agent reports no arguments.</span>
        ) : (
          <ul
            aria-label="Command line now"
            className="max-h-32 overflow-y-auto rounded-sm border border-border px-1 py-0.5"
          >
            {command.map((arg, i) => (
              <li key={`${i} ${arg}`}>
                <button
                  type="button"
                  title="Add to the removals"
                  disabled={form.busy}
                  onClick={() => {
                    if (!remove.includes(arg)) set({ remove: [...remove, arg].join('\n') });
                  }}
                  className={cx(
                    'block w-full rounded-xs px-1 text-left font-mono break-all hover:bg-surface-hover',
                    remove.includes(arg) && 'text-danger line-through'
                  )}
                >
                  {arg}
                </button>
              </li>
            ))}
          </ul>
        )}
        {existing?.applied && hasFlags(existing) && (
          <span className="text-muted">This is the command line with the current override applied.</span>
        )}
      </div>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <label className="flex flex-col gap-1 text-xs/5">
          <span className="font-semibold text-muted uppercase">Remove (one per line)</span>
          <textarea
            data-autofocus
            aria-label="Flags to remove"
            rows={4}
            value={shown.remove}
            disabled={form.busy}
            spellCheck={false}
            onChange={e => {
              set({ remove: e.target.value });
            }}
            className={cx(INPUT_CLASS, 'h-auto py-1 font-mono')}
            placeholder="--name"
          />
        </label>
        <label className="flex flex-col gap-1 text-xs/5">
          <span className="font-semibold text-muted uppercase">Add (one per line)</span>
          <textarea
            aria-label="Flags to add"
            rows={4}
            value={shown.add}
            disabled={form.busy}
            spellCheck={false}
            onChange={e => {
              set({ add: e.target.value });
            }}
            className={cx(INPUT_CLASS, 'h-auto py-1 font-mono')}
            placeholder="--name=value"
          />
        </label>
      </div>

      <Disclosure summary="How flags are applied">
        <ul aria-label="How flags are applied" className="flex list-disc flex-col gap-0.5 pl-4 text-muted">
          {FLAG_HELP.map(line => (
            <li key={line}>{line}</li>
          ))}
        </ul>
      </Disclosure>

      <div className="flex flex-col gap-1 text-xs/5">
        <span className="font-semibold text-muted uppercase">
          Command line after{' '}
          <span className="font-normal normal-case">
            ({result.length} {result.length === 1 ? 'argument' : 'arguments'}; what is new is marked)
          </span>
        </span>
        <ul
          aria-label="Command line after"
          className="max-h-32 overflow-y-auto rounded-sm border border-border px-2 py-0.5"
        >
          {result.map((arg, i) => (
            <li
              key={`${i} ${arg}`}
              className={cx('font-mono break-all', !command.includes(arg) && 'font-semibold text-success')}
            >
              {arg}
            </li>
          ))}
          {result.length === 0 && <li className="text-muted">no arguments</li>}
        </ul>
      </div>
    </Shell>
  );
}
