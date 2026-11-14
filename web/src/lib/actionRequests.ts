// Builders for the action request bodies (scheduler.RollRequest etc.) and the
// sentences that tell an operator exactly what an action will do.

import type { ActionCall } from '../api/client';
import type {
  EndLeaseRequest,
  HoldRequest,
  RebootRequest,
  RollRequest,
  RolloutHostsRequest,
  ScopeRequest,
} from '../api/types';
import { hostList, plural } from './format';

/** "now": granted as fast as budget and gate allow; "staggered": a manual rollout through the normal rules. */
export type Timing = 'now' | 'staggered';

export interface HostSelection {
  /** hosts is ignored when all is set. */
  hosts: readonly string[];
  /** all selects the whole fleet. */
  all: boolean;
}

export interface DisruptiveOptions extends HostSelection {
  timing: Timing;
  /** force only applies to a staggered action; "now" is forced by definition. */
  force: boolean;
  ignoreBudget: boolean;
  reason: string;
}

export interface RollOptions extends DisruptiveOptions {
  /** containers limits the roll; empty means every managed, non-custom container. */
  containers: readonly string[];
}

function selectionHosts(sel: HostSelection): string[] {
  return sel.all ? [] : [...new Set(sel.hosts)].sort();
}

/** buildRollRequest builds the body of POST /api/v1/actions/roll. */
export function buildRollRequest(o: RollOptions): RollRequest {
  return {
    hosts: selectionHosts(o),
    all: o.all,
    containers: [...new Set(o.containers)].sort(),
    staggered: o.timing === 'staggered',
    force: o.timing === 'staggered' && o.force,
    ignoreBudget: o.ignoreBudget,
    reason: o.reason.trim(),
  };
}

/** buildRebootRequest builds the body of POST /api/v1/actions/reboot. */
export function buildRebootRequest(o: DisruptiveOptions): RebootRequest {
  return {
    hosts: selectionHosts(o),
    all: o.all,
    staggered: o.timing === 'staggered',
    force: o.timing === 'staggered' && o.force,
    ignoreBudget: o.ignoreBudget,
    reason: o.reason.trim(),
  };
}

/** buildHoldRequest builds the body of the hold, unhold and forget actions. */
export function buildHoldRequest(hosts: readonly string[], reason: string): HoldRequest {
  return { hosts: [...new Set(hosts)].sort(), reason: reason.trim() };
}

/** buildEndLeaseRequest builds the body of end-lease. */
export function buildEndLeaseRequest(lease: string, reason: string): EndLeaseRequest {
  return { lease, reason: reason.trim() };
}

/** buildScopeRequest builds the body of pause, resume and abort; an empty rollout is the global scope. */
export function buildScopeRequest(rollout: string, reason: string): ScopeRequest {
  return { rollout, reason: reason.trim() };
}

/** buildRolloutHostsRequest builds the body of skip and retry. */
export function buildRolloutHostsRequest(
  rollout: string,
  hosts: readonly string[],
  reason: string
): RolloutHostsRequest {
  return { rollout, hosts: [...new Set(hosts)].sort(), reason: reason.trim() };
}

function selectionText(sel: HostSelection): string {
  if (sel.all) return 'every host of the fleet';
  const hosts = selectionHosts(sel);
  return hosts.length === 1 ? (hosts[0] as string) : `${plural(hosts.length, 'host')} (${hostList(hosts)})`;
}

function timingText(o: DisruptiveOptions): string[] {
  const lines: string[] = [];

  // With ignoreBudget the budget is not a limit, so the sentences must not promise one.
  const budget = !o.ignoreBudget;

  if (o.timing === 'now') {
    lines.push(
      `Now: each host is granted a slot ${
        budget ? 'as soon as the availability budget and the network gate allow' : 'at once'
      }, without a canary and without waiting for readiness between hosts.`
    );
  } else if (o.force) {
    lines.push(
      'Staggered, forced: a manual rollout that skips the order and the ready check.' +
        (budget ? ' Hosts are still granted under the availability budget and the network gate.' : '')
    );
  } else {
    lines.push(
      'Staggered: a manual rollout: one host first as the canary, ' +
        `then all the others${budget ? ' as the availability budget allows' : ''}; ` +
        'the others start once the canary is back at head with peers and its validators attest again.'
    );
  }

  if (o.ignoreBudget) {
    lines.push(
      'IGNORE BUDGET: the availability budget, the host limit and the network gate are skipped. ' +
        'Nothing limits how many of these hosts are down at the same time. This is recorded in the audit log.'
    );
  }

  return lines;
}

