// Builders for the action request bodies (scheduler.RollRequest etc.) and the
// sentences that tell an operator exactly what an action will do.

import type { ActionCall } from '../api/client';
import type {
  ApproveLeaseRequest,
  ClientPolicyRequest,
  EndLeaseRequest,
  HoldRequest,
  OfflineRequest,
  RebootRequest,
  RollRequest,
  RolloutHostsRequest,
  ScopeRequest,
} from '../api/types';
import { hostList, plural } from './format';

/** "now": granted as fast as budget and gate allow; "staggered": a manual rollout through the normal rules. */
export type Timing = 'now' | 'staggered';

interface HostSelection {
  /** hosts is ignored when all is set. */
  hosts: readonly string[];
  /** all selects the whole fleet. */
  all: boolean;
}

interface DisruptiveOptions extends HostSelection {
  timing: Timing;
  /** force only applies to a staggered action; "now" is forced by definition. */
  force: boolean;
  ignoreBudget: boolean;
  /** ignoreGate passes a closed network gate; absent is false. */
  ignoreGate?: boolean;
  reason: string;
}

interface RollOptions extends DisruptiveOptions {
  /** containers limits the roll; empty means every managed container. */
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
    ignoreGate: o.ignoreGate === true,
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
    ignoreGate: o.ignoreGate === true,
    reason: o.reason.trim(),
  };
}

/** buildHoldRequest builds the body of the hold, unhold and forget actions. */
export function buildHoldRequest(hosts: readonly string[], reason: string, holders?: readonly string[]): HoldRequest {
  const body: HoldRequest = { hosts: [...new Set(hosts)].sort(), reason: reason.trim() };
  if (holders && holders.length > 0) body.holders = [...holders];
  return body;
}

/** buildOfflineRequest builds the body of mark-offline and unmark-offline. */
export function buildOfflineRequest(hosts: readonly string[], reason: string, until?: string): OfflineRequest {
  return { hosts: [...new Set(hosts)].sort(), reason: reason.trim(), ...(until ? { until } : {}) };
}

/** buildEndLeaseRequest builds the body of end-lease. */
export function buildEndLeaseRequest(lease: string, reason: string): EndLeaseRequest {
  return { lease, reason: reason.trim() };
}

/** buildClientPolicyRequest builds the body of client-policy; an empty policy returns the client to the default. */
export function buildClientPolicyRequest(
  client: string,
  policy: ClientPolicyRequest['policy'],
  reason: string
): ClientPolicyRequest {
  return { client, policy, reason: reason.trim() };
}

/** buildScopeRequest builds the body of pause, resume and abort; an empty rollout is the global scope. */
export function buildScopeRequest(rollout: string, reason: string): ScopeRequest {
  return { rollout, reason: reason.trim() };
}

