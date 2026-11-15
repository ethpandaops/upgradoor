import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { Lease, LeaseState, StepStatus } from '../api/types';
import { describeCall } from '../lib/actionRequests';
import { endLeaseIntent } from '../lib/intents';
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
    render(
      <LeaseTable
        label="Active leases"
        onEnd={onEnd}
        leases={[
          lease('host-granted', 'granted'),
          lease('host-running', 'running'),
          lease('host-verifying', 'verifying'),
          lease('host-ending', 'running', { cancelled }),
        ]}
      />
    );

    for (const host of ['host-granted', 'host-running', 'host-verifying']) {
      expect(within(row(host)).getByRole('button', { name: 'End…' })).toBeEnabled();
      expect(within(row(host)).queryByText(/ending/)).toBeNull();
    }

    expect(within(row('host-ending')).getByText('ending (cancelled by alice@example.org)')).toBeInTheDocument();
    expect(within(row('host-ending')).getByRole('button', { name: 'End…' })).toBeDisabled();

    await userEvent.setup().click(within(row('host-granted')).getByRole('button', { name: 'End…' }));
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

describe('endLeaseIntent', () => {
  it('describes the end of a lease as a request, without a broken host', () => {
    const intent = endLeaseIntent('ls-1', 'host-a');
    if (intent.kind !== 'call') throw new Error('not a call intent');
    expect(intent.title).toBe('End the lease of host-a');

    const call = intent.make('why');
    expect(call).toStrictEqual({ action: 'end-lease', body: { lease: 'ls-1', reason: 'why' } });

    const text = describeCall(call).join(' ');
    expect(text).toContain('completes the step in progress');
    expect(text).toContain('The host is put on hold');
    expect(text).toContain('stays open');
    expect(text).toContain('Unhold the host when the lease has ended and it is in order');
    expect(text).toContain('unhold is refused');
    expect(text).not.toMatch(/broken|as failed \(/);
  });
});
