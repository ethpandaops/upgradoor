// How each audit action reads: a title, a one-line summary and the fields of
// its detail. An action without a renderer is shown by the fallback, which
// lists every field of the detail.

import type { AuditEntry } from '../../api/types';
import type { AuditFilter } from '../../lib/auditCatalog';
import { changeOf, diffValues } from '../../lib/auditDiff';
import { formatDuration, hostList, plural } from '../../lib/format';
import type { Tone } from '../primitives';

/** What a renderer may ask of the page it is shown on. */
export interface AuditContext {
  /** rolloutTitle names a rollout the page knows. */
  rolloutTitle: (id: string) => string | undefined;
  /** onFilter narrows the audit page; absent elsewhere. */
  onFilter?: (f: Partial<AuditFilter>) => void;
}

/** How one value of a detail is shown; null hides its row. */
export type ValueKind =
  | 'text'
  | 'quote'
  | 'mono'
  | 'hosts'
  | 'rollouts'
  | 'list'
  | 'flags'
  | 'time'
  | 'yes'
  | 'number'
  | 'map';

export interface FieldSpec {
  key: string;
  label: string;
  kind: ValueKind;
}

export interface AuditRenderer {
  /** title says what happened, in a few words. */
  title: (e: AuditEntry) => string;
  tone?: (e: AuditEntry) => Tone | undefined;
  /** summary is one short line next to the title. */
  summary?: (e: AuditEntry) => string | undefined;
  /** fields are the detail's fields in reading order; others follow as they are. */
  fields?: readonly FieldSpec[];
  /** changeLabel names a field of a before / after change. */
  changeLabel?: (key: string) => string;
}

// ------------------------------------------------------------------ helpers

export function str(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined;
}

export function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
}

const d = (e: AuditEntry, key: string): unknown => e.detail?.[key];

const reason = (e: AuditEntry) => str(d(e, 'reason'));

/** changed lists the fields a before / after change changes. */
function changed(e: AuditEntry): string[] {
  const change = changeOf(e.detail);
  return change ? diffValues(change.before, change.after).map(r => r.key) : [];
}

const onSettings = (e: AuditEntry) => e.target === 'settings';

const F = (key: string, label: string, kind: ValueKind = 'text'): FieldSpec => ({ key, label, kind });

const REASON = F('reason', 'Reason', 'quote');
const LEASE = F('lease', 'Lease', 'mono');
const ROLLOUTS = F('rollouts', 'Rollouts', 'rollouts');
const HOSTS = F('hosts', 'Hosts', 'hosts');
const REJECTED = F('rejected', 'Refused', 'map');
const FORCE = F('force', 'Forced', 'yes');
const IGNORE_GATE = F('ignoreGate', 'Passes a closed gate', 'yes');
const IGNORE_BUDGET = F('ignoreBudget', 'Ignores the budget', 'yes');
const READINESS = F('readiness', 'Readiness');
const ERROR = F('error', 'Error');
const KEY = F('key', 'Key', 'mono');
const TITLE = F('title', 'Title');
const DIRECTIVE = F('directive', 'Instruction', 'mono');
const CONTAINER = F('container', 'Container', 'mono');

/** Settings keys read as words in a change table. */
function settingsLabel(labels: ReadonlyMap<string, string>) {
  return (key: string) => {
    const [head = '', ...rest] = key.split('.');
    const label = labels.get(head);
    return label ? [label, ...rest].join(' · ') : key;
  };
}

/** Tones of the summary of how a lease ended. */
const failed = (): Tone => 'danger';
const warned = (): Tone => 'warning';

// ------------------------------------------------------------------ registry

/**
 * auditRenderers builds the registry; settingsLabels names the settings (it
 * comes from the settings layout, which this file does not import, so the
 * registry stays cheap to test).
 */
