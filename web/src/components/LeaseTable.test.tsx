import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ActionRun, Lease, LeaseState, StepStatus } from '../api/types';
import { createHarness, renderWithApp } from '../test/harness';
import { LeaseTable } from './LeaseTable';

function lease(host: string, state: LeaseState, over: Partial<Lease> = {}): Lease {
  return {
    id: `ls-${host}`,
    host,
    state,
    needs: [{ id: 'reboot', kind: 'reboot' }],
    rollouts: {},
    weight: 0,
    charged: 0,
    force: false,
    grantedBy: 'scheduler',
    grantedAt: '2026-01-01T00:00:00Z',
    expiresAt: '2026-01-01T00:30:00Z',
    ...over,
  };
}

const row = (host: string) => screen.getByText(host).closest('tr') as HTMLElement;

describe('LeaseTable', () => {
  it('offers End for every open lease, and not again for one that is ending', async () => {
    const onEnd = vi.fn<(l: Lease) => void>();
    const cancelled = { at: '2026-01-01T00:01:00Z', by: 'alice@example.org', state: 'running' as const };
    renderWithApp(
      <LeaseTable
        label="Active leases"
        onEnd={onEnd}
        leases={[
          lease('host-granted', 'granted'),
          lease('host-running', 'running'),
          lease('host-verifying', 'verifying'),
          lease('host-ending', 'running', { cancelled }),
        ]}
      />,
      await createHarness({ mode: 'open' })
    );

    for (const host of ['host-granted', 'host-running', 'host-verifying']) {
      expect(within(row(host)).getByRole('button', { name: /^End the lease of/ })).toBeEnabled();
      expect(within(row(host)).queryByText(/ending/)).toBeNull();
    }

    expect(within(row('host-ending')).getByText('ending (cancelled by alice@example.org)')).toBeInTheDocument();
    expect(within(row('host-ending')).getByRole('button', { name: /^End the lease of/ })).toBeDisabled();

    await userEvent.setup().click(within(row('host-granted')).getByRole('button', { name: /^End the lease of/ }));
    expect(onEnd).toHaveBeenCalledTimes(1);
    expect(onEnd.mock.calls[0]?.[0].host).toBe('host-granted');
  });

  it('does not call an ended lease ending, and shows a step that was cancelled', () => {
    const cancelled = { at: '2026-01-01T00:01:00Z', by: 'alice@example.org', state: 'running' as const };
    const steps: { needId: string; status: StepStatus }[] = [{ needId: 'reboot', status: 'cancelled' }];
    render(
      <LeaseTable
        label="Recent leases"
        closed
        leases={[
          lease('host-ended', 'failed', {
            cancelled,
            error: 'ended by operator',
            endedAt: '2026-01-01T00:02:00Z',
            operation: {
              leaseId: 'ls-host-ended',
              phase: 'done',
              startedAt: '2026-01-01T00:00:10Z',
              updatedAt: '2026-01-01T00:02:00Z',
              steps,
              disrupted: true,
            },
          }),
        ]}
      />
    );

    expect(within(row('host-ended')).queryByText(/cancelled by/)).toBeNull();
    expect(within(row('host-ended')).getByText('cancelled')).toBeInTheDocument();
    expect(within(row('host-ended')).getByText('ended by operator')).toBeInTheDocument();
  });
});

describe('LeaseTable, the comparison with the fleet', () => {
  it('shows the peers of each client next to the readiness note, and marks the one that is below', () => {
    render(
      <LeaseTable
        label="Active leases"
        leases={[
          lease('host-below', 'verifying', {
            readiness: "CL peers 4, the fleet's median is 31 (needs 16)",
            peerChecks: [
              { layer: 'EL', peers: 28, median: 31, needs: 16 },
              { layer: 'CL', peers: 4, median: 31, needs: 16 },
            ],
          }),
          lease('host-plain', 'verifying', { readiness: 'node is syncing' }),
        ]}
      />
    );

    const below = row('host-below');
    expect(below).toHaveTextContent("CL peers 4, the fleet's median is 31 (needs 16)");

    const checks = within(below).getByLabelText('Peers compared with the fleet');
    expect(checks).toHaveTextContent('EL 28 peers, fleet median 31, needs 16 · CL 4 peers, fleet median 31, needs 16');
    expect(within(checks).getByText(/CL 4 peers/)).toHaveClass('text-danger');
    expect(within(checks).getByText(/EL 28 peers/)).not.toHaveClass('text-danger');

    expect(within(row('host-plain')).queryByLabelText('Peers compared with the fleet')).toBeNull();
  });
});

