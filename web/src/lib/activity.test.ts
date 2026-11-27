import { describe, expect, it } from 'vitest';

import type { Gate, HostSummary, QueueEntry, Rollout, RolloutHost } from '../api/types';
import { attentionItems } from './activity';

function entry(
  host: string,
  position: number,
  wait: QueueEntry['wait'],
  rollouts: string[] = [],
  rollout?: string
): QueueEntry {
  return { host, position, reason: `reason ${host}`, wait, rollouts, rollout };
}

const rollout = (id: string, over: Partial<Rollout> = {}) =>
  ({ id, title: `Rollout ${id}`, state: 'active', ...over }) as Rollout;
const gate = (ok: boolean, reasons: string[] = []) => ({ ok, reasons }) as Gate;

describe('attentionItems', () => {
  it('is empty when everything flows', () => {
    expect(
      attentionItems({ gate: gate(true), paused: false, queue: [entry('a', 1, 'budget')], rollouts: [], hosts: [] })
    ).toEqual([]);
  });

  it('groups the waiting hosts by what blocks them', () => {
    const items = attentionItems({
      gate: gate(false, ['participation 80% < 90%']),
      paused: true,
      queue: [
        entry('g', 1, 'gate'),
        entry('a1', 2, 'approval', ['r1'], 'r1'),
        entry('a2', 3, 'approval', ['r1'], 'r1'),
        entry('h1', 4, 'halted', ['r1', 'r2'], 'r2'),
        entry('held', 5, 'held'),
        entry('quiet', 6, 'silent'),
      ],
      rollouts: [rollout('r1'), rollout('r2', { state: 'halted', reason: 'x failed' })],
      hosts: [{ name: 'quiet', stale: true } as HostSummary, { name: 'gone', stale: true } as HostSummary],
    });

    expect(items.map(i => [i.kind, i.title, i.hosts])).toEqual([
      ['gate', 'The network gate is closed', ['g']],
      ['halted', 'Rollout r2 is halted', ['h1']],
      ['paused', 'The scheduler is paused', []],
      ['approval', 'Rollout r1 waits for approval', ['a1', 'a2']],
      ['held', 'held is held', ['held']],
      ['silent', '2 hosts are silent', ['quiet', 'gone']],
    ]);
    expect(items[0]?.detail).toBe('participation 80% < 90%. 1 host waits for it to open.');
    expect(items[3]?.detail).toBe('2 hosts wait.');
    expect(items[1]?.detail).toBe('x failed. 1 host waits.');
    expect(items[1]?.rollout?.id).toBe('r2');
  });

  it('says until when the deployment window holds the hosts, and nothing once it is over', () => {
    const until = '2026-11-28T10:00:00Z';
    const items = attentionItems({
      gate: gate(true),
      paused: false,
      deployWindow: { open: true, until },
      queue: [entry('a', 1, 'deploy-window'), entry('b', 2, 'deploy-window')],
      rollouts: [],
      hosts: [],
    });

    expect(items.map(i => [i.kind, i.hosts])).toEqual([['deploy-window', ['a', 'b']]]);
    expect(items[0]?.title).toContain('Initial deployment window until ');
    expect(items[0]?.title).toContain(until.replace('Z', '.000Z'));
    expect(items[0]?.detail).toBe(
      'Nothing but Ansible deploys is granted until it ends or upgradoor is armed. 2 hosts wait.'
    );

    expect(
      attentionItems({
        gate: gate(true),
        paused: false,
        deployWindow: { open: false, until, armedAt: until },
        queue: [],
        rollouts: [],
        hosts: [],
      })
    ).toEqual([]);
  });

  it('names the hosts with an unsupported agent apart from the silent ones', () => {
    const reason = 'agent protocol 0 not supported (controller supports 1..1)';
    const items = attentionItems({
      gate: gate(true),
      paused: false,
      queue: [entry('old', 1, 'silent'), entry('quiet', 2, 'silent')],
      rollouts: [],
      hosts: [{ name: 'old', stale: false, incompatible: reason } as HostSummary],
    });

    expect(items.map(i => [i.kind, i.title, i.hosts])).toEqual([
      ['incompatible', 'old runs an unsupported agent', ['old']],
      ['silent', 'quiet is silent', ['quiet']],
    ]);
  });

  it('acts on the rollout the plan names, not on the first one of the host', () => {
    // h is in a rollout somebody paused (A) and in one that waits for
    // approval (B). Approving must not resume A.
    const [item] = attentionItems({
      gate: gate(true),
      paused: false,
      queue: [entry('h', 1, 'approval', ['A', 'B'], 'B')],
      rollouts: [rollout('A', { paused: true }), rollout('B')],
      hosts: [],
    });

    expect(item?.title).toBe('Rollout B waits for approval');
    expect(item?.rollout?.id).toBe('B');
  });

  it('asks for the approval of an auto rollout that waits at the closed gate', () => {
    const member = (host: string) => ({ host }) as RolloutHost;
    const items = attentionItems({
      gate: gate(false, ['finality lag 5 epochs > 3']),
      paused: false,
      queue: [
        entry('g1', 1, 'gate', ['A'], 'A'),
        entry('g2', 2, 'gate', ['A'], 'A'),
        entry('m', 3, 'approval', ['M'], 'M'),
        entry('p', 4, 'gate', ['P'], 'P'),
        entry('i', 5, 'gate', ['I'], 'I'),
      ],
      rollouts: [
        rollout('A', { policy: 'auto', hosts: [member('g1'), member('g2')] }),
        rollout('M', { policy: 'manual', hosts: [member('m')] }),
        rollout('P', { policy: 'auto', approved: true, hosts: [member('p')] }),
        rollout('I', { policy: 'auto', ignoreGate: true, hosts: [member('i')] }),
      ],
      hosts: [],
    });

    expect(items.map(i => [i.kind, i.title, i.hosts])).toEqual([
      ['gate', 'The network gate is closed', ['g1', 'g2', 'p', 'i']],
      ['approval', 'Rollout M waits for approval', ['m']],
      ['gate-approval', 'Rollout A waits at the closed gate', ['g1', 'g2']],
    ]);
    expect(items[2]?.detail).toBe(
      'Auto rollouts never roll through a closed network gate; approved, it rolls one host at a time. 2 hosts wait.'
    );
    expect(items[2]?.rollout?.id).toBe('A');
  });

  it('says why a rollout is paused: after its canary, by an abort, or by a person', () => {
    const items = attentionItems({
      gate: gate(true),
      paused: false,
      queue: [
        entry('h1', 1, 'rollout-paused', ['A'], 'A'),
        entry('h2', 2, 'rollout-paused', ['B'], 'B'),
        entry('h3', 3, 'rollout-paused', ['C'], 'C'),
      ],
      rollouts: [
        rollout('A', { paused: true, pauseReason: 'paused after the canary; resume to continue' }),
        rollout('B', { paused: true }),
        rollout('C', { paused: true, pauseReason: 'aborted: wrong digest; resume to continue' }),
      ],
      hosts: [],
    });

    expect(items.map(i => [i.kind, i.title, i.rollout?.id])).toEqual([
      ['rollout-paused', 'Rollout A is paused after the canary', 'A'],
      ['rollout-paused', 'Rollout B is paused', 'B'],
      ['rollout-paused', 'Rollout C is paused (aborted: wrong digest)', 'C'],
    ]);
  });
});
