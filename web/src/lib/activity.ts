// What the Overview says about the scheduler's state: which queued hosts will
// be served by themselves, and what is stuck until somebody acts.

import { list, type Gate, type HostSummary, type QueueEntry, type Rollout, type WaitKind } from '../api/types';

export type AttentionKind =
  | 'gate'
  | 'paused'
  | 'halted'
  | 'approval'
  | 'rollout-paused'
  | 'policy-paused'
  | 'held'
  | 'silent'
  | 'verb';

export interface AttentionItem {
  /** Stable key for the list. */
  id: string;
  kind: AttentionKind;
  tone: 'danger' | 'warning';
  title: string;
  detail: string;
  /** The hosts that wait because of it. */
  hosts: string[];
  /** The rollout to act on (halted, approval, rollout-paused). */
  rollout?: Rollout;
}

const ROLLOUT_WAITS: ReadonlySet<WaitKind> = new Set(['halted', 'approval', 'rollout-paused', 'policy-paused']);

function waitingHosts(n: number): string {
  return n === 1 ? '1 host waits' : `${n} hosts wait`;
}

/**
 * attentionItems groups everything that keeps hosts from being served until
 * the network recovers or a person acts: what is broken (a closed gate, a
 * halted rollout) first, then what merely waits for a decision.
 */
export function attentionItems(input: {
  gate: Gate | undefined;
  paused: boolean;
  queue: readonly QueueEntry[];
  rollouts: readonly Rollout[];
  hosts: readonly HostSummary[];
}): AttentionItem[] {
  const { queue } = input;
  const items: AttentionItem[] = [];
  const hostsWaiting = (wait: WaitKind) => queue.filter(q => q.wait === wait).map(q => q.host);

  if (input.gate && !input.gate.ok) {
    const waiting = hostsWaiting('gate');
    items.push({
      id: 'gate',
      kind: 'gate',
      tone: 'danger',
      title: 'The network gate is closed',
      detail:
        (list(input.gate.reasons).join(' · ') || 'no reason given') +
        (waiting.length > 0 ? `. ${waitingHosts(waiting.length)} for it to open.` : '. Nothing new is started.'),
      hosts: waiting,
    });
  }

  if (input.paused) {
    items.push({
      id: 'paused',
      kind: 'paused',
      tone: 'warning',
      title: 'The scheduler is paused',
      detail: 'No lease is granted until it is resumed.',
      hosts: hostsWaiting('paused'),
    });
  }

  const byId = new Map(input.rollouts.map(r => [r.id, r]));
  const groups = new Map<string, AttentionItem>();

  for (const q of queue) {
    if (!q.wait || !ROLLOUT_WAITS.has(q.wait)) continue;

    // The rollout the reason is about. The plan names it; an older
    // controller does not, and then only a host in a single rollout is
    // unambiguous.
    const candidates = list(q.rollouts);
    const about = q.rollout ?? (candidates.length === 1 ? candidates[0] : undefined);
    const rollout = about === undefined ? undefined : byId.get(about);
    const id = `${q.wait}/${rollout?.id ?? ''}`;

    let item = groups.get(id);
    if (!item) {
      const name = rollout?.title ?? 'A rollout';
      item = {
        id,
        kind: q.wait as AttentionKind,
        tone: q.wait === 'halted' ? 'danger' : 'warning',
        title:
          q.wait === 'halted'
            ? `${name} is halted`
            : q.wait === 'approval'
              ? `${name} waits for approval`
              : q.wait === 'rollout-paused'
                ? rollout?.pauseReason
                  ? `${name} is paused after its canary`
                  : `${name} is paused`
                : `${name}: its policy is set to paused`,
        detail: q.wait === 'halted' ? (rollout?.reason ?? q.reason) : '',
        hosts: [],
        rollout,
      };
      groups.set(id, item);
      items.push(item);
    }
    item.hosts.push(q.host);
  }

  for (const item of groups.values()) {
    const waits = `${waitingHosts(item.hosts.length)}.`;
    item.detail = item.detail ? `${item.detail}. ${waits}` : waits;
  }

  const held = hostsWaiting('held');
  if (held.length > 0) {
    items.push({
      id: 'held',
      kind: 'held',
      tone: 'warning',
      title: held.length === 1 ? `${held[0] as string} is held` : `${held.length} hosts are held`,
      detail: 'Held hosts have updates pending and are skipped until the hold is lifted.',
      hosts: held,
    });
  }

  // Every silent host counts, queued or not: nobody knows what it needs.
  const silent = [...new Set([...hostsWaiting('silent'), ...input.hosts.filter(h => h.stale).map(h => h.name)])];
  if (silent.length > 0) {
    items.push({
      id: 'silent',
      kind: 'silent',
      tone: 'warning',
      title: silent.length === 1 ? `${silent[0] as string} is silent` : `${silent.length} hosts are silent`,
      detail: 'The agent has stopped syncing; the host is left alone until it reports again.',
      hosts: silent,
    });
  }

  const verb = hostsWaiting('verb');
  if (verb.length > 0) {
    items.push({
      id: 'verb',
      kind: 'verb',
      tone: 'warning',
      title: verb.length === 1 ? `${verb[0] as string} refuses the work` : `${verb.length} hosts refuse the work`,
      detail: 'The agent configuration does not allow what the rollout needs.',
      hosts: verb,
    });
  }

  // Stable: within a tone the order above stands.
  return [...items.filter(i => i.tone === 'danger'), ...items.filter(i => i.tone !== 'danger')];
}
