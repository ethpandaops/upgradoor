import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { MockProfile } from '../mock/mockController';
import { createHarness, fakeClock, renderWithApp, type Harness, PRIMARY } from '../test/harness';
import { CLIENT_PAGE_SIZE, ClientsTab, defaultClient } from './ClientsTab';

async function open(profile: MockProfile = 'staff', hash = ''): Promise<Harness & { onOpenTab: () => void }> {
  window.location.hash = hash;
  const h = await createHarness({ mode: 'auth', loggedIn: true, profile });
  const onOpenTab = vi.fn();
  renderWithApp(<ClientsTab onOpenTab={onOpenTab} />, h);
  return { ...h, onOpenTab };
}

const clientTabs = () => screen.getAllByRole('tablist').filter(l => l.getAttribute('aria-label')?.endsWith(' clients'));
const selectedClient = () =>
  clientTabs()
    .flatMap(l => within(l).queryAllByRole('tab', { selected: true }))
    .map(t => t.textContent)
    .join();
const hostNames = (client: string) =>
  within(screen.getByRole('table', { name: `Hosts of ${client}` }))
    .getAllByRole('row')
    .slice(1)
    .map(row => row.querySelector('td:nth-child(2)')?.textContent ?? '');
const hostCalls = (h: Harness) => h.calls.map(c => c.path).filter(p => p.startsWith('/api/v1/clients/'));
/** chip is the count on a filter chip of the host list. */
const chip = (label: string) =>
  within(screen.getByRole('group', { name: 'Filter by state' }))
    .getByRole('button', { name: new RegExp(`^${label}\\s*\\d+$`) })
    .textContent.replace(label, '');

