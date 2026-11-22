// The confirm-dialog intents shared by several views.

import type { ActionIntent } from '../components/ActionDialog';
import { list, type Lease, type Policy, type RolloutKind } from '../api/types';
import {
  buildApproveLeaseRequest,
  buildClientPolicyRequest,
  buildEndLeaseRequest,
  buildHoldRequest,
  buildOfflineRequest,
  buildRolloutHostsRequest,
  buildScopeRequest,
} from './actionRequests';
import { plural } from './format';
import type { RolloutAction } from './hostActions';

/** holdIntent builds the confirm dialog of hold / unhold / forget; holders names the holds an unhold lifts. */
export function holdIntent(
  action: 'hold' | 'unhold' | 'forget',
  hosts: readonly string[],
  hiddenHosts: readonly string[] = [],
  holders: readonly string[] = []
): ActionIntent {
  const verb = action === 'hold' ? 'Hold' : action === 'unhold' ? 'Unhold' : 'Forget';
  const of = holders.length > 0 ? ` (hold of ${holders.join(', ')})` : '';
  return {
    kind: 'call',
    title: (hosts.length === 1 ? `${verb} ${hosts[0] as string}` : `${verb} ${plural(hosts.length, 'host')}`) + of,
    verb,
    danger: action === 'forget',
    hiddenHosts,
    make: reason => ({ action, body: buildHoldRequest(hosts, reason, holders) }),
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

/**
 * scopeIntent pauses, resumes or aborts one rollout, or everything with an
 * empty rollout ID. client is the client the rollout is for ("" for none):
 * its team may pause and resume (approve) it. hostAction is the action and
 * members of an action rollout: who may run it on all of them may resume it.
 */
export function scopeIntent(
  action: 'pause' | 'resume' | 'abort',
  title: string,
  rollout: string,
  client?: string,
  hostAction?: RolloutAction
): ActionIntent {
  return {
    kind: 'call',
    title,
    verb: action === 'pause' ? 'Pause' : action === 'resume' ? 'Resume' : 'Abort',
    danger: action === 'abort',
    rolloutClient: rollout === '' ? undefined : (client ?? ''),
    rolloutAction: rollout === '' ? undefined : hostAction,
    make: reason => ({ action, body: buildScopeRequest(rollout, reason) }),
  };
}

/**
 * approveIntent approves one rollout that waits on a manual policy: a resume
 * of a rollout that is not paused, labelled for what it does.
 */
export function approveIntent(
  title: string,
  rollout: string,
  client: string | undefined,
  hostAction?: RolloutAction
): ActionIntent {
  return {
    kind: 'call',
    title,
    verb: 'Approve',
    rolloutClient: client ?? '',
    rolloutAction: hostAction,
    make: reason => ({ action: 'resume', body: buildScopeRequest(rollout, reason) }),
  };
}

/**
 * ignoreGateIntent lets one rollout pass a closed network gate. reasons are
 * the gate's reasons now, shown in the dialog.
 */
export function ignoreGateIntent(
  title: string,
  rollout: string,
  client: string | undefined,
  hostAction: RolloutAction | undefined,
  reasons: readonly string[]
): ActionIntent {
  return {
    kind: 'call',
    title,
    verb: 'Roll despite network state',
    danger: true,
    notes: reasons.map(r => `Network gate closed: ${r}`),
    rolloutClient: client ?? '',
    rolloutAction: hostAction,
    make: reason => ({ action: 'ignore-gate', body: buildScopeRequest(rollout, reason) }),
  };
}

/**
 * approveLeaseIntent releases the verifying lease of one member now. readiness
 * is the lease's readiness note, shown in the dialog.
 */
export function approveLeaseIntent(
  rollout: string,
  host: string,
  client: string | undefined,
  hostAction: RolloutAction | undefined,
  readiness: string | undefined
): ActionIntent {
  return {
    kind: 'call',
    title: `Approve the lease of ${host} now`,
    verb: 'Approve now',
    notes: [`Readiness: ${readiness || 'no note yet'}`],
    rolloutClient: client ?? '',
    rolloutAction: hostAction,
    make: reason => ({ action: 'approve-lease', body: buildApproveLeaseRequest(rollout, host, reason) }),
  };
}

/**
 * hostsIntent skips or retries hosts of a rollout; no hosts means all of them. of is the rollout's kind and
 * team: a team recovers members of a roll of its own containers.
 */
export function hostsIntent(
  action: 'skip' | 'retry',
  title: string,
  rollout: string,
  hosts: readonly string[],
  of?: { kind: RolloutKind; team?: string }
): ActionIntent {
  return {
    kind: 'call',
    title,
    verb: action === 'skip' ? 'Skip' : 'Retry',
    rolloutKind: of?.kind,
    rolloutClient: of?.team ?? '',
    make: reason => ({ action, body: buildRolloutHostsRequest(rollout, hosts, reason) }),
  };
}

/**
 * endLeaseIntent asks to end one open lease (granted, running or verifying):
 * the agent stops after the step in progress and the lease ends as cancelled.
 */
export function endLeaseIntent(lease: Pick<Lease, 'id' | 'host' | 'needs'>): ActionIntent {
  return {
    kind: 'call',
    title: `End the lease of ${lease.host}`,
    verb: 'End the lease',
    danger: true,
    lease: { host: lease.host, needs: list(lease.needs) },
    make: reason => ({ action: 'end-lease', body: buildEndLeaseRequest(lease.id, reason) }),
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
