import { describe, expect, it } from 'vitest';

import type { Gate, QueueEntry, Rollout, RolloutHost } from '../api/types';
import { approvableAtGate, gateApprovalNotes, waitReasons, waitsForApproval } from './approvals';

function rollout(p: Partial<Rollout>): Rollout {
  return {
    id: 'ro-1',
    kind: 'group',
    key: 'unit:cl#prysm@0011223344556677',
    title: 'prysm cl',
    state: 'active',
    paused: false,
    approved: false,
    force: false,
    ignoreBudget: false,
    ignoreGate: false,
    createdBy: 'scheduler',
    createdAt: '2026-11-20T10:00:00Z',
    updatedAt: '2026-11-20T10:00:00Z',
    hosts: [{ host: 'a' } as RolloutHost],
    policy: 'auto',
    ...p,
  };
}

const queue = (wait: QueueEntry['wait'], rolloutId = 'ro-1') =>
  new Map<string, QueueEntry>([
    ['a', { host: 'a', position: 1, reason: `${wait} reason`, wait, rollouts: [rolloutId], rollout: rolloutId }],
  ]);

describe('waitsForApproval', () => {
  it('holds a rollout on the manual policy until it is approved', () => {
    expect(waitsForApproval(rollout({ policy: 'manual' }))).toBe(true);
    expect(waitsForApproval(rollout({ policy: 'manual', approved: true }))).toBe(false);
    expect(waitsForApproval(rollout({ policy: 'auto' }))).toBe(false);
  });
});

describe('waitReasons', () => {
  it('names the reasons of the members that wait for this rollout', () => {
    expect(waitReasons(rollout({}), queue('gate'), 'gate')).toEqual(['gate reason']);
    expect(waitReasons(rollout({}), queue('gate', 'ro-2'), 'gate')).toEqual([]);
    expect(waitReasons(rollout({}), queue('budget'), 'gate')).toEqual([]);
  });
});

describe('approvableAtGate', () => {
  it('holds an auto rollout at the closed gate until it is approved', () => {
    expect(approvableAtGate(rollout({}), queue('gate'))).toBe(true);
    expect(approvableAtGate(rollout({ approved: true }), queue('gate'))).toBe(false);
    expect(approvableAtGate(rollout({ policy: 'manual' }), queue('gate'))).toBe(false);
    expect(approvableAtGate(rollout({ paused: true }), queue('gate'))).toBe(false);
    expect(approvableAtGate(rollout({ ignoreGate: true }), queue('gate'))).toBe(false);
    expect(approvableAtGate(rollout({ ignoreBudget: true }), queue('gate'))).toBe(false);
    expect(approvableAtGate(rollout({}), queue('budget'))).toBe(false);
    expect(approvableAtGate(rollout({}), queue('gate', 'ro-2'))).toBe(false);
  });
});

describe('gateApprovalNotes', () => {
  it('names why the gate is closed and what approving does', () => {
    expect(gateApprovalNotes({ ok: false, reasons: ['finality lag 5 epochs > 3'] } as Gate)).toEqual([
      'Network gate closed: finality lag 5 epochs > 3',
      'Approved, the rollout rolls one host at a time while the gate stays closed.',
    ]);
  });
});
