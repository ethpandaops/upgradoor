// Which open rollouts wait for a person to approve them: on the manual
// policy, or an auto rollout at the closed network gate.

import { list, type Gate, type QueueEntry, type Rollout } from '../api/types';

/** GATE_APPROVAL_HINT says what approving a rollout at the closed gate does. */
export const GATE_APPROVAL_HINT =
  'Auto rollouts never roll through a closed network gate; approved, it rolls one host at a time';

/** waitsForApproval says whether a rollout waits for a person to approve it: its policy is manual. */
export function waitsForApproval(rollout: Rollout): boolean {
  return rollout.policy === 'manual' && !rollout.approved;
}

/**
 * waitReasons returns the distinct reasons of the members that wait for wait
 * in this rollout; empty when none does.
 */
export function waitReasons(
  rollout: Rollout,
  queue: ReadonlyMap<string, QueueEntry>,
  wait: QueueEntry['wait']
): string[] {
  const reasons = new Set<string>();
  for (const h of list(rollout.hosts)) {
    const q = queue.get(h.host);
    if (q?.wait === wait && q.rollout === rollout.id) reasons.add(q.reason);
  }
  return [...reasons];
}

/**
 * approvableAtGate says whether an auto rollout waits at the closed network
 * gate for a person: it never rolls through a closed gate unless approved.
 */
export function approvableAtGate(rollout: Rollout, queue: ReadonlyMap<string, QueueEntry>): boolean {
  return (
    rollout.policy !== 'manual' &&
    !rollout.approved &&
    !rollout.paused &&
    !rollout.ignoreGate &&
    !rollout.ignoreBudget &&
    waitReasons(rollout, queue, 'gate').length > 0
  );
}

/** gateApprovalNotes are the dialog notes of approving a rollout at the closed gate. */
export function gateApprovalNotes(gate: Gate | undefined): string[] {
  return [
    ...list(gate?.reasons).map(reason => `Network gate closed: ${reason}`),
    'Approved, the rollout rolls one host at a time while the gate stays closed.',
  ];
}
