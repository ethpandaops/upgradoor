// The confirm-dialog intents shared by several views.

import type { ActionIntent } from '../components/ActionDialog';
import { buildHoldRequest, buildRolloutHostsRequest, buildScopeRequest } from './actionRequests';
import { plural } from './format';

/** holdIntent builds the confirm dialog of hold / unhold / forget. */
export function holdIntent(
  action: 'hold' | 'unhold' | 'forget',
  hosts: readonly string[],
  hiddenHosts: readonly string[] = []
): ActionIntent {
  const verb = action === 'hold' ? 'Hold' : action === 'unhold' ? 'Unhold' : 'Forget';
  return {
    kind: 'call',
    title: hosts.length === 1 ? `${verb} ${hosts[0] as string}` : `${verb} ${plural(hosts.length, 'host')}`,
    danger: action === 'forget',
    hiddenHosts,
    make: reason => ({ action, body: buildHoldRequest(hosts, reason) }),
  };
}

/** scopeIntent pauses, resumes or aborts one rollout, or everything with an empty rollout ID. */
export function scopeIntent(action: 'pause' | 'resume' | 'abort', title: string, rollout: string): ActionIntent {
  return {
    kind: 'call',
    title,
    danger: action === 'abort',
    make: reason => ({ action, body: buildScopeRequest(rollout, reason) }),
  };
}

/** hostsIntent skips or retries hosts of a rollout; no hosts means all of them. */
export function hostsIntent(
  action: 'skip' | 'retry',
  title: string,
  rollout: string,
  hosts: readonly string[]
): ActionIntent {
  return {
    kind: 'call',
    title,
    make: reason => ({ action, body: buildRolloutHostsRequest(rollout, hosts, reason) }),
  };
}