/** buildApproveLeaseRequest builds the body of approve-lease: the verifying lease of one member of a rollout. */
export function buildApproveLeaseRequest(rollout: string, host: string, reason: string): ApproveLeaseRequest {
  return { rollout, host, reason: reason.trim() };
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

  if (o.ignoreGate === true && !o.ignoreBudget) {
    lines.push(
      'Despite the network state: the hosts pass a closed network gate, whatever its reason. ' +
        'The budget, the host limit and the canary still apply. This is recorded in the audit log.'
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
      ? 'every managed container'
      : `${plural(containers.length, 'container')} (${containers.join(', ')})`;

  return [
    `Roll ${what} on ${selectionText(o)}: pull the tag the container already uses, stop it, ` +
      'recreate it with identical configuration and start it. Unmanaged containers are not touched.',
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

const OVERRIDE_HOW: readonly string[] = [
  'The controller passes this to the agent of the host, which may refuse it (its configuration says what may be overridden). ' +
    'Nothing changes at once: the agent recreates the container under its next lease, through the budget and the network gate like any other update.',
];

function untilText(until: string | undefined): string {
  return until ? `, until ${new Date(until).toLocaleString([], { hourCycle: 'h23' })}` : ', for 24 hours';
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
    case 'mark-offline':
      return [
        `Mark ${selectionText({ hosts: call.body.hosts, all: false })} as known offline${call.body.until ? untilText(call.body.until) : ' until a person lifts it'}: ` +
          'its validators are left out of the network gate and the budget, and it gets no lease. Everything else is still shown.',
      ];
    case 'unmark-offline':
      return [
        `Lift the known-offline mark on ${selectionText({ hosts: call.body.hosts, all: false })}: ` +
          'its validators count for the gate and the budget again.',
      ];
    case 'forget':
      return [
        `Forget ${selectionText({ hosts: call.body.hosts, all: false })}: delete the host with its inventory, ` +
          'packages, needs and liveness. Rollout, lease and audit history is kept. ' +
          'A host whose agent still runs registers again on its next sync. Refused while the host holds a lease.',
      ];
    case 'end-lease':
      return [
        `End lease ${call.body.lease}. The agent begins no further step: it completes the step in progress ` +
          'and stops. A lease the agent has not started is not started.',
        'The lease stays open and keeps its slot until the agent reports; then it ends as cancelled (as failed ' +
          'when the agent goes silent or a step failed).',
        'A cancelled lease pauses its rollouts: resume them to go on with the other hosts; this host waits ' +
          'until somebody retries it. No hold is set: hold the host if it is to be left alone.',
      ];
    case 'client-policy': {
      const { client, policy } = call.body;
      if (policy === '') {
        return [
          `Remove the image policy of ${client}: its image rollouts answer to the policy of their kind, else the policy for images, again.`,
          'An open rollout that was approved stays approved.',
        ];
      }
      return [
        `Set the image policy of ${client} to ${policy}: ` +
          (policy === 'manual'
            ? 'a new image of the client is queued and waits for a person to approve the rollout.'
            : 'a new image of the client rolls out by itself.'),
        policy === 'manual'
          ? 'An open image rollout of the client that nobody approved waits for approval from now on; one that was approved stays approved.'
          : 'An open image rollout of the client that waits for approval starts.',
        'The other clients, packages and reboots keep their policies.',
      ];
    }
    case 'image-override': {
      const { host, container, image, until } = call.body;
      return image === ''
        ? [
            `Clear the image override of ${container} on ${host}: the container goes back to its configured image.`,
            ...OVERRIDE_HOW,
          ]
        : [
            `Run ${container} on ${host} from ${image} instead of its configured image${untilText(until)}.`,
            ...OVERRIDE_HOW,
            'While the override is in effect the container is not rolled with its configured tag. When it ends the host puts the configured image back, again under a lease.',
          ];
    }
    case 'flag-override': {
      const { host, container, addFlags, removeFlags, until } = call.body;
      return addFlags.length + removeFlags.length === 0
        ? [
            `Clear the flag override of ${container} on ${host}: the container goes back to its configured command line.`,
            ...OVERRIDE_HOW,
          ]
        : [
            `Change the command line of ${container} on ${host}${untilText(until)}: ` +
              [
                removeFlags.length > 0 ? `remove ${removeFlags.join(', ')}` : '',
                addFlags.length > 0 ? `add ${addFlags.join(', ')}` : '',
              ]
                .filter(Boolean)
                .join('; ') +
              '.',
            ...OVERRIDE_HOW,
            'When the override ends the host puts the configured command line back, again under a lease.',
          ];
    }
    case 'client-override': {
      const { client, kind, hosts, part, image, addFlags, removeFlags, until } = call.body;
      const on = `${client} (${kind}) on ${hosts.length === 1 ? hosts[0] : `${hosts.length} hosts`}`;
      const flags = [
        (removeFlags ?? []).length > 0 ? `remove ${(removeFlags ?? []).join(', ')}` : '',
        (addFlags ?? []).length > 0 ? `add ${(addFlags ?? []).join(', ')}` : '',
      ]
        .filter(Boolean)
        .join('; ');
      if (part === 'image') {
        return image
          ? [`Run ${on} from ${image} instead of the configured image${untilText(until)}.`, ...OVERRIDE_HOW]
          : [`Clear the image override of ${on}: the containers go back to the configured image.`, ...OVERRIDE_HOW];
      }
      return flags
        ? [`Change the command line of ${on}${untilText(until)}: ${flags}.`, ...OVERRIDE_HOW]
        : [`Clear the flag override of ${on}: the containers go back to the configured command line.`, ...OVERRIDE_HOW];
    }
    case 'pause':
      return call.body.rollout === ''
        ? ['Pause everything: no new lease is granted for any rollout. Open leases finish.']
        : [`Pause rollout ${call.body.rollout}: no new lease is granted for it. Open leases finish.`];
    case 'resume':
      return call.body.rollout === ''
        ? ['Lift the global pause: the scheduler grants leases again.']
        : [
            `Resume rollout ${call.body.rollout}: lift its pause and approve it if it waits on a manual policy ` +
              'or at the closed network gate (then one host at a time while the gate stays closed). ' +
              'Hosts are then granted under the normal rules.',
          ];
    case 'ignore-gate':
      return [
        `Let rollout ${call.body.rollout} pass the network gate while it is closed, whatever its reason: ` +
          'missing or stale data included.',
        'The availability budget, the host limit and the canary still apply. Other rollouts keep waiting at the gate. ' +
          'This is recorded in the audit log.',
      ];
    case 'ignore-budget':
      return [
        `Let rollout ${call.body.rollout} ignore the availability budget and the network gate from now on.`,
        'The host limit and the canary still apply. Other rollouts keep waiting for the budget. ' +
          'This is recorded in the audit log.',
      ];
    case 'approve-lease':
      return [
        `Approve the lease of ${call.body.host} in rollout ${call.body.rollout} now: it is released as ready ` +
          'without waiting any longer for the node to be back at head and its validators to attest.',
        'The host counts as served; the next hosts of the rollout may start. This is recorded in the audit log.',
      ];
    case 'abort':
      return [
        `Abort rollout ${call.body.rollout}. A manual rollout is closed, and what it has not started is cancelled.`,
        'A rollout of an image, of packages or of a reboot stays open for as long as hosts report its cause: ' +
          'it is paused instead, and nothing more is granted in it until someone resumes it or the cause changes.',
        'Open leases finish.',
      ];
    case 'skip':
      return [
        `Skip ${selectionText({ hosts: call.body.hosts, all: false })} in rollout ${call.body.rollout}: ` +
          'the host is not served for this rollout and nobody waits for it.',
        'A rollout of an image, of packages or of a reboot stays open while a skipped host still has the need; ' +
          'the host counts as done once it reports without it. In a manual rollout the skip is final.',
      ];
    case 'retry':
      return [
        call.body.hosts.length === 0
          ? `Retry rollout ${call.body.rollout}: every failed host returns to pending and the rollout becomes active again.`
          : `Retry ${selectionText({ hosts: call.body.hosts, all: false })} in rollout ${call.body.rollout}: ` +
            'a host that failed or was skipped returns to pending, and a halted rollout becomes active again ' +
            'once none of its hosts is failed.',
        'Only an open rollout can be retried.',
      ];
    case 'host-action': {
      const { action, hosts, all, client, pauseAfterCanary } = call.body;
      const on = all
        ? 'every host that has it'
        : client
          ? `the hosts of ${client}`
          : selectionText({ hosts: hosts ?? [], all: false });
      const one = !all && !client && (hosts ?? []).length === 1;
      return [
        (one
          ? `Run the host action ${action} on ${on} under a lease.`
          : `Run the host action ${action} on ${on}, one lease per host, staggered: one canary first, then the others.`) +
          (pauseAfterCanary ? ' The rollout pauses after the canary until a person resumes it.' : ''),
        'Each lease goes through the budget and the network gate like any other update. ' +
          'A host the login may not start the action on is rejected; a failed run halts the rollout.',
      ];
    }
  }
}
