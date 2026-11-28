// The SSH logins page: its filter as the URL and GET /api/v1/ssh-logins take
// it, and how a login's identity reads.

import type { SSHLoginQuery } from '../api/client';
import type { SSHLoginCluster, SSHLoginGroup } from '../api/types';
import { localToIso } from './auditCatalog';
import { formatDuration } from './format';

/** fleet: one row per identity, login user and overlapping time across hosts; groups: one row per host's group. */
export type SSHView = 'fleet' | 'groups';

export interface SSHFilter {
  view: SSHView;
  host: string;
  identity: string;
  user: string;
  /** from and to are local "YYYY-MM-DDTHH:MM", as a datetime-local input has them. */
  from: string;
  to: string;
  unknown: boolean;
  open: boolean;
}

export const DEFAULT_SSH_FILTER: SSHFilter = {
  view: 'fleet',
  host: '',
  identity: '',
  user: '',
  from: '',
  to: '',
  unknown: false,
  open: false,
};

const TEXT_KEYS = ['host', 'identity', 'user', 'from', 'to'] as const;

export function sshFilterFromParams(params: URLSearchParams): SSHFilter {
  const f: SSHFilter = { ...DEFAULT_SSH_FILTER };
  if (params.get('view') === 'groups') f.view = 'groups';
  for (const k of TEXT_KEYS) f[k] = params.get(k) ?? '';
  f.unknown = params.get('unknown') === '1';
  f.open = params.get('open') === '1';
  return f;
}

/** sshFilterToParams leaves out what is the default. */
export function sshFilterToParams(f: SSHFilter): URLSearchParams {
  const params = new URLSearchParams();
  if (f.view !== 'fleet') params.set('view', f.view);
  for (const k of TEXT_KEYS) if (f[k] !== '') params.set(k, f[k]);
  if (f.unknown) params.set('unknown', '1');
  if (f.open) params.set('open', '1');
  return params;
}

/** sshApiQuery is the query of GET /api/v1/ssh-logins for a filter. */
export function sshApiQuery(f: SSHFilter): SSHLoginQuery {
  return {
    view: f.view,
    host: f.host.trim(),
    identity: f.identity.trim(),
    user: f.user.trim(),
    from: localToIso(f.from),
    to: localToIso(f.to),
    unknown: f.unknown,
    open: f.open,
  };
}

/** clusterQuery asks for the groups of a cluster: its identity (or key) and user, over its starts. */
export function clusterQuery(c: SSHLoginCluster): SSHLoginQuery {
  return {
    view: 'groups',
    identity: c.identity ?? '',
    fingerprint: c.identity ? '' : (c.fingerprint ?? ''),
    user: c.user,
    from: c.firstLogin,
    to: new Date(Date.parse(c.lastStart) + 1).toISOString(),
  };
}

/** githubName is the GitHub login of a "github:<name>" identity. */
export function githubName(identity: string | undefined): string | undefined {
  const m = /^github:([A-Za-z0-9-]{1,39})$/.exec(identity ?? '');
  return m?.[1];
}

/** durationText is "total / longest" of a group or cluster. */
export function durationText(g: Pick<SSHLoginGroup, 'totalMs' | 'longestMs' | 'sessions'>): string {
  const total = formatDuration(g.totalMs / 1000);
  return g.sessions > 1 ? `${total} / ${formatDuration(g.longestMs / 1000)}` : total;
}