describe('LeaseTable, the state before the lease', () => {
  it('says how the host was when the lease was granted, and how a lease of a host that was not ready ended', () => {
    const node = { el: { reachable: true, syncing: false, peers: 30, head: 120 } };
    render(
      <LeaseTable
        label="Recent leases"
        closed
        leases={[
          lease('host-ready', 'released', {
            readiness: 'ready: node at head with peers',
            baseline: { at: '2026-01-01T00:00:00Z', node, duty: 2, live: 2, liveKnown: true },
          }),
          lease('host-no-worse', 'released', {
            readiness: 'not ready before the lease: EL is not reachable; no worse after',
            outcome: 'no-worse',
            baseline: {
              at: '2026-01-01T00:00:00Z',
              node: { el: { reachable: false, syncing: false, peers: 0, head: 0, error: 'connection refused' } },
              duty: 2,
              live: 0,
              liveKnown: true,
              containers: [
                { name: 'geth', kind: 'el', state: 'restarting', restartCount: 7 },
                { name: 'lighthouse', kind: 'cl', state: 'running', restartCount: 0 },
              ],
              unready: 'EL is not reachable',
              root: 'el',
            },
          }),
          lease('host-recovered', 'released', { outcome: 'recovered' }),
          lease('host-worse', 'failed', { outcome: 'worse', error: 'worse: EL head 100, was 120' }),
          lease('host-unknown', 'released', { readiness: 'ready: node at head with peers' }),
        ]}
      />
    );

    expect(within(row('host-ready')).getByLabelText('Before the lease')).toHaveTextContent(/^Before the lease: ready$/);

    const before = within(row('host-no-worse')).getByLabelText('Before the lease');
    expect(before).toHaveTextContent(
      'Before the lease: not ready — EL is not reachable (root el) · EL not reachable · validators 0 of 2 attesting · geth restarting, 7 restarts'
    );
    expect(before).not.toHaveTextContent('lighthouse');

    expect(within(row('host-no-worse')).getByText('no worse')).toHaveAttribute(
      'title',
      expect.stringMatching(/^Released as no worse than before its lease/) as string
    );
    expect(within(row('host-recovered')).getByText('recovered')).toBeInTheDocument();
    expect(within(row('host-worse')).getByText('worse')).toBeInTheDocument();

    // A lease without a baseline has no line and no badge.
    expect(within(row('host-unknown')).queryByLabelText('Before the lease')).toBeNull();
    expect(within(row('host-unknown')).queryByText(/recovered|worse/)).toBeNull();
  });
});

describe('LeaseTable, a pull', () => {
  it('shows the pull in flight in place of the readiness note', () => {
    const op = (host: string, pull?: NonNullable<Lease['operation']>['pull']): Lease['operation'] => ({
      leaseId: `ls-${host}`,
      phase: 'pulling',
      startedAt: new Date(Date.now() - 60_000).toISOString(),
      updatedAt: new Date().toISOString(),
      steps: [],
      disrupted: false,
      pull,
    });
    render(
      <LeaseTable
        label="Active leases"
        leases={[
          lease('host-new', 'running', {
            operation: op('host-new', {
              image: 'ethpandaops/geth:master',
              attempt: 2,
              bytesDone: 10 * 1024 * 1024,
              bytesTotal: 20 * 1024 * 1024,
              startedAt: new Date(Date.now() - 120_000).toISOString(),
              lastProgressAt: new Date().toISOString(),
            }),
          }),
          lease('host-old', 'running', { operation: op('host-old') }),
        ]}
      />
    );

    expect(row('host-new')).toHaveTextContent('pulling ethpandaops/geth:master — 10 MiB of 20 MiB, attempt 2, 2m');
    expect(row('host-old')).toHaveTextContent('pulling for 1m');
  });
});

describe('LeaseTable, a host action', () => {
  it('shows the exit code of each run and folds its output away', async () => {
    const user = userEvent.setup();
    const need = {
      id: 'manual/dir-1',
      kind: 'action' as const,
      action: { name: 'security-upgrades', impact: 'node' as const },
    };
    const run = (host: string, action: ActionRun) =>
      lease(host, 'failed', {
        needs: [need],
        operation: {
          leaseId: `ls-${host}`,
          phase: 'failed',
          startedAt: '2026-01-01T00:00:00Z',
          updatedAt: '2026-01-01T00:01:00Z',
          steps: [{ needId: need.id, status: 'failed', action }],
          disrupted: true,
        },
      });
    renderWithApp(
      <LeaseTable
        label="Recent leases"
        closed
        leases={[
          run('host-failed', { exitCode: 100, output: 'Reading package lists...\nE: Could not get lock\n' }),
          run('host-slow', { exitCode: -1, timedOut: true }),
          run('host-restarted', { exitCode: 0, unknown: true }),
        ]}
      />,
      await createHarness({ mode: 'open' })
    );

    expect(within(row('host-failed')).getByText('security-upgrades')).toBeInTheDocument();
    expect(within(row('host-failed')).getByText('exit 100')).toBeInTheDocument();
    const output = within(row('host-failed')).getByLabelText('Output of security-upgrades on host-failed');
    expect(output).toHaveTextContent('E: Could not get lock');
    expect(output.closest('details')).not.toHaveAttribute('open');
    await user.click(within(row('host-failed')).getByText('output (last 2 lines)'));
    expect(output.closest('details')).toHaveAttribute('open');

    expect(within(row('host-slow')).getByText('timed out')).toBeInTheDocument();
    expect(within(row('host-slow')).queryByText(/^output/)).toBeNull();
    expect(within(row('host-restarted')).getByText('outcome unknown')).toBeInTheDocument();
    expect(within(row('host-restarted')).queryByText(/^exit/)).toBeNull();
  });
});
