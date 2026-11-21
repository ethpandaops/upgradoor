// The queue as the Overview shows it: the kind of work a host waits for and
// which queued hosts start without anybody acting.

import { list, type NeedKind, type QueueEntry, type WaitKind } from '../api/types';

/** WorkKind is what a lease does to a host; the timeline colours by it. */
type WorkKind = 'image' | 'packages' | 'reboot';

const AUTO_WAITS: ReadonlySet<WaitKind> = new Set(['budget', 'host-limit', 'order']);

/** clearsByItself: the host is served without anybody acting, as the rollout proceeds. */
export function clearsByItself(wait: WaitKind): boolean {
  return AUTO_WAITS.has(wait);
}

const NOBODY_WAITS: ReadonlySet<WaitKind> = new Set(['held', 'silent', 'verb']);

/** holdsBack: hosts behind this one in the order of a rollout wait for it. */
function holdsBack(wait: WaitKind): boolean {
  return !NOBODY_WAITS.has(wait);
}

const KIND_RANK: Record<WorkKind, number> = { image: 0, packages: 1, reboot: 2 };

/**
 * workKind is the most disruptive of the given need kinds; a roll when there
 * is none. An override or spec need recreates a container like an image need does.
 */
export function workKind(kinds: readonly NeedKind[]): WorkKind {
  let best: WorkKind = 'image';
  for (const k of kinds) {
    const work: WorkKind = k === 'override' || k === 'spec' ? 'image' : k;
    if (KIND_RANK[work] > KIND_RANK[best]) best = work;
  }
  return best;
}

/**
 * StageOf is a host's stage in a rollout (fleet.Stage, in serving order: 1
 * repair, the members whose host is broken; 2 canary; 3 rest), or undefined
 * when unknown. A host waits for its place in the order behind the hosts of
 * an earlier stage.
 */
export type StageOf = (rollout: string, host: string) => number | undefined;

/**
 * splitQueue separates the queued hosts that start without anybody acting
 * from the rest. A host that waits for its place in a rollout's order is only
 * in the first group if nothing ahead of it in that rollout is stuck: behind
 * a broken host or a canary that waits for the gate or an approval it waits
 * as long as that host does. A host that is held, silent or reports a verb
 * its agent does not allow holds nobody back: the scheduler does not make
 * anybody wait for it.
 */
export function splitQueue(
  queue: readonly QueueEntry[],
  stageOf: StageOf
): { next: QueueEntry[]; behind: QueueEntry[] } {
  const next: QueueEntry[] = [];
  const behind: QueueEntry[] = [];
  const stuck: QueueEntry[] = [];

  for (const q of [...queue].sort((a, b) => a.position - b.position)) {
    if (!clearsByItself(q.wait)) {
      stuck.push(q);
      continue;
    }

    const rollouts = q.rollout ? [q.rollout] : list(q.rollouts);
    const blocked =
      q.wait === 'order' &&
      stuck.some(
        s =>
          holdsBack(s.wait) &&
          rollouts.some(r => {
            const mine = stageOf(r, q.host);
            const theirs = list(s.rollouts).includes(r) ? stageOf(r, s.host) : undefined;
            return mine !== undefined && theirs !== undefined && theirs < mine;
          })
      );

    if (blocked) {
      behind.push(q);
      stuck.push(q);
    } else {
      next.push(q);
    }
  }

  return { next, behind };
}
