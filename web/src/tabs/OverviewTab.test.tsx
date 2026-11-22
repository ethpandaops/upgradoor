import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { list, type Gate, type LiveViews } from '../api/types';
import type * as wireModule from '../api/wire';
import { LIGHTHOUSE_IMAGE } from '../mock/fixture';
import { createHarness, renderWithApp, PRIMARY } from '../test/harness';
import { OverviewTab } from './OverviewTab';

const noTab = () => undefined;

/** rewrite.views, when set, changes the views the page assembled from the stream. */
const rewrite: { views?: (views: Partial<LiveViews>) => Partial<LiveViews> } = {};

vi.mock('../api/wire', async importOriginal => {
  const wire = await importOriginal<typeof wireModule>();
  return {
    ...wire,
    buildViews: (...args: Parameters<typeof wire.buildViews>) => {
      const views = wire.buildViews(...args);
      return rewrite.views ? rewrite.views(views) : views;
    },
  };
});

afterEach(() => {
  rewrite.views = undefined;
});

describe('OverviewTab, logged out (public data only)', () => {
  it('shows the gate, the participation and the images, and requests no authenticated route', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const gate = await screen.findByRole('region', { name: 'Network gate' });
    expect(gate).toHaveTextContent('Open');
    expect(gate).toHaveTextContent('Finality lag 2 epochs');

    expect(await screen.findByRole('region', { name: 'Participation' })).toHaveTextContent(/[\d.]+%/);
    // Per slot, and without a login without host rows.
    expect(
      await screen.findByRole('img', { name: /^Attestations: \d+ of \d+ included in slots \d+–\d+ \([\d.]+%\)$/ })
    ).toBeInTheDocument();

    const images = await screen.findByRole('region', { name: 'Client images' });
    const lighthouse = (await within(images).findByText(LIGHTHOUSE_IMAGE)).closest('li');
    expect(lighthouse).toHaveTextContent('3/7 · 4 behind');

    expect(await screen.findByRole('region', { name: 'Updates pending' })).toHaveTextContent(/of 24 hosts/);

    // Nothing about single hosts' work is on the public page.
    for (const name of ['In progress', 'Up next', 'Needs attention', 'Outage budget', 'Scheduler']) {
      expect(screen.queryByRole('region', { name })).not.toBeInTheDocument();
    }
    const legend = await screen.findByRole('list', { name: 'Legend' });
    expect(legend).toHaveTextContent('AttestedMissedGate threshold 90%');
    expect(legend).not.toHaveTextContent('Update by upgradoor');
    expect(screen.getByText(/Log in to see which hosts are being updated/)).toBeInTheDocument();

    expect(h.userCalls()).toEqual([]);
    expect(h.calls.every(c => c.authorization === null)).toBe(true);
  });

  it('shows what the controller pushes without asking again', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const gate = await screen.findByRole('region', { name: 'Network gate' });
    expect(gate).toHaveTextContent('Open');

    h.controller.setGateFailing(true);

    await waitFor(() => {
      expect(gate).toHaveTextContent('Closed');
    });
    // The window of the graph was asked for once; everything after it came over the stream.
    expect(h.calls.map(c => c.path).sort()).toEqual(['/api/v1/public/events', '/api/v1/public/slots?window=10800']);
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
});

