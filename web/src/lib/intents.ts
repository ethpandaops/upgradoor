// The confirm-dialog intents shared by several views.

import type { ActionIntent } from '../components/ActionDialog';
import type { Policy } from '../api/types';
import {
  buildClientPolicyRequest,
  buildEndLeaseRequest,
  buildHoldRequest,
  buildOfflineRequest,
  buildRolloutHostsRequest,
  buildScopeRequest,
} from './actionRequests';
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
    verb,
    danger: action === 'forget',
    hiddenHosts,
    make: reason => ({ action, body: buildHoldRequest(hosts, reason) }),
  };
}

/** offlineIntent marks hosts as known offline, or lifts the mark. */
export function offlineIntent(mark: boolean, hosts: readonly string[]): ActionIntent {
  const verb = mark ? 'Mark offline' : 'Lift offline mark';
  return {
    kind: 'call',
    title: hosts.length === 1 ? `${verb}: ${hosts[0] as string}` : `${verb}: ${plural(hosts.length, 'host')}`,
    verb,
    reasonRequired: mark,
    ends: mark,
    make: (reason, until) => ({
      action: mark ? 'mark-offline' : 'unmark-offline',
      body: buildOfflineRequest(hosts, reason, until),
    }),
  };
}

/** scopeIntent pauses, resumes or aborts one rollout, or everything with an empty rollout ID. */
export function scopeIntent(action: 'pause' | 'resume' | 'abort', title: string, rollout: string): ActionIntent {
  return {
    kind: 'call',
    title,
    verb: action === 'pause' ? 'Pause' : action === 'resume' ? 'Resume' : 'Abort',
    danger: action === 'abort',
    make: reason => ({ action, body: buildScopeRequest(rollout, reason) }),
  };
}

/** hostsIntent skips or retries hosts of a rollout; no hosts means all of them. */
export function hostsIntent(
  action: 'skip' | 'retry',
  title: string,
  rollout: string,
  hosts: readonly string[],
  /** rolloutState is the state of the rollout, for the permission to retry hosts of it. */
  rolloutState?: string
): ActionIntent {
  return {
    kind: 'call',
    title,
    verb: action === 'skip' ? 'Skip' : 'Retry',
    rolloutState,
    make: reason => ({ action, body: buildRolloutHostsRequest(rollout, hosts, reason) }),
  };
}

/**
 * endLeaseIntent asks to end one open lease (granted, running or verifying):
 * the agent stops after the step in progress and the lease ends as failed.
 */
export function endLeaseIntent(lease: string, host: string): ActionIntent {
  return {
    kind: 'call',
    title: `End the lease of ${host}`,
    verb: 'End the lease',
    danger: true,
    leaseHost: host,
    make: reason => ({ action: 'end-lease', body: buildEndLeaseRequest(lease, reason) }),
  };
}

/** clientPolicyIntent sets the image policy of one client; an empty policy returns it to the global default. */
export function clientPolicyIntent(client: string, policy: Policy | ''): ActionIntent {
  return {
    kind: 'call',
    title: policy === '' ? `Image policy of ${client}: back to the default` : `Image policy of ${client}: ${policy}`,
    verb: 'Set the policy',
    make: reason => ({ action: 'client-policy', body: buildClientPolicyRequest(client, policy, reason) }),
  };
}
