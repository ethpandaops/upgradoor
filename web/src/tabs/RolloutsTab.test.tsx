import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { Rollout } from '../api/types';
import { createHarness, PRIMARY, renderWithApp } from '../test/harness';
import { RecentRollouts, RolloutTargets, RolloutsTab, policyText, waitsForApproval } from './RolloutsTab';

const digest = (c: string) => `sha256:${c.repeat(64)}`;

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
    hosts: [],
    policy: 'auto',
    ...p,
  };
}

describe('RolloutsTab', () => {
  it('shows a group rollout with its kind, its team and what it moves them to', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<RolloutsTab />, h);

    const card = await screen.findByRole('article', { name: /^lighthouse cl: sigp\/lighthouse:unstable to / });
    expect(within(card).getByTitle(/^Moves the containers of one kind of one team/)).toHaveTextContent('group');
    expect(card).toHaveTextContent('kind cl');
    expect(card).toHaveTextContent('team lighthouse');

    const targets = within(card).getByRole('list', { name: /^Targets of / });
    expect(
      within(targets)
        .getAllByRole('listitem')
        .map(li => li.textContent)
    ).toEqual([expect.stringMatching(/^sigp\/lighthouse:unstable@[0-9a-f]{12}revision [0-9a-f]{8}$/) as string]);

    // A host that waits to pull its image again says so.
    const hosts = within(card).getByRole('table', { name: /^Hosts of / });
    expect(within(hosts).getByText('lighthouse-geth-4').closest('tr')).toHaveTextContent(
      'waiting to pull its image again after a failed pull'
    );
  });
});

describe('RolloutsTab, a held member', () => {
  it('names the holders instead of a plain pending, and counts the held members', async () => {
    const h = await createHarness({ mode: 'open' });
    await h.api.action({ action: 'hold', body: { hosts: ['lighthouse-geth-3'], reason: 'disk swap' } });
    renderWithApp(<RolloutsTab />, h);

    const card = await screen.findByRole('article', { name: /^lighthouse cl: / });
    const row = within(within(card).getByRole('table', { name: /^Hosts of / }))
      .getByText('lighthouse-geth-3')
      .closest('tr') as HTMLElement;
    expect(await within(row).findAllByText('held by staff')).toHaveLength(2);
    expect(row).toHaveTextContent('not rolled while it lasts');
    expect(row).not.toHaveTextContent('pending');
    expect(within(card).getByRole('img', { name: /^\d+ hosts: / })).toHaveAccessibleName(
      expect.stringMatching(/ waiting, 1 held/) as string
    );
  });
});

describe('RolloutsTab, a member whose host is leased for other work', () => {
  it('names the rollout of that lease instead of a missing queue place', async () => {
    const h = await createHarness({ mode: 'open' });
    await h.api.action({
      action: 'reboot',
      body: {
        hosts: ['lighthouse-geth-2'],
        all: false,
        staggered: true,
        force: false,
        ignoreBudget: false,
        reason: '',
      },
    });
    renderWithApp(<RolloutsTab />, h);

    const card = await screen.findByRole('article', { name: 'Manual reboot of lighthouse-geth-2' });
    const row = within(within(card).getByRole('table', { name: /^Hosts of / }))
      .getByText('lighthouse-geth-2')
      .closest('tr') as HTMLElement;
    expect(row).toHaveTextContent(
      /host is leased for lighthouse cl: .+ \(lease verifying\); waits for that lease to end/
    );
    expect(row).not.toHaveTextContent('not in the queue');
  });
});

describe('RolloutsTab, decisions of a person', () => {
  it('approves a verifying lease now, showing its readiness note', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<RolloutsTab />, h);

    const card = await screen.findByRole('article', { name: /^lighthouse cl: / });
    const row = within(within(card).getByRole('table', { name: /^Hosts of / }))
      .getByText('lighthouse-geth-2')
      .closest('tr') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: 'Approve now…' }));

    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByRole('list', { name: 'Current state' })).toHaveTextContent(/^Readiness: .+/);
    await user.click(within(dialog).getByRole('button', { name: 'Approve now' }));

    await waitFor(() => {
      expect(h.calls.some(c => c.path === '/api/v1/actions/approve-lease')).toBe(true);
    });
    expect(h.calls.find(c => c.path === '/api/v1/actions/approve-lease')?.body).toMatchObject({
      host: 'lighthouse-geth-2',
      reason: '',
    });
  });

  it('ends a lease as cancelled: the rollout pauses and the host waits for a retry, not failed', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<RolloutsTab />, h);

    const leases = await screen.findByRole('table', { name: 'Active leases' });
    await user.click(within(leases).getByRole('button', { name: 'End the lease of lighthouse-geth-2' }));
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'End the lease' }));

    const card = await screen.findByRole('article', { name: /^lighthouse cl: / });
    await waitFor(() => {
      expect(card).toHaveTextContent('paused (lighthouse-geth-2 cancelled by anonymous)');
    });
    const row = within(within(card).getByRole('table', { name: /^Hosts of / }))
      .getByText('lighthouse-geth-2')
      .closest('tr') as HTMLElement;
    expect(row).toHaveTextContent('cancelled');
    expect(row).not.toHaveTextContent('failed');
    expect(within(row).getByRole('button', { name: /^Retry lighthouse-geth-2/ })).toBeEnabled();
  });

  it('lets a rollout that waits at the closed gate pass it, and badges it', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    h.controller.setGateFailing(true);
    renderWithApp(<RolloutsTab />, h);

    const button = (await screen.findAllByRole('button', { name: 'Roll despite network state…' }))[0];
    if (!button) throw new Error('no rollout waits at the gate');
    const card = button.closest('article') as HTMLElement;
    await user.click(button);

    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByRole('list', { name: 'Current state' })).toHaveTextContent(/Network gate closed: /);
    await user.click(within(dialog).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(within(card).getByText('ignores gate')).toBeInTheDocument();
    });
    expect(within(card).queryByRole('button', { name: 'Roll despite network state…' })).not.toBeInTheDocument();
    expect(h.calls.some(c => c.path === '/api/v1/actions/ignore-gate')).toBe(true);
  });
});

