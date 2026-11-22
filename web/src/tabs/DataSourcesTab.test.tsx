import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createHarness, renderWithApp } from '../test/harness';
import { DataSourcesTab, stateLabel } from './DataSourcesTab';

async function openSources() {
  const h = await createHarness({ mode: 'auth', loggedIn: true });
  renderWithApp(<DataSourcesTab />, h);
  const ranges = await screen.findByRole('region', { name: 'Validator ranges' });
  return { h, ranges, user: userEvent.setup() };
}

describe('DataSourcesTab', () => {
  it('shows one row per beacon endpoint and the validator ranges', async () => {
    const { ranges } = await openSources();

    const beacon = screen.getByRole('table', { name: 'Beacon endpoints' });
    const rows = within(beacon).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent(/^https:\/\/bn-1\.devnet\.exampleok181042/);
    expect(rows[1]).toHaveTextContent('behind by 3 slots');
    expect(rows[2]).toHaveTextContent('down');
    expect(rows[2]).toHaveTextContent('HTTP 503: beacon node is syncing');
    expect(screen.getByRole('region', { name: 'Beacon endpoints' })).not.toHaveTextContent(/failover|in use/i);

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

  it('says how far behind an endpoint is, against the highest head of those that are ok', () => {
    const e = { label: 'x', state: 'behind', headSlot: 90 } as const;
    expect(stateLabel(e, 100)).toBe('behind by 10 slots');
    expect(stateLabel(e, undefined)).toBe('behind');
    expect(stateLabel({ label: 'y', state: 'forked', headSlot: 90 }, 100)).toBe('forked');
  });

  it('filters both tables by host', async () => {
    const { ranges, user } = await openSources();

    await user.type(within(ranges).getByRole('searchbox', { name: 'Filter by host' }), 'spare');

    const perHost = within(ranges).getByRole('table', { name: 'Validators per host' });
    expect(within(perHost).getAllByRole('row')).toHaveLength(2);
    expect(within(ranges).getByText('Ranges (1)')).toBeInTheDocument();
  });

  it('refreshes the validator ranges and says what came of it', async () => {
    const { ranges, h, user } = await openSources();
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
    const { ranges, h } = await openSources();
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