describe('ClientsTab', () => {
  it('opens on the client of the team of the login, with its images, counts and hosts', async () => {
    await open('team');

    await screen.findByRole('table', { name: 'Hosts of lighthouse' });
    expect(selectedClient()).toMatch(/^lighthouse/);

    // One row of clients: consensus, then execution.
    const rows = clientTabs().map(l =>
      within(l)
        .getAllByRole('tab')
        .map(t => t.textContent)
    );
    expect(rows).toEqual([
      ['grandine2', 'lighthouse7', 'lodestar3', 'nimbus3', 'prysm4', 'teku4'],
      ['besu3', 'erigon3', 'geth9', 'nethermind4', 'reth4'],
    ]);

    // The configuration first: per kind the image, what it points at, and the flags the hosts report as their base.
    const config = screen.getByRole('region', { name: 'Client lighthouse' });
    expect(config).toHaveTextContent(/BEACON\s*7 hosts · 4 behind\s*sigp\/lighthouse:unstable/i);
    const flags = within(config).getByRole('list', { name: 'Flags of the beacon' });
    expect(
      within(flags)
        .getAllByRole('listitem')
        .map(li => li.textContent)
    ).toEqual([
      '--network=mock-devnet-0',
      '--datadir=/data',
      '--execution-endpoint=http://execution:8551',
      '--jwt-secret=/jwt/jwtsecret',
      '--checkpoint-sync-url=https://checkpoint-sync.mock-devnet-0.ethpandaops.io',
      '--http',
      '--http-address=0.0.0.0',
      '--metrics',
      expect.stringMatching(/^--metrics-port=\d+$/),
    ]);
    expect(within(config).queryByRole('button', { name: /Show all/ })).not.toBeInTheDocument();
    expect(within(config).getByRole('list', { name: 'Flags of the validator' })).toBeInTheDocument();

    // How many hosts deviate, as chips that filter the list.
    expect(within(config).getByRole('group', { name: 'Hosts that deviate' })).toHaveTextContent(
      '0image override0flag override4behind0held0failed'
    );

    expect(['all', 'up to date', 'behind', 'rolling', 'overridden', 'held', 'failed'].map(chip)).toEqual([
      '7',
      '2',
      '4',
      '1',
      '0',
      '0',
      '0',
    ]);
    expect(
      within(screen.getByRole('group', { name: 'Filter by state' })).queryByRole('button', { name: /^custom/ })
    ).not.toBeInTheDocument();

    expect(hostNames('lighthouse')).toEqual([
      'bootnode-1',
      'lighthouse-geth-1',
      'lighthouse-geth-2',
      'lighthouse-geth-3',
      'lighthouse-geth-4',
      'lighthouse-nethermind-1',
      'rpc-1',
    ]);
  });

  it('keeps the selected client in the URL hash and shows what an override says', async () => {
    const user = userEvent.setup();
    const h = await open('staff', '#clients/prysm');

    const table = await screen.findByRole('table', { name: 'Hosts of prysm' });
    const row = within(table).getByRole('row', { name: /prysm-geth-1/ });
    expect(row).toHaveTextContent('overridden');
    expect(row).toHaveTextContent(/until in 4h \d+m/);
    expect(row).toHaveTextContent('by alice@example.org');
    expect(within(table).getByRole('row', { name: /prysm-geth-3/ })).toHaveTextContent('silent');

    await user.click(screen.getByRole('tab', { name: /^reth/ }));
    expect(window.location.hash).toBe('#clients/reth');
    const reth = await screen.findByRole('table', { name: 'Hosts of reth' });
    expect(within(reth).getByRole('row', { name: /lodestar-reth-2/ })).toHaveTextContent('restoring');

    await user.click(screen.getByRole('tab', { name: /^besu/ }));
    const besu = await screen.findByRole('table', { name: 'Hosts of besu' });
    expect(within(besu).getByRole('row', { name: /teku-besu-2/ })).toHaveTextContent('override error');

    // A host leads to the Hosts tab, filtered to it.
    await user.click(within(besu).getByRole('button', { name: 'teku-besu-2' }));
    expect(h.onOpenTab).toHaveBeenCalledWith('hosts');
  });

  it('asks for a page at a time, filtered on the controller, and again when the list changed', async () => {
    const user = fakeClock();
    const h = await createHarness({ mode: 'open', fleet: 'large' });
    const calls = () => hostCalls(h);
    window.location.hash = '#clients/lighthouse';
    renderWithApp(<ClientsTab onOpenTab={() => undefined} />, h);

    const pager = () => screen.getByRole('group', { name: 'Pages' });
    await screen.findByRole('table', { name: 'Hosts of lighthouse' });
    expect(hostNames('lighthouse')).toHaveLength(CLIENT_PAGE_SIZE);
    expect(pager()).toHaveTextContent(/^1–50 of 253 hosts/);
    expect(within(pager()).getByRole('button', { name: /Previous/ })).toBeDisabled();

    await user.click(within(pager()).getByRole('button', { name: /Next/ }));
    await waitFor(() => {
      expect(pager()).toHaveTextContent(/^51–100 of 253 hosts/);
    });

    // A state chip narrows the list on the controller and starts at its first page.
    const chips = screen.getByRole('group', { name: 'Filter by state' });
    await user.click(within(chips).getByRole('button', { name: /^behind/ }));
    await waitFor(() => {
      expect(pager()).toHaveTextContent(/^1–4 of 4 hosts/);
    });

    await user.type(screen.getByRole('searchbox', { name: 'Filter hosts by name' }), 'nethermind');
    await waitFor(() => {
      expect(hostNames('lighthouse')).toEqual(['lighthouse-nethermind-1']);
    });

    expect(calls()).toEqual([
      `/api/v1/clients/lighthouse/hosts?limit=${CLIENT_PAGE_SIZE}`,
      `/api/v1/clients/lighthouse/hosts?offset=50&limit=${CLIENT_PAGE_SIZE}`,
      `/api/v1/clients/lighthouse/hosts?state=pending&limit=${CLIENT_PAGE_SIZE}`,
      `/api/v1/clients/lighthouse/hosts?state=pending&q=nethermind&limit=${CLIENT_PAGE_SIZE}`,
    ]);

    // The stream reports another version of the client's list: the page is asked for once more.
    await h.controller.fetch('/api/v1/actions/hold', {
      method: 'POST',
      body: JSON.stringify({ hosts: ['lighthouse-nethermind-1'], reason: '' }),
    });
    await waitFor(() => {
      expect(calls()).toHaveLength(5);
    });
    expect(calls()[4]).toBe(calls()[3]);
    await waitFor(() => {
      expect(screen.getByRole('table', { name: 'Hosts of lighthouse' })).toHaveTextContent('No host of lighthouse');
    });
  });

  it('rolls the containers of the client on the ticked hosts, and holds a host', async () => {
    const user = userEvent.setup();
    const h = await open('staff', '#clients/lighthouse');
    const table = await screen.findByRole('table', { name: 'Hosts of lighthouse' });
    const bulk = screen.getByRole('group', { name: 'Actions on the selected hosts' });

    expect(within(bulk).getByRole('button', { name: 'Roll' })).toBeDisabled();
    await user.click(within(table).getByRole('checkbox', { name: 'Select lighthouse-geth-3' }));
    await user.click(within(table).getByRole('checkbox', { name: 'Select lighthouse-geth-4' }));
    expect(bulk).toHaveTextContent('2 hosts selected');

    await user.click(within(bulk).getByRole('button', { name: 'Roll' }));
    const dialog = await screen.findByRole('dialog');
    // The dialog offers every container of the hosts and starts with the client's.
    expect(within(dialog).getByRole('checkbox', { name: 'lighthouse' })).toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'lighthouse-validator' })).toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'geth' })).not.toBeChecked();
    await user.click(within(dialog).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(h.calls.find(c => c.path === '/api/v1/actions/roll')?.body).toMatchObject({
        hosts: ['lighthouse-geth-3', 'lighthouse-geth-4'],
        containers: ['lighthouse', 'lighthouse-validator'],
        staggered: true,
      });
    });

    await user.click(
      within(within(table).getByRole('group', { name: 'Actions for rpc-1' })).getByRole('button', {
        name: 'Hold rpc-1',
      })
    );
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', PRIMARY));
    // The hold reaches the list through the stream: the row is held and offers the way back.
    expect(await within(table).findByRole('button', { name: 'Unhold rpc-1' }, { timeout: 3000 })).toBeInTheDocument();
    await waitFor(() => {
      expect(within(table).getByRole('row', { name: /rpc-1/ })).toHaveTextContent('held');
    });
  });

  it('offers the overrides of the client\u2019s containers in the row, and says where an override stands', async () => {
    const user = userEvent.setup();
    const h = await open('staff', '#clients/besu');
    const table = await screen.findByRole('table', { name: 'Hosts of besu' });
    const actions = (host: string) => within(table).getByRole('group', { name: `Actions for ${host}` });

    // The error of an override is what the row says about it.
    expect(within(table).getByRole('row', { name: /teku-besu-2/ })).toHaveTextContent(
      /override error.*is not allowed for this container/
    );

    await user.click(within(actions('teku-besu-1')).getByRole('button', { name: 'Override the flags on this host' }));
    const dialog = await screen.findByRole('dialog', { name: 'Flag override on teku-besu-1' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Flags to add' }), '--logging=DEBUG');
    await user.type(within(dialog).getByPlaceholderText('Recorded in the audit log'), 'look at the sync');
    await user.click(within(dialog).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(h.calls.find(c => c.path === '/api/v1/actions/flag-override')?.body).toMatchObject({
        host: 'teku-besu-1',
        container: 'besu',
        addFlags: ['--logging=DEBUG'],
      });
    });
    // The list follows through the stream: the host is overridden, and waits for its lease.
    await waitFor(() => {
      expect(within(table).getByRole('row', { name: /teku-besu-1/ })).toHaveTextContent(
        /overridden.*waiting for a lease.*by mock-user@example.org · look at the sync/
      );
    });
  });

  it('sets the image policy of a client through the dialog, and offers the way back to the default', async () => {
    const user = userEvent.setup();
    const h = await open('staff', '#clients/geth');
    await screen.findByRole('table', { name: 'Hosts of geth' });

    const policy = screen.getByRole('group', { name: 'Roll policy' });
    expect(within(policy).getByRole('button', { name: 'Automatic' })).toHaveAttribute('aria-pressed', 'true');
    expect(policy).toHaveTextContent('the default for images (automatic)');
    expect(within(policy).queryByRole('button', { name: /Use the default/ })).not.toBeInTheDocument();

    await user.click(within(policy).getByRole('button', { name: 'Manual' }));
    const dialog = await screen.findByRole('dialog', { name: 'Image policy of geth: manual' });
    await user.type(within(dialog).getByRole('textbox'), 'release week');
    await user.click(within(dialog).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(within(policy).getByRole('button', { name: 'Manual' })).toHaveAttribute('aria-pressed', 'true');
    });
    expect(h.calls.find(c => c.path === '/api/v1/actions/client-policy')?.body).toEqual({
      client: 'geth',
      policy: 'manual',
      reason: 'release week',
    });
    expect(within(policy).getByRole('button', { name: 'Use the default (automatic)…' })).toBeEnabled();
  });

  it('disables the policy switch, with the reason, for a login that may not set it', async () => {
    await open('reader', '#clients/geth');
    await screen.findByRole('table', { name: 'Hosts of geth' });

    const policy = screen.getByRole('group', { name: 'Roll policy' });
    const reason =
      'Needs upgradoor:client_policy or upgradoor:team_client_policy for a team that owns every container of this client';
    for (const name of ['Automatic', 'Manual']) {
      expect(within(policy).getByRole('button', { name })).toBeDisabled();
      expect(within(policy).getByRole('button', { name })).toHaveAttribute('title', reason);
    }
    expect(policy).toHaveTextContent(`Read only: ${reason}.`);
  });

  it('lets a team set the policy of its own client and of no other', async () => {
    const user = userEvent.setup();
    await open('team');
    await screen.findByRole('table', { name: 'Hosts of lighthouse' });
    expect(
      within(screen.getByRole('group', { name: 'Roll policy' })).getByRole('button', { name: 'Manual' })
    ).toBeEnabled();

    await user.click(screen.getByRole('tab', { name: /^geth/ }));
    await screen.findByRole('table', { name: 'Hosts of geth' });
    const policy = screen.getByRole('group', { name: 'Roll policy' });
    expect(within(policy).getByRole('button', { name: 'Manual' })).toBeDisabled();
    expect(policy).toHaveTextContent('upgradoor:team_client_policy does not cover client geth');
  });

  it("shows the client's rollouts, and calls for the approval of one that waits on the manual policy", async () => {
    const user = userEvent.setup();
    const h = await open('staff', '#clients/lighthouse');
    await screen.findByRole('table', { name: 'Hosts of lighthouse' });
    expect(screen.getByRole('tab', { name: /^Rollouts/ })).toHaveTextContent('Rollouts1');
    expect(screen.queryByRole('region', { name: /waiting for approval/ })).not.toBeInTheDocument();

    await user.click(
      within(screen.getByRole('group', { name: 'Roll policy' })).getByRole('button', { name: 'Manual' })
    );
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', PRIMARY));

    const callout = await screen.findByRole('region', { name: 'Rollouts of lighthouse waiting for approval' });
    await user.click(within(callout).getByRole('button', { name: 'Show the rollouts' }));
    expect(window.location.hash).toBe('#clients/lighthouse/rollouts');
    // The open rollout as on the Rollouts tab; the finished ones of the client below it.
    expect(screen.getByRole('article', { name: /sigp\/lighthouse/ })).toHaveTextContent('waiting for approval');
    expect(screen.getByRole('table', { name: 'Finished rollouts of lighthouse' })).toHaveTextContent('superseded');

    await user.click(within(callout).getByRole('button', { name: 'Approve…' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: /waiting for approval/ })).not.toBeInTheDocument();
    });
    expect(h.calls.some(c => c.path === '/api/v1/actions/resume')).toBe(true);
  });
});