describe('RolloutsTab, a host action', () => {
  it("names the action and shows each host's exit code and output", async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    await h.api.action({
      action: 'host-action',
      body: { action: 'security-upgrades', hosts: ['lighthouse-geth-1', 'prysm-geth-1', 'teku-besu-1'], reason: '' },
    });
    renderWithApp(<RolloutsTab />, h);

    const card = await screen.findByRole('article', { name: 'action security-upgrades on 3 host(s)' });
    expect(card).toHaveTextContent('action security-upgrades');
    expect(card).toHaveTextContent('halted: prysm-geth-1 failed');

    const hosts = within(card).getByRole('table', { name: /^Hosts of / });
    const member = (host: string) => within(hosts).getByText(host).closest('tr') as HTMLElement;
    expect(within(member('lighthouse-geth-1')).getByText('exit 0')).toBeInTheDocument();
    expect(within(member('teku-besu-1')).getByText('timed out')).toBeInTheDocument();

    const failed = member('prysm-geth-1');
    expect(failed).toHaveTextContent('action security-upgrades exited with code 100');
    expect(within(failed).getByText('exit 100')).toBeInTheDocument();
    const output = within(failed).getByLabelText('Output of security-upgrades on prysm-geth-1');
    expect(output.closest('details')).not.toHaveAttribute('open');
    await user.click(within(failed).getByText(/^output \(last \d+ lines\)$/));
    expect(output.closest('details')).toHaveAttribute('open');
    expect(output).toHaveTextContent('Could not get lock /var/lib/dpkg/lock-frontend');
  });
});

describe('RolloutTargets', () => {
  it('names a tag at its digest, and a soft deploy with its container and a changed command line', () => {
    render(
      <RolloutTargets
        label="Targets"
        targets={[
          { image: 'gcr.io/offchainlabs/prysm/beacon-chain:latest', digest: digest('a') },
          {
            image: 'gcr.io/offchainlabs/prysm/validator:latest',
            digest: digest('b'),
            spec: 'k1',
            container: 'prysm-validator',
            flags: true,
          },
        ]}
      />
    );

    expect(screen.getAllByRole('listitem').map(li => li.textContent)).toEqual([
      'gcr.io/offchainlabs/prysm/beacon-chain:latest@aaaaaaaaaaaa',
      'specprysm-validator→gcr.io/offchainlabs/prysm/validator:latest@bbbbbbbbbbbbcommand line changed',
    ]);
  });
});

describe('RecentRollouts', () => {
  it('wraps a long title anywhere instead of keeping it on one line', () => {
    const title = 'unlabelled: ghcr.io/ethpandaops/some-long-image-name/with-path:master-latest to 1ca62ee4ba52';
    render(<RecentRollouts rollouts={[rollout({ title, state: 'completed' })]} leases={new Map()} />);

    const cell = screen.getByRole('cell', { name: title });
    expect(cell).toHaveClass('whitespace-normal', 'wrap-anywhere');
    expect(cell).not.toHaveClass('whitespace-nowrap');
  });
});

describe('waitsForApproval', () => {
  it('holds a rollout on the manual policy until it is approved', () => {
    expect(waitsForApproval(rollout({ policy: 'manual' }))).toBe(true);
    expect(waitsForApproval(rollout({ policy: 'manual', approved: true }))).toBe(false);
    expect(waitsForApproval(rollout({ policy: 'auto' }))).toBe(false);
  });
});

describe('policyText', () => {
  it('names the setting the policy of an image rollout comes from', () => {
    expect(policyText(rollout({ policy: 'manual', policySource: 'kind', containerKind: 'cl' }))).toBe(
      'policy manual (kind cl)'
    );
    expect(policyText(rollout({ policy: 'manual', policySource: 'kind', containerKind: undefined }))).toBe(
      'policy manual (kind other)'
    );
    expect(policyText(rollout({ policy: 'manual', policySource: 'client', team: 'prysm' }))).toBe(
      'policy manual (client prysm)'
    );
    expect(policyText(rollout({ policy: 'manual', policySource: 'default' }))).toBe(
      'policy manual (default for images)'
    );
    expect(policyText(rollout({ kind: 'reboot', policy: 'manual' }))).toBe('policy manual');
  });
});