/** describeRoll states what a roll request will do, one sentence per line. */
export function describeRoll(o: RollOptions): string[] {
  const containers = [...new Set(o.containers)].sort();
  const what =
    containers.length === 0
      ? 'every managed, non-custom container'
      : `${plural(containers.length, 'container')} (${containers.join(', ')})`;

  return [
    `Roll ${what} on ${selectionText(o)}: pull the tag the container already uses, stop it, ` +
      'recreate it with identical configuration and start it. Custom and unmanaged containers are not touched.',
    ...timingText(o),
  ];
}

/** describeReboot states what a reboot request will do. */
export function describeReboot(o: DisruptiveOptions): string[] {
  return [
    `Reboot ${selectionText(o)}: the agent stops the validator, consensus and execution clients in that order, ` +
      'then reboots the host.',
    ...timingText(o),
  ];
}

/** describeCall states what a simple (non roll / reboot) action will do. */
export function describeCall(call: ActionCall): string[] {
  switch (call.action) {
    case 'roll':
    case 'reboot':
      return [];
    case 'hold':
      return [
        `Put ${selectionText({ hosts: call.body.hosts, all: false })} on controller hold: ` +
          'the agent reports no needs and refuses leases until the hold is lifted. A running lease finishes.',
      ];
    case 'unhold':
      return [
        `Lift the controller hold on ${selectionText({ hosts: call.body.hosts, all: false })}. ` +
          'A local hold file on the host stays in effect; only a person on the host can remove it.',
      ];
    case 'forget':
      return [
        `Forget ${selectionText({ hosts: call.body.hosts, all: false })}: delete the host with its inventory, ` +
          'packages, needs and liveness. Rollout, lease and audit history is kept. ' +
          'A host whose agent still runs registers again on its next sync. Refused while the host holds a lease.',
      ];
    case 'end-lease':
      return [
        `End lease ${call.body.lease} as failed ("ended by operator"). The host no longer counts as holding a ` +
          'lease, the rollouts the lease served halt, and rollouts that waited for it go on.',
        'The host is broken from then on: it goes first in every rollout of the same image or the packages ' +
          'until a lease succeeds on it. Hold the host to take it out of the way.',
        'The agent is not stopped: it finishes the operation it has started. ' +
          'Refused for a lease the agent has not started; that one ends by itself.',
      ];
    case 'pause':
      return call.body.rollout === ''
        ? ['Pause everything: no new lease is granted for any rollout. Open leases finish.']
        : [`Pause rollout ${call.body.rollout}: no new lease is granted for it. Open leases finish.`];
    case 'resume':
      return call.body.rollout === ''
        ? ['Lift the global pause: the scheduler grants leases again.']
        : [
            `Resume rollout ${call.body.rollout}: lift its pause and approve it if it waits on a manual policy. ` +
              'Hosts are then granted under the normal rules.',
          ];
    case 'abort':
      return [
        `Abort rollout ${call.body.rollout}: it is closed and the same cause opens no new rollout ` +
          'until someone retries it or the cause changes. Open leases finish.',
      ];
    case 'skip':
      return [
        `Skip ${selectionText({ hosts: call.body.hosts, all: false })} in rollout ${call.body.rollout}: ` +
          'the host is not served for this rollout and nobody waits for it any more.',
      ];
    case 'retry':
      return [
        call.body.hosts.length === 0
          ? `Retry rollout ${call.body.rollout}: every failed host returns to pending and the rollout becomes active again.`
          : `Retry ${selectionText({ hosts: call.body.hosts, all: false })} in rollout ${call.body.rollout}: ` +
            'the host returns to pending and the rollout becomes active again.',
        'For an image rollout this lifts the block on the digest.',
      ];
  }
}
