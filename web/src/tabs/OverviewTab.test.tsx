import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { LIGHTHOUSE_IMAGE } from '../mock/fixture';
import { createHarness, renderWithApp } from '../test/harness';
import { OverviewTab } from './OverviewTab';

const noTab = () => undefined;

describe('OverviewTab, logged out (public data only)', () => {
  it('shows the gate, the participation and the images, and requests no authenticated route', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const gate = await screen.findByRole('region', { name: 'Network gate' });
    expect(gate).toHaveTextContent('Open');
    expect(gate).toHaveTextContent('Finality lag 2 epochs');

    expect(await screen.findByRole('region', { name: 'Participation' })).toHaveTextContent(/[\d.]+%/);
    expect(await screen.findByRole('img', { name: /^Participation [\d.]+% in epoch \d+$/ })).toBeInTheDocument();

    const images = await screen.findByRole('region', { name: 'Client images' });
    const lighthouse = (await within(images).findByText(LIGHTHOUSE_IMAGE)).closest('li');
    expect(lighthouse).toHaveTextContent('3/7 · 4 behind');

    expect(await screen.findByRole('region', { name: 'Updates pending' })).toHaveTextContent(/of 24 hosts/);

    // Nothing about single hosts' work is on the public page.
    for (const name of ['In progress', 'Up next', 'Needs attention', 'Outage budget', 'Scheduler']) {
      expect(screen.queryByRole('region', { name })).not.toBeInTheDocument();
    }
    const legend = await screen.findByRole('list', { name: 'Legend' });
    expect(legend).toHaveTextContent('Gate threshold 90%, not counting hosts being updated');
    expect(legend).not.toHaveTextContent('Image roll');
    expect(screen.getByText(/Log in to see which hosts are being updated/)).toBeInTheDocument();

    expect(h.userCalls()).toEqual([]);
    expect(h.calls.every(c => c.authorization === null)).toBe(true);
  });

  it('shows a closed gate with its reasons', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    h.controller.setGateFailing(true);
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const gate = await screen.findByRole('region', { name: 'Network gate' });
    expect(gate).toHaveTextContent('Closed');
    expect(gate).toHaveTextContent('finality lag 5 epochs > 3');
    expect(gate).toHaveTextContent(/participation [\d.]+% < 90%/);
  });

  it('says what is used instead when the beacon node does not serve liveness', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    h.controller.setLivenessUnsupported(true);
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const tile = await screen.findByRole('region', { name: 'Participation' });
    expect(tile).toHaveTextContent('Not served');
    expect(tile).toHaveTextContent('readiness and the budget use the agents’ probes');
  });
});

describe('OverviewTab, logged in', () => {
  it('shows the budget, what is in progress, what is next and what is stuck', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const progress = await screen.findByRole('region', { name: 'In progress' });
    const lease = (await within(progress).findByText('lighthouse-geth-2')).closest('li');
    expect(lease).toHaveTextContent('Image roll');
    expect(within(lease as HTMLElement).getByRole('list', { name: 'Step 5 of 5: Soak' })).toBeInTheDocument();
    expect(lease).toHaveTextContent('0 of 1 complete soak epochs');

    expect(screen.getByRole('region', { name: 'Outage budget' })).toHaveTextContent(
      /[\d.]+% of 10% validators offline/
    );
    expect(screen.getByRole('region', { name: 'Scheduler' })).toHaveTextContent('1 in progress · 4 up next · 6 stuck');

    // Up next holds only what starts by itself, in queue order, with an estimate.
    const next = screen.getByRole('region', { name: 'Up next' });
    const hosts = within(next)
      .getAllByRole('listitem')
      .map(li => li.textContent);
    expect(hosts).toHaveLength(4);
    expect(hosts[0]).toMatch(/^1lighthouse-nethermind-1Image rollwaits: budget [\d.]+% of 10%(~\d+m|<1m)$/);
    expect(hosts[3]).toMatch(/^4bootnode-1Image rollwaits: after lighthouse-geth-2/);
    expect(next).not.toHaveTextContent('teku-besu-1');
    // Every lane bar has a name of its own, not only a colour.
    expect(document.querySelectorAll('svg[role="img"] rect > title').length).toBeGreaterThanOrEqual(5);

    const attention = screen.getByRole('region', { name: 'Needs attention' });
    expect(attention).toHaveTextContent('waits for approval');
    expect(attention).toHaveTextContent('4 hosts wait.');
    expect(attention).toHaveTextContent('is halted');
    expect(attention).toHaveTextContent('prysm-geth-3 is silent');

    // The timeline has a lane per host with work and a legend for the lanes.
    expect(screen.getByRole('list', { name: 'Legend' })).toHaveTextContent('Planned (estimate)');
    const chart = screen.getByRole('img', { name: /hosts with work in the window/ });
    for (const host of ['lighthouse-geth-2', 'lighthouse-nethermind-1', 'bootnode-1']) {
      expect(within(chart).getByText(host)).toBeInTheDocument();
    }
    expect(within(chart).queryByText('teku-besu-1')).not.toBeInTheDocument();

    expect(h.userCalls().every(c => c.authorization === 'Bearer mock-token')).toBe(true);
  });

  it('approves a waiting rollout from the attention list', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const attention = await screen.findByRole('region', { name: 'Needs attention' });
    await user.click(await within(attention).findByRole('button', { name: 'Approve…' }));
    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveTextContent('Approve rollout: Reboot');
    await user.click(within(dialog).getByRole('button', { name: 'Confirm' }));

    await vi.waitFor(() => {
      expect(h.calls.filter(c => c.method === 'POST')).toHaveLength(1);
    });
    expect(h.calls.find(c => c.method === 'POST')?.path).toBe('/api/v1/actions/resume');
    await vi.waitFor(() => {
      expect(screen.getByRole('region', { name: 'Needs attention' })).not.toHaveTextContent('waits for approval');
    });
  });

  it('lists a paused scheduler and a closed gate first', async () => {
    const h = await createHarness({ mode: 'open' });
    h.controller.setGateFailing(true);
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const attention = await screen.findByRole('region', { name: 'Needs attention' });
    const first = (await within(attention).findAllByRole('listitem'))[0];
    expect(first).toHaveTextContent('The network gate is closed');
    expect(screen.getByRole('region', { name: 'Up next' })).toHaveTextContent('Nothing will start by itself');
  });

  it('speaks of hosts, not validators, on a fleet without validators', async () => {
    const h = await createHarness({ mode: 'open' });
    const gate = await h.api.publicNetwork();
    const plan = await h.api.plan();
    const empty = {
      ...gate,
      ok: true,
      reasons: [],
      network: { ...gate.network, livenessKnown: false, tracked: 0, live: 0 },
    };
    vi.spyOn(h.api, 'publicNetwork').mockResolvedValue(empty);
    vi.spyOn(h.api, 'publicHistory').mockResolvedValue({ epochs: [], minParticipationPercent: 90, epochSeconds: 8 });
    vi.spyOn(h.api, 'plan').mockResolvedValue({
      ...plan,
      gate: empty,
      budget: { totalWeight: 0, allowedWeight: 0, unavailableWeight: 0, leasedHosts: 1, maxLeasedHosts: 2 },
    });
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const budget = await screen.findByRole('region', { name: 'Outage budget' });
    expect(budget).toHaveTextContent('1 of 2 hosts down');
    expect(budget).toHaveTextContent('only the host limit applies');
    expect(screen.getByRole('region', { name: 'Participation' })).toHaveTextContent(
      'No host of this fleet has validators'
    );
    expect(await screen.findByText('No validators to watch on this fleet')).toBeInTheDocument();
  });
});