describe('OverviewTab, logged in', () => {
  it('shows the budget, what is in progress, what is next and what is stuck', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<OverviewTab onOpenTab={noTab} />, h);

    const progress = await screen.findByRole('region', { name: 'In progress' });
    const lease = (await within(progress).findByText('lighthouse-geth-2')).closest('li');
    expect(lease).toHaveTextContent('Image roll');
    expect(within(lease as HTMLElement).getByRole('list', { name: 'Step 6 of 6: Ready check' })).toBeInTheDocument();
    expect(lease).toHaveTextContent(/node at head with peers, ready in (\d+m )?\d+s/);

    expect(screen.getByRole('region', { name: 'Outage budget' })).toHaveTextContent(
      /[\d.]+% of 10% validators offline/
    );
    expect(screen.getByRole('region', { name: 'Scheduler' })).toHaveTextContent('1 in progress · 3 up next · 7 stuck');

    // Up next holds only what waits and starts by itself, in queue order.
    const next = screen.getByRole('region', { name: 'Up next' });
    const hosts = within(next)
      .getAllByRole('listitem')
      .map(li => li.textContent);
    expect(hosts).toHaveLength(3);
    expect(hosts[0]).toMatch(
      /^1lighthouse-nethermind-1Image rollwaits: validators down: [\d.]+% now, [\d.]+% with this host, limit 10%$/
    );
    // A failed pull is tried again without anybody acting.
    expect(hosts).toContainEqual(
      expect.stringMatching(/lighthouse-geth-4Image rollwaits: waiting to pull its image again after a failed pull$/)
    );
    // A bootnode costs the budget nothing and waits for nothing: the queue does not hold it.
    expect(next).not.toHaveTextContent('bootnode-1');
    expect(next).not.toHaveTextContent('teku-besu-1');
    // What was off is drawn as blocks, each with a name of its own, not only a colour; no row per host.
    const graph = screen.getByRole('region', { name: 'Participation and outages' });
    expect(await within(graph).findByRole('button', { name: 'nimbus-nethermind-1: offline' })).toBeInTheDocument();
    expect(within(graph).getAllByRole('button', { name: /: image roll/ }).length).toBeGreaterThanOrEqual(3);

    const attention = screen.getByRole('region', { name: 'Needs attention' });
    expect(attention).toHaveTextContent('waits for approval');
    expect(attention).toHaveTextContent('4 hosts wait.');
    expect(attention).toHaveTextContent('is halted');
    expect(attention).toHaveTextContent('is paused after the canary');
    expect(attention).toHaveTextContent('prysm-geth-3 is silent');

    // The graph has a row per host: the one being updated, the one whose
    // update failed, and one without validators that was updated.
    const legend = screen.getByRole('list', { name: 'Legend' });
    expect(legend).toHaveTextContent('Image roll');
    expect(legend).toHaveTextContent('Offline');
    const chart = screen.getByRole('img', { name: /^Attestations: \d+ of \d+ included in .*; \d+ outage blocks$/ });
    // Two bands, whatever the number of hosts; a failed roll is a block of its own.
    expect(within(chart).getByText('Updates')).toBeInTheDocument();
    expect(within(chart).getByText('Offline')).toBeInTheDocument();
    expect(within(chart).getByRole('button', { name: 'nimbus-nethermind-1: image roll, failed' })).toBeInTheDocument();
    expect(within(chart).queryByText('bootnode-1')).not.toBeInTheDocument();
    expect(screen.getByText(/One column is 4 slots \(48s\).* 5 hosts were off in this window/)).toBeInTheDocument();

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
    await user.click(within(dialog).getByRole('button', PRIMARY));

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

    // A host holds a lease, so the closed gate lets nobody pass: every host
    // that could be served waits for it. The host that waits to pull its
    // image again is the only one up next.
    expect(first).toHaveTextContent(/\d+ hosts? waits? for it to open/);
    const next = screen.getByRole('region', { name: 'Up next' });
    expect(
      within(next)
        .getAllByRole('listitem')
        .map(li => li.textContent)
    ).toEqual(['1lighthouse-geth-4Image rollwaits: waiting to pull its image again after a failed pull']);
  });

  it('speaks of hosts, not validators, on a fleet without validators', async () => {
    const h = await createHarness({ mode: 'open' });
    const empty = (gate: Gate): Gate => ({
      ...gate,
      ok: true,
      reasons: [],
      network: { ...gate.network, livenessKnown: true, tracked: 0, live: 0 },
    });

    // The mock's fleet has validators: rewrite what the page reads from its stream.
    rewrite.views = views => ({
      ...views,
      ...(views['public-network'] ? { 'public-network': empty(views['public-network']) } : {}),
      slots: { rows: [] },
      ...(views.plan
        ? {
            plan: {
              ...views.plan,
              gate: empty(views.plan.gate),
              budget: { totalWeight: 0, allowedWeight: 0, unavailableWeight: 0, leasedHosts: 1, maxLeasedHosts: 2 },
            },
          }
        : {}),
    });
    const slots = await h.api.slots(3 * 3600);
    vi.spyOn(h.api, 'slots').mockResolvedValue({
      ...slots,
      assigned: list(slots.assigned).map(() => 0),
      included: list(slots.included).map(() => 0),
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