describe("ClientsTab: a client's team", () => {
  it("approves the client's rollout that waits on the manual policy", async () => {
    const user = userEvent.setup();
    const h = await open('team', '#clients/lighthouse');
    await screen.findByRole('table', { name: 'Hosts of lighthouse' });

    await user.click(
      within(screen.getByRole('group', { name: 'Roll policy' })).getByRole('button', { name: 'Manual' })
    );
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', PRIMARY));

    const callout = await screen.findByRole('region', { name: 'Rollouts of lighthouse waiting for approval' });
    const approve = within(callout).getByRole('button', { name: 'Approve…' });
    expect(approve).toBeEnabled();

    await user.click(approve);
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: /waiting for approval/ })).not.toBeInTheDocument();
    });
    expect(h.calls.some(c => c.path === '/api/v1/actions/resume')).toBe(true);
  });
});

describe('ClientsTab: changes for a group of hosts', () => {
  it('selects every host of the filter and changes their image in one request', async () => {
    const user = userEvent.setup();
    const h = await open('staff', '#clients/lighthouse');
    const table = await screen.findByRole('table', { name: 'Hosts of lighthouse' });
    const bulk = screen.getByRole('group', { name: 'Actions on the selected hosts' });

    await user.click(
      within(screen.getByRole('group', { name: 'Filter by state' })).getByRole('button', { name: /^behind/ })
    );
    await waitFor(() => {
      expect(hostNames('lighthouse')).toHaveLength(4);
    });
    await user.click(within(table).getByRole('checkbox', { name: 'Select bootnode-1' }));
    await user.click(within(bulk).getByRole('button', { name: 'Select all 4 matching hosts' }));
    expect(bulk).toHaveTextContent('4 hosts selected');

    await user.click(within(bulk).getByRole('button', { name: 'Change image' }));
    const dialog = await screen.findByRole('dialog', { name: 'Change the image of lighthouse' });
    // The names come from the controller: the whole filter, not the page.
    expect(h.calls.some(c => c.path.includes('/hosts?') && c.path.includes('names=true'))).toBe(true);
    expect(dialog).toHaveTextContent(
      'Run the lighthouse beacon containers on 4 hosts from another image for 24 hours.'
    );
    expect(within(dialog).getByRole('button', PRIMARY)).toBeDisabled();

    await user.type(within(dialog).getByRole('textbox', { name: 'Image' }), 'sigp/lighthouse:v8.0.0');
    await user.type(within(dialog).getByPlaceholderText('Recorded in the audit log'), 'test the release');
    await user.click(within(dialog).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(h.calls.filter(c => c.path === '/api/v1/actions/client-override').map(c => c.body)).toEqual([
        expect.objectContaining({
          client: 'lighthouse',
          kind: 'cl',
          part: 'image',
          image: 'sigp/lighthouse:v8.0.0',
          reason: 'test the release',
          hosts: expect.arrayContaining(['bootnode-1']) as string[],
        }),
      ]);
    });
    // The answer says per host what was sent.
    expect(await screen.findByRole('status')).toHaveTextContent(/sent to \d of 4 hosts/);
  });

  it('changes the flags of every host of the client from the header', async () => {
    const user = userEvent.setup();
    const h = await open('staff', '#clients/teku');
    await screen.findByRole('table', { name: 'Hosts of teku' });

    const header = screen.getByRole('group', { name: 'Change every host of teku' });
    await user.click(within(header).getByRole('button', { name: 'Change flags…' }));
    const dialog = await screen.findByRole('dialog', { name: 'Change the flags of teku' });
    expect(dialog).toHaveTextContent('4 hosts');

    // A click on a configured flag removes it; the preview shows the command line after.
    await user.click(within(dialog).getByRole('button', { name: '--http' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Flags to add' }), '--debug-level=trace');
    expect(within(dialog).getByRole('list', { name: 'Command line after' })).toHaveTextContent('--debug-level=trace');
    await user.type(within(dialog).getByPlaceholderText('Recorded in the audit log'), 'debug');
    await user.click(within(dialog).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(h.calls.find(c => c.path === '/api/v1/actions/client-override')?.body).toMatchObject({
        client: 'teku',
        part: 'flags',
        addFlags: ['--debug-level=trace'],
        removeFlags: ['--http'],
      });
    });
  });

  it('disables the group changes, with the reason, for a login without the features', async () => {
    await open('reader', '#clients/lighthouse');
    await screen.findByRole('table', { name: 'Hosts of lighthouse' });

    const header = screen.getByRole('group', { name: 'Change every host of lighthouse' });
    const button = within(header).getByRole('button', { name: 'Change image…' });
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute('title', expect.stringMatching(/image_override/) as string);
  });
});

describe('the helpers of the Clients tab', () => {
  it('open on the first client that is a team of the login, else on the first', () => {
    const clients = [{ name: 'grandine' }, { name: 'teku' }] as unknown as Parameters<typeof defaultClient>[0];

    expect(defaultClient(clients, ['besu', 'teku'])).toBe('teku');
    expect(defaultClient(clients, ['geth'])).toBe('grandine');
    expect(defaultClient([], ['geth'])).toBeUndefined();
  });
});
