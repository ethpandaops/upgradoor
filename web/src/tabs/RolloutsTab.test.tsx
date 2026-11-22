import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { Rollout } from '../api/types';
import { defaultSettings } from '../mock/fixture';
import { createHarness, renderWithApp } from '../test/harness';
import { RolloutTargets, RolloutsTab, policyFor } from './RolloutsTab';

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
    createdBy: 'scheduler',
    createdAt: '2026-11-20T10:00:00Z',
    updatedAt: '2026-11-20T10:00:00Z',
    hosts: [],
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

describe('policyFor', () => {
  it('gives a group rollout the image policy of its team where one is set, policyImage without a team', () => {
    const settings = { ...defaultSettings(), policyClients: { besu: 'manual' as const }, policyImage: 'auto' as const };
    expect(policyFor(settings, rollout({ team: 'besu' }))).toBe('manual');
    expect(policyFor(settings, rollout({ team: 'prysm' }))).toBe('auto');
    expect(policyFor({ ...settings, policyImage: 'manual' }, rollout({ containerKind: 'snooper-el' }))).toBe('manual');
    expect(policyFor(settings, rollout({ kind: 'local' }))).toBeUndefined();
  });

  it("gives a due host action its own policy, manual without one, and a person's start none", () => {
    const settings = { ...defaultSettings(), actionPolicies: { 'security-upgrades': 'auto' as const } };
    const due = (action: string) => rollout({ kind: 'action', key: `action:${action}`, action });
    expect(policyFor(settings, due('security-upgrades'))).toBe('auto');
    expect(policyFor(settings, due('docker-prune'))).toBe('manual');
    expect(
      policyFor(settings, rollout({ kind: 'action', key: 'manual:ro-1', action: 'docker-prune' }))
    ).toBeUndefined();
  });
});
