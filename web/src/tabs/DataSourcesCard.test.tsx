import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createHarness, renderWithApp } from '../test/harness';
import { DataSourcesCard } from './DataSourcesCard';

async function openSources() {
  const h = await createHarness({ mode: 'auth', loggedIn: true });
  renderWithApp(<DataSourcesCard />, h);
  const card = await screen.findByRole('region', { name: 'Data sources' });
  await within(card).findByRole('region', { name: 'Validator ranges' });
  return { h, card, user: userEvent.setup() };
}

describe('DataSourcesCard', () => {
  it('shows the beacon endpoints and the validator ranges', async () => {
    const { card } = await openSources();

    const beacon = within(card).getByRole('region', { name: 'Beacon endpoints' });
    const items = within(beacon).getAllByRole('listitem');
    expect(items).toHaveLength(2);
    expect(items[0]).toHaveTextContent('https://bn-1.devnet.example');
    expect(items[0]).toHaveTextContent('in use');
    expect(items[0]).toHaveTextContent('Head slot 181042');
    expect(items[1]).not.toHaveTextContent('in use');
    expect(items[1]).toHaveTextContent('HTTP 503: beacon node is syncing');

    const ranges = within(card).getByRole('region', { name: 'Validator ranges' });
    expect(ranges).toHaveTextContent('https://config.devnet.example/api/v1/nodes/validator-ranges');
    expect(within(ranges).getByLabelText('Fetch status')).toHaveTextContent('Last fetch');
    expect(ranges).toHaveTextContent('2 unknown to upgradoor');

    const perHost = within(ranges).getByRole('table', { name: 'Validators per host' });
    const unknown = within(perHost).getByText('spare-keys-1').closest('tr');
    expect(unknown).toHaveTextContent('unknown');
    expect(unknown).toHaveTextContent('64');

    const table = within(ranges).getByRole('table', { name: 'Validator ranges' });
    expect(within(table).getAllByRole('row').length).toBeGreaterThan(3);
  });

  it('filters both tables by host', async () => {
    const { card, user } = await openSources();
    const ranges = within(card).getByRole('region', { name: 'Validator ranges' });

    await user.type(within(ranges).getByRole('searchbox', { name: 'Filter by host' }), 'spare');

    const perHost = within(ranges).getByRole('table', { name: 'Validators per host' });
    expect(within(perHost).getAllByRole('row')).toHaveLength(2);
    expect(within(ranges).getByText('Ranges (1)')).toBeInTheDocument();
  });

  it('refreshes the validator ranges and says what came of it', async () => {
    const { card, h, user } = await openSources();
    const ranges = within(card).getByRole('region', { name: 'Validator ranges' });
    const button = within(ranges).getByRole('button', { name: 'Refresh' });

    await user.click(button);
    await waitFor(() => {
      expect(within(ranges).getByRole('status')).toHaveTextContent('fetched: unchanged.');
    });
    expect(button).toBeEnabled();
    expect(h.calls.filter(c => c.method === 'POST').map(c => c.path)).toEqual([
      '/api/v1/settings/sources/validator-ranges/refresh',
    ]);
    expect(h.controller.data.audit.at(-1)?.action).toBe('sources.validator-ranges.refresh');

    // Again right away: within the floor, nothing is fetched.
    await user.click(button);
    await waitFor(() => {
      expect(within(ranges).getByRole('status')).toHaveTextContent('fetched within the last 10s. Again at');
    });
  });

  it('disables the refresh while it runs', async () => {
    const { card, h } = await openSources();
    const ranges = within(card).getByRole('region', { name: 'Validator ranges' });
    const button = within(ranges).getByRole('button', { name: 'Refresh' });

    let release: () => void = () => undefined;
    const gate = new Promise<void>(resolve => {
      release = resolve;
    });
    const inner = h.api.refreshValidatorRanges.bind(h.api);
    h.api.refreshValidatorRanges = async () => {
      await gate;
      return inner();
    };

    button.click();
    await waitFor(() => {
      expect(within(ranges).getByRole('button', { name: 'Refreshing…' })).toBeDisabled();
    });

    release();
    await waitFor(() => {
      expect(within(ranges).getByRole('button', { name: 'Refresh' })).toBeEnabled();
    });
  });
});
