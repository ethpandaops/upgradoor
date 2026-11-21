import { render, screen, within } from '@testing-library/react';
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
    key: 'group:consensus#prysm@0011223344556677',
    title: 'prysm consensus',
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
  it('shows a group rollout with its group, its client and what it moves the group to', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<RolloutsTab />, h);

    const card = await screen.findByRole('article', { name: /^lighthouse consensus: sigp\/lighthouse:unstable to / });
    expect(within(card).getByTitle(/^Moves the containers of one roll group of one client/)).toHaveTextContent('group');
    expect(card).toHaveTextContent('group consensus');
    expect(card).toHaveTextContent('client lighthouse');

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
  it('gives a group rollout the image policy of its client where one is set', () => {
    const settings = { ...defaultSettings(), policyClients: { besu: 'manual' as const } };
    expect(policyFor(settings, rollout({ client: 'besu' }))).toBe('manual');
    expect(policyFor(settings, rollout({ client: 'prysm' }))).toBe('auto');
    expect(policyFor(settings, rollout({ kind: 'local' }))).toBeUndefined();
  });
});
