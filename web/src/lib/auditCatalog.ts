// The audit log's actions and filters: what the controller records
// (fleet.AuditActions) and the filter of GET /api/v1/audit, kept in the URL.

import type { AuditEntry } from '../api/types';

/** fleet.AuditActorKind; "all" is no actor filter. */
export type AuditWho = 'people' | 'scheduler' | 'agents' | 'all';

/** fleet.AuditGroup */
export type AuditGroup =
  | 'rollouts'
  | 'host-actions'
  | 'leases'
  | 'holds'
  | 'overrides'
  | 'instructions'
  | 'settings'
  | 'sources'
  | 'hosts'
  | 'access';

/** fleet.AuditTargetKind */
export type AuditTargetKind = 'host' | 'rollout' | 'settings' | 'sources' | 'request';

/** fleet.AuditAction */
export interface AuditActionInfo {
  action: string;
  group: AuditGroup;
  target: AuditTargetKind;
  /** scheduler: the scheduler records it on its own. */
  scheduler?: boolean;
}

const R = (action: string, scheduler = false): AuditActionInfo => ({
  action,
  group: 'rollouts',
  target: 'rollout',
  ...(scheduler ? { scheduler } : {}),
});
const L = (action: string, scheduler = false): AuditActionInfo => ({
  action,
  group: 'leases',
  target: 'host',
  ...(scheduler ? { scheduler } : {}),
});
const H = (action: string, group: AuditGroup): AuditActionInfo => ({ action, group, target: 'host' });

/** AUDIT_ACTIONS mirrors fleet.AuditActions, in its order; a test holds them equal. */
export const AUDIT_ACTIONS: readonly AuditActionInfo[] = [
  R('rollout.created', true),
  R('rollout.paused', true),
  R('rollout.halted', true),
  R('rollout.completed', true),
  R('rollout.superseded', true),
  R('rollout.dropped', true),
  R('action.roll'),
  R('action.reboot'),
  R('action.pause'),
  R('action.resume'),
  R('action.ignore-gate'),
  R('action.ignore-budget'),
  R('action.skip'),
  R('action.retry'),
  R('action.abort'),
  { action: 'action.host-action', group: 'host-actions', target: 'rollout' },
  L('lease.granted', true),
  L('lease.released', true),
  L('lease.failed', true),
  L('lease.expired', true),
  L('lease.cancelled', true),
  L('action.approve-lease'),
  L('action.end-lease'),
  H('action.hold', 'holds'),
  H('action.unhold', 'holds'),
  H('action.mark-offline', 'holds'),
  H('action.unmark-offline', 'holds'),
  H('action.image-override', 'overrides'),
  H('action.flag-override', 'overrides'),
  { action: 'directive.accepted', group: 'instructions', target: 'host', scheduler: true },
  { action: 'directive.rejected', group: 'instructions', target: 'host', scheduler: true },
  { action: 'directive.cancelled', group: 'instructions', target: 'host', scheduler: true },
  { action: 'settings.update', group: 'settings', target: 'settings' },
  { action: 'action.client-policy', group: 'settings', target: 'settings' },
  { action: 'action.arm-deploy-window', group: 'settings', target: 'settings' },
  { action: 'sources.validator-ranges.refresh', group: 'sources', target: 'sources' },
  H('host.registered', 'hosts'),
  H('host.certificate-changed', 'hosts'),
  H('action.forget', 'hosts'),
  H('action.forget.failed', 'hosts'),
  { action: 'auth.refused', group: 'access', target: 'request' },
];

const BY_ACTION = new Map(AUDIT_ACTIONS.map(a => [a.action, a]));

export function auditActionOf(action: string): AuditActionInfo | undefined {
  return BY_ACTION.get(action);
}

export const WHO_OPTIONS: readonly { value: AuditWho; label: string; title: string }[] = [
  { value: 'people', label: 'People', title: 'What logins, tokens and the open API did' },
  {
    value: 'scheduler',
    label: 'Scheduler',
    title: 'What the scheduler decided on its own: rollouts, leases, instructions',
  },
  { value: 'agents', label: 'Agents', title: 'What the agents reported when they synced: registrations, certificates' },
  { value: 'all', label: 'All', title: 'Everything' },
];

export const GROUP_LABEL: Record<AuditGroup, string> = {
  rollouts: 'Rollouts',
  'host-actions': 'Host actions',
  leases: 'Leases',
  holds: 'Holds and offline marks',
  overrides: 'Overrides',
  instructions: 'Agent answers',
  settings: 'Settings',
  sources: 'Data sources',
  hosts: 'Host registry',
  access: 'Refused requests',
};

export const TARGET_KIND_LABEL: Record<AuditTargetKind, string> = {
  host: 'Host',
  rollout: 'Rollout',
  settings: 'Settings',
  sources: 'Data source',
  request: 'API request',
};