export function auditRenderers(settingsLabels: ReadonlyMap<string, string> = new Map()): Record<string, AuditRenderer> {
  const settingsChange = settingsLabel(settingsLabels);
  const settingsSummary = (e: AuditEntry) => {
    const keys = [...new Set(changed(e).map(k => settingsChange(k.split('.')[0] ?? k)))];
    const what = keys.length === 0 ? undefined : keys.length <= 3 ? keys.join(', ') : plural(keys.length, 'setting');
    return [what, reason(e)].filter(Boolean).join(' · ') || undefined;
  };

  const rollout = (title: string, tone?: () => Tone): AuditRenderer => ({
    title: () => title,
    tone,
    summary: reason,
    fields: [TITLE, REASON, F('host', 'Host', 'hosts'), LEASE, KEY],
  });

  const manual = (verb: string): AuditRenderer => ({
    title: e => `Started a ${verb} of ${plural(strings(d(e, 'hosts')).length, 'host')}`,
    summary: e => [d(e, 'staggered') ? 'staggered' : 'all at once', reason(e)].filter(Boolean).join(' · '),
    fields: [
      HOSTS,
      F('containers', 'Containers', 'list'),
      F('staggered', 'Staggered', 'yes'),
      REASON,
      REJECTED,
      FORCE,
      IGNORE_BUDGET,
      IGNORE_GATE,
    ],
  });

  const rolloutOrScheduler = (rolloutTitle: string, globalTitle: string): AuditRenderer => ({
    title: e => (onSettings(e) ? globalTitle : rolloutTitle),
    tone: warned,
    summary: reason,
    fields: [REASON],
    changeLabel: settingsChange,
  });

  const directive = (title: string, tone?: () => Tone): AuditRenderer => ({
    title: () => title,
    tone,
    summary: e => [str(d(e, 'kind')), str(d(e, 'container')), str(d(e, 'error'))].filter(Boolean).join(' · '),
    fields: [F('kind', 'Instruction'), CONTAINER, F('holder', 'Holder'), ERROR, DIRECTIVE],
  });

  const override = (part: string): AuditRenderer => ({
    title: e =>
      d(e, 'clear') ? `Cleared the ${part} override` : `Set ${part === 'image' ? 'an' : 'a'} ${part} override`,
    summary: e => [str(d(e, 'container')), str(d(e, 'image')), reason(e)].filter(Boolean).join(' · ') || undefined,
    fields: [
      CONTAINER,
      F('image', 'Image', 'mono'),
      F('addFlags', 'Adds', 'flags'),
      F('removeFlags', 'Removes', 'flags'),
      F('until', 'Until', 'time'),
      F('clear', 'Clears it', 'yes'),
      REASON,
      F('part', 'Part'),
      DIRECTIVE,
    ],
  });

  return {
    'rollout.created': {
      title: () => 'Rollout opened',
      fields: [TITLE, KEY],
    },
    'rollout.paused': rollout('Rollout paused', warned),
    'rollout.halted': rollout('Rollout halted', failed),
    'rollout.completed': { title: () => 'Rollout completed', tone: () => 'success', fields: [KEY, REASON] },
    'rollout.superseded': rollout('Rollout superseded'),
    'rollout.dropped': rollout('Rollout dropped'),
    'action.roll': manual('roll'),
    'action.reboot': manual('reboot'),
    'action.pause': rolloutOrScheduler('Paused the rollout', 'Paused the scheduler'),
    'action.resume': {
      title: e => (onSettings(e) ? 'Resumed the scheduler' : 'Resumed the rollout'),
      summary: reason,
      fields: [REASON],
      changeLabel: settingsChange,
    },
    'action.ignore-gate': {
      title: () => 'Let the rollout pass the closed gate',
      tone: warned,
      summary: reason,
      fields: [REASON],
    },
    'action.ignore-budget': {
      title: () => 'Let the rollout ignore the budget',
      tone: failed,
      summary: reason,
      fields: [REASON],
    },
    'action.skip': {
      title: e => `Skipped ${plural(strings(d(e, 'hosts')).length, 'host')}`,
      summary: e => [hostList(strings(d(e, 'hosts')), 3), reason(e)].filter(Boolean).join(' · '),
      fields: [HOSTS, REASON],
    },
    'action.retry': {
      title: e => {
        const n = strings(d(e, 'hosts')).length;
        return n === 0 ? 'Retried the rollout' : `Retried ${plural(n, 'host')}`;
      },
      summary: reason,
      fields: [HOSTS, REASON],
    },
    'action.abort': { title: () => 'Aborted the rollout', tone: failed, summary: reason, fields: [REASON] },
    'action.host-action': {
      title: e =>
        `Started ${str(d(e, 'action')) ?? 'a host action'} on ${plural(strings(d(e, 'hosts')).length, 'host')}`,
      summary: reason,
      fields: [
        F('action', 'Action', 'mono'),
        HOSTS,
        REASON,
        F('pauseAfterCanary', 'Pauses after the canary', 'yes'),
        IGNORE_GATE,
        REJECTED,
        F('basis', 'Basis', 'map'),
      ],
    },
    'lease.granted': {
      title: e => (d(e, 'recheck') ? 'Lease granted to recheck' : 'Lease granted'),
      tone: e => (d(e, 'ignoreBudget') || d(e, 'ignoredGate') ? 'warning' : undefined),
      summary: e => {
        const n = strings(d(e, 'rollouts')).length;
        return [n > 1 ? `${n} rollouts` : undefined, d(e, 'force') ? 'forced' : undefined, str(d(e, 'impact'))]
          .filter(Boolean)
          .join(' · ');
      },
      fields: [
        ROLLOUTS,
        LEASE,
        F('impact', 'Impact'),
        F('cost', 'Cost', 'number'),
        F('unavailable', 'Unavailable before', 'number'),
        F('allowed', 'Allowed', 'number'),
        FORCE,
        IGNORE_BUDGET,
        F('ignoredGate', 'Passed the closed gate', 'yes'),
        F('closedGate', 'Granted at the closed gate', 'yes'),
        F('recheck', 'Recheck', 'yes'),
      ],
    },
    'lease.released': {
      title: () => 'Lease released',
      tone: () => 'success',
      summary: e => str(d(e, 'outcome')) ?? str(d(e, 'readiness')),
      fields: [F('outcome', 'Outcome'), READINESS, F('approvedBy', 'Approved by'), FORCE, LEASE],
    },
    'lease.failed': {
      title: () => 'Lease failed',
      tone: failed,
      summary: reason,
      fields: [REASON, F('rollouts', 'Halted rollouts', 'rollouts'), LEASE],
    },
    'lease.expired': { title: () => 'Lease expired', tone: warned, summary: reason, fields: [REASON, LEASE] },
    'lease.cancelled': {
      title: () => 'Lease cancelled',
      tone: warned,
      summary: e => (str(d(e, 'by')) ? `ended by ${str(d(e, 'by')) as string}` : undefined),
      fields: [F('by', 'Ended by'), F('rollouts', 'Paused rollouts', 'rollouts'), LEASE],
    },
    'action.approve-lease': {
      title: () => 'Approved the lease',
      summary: reason,
      fields: [REASON, READINESS, ROLLOUTS, LEASE],
    },
    'action.end-lease': {
      title: () => 'Ended the lease',
      tone: warned,
      summary: reason,
      fields: [REASON, F('state', 'State then'), LEASE],
    },
    'action.hold': {
      title: () => 'Held the host',
      tone: warned,
      summary: reason,
      fields: [REASON, F('holder', 'Holder'), DIRECTIVE],
    },
    'action.unhold': {
      title: () => 'Lifted the hold',
      summary: reason,
      fields: [REASON, F('holder', 'Holder'), DIRECTIVE],
    },
    'action.mark-offline': { title: () => 'Marked offline', tone: warned, summary: reason, fields: [REASON] },
    'action.unmark-offline': { title: () => 'Lifted the offline mark', summary: reason, fields: [REASON] },
    'action.image-override': override('image'),
    'action.flag-override': override('flag'),
    'directive.accepted': directive('Agent took the instruction'),
    'directive.rejected': directive('Agent refused the instruction', failed),
    'directive.cancelled': directive('Instruction withdrawn', warned),
    'settings.update': {
      title: () => 'Changed the settings',
      summary: settingsSummary,
      fields: [REASON],
      changeLabel: settingsChange,
    },
    'action.client-policy': {
      title: () => 'Changed a client image policy',
      summary: settingsSummary,
      fields: [REASON],
      changeLabel: settingsChange,
    },
    'action.arm-deploy-window': {
      title: () => 'Armed upgradoor: ended the deployment window',
      tone: warned,
      summary: reason,
      fields: [REASON],
      changeLabel: key => (key === 'armedAt' ? 'Armed at' : key),
    },
    'sources.validator-ranges.refresh': {
      title: () => 'Refreshed the validator ranges',
      tone: e => (str(d(e, 'error')) ? 'danger' : undefined),
      summary: e => str(d(e, 'error')) ?? (d(e, 'changed') ? 'the ranges changed' : 'no change'),
      fields: [F('changed', 'Changed', 'yes'), ERROR],
    },
    'host.registered': {
      title: () => 'Host registered',
      summary: e => {
        const roles = strings(d(e, 'roles'));
        return roles.length > 0 ? roles.join(', ') : undefined;
      },
      fields: [
        F('roles', 'Roles', 'list'),
        F('validators', 'Validators', 'number'),
        F('certHash', 'Certificate', 'mono'),
        F('remoteAddr', 'From', 'mono'),
      ],
    },
    'host.certificate-changed': {
      title: () => 'Certificate changed',
      tone: warned,
      fields: [F('remoteAddr', 'From', 'mono')],
    },
    'action.forget': { title: () => 'Forgot the host', tone: failed, summary: reason, fields: [REASON] },
    'action.forget.failed': {
      title: () => 'Forgetting the host failed',
      tone: failed,
      summary: e => str(d(e, 'error')),
      fields: [ERROR],
    },
    'ssh.session': {
      title: e => {
        const n = d(e, 'sessions');
        const who = str(d(e, 'identity')) ?? (str(d(e, 'fingerprint')) ? 'an unknown key' : str(d(e, 'keyType')));
        const sessions = typeof n === 'number' ? plural(n, 'SSH session') : 'SSH sessions';
        return who ? `${sessions} of ${who}` : sessions;
      },
      tone: e => (str(d(e, 'identity')) ? undefined : 'warning'),
      summary: e => {
        const total = d(e, 'totalMs');
        return [
          str(d(e, 'user')) && `as ${str(d(e, 'user')) as string}`,
          str(d(e, 'from')) && `from ${str(d(e, 'from')) as string}`,
          typeof total === 'number' ? formatDuration(total / 1000) : undefined,
          d(e, 'endUnknown') ? 'end unknown' : undefined,
        ]
          .filter(Boolean)
          .join(' · ');
      },
      fields: [
        F('identity', 'Identity'),
        F('source', 'Key source'),
        F('fingerprint', 'Fingerprint', 'mono'),
        F('keyType', 'Key type'),
        F('user', 'Login user', 'mono'),
        F('from', 'From', 'mono'),
        F('firstLogin', 'First login', 'time'),
        F('lastLogout', 'Last logout', 'time'),
        F('sessions', 'Sessions', 'number'),
        F('longestMs', 'Longest (ms)', 'number'),
        F('totalMs', 'Total (ms)', 'number'),
        F('endUnknown', 'A session ended without a logout', 'yes'),
      ],
    },
    'debug.pprof': {
      title: e => {
        const profile = str(d(e, 'profile'));
        return profile ? `Downloaded profile ${profile}` : 'Downloaded a profile';
      },
      tone: e => {
        const status = d(e, 'status');
        return typeof status === 'number' && status >= 400 ? 'danger' : undefined;
      },
      summary: e => {
        const status = d(e, 'status');
        return typeof status === 'number' && status >= 400 ? `status ${status}` : str(d(e, 'query'));
      },
      fields: [F('profile', 'Profile', 'mono'), F('query', 'Query', 'mono'), F('status', 'Status', 'number')],
    },
  };
}

/**
 * FALLBACK_ACTIONS are the actions shown by the fallback on purpose: their
 * detail is a plain list of fields.
 */
export const FALLBACK_ACTIONS: ReadonlySet<string> = new Set(['auth.refused']);

/** humanize turns an action or key into words: "action.mark-offline" → "Mark offline". */
export function humanize(name: string): string {
  const last = name.replace(/^(action|settings|rollout|lease|host|directive|auth|sources|debug|ssh)\./, '');
  const words = last
    .replace(/[.-]/g, ' ')
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .toLowerCase();
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** FALLBACK is the renderer of an action without one: its name in words and every field. */
export const FALLBACK: AuditRenderer = {
  title: e => humanize(e.action),
  tone: e => (e.action === 'auth.refused' ? 'danger' : undefined),
  summary: e => str(d(e, 'reason')) ?? str(d(e, 'error')),
};