export const AUDIT_GROUPS = Object.keys(GROUP_LABEL) as AuditGroup[];
export const TARGET_KINDS = Object.keys(TARGET_KIND_LABEL) as AuditTargetKind[];

/** actorKindOf is fleet.AuditActorKindOf. */
export function actorKindOf(actor: string): Exclude<AuditWho, 'all'> {
  if (actor === 'scheduler') return 'scheduler';
  if (actor.startsWith('agent:')) return 'agents';
  return 'people';
}

/** Every entry about the settings is of the settings group and kind, whatever its action (a global pause). */
export function groupOf(e: AuditEntry): AuditGroup | undefined {
  return e.target === 'settings' ? 'settings' : auditActionOf(e.action)?.group;
}

export function targetKindOf(e: AuditEntry): AuditTargetKind | undefined {
  return e.target === 'settings' ? 'settings' : auditActionOf(e.action)?.target;
}

/** AuditFilter is the filter of the page, as GET /api/v1/audit and the URL take it. */
export interface AuditFilter {
  who: AuditWho;
  actor: string;
  target: string;
  kind: AuditTargetKind | '';
  group: AuditGroup | '';
  /** action is an action prefix; the page offers whole actions. */
  action: string;
  /** from and to are local "YYYY-MM-DDTHH:MM", as a datetime-local input has them. */
  from: string;
  to: string;
  q: string;
}

/** The page opens on what people did: the scheduler's entries are most of the log. */
export const DEFAULT_FILTER: AuditFilter = {
  who: 'people',
  actor: '',
  target: '',
  kind: '',
  group: '',
  action: '',
  from: '',
  to: '',
  q: '',
};

const TEXT_KEYS = ['actor', 'target', 'action', 'from', 'to', 'q'] as const;

export function filterFromParams(params: URLSearchParams): AuditFilter {
  const f: AuditFilter = { ...DEFAULT_FILTER };
  const who = params.get('who');
  if (who && WHO_OPTIONS.some(o => o.value === who)) f.who = who as AuditWho;
  const kind = params.get('kind');
  if (kind && kind in TARGET_KIND_LABEL) f.kind = kind as AuditTargetKind;
  const group = params.get('group');
  if (group && group in GROUP_LABEL) f.group = group as AuditGroup;
  for (const k of TEXT_KEYS) f[k] = params.get(k) ?? '';
  return f;
}

/** filterToParams leaves out what is the default: "who" only when it is not people. */
export function filterToParams(f: AuditFilter): URLSearchParams {
  const params = new URLSearchParams();
  if (f.who !== DEFAULT_FILTER.who) params.set('who', f.who);
  for (const k of ['actor', 'target', 'kind', 'group', 'action', 'from', 'to', 'q'] as const) {
    if (f[k] !== '') params.set(k, f[k]);
  }
  return params;
}

/** auditHash links the audit page with a filter: everyone's entries unless it says otherwise. */
export function auditHash(f: Partial<AuditFilter>): string {
  const query = filterToParams({ ...DEFAULT_FILTER, who: 'all', ...f }).toString();
  return `#audit/${query === '' ? '' : `?${query}`}`;
}

/** isFiltered: the filter narrows more than the default view does. */
export function isFiltered(f: AuditFilter): boolean {
  return filterToParams(f).toString() !== '';
}

/** localToIso turns a datetime-local value into an RFC 3339 instant; "" stays "". */
export function localToIso(local: string): string {
  if (local === '') return '';
  const d = new Date(local);
  return Number.isNaN(d.getTime()) ? '' : d.toISOString();
}

/** The query of GET /api/v1/audit for a filter. */
export function apiQuery(f: AuditFilter): Record<string, string> {
  return {
    who: f.who === 'all' ? '' : f.who,
    actor: f.actor,
    target: f.target,
    kind: f.kind,
    group: f.group,
    action: f.action,
    from: localToIso(f.from),
    to: localToIso(f.to),
    q: f.q.trim(),
  };
}

/** matchesFilter applies the filter as the controller does, to entries in memory (the mock). */
export function matchesFilter(
  e: AuditEntry,
  f: Partial<Record<keyof AuditFilter, string>>,
  searchDetail = true
): boolean {
  const time = Date.parse(e.time);
  const text = `${e.actor} ${e.action} ${e.target ?? ''}${searchDetail && e.detail ? ` ${JSON.stringify(e.detail)}` : ''}`;

  return (
    (!f.who || f.who === 'all' || actorKindOf(e.actor) === f.who) &&
    (!f.actor || e.actor === f.actor) &&
    (!f.target || e.target === f.target) &&
    (!f.kind || targetKindOf(e) === f.kind) &&
    (!f.group || groupOf(e) === f.group) &&
    (!f.action || e.action.startsWith(f.action)) &&
    (!f.from || time >= Date.parse(f.from)) &&
    (!f.to || time < Date.parse(f.to)) &&
    (!f.q || text.toLowerCase().includes(f.q.toLowerCase()))
  );
}
