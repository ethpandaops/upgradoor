import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { shortDigest } from '../lib/format';
import { LIGHTHOUSE_IMAGE, currentDigest } from '../mock/fixture';
import { createHarness, renderWithApp, PRIMARY } from '../test/harness';
import { HostsTab } from './HostsTab';

const PUBLIC_COLUMNS = [
  'Host',
  'Execution client',
  'Execution',
  'EL digest',
  'Consensus client',
  'Consensus',
  'CL digest',
  'State',
];
const PRIVATE_COLUMNS = ['Reason', 'Attesting', 'CPU', 'Memory', 'Kernel', 'Uptime', 'Updates', 'Seen'];

function columnNames(table: HTMLElement): string[] {
  return within(table)
    .getAllByRole('columnheader')
    .map(th => th.textContent.replace(/[▲▼]/g, '').trim())
    .filter(Boolean);
}

function rowOf(table: HTMLElement, host: string): HTMLElement {
  const row = table.querySelector<HTMLElement>(`tr[data-host="${host}"]`);
  if (!row) throw new Error(`no row for ${host}`);
  return row;
}

describe('HostsTab, logged out (public data only)', () => {
  it('shows only the public columns and requests no authenticated route', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    renderWithApp(<HostsTab />, h);

    const table = await screen.findByRole('table', { name: 'Hosts' });

    expect(columnNames(table)).toEqual(PUBLIC_COLUMNS);
    expect(within(table).getAllByRole('row')).toHaveLength(1 + 24);
    expect(screen.queryByRole('group', { name: 'Actions on the selected hosts' })).not.toBeInTheDocument();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    expect(screen.getByText(/Log in for host details/)).toBeInTheDocument();

    // Nothing that is only served to users may be on the page.
    expect(table).not.toHaveTextContent('6.12.57+deb13-amd64');
    expect(table).not.toHaveTextContent('debian');

    expect(h.userCalls()).toEqual([]);
    expect(h.calls.every(c => c.authorization === null)).toBe(true);
    // One request: the public event stream.
    expect(h.calls.map(c => c.path)).toEqual(['/api/v1/public/events']);
  });

  it('renders image, short digest and the update marker', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: false });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });

    const behind = rowOf(table, 'lighthouse-geth-3');
    expect(behind).toHaveTextContent('lighthouse:unstable');
    expect(within(behind).getByTitle(LIGHTHOUSE_IMAGE)).toHaveTextContent(/^lighthouse:unstable$/);
    // Each client has a column of its own, right before its image.
    const [, elClient, execution, , clClient, consensus] = within(behind).getAllByRole('cell');
    if (!elClient || !execution || !clClient || !consensus) throw new Error('missing cells');
    expect(within(elClient).getByTitle('Execution client')).toHaveTextContent(/^geth$/);
    expect(execution).toHaveTextContent(/^geth:master$/);
    expect(within(clClient).getByTitle('Consensus client')).toHaveTextContent(/^lighthouse$/);
    expect(consensus).toHaveTextContent(/^lighthouse:unstable$/);
    expect(within(behind).getByText('pending')).toBeInTheDocument();

    const current = rowOf(table, 'lighthouse-geth-1');
    const digest = currentDigest(LIGHTHOUSE_IMAGE);
    const button = within(current).getByRole('button', { name: `digest ${digest}, click to copy` });
    expect(button).toHaveTextContent(shortDigest(digest));
    expect(shortDigest(digest)).toHaveLength(12);
    expect(button).toHaveAttribute('title', expect.stringContaining(digest));
    expect(within(current).getByText('✓ up to date')).toBeInTheDocument();

    expect(within(rowOf(table, 'grandine-erigon-2')).getByText('pending')).toBeInTheDocument();
  });
});

describe('HostsTab, logged in', () => {
  it('adds the authenticated columns to the same table', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);

    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    expect(columnNames(table)).toEqual([...PUBLIC_COLUMNS, ...PRIVATE_COLUMNS]);
    expect(screen.getByRole('group', { name: 'Actions on the selected hosts' })).toBeInTheDocument();

    // Everything on the page came over the one authenticated event stream.
    expect(h.calls.map(c => c.path)).toEqual(['/api/v1/events']);
    expect(h.userCalls().every(c => c.authorization === 'Bearer mock-token')).toBe(true);
  });

  it('shows kernel, reboot flag, security updates, attesting share, staleness and the state', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    const reboot = rowOf(table, 'teku-besu-1');
    expect(reboot).toHaveTextContent('6.12.48+deb13-amd64');
    expect(within(reboot).getByText('reboot')).toBeInTheDocument();
    expect(within(reboot).getByText(/^(next|#\d+ in line)$/)).toBeInTheDocument();
    expect(reboot).toHaveTextContent('waiting for approval (policy manual)');

    const failed = rowOf(table, 'nimbus-nethermind-1');
    expect(failed).toHaveTextContent('0%0/128');
    // Its EL is down: the plan says so for a host without a lease.
    expect(within(failed).getByText('not ready')).toHaveAttribute(
      'title',
      'Not ready by its last sync: EL is not reachable'
    );
    expect(failed).toHaveTextContent('EL is not reachable');
    expect(within(rowOf(table, 'lighthouse-geth-1')).queryByText('not ready')).toBeNull();
    expect(rowOf(table, 'prysm-erigon-1')).toHaveTextContent('3 security');
    expect(within(failed).getByText('failed')).toBeInTheDocument();

    const stale = rowOf(table, 'prysm-geth-3');
    expect(within(stale).getByText('silent')).toBeInTheDocument();
    expect(stale).toHaveTextContent('the agent stopped syncing');

    expect(within(rowOf(table, 'lighthouse-geth-2')).getByText('verifying')).toBeInTheDocument();
    expect(within(rowOf(table, 'lodestar-reth-3')).getByText('held')).toBeInTheDocument();
    expect(within(rowOf(table, 'lodestar-reth-3')).getByText('local hold')).toBeInTheDocument();
    expect(rowOf(table, 'lodestar-reth-3')).toHaveTextContent('held by the host');
    expect(within(rowOf(table, 'prysm-geth-1')).getByText('✓ up to date')).toBeInTheDocument();
    expect(rowOf(table, 'teku-reth-1')).toHaveTextContent('debian 13');
    expect(rowOf(table, 'lighthouse-geth-3')).toHaveTextContent(
      /validators down: [\d.]+% now, [\d.]+% with this host, limit 10%/
    );

    // A host whose last lease could not pull its image waits to try again.
    expect(rowOf(table, 'lighthouse-geth-4')).toHaveTextContent('waiting to pull its image again after a failed pull');
  });

  it('sums up CPU and memory of the fleet, and a tile keeps the busy hosts', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });

    const tiles = await screen.findByRole('group', { name: 'Fleet resources' });
    const cpu = within(tiles).getByRole('button', { name: /^CPU/ });
    const busy = Number(/(\d+) ≥80%/.exec(cpu.textContent)?.[1]);
    expect(busy).toBeGreaterThan(0);
    expect(cpu).toHaveTextContent(/worst\s*\S+\s*9\d(\.\d)?%/);

    await user.click(cpu);
    expect(screen.getByRole('searchbox', { name: 'Filter hosts' })).toHaveValue('cpu>=80');
    expect(within(table).getAllByRole('row')).toHaveLength(1 + busy);
    for (const meter of within(table).getAllByRole('meter', { name: /^CPU of / })) {
      expect(Number(meter.getAttribute('aria-valuenow'))).toBeGreaterThanOrEqual(80);
    }

    await user.click(cpu);
    expect(screen.getByRole('searchbox', { name: 'Filter hosts' })).toHaveValue('');
  });

  it('shows a login without details no resource use', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'reader' });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Attesting/ });

    expect(within(table).queryByRole('columnheader', { name: /CPU/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('group', { name: 'Fleet resources' })).not.toBeInTheDocument();
  });

  it('filters by name, image and role, and sorts by a column', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    const filter = screen.getByRole('searchbox', { name: 'Filter hosts' });

    await user.type(filter, 'bootnode');
    expect(within(table).getAllByRole('row')).toHaveLength(2);
    expect(rowOf(table, 'bootnode-1')).toBeInTheDocument();

    await user.clear(filter);
    await user.type(filter, 'nethermind');
    expect(within(table).getAllByRole('row')).toHaveLength(1 + 4);

    await user.clear(filter);
    await user.type(filter, 'teku besu');
    expect(within(table).getAllByRole('row')).toHaveLength(1 + 3);

    await user.clear(filter);
    await user.type(filter, 'no-such-host');
    expect(screen.getByText(/No host matches/)).toBeInTheDocument();

    await user.clear(filter);
    const firstHost = () => within(table).getAllByRole('row')[1]?.textContent ?? '';
    expect(firstHost()).toContain('bootnode-1');

    const hostHeader = within(table).getByRole('columnheader', { name: /Host/ });
    await user.click(within(hostHeader).getByRole('button'));
    expect(hostHeader).toHaveAttribute('aria-sort', 'descending');
    expect(firstHost()).toContain('tooling-1');

    const uptime = within(table).getByRole('columnheader', { name: /Uptime/ });
    await user.click(within(uptime).getByRole('button', { name: 'Uptime' }));
    await user.click(within(uptime).getByRole('button', { name: 'Uptime' }));
    expect(uptime).toHaveAttribute('aria-sort', 'descending');
    expect(firstHost()).toMatch(/lodestar-reth-1|teku-besu-/);
  });

  it('marks selected hosts the filter hides, in the toolbar and in the confirmation', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    await user.click(within(table).getByRole('checkbox', { name: 'Select all hosts the filter shows, on every page' }));
    const bulk = screen.getByRole('group', { name: 'Actions on the selected hosts' });
    expect(bulk).toHaveTextContent('24 hosts selected');
    expect(bulk).not.toHaveTextContent('hidden by the filter');

    await user.type(screen.getByRole('searchbox', { name: 'Filter hosts' }), 'bootnode');
    expect(within(table).getAllByRole('row')).toHaveLength(2);
    expect(bulk).toHaveTextContent('24 hosts selected');
    expect(bulk).toHaveTextContent('23 hidden by the filter');

    // The confirmation says that the action reaches hosts that are not on screen.
    await user.click(within(bulk).getByRole('button', { name: 'Reboot' }));
    const dialog = screen.getByRole('dialog');
    const warning = within(dialog).getByRole('alert');
    expect(warning).toHaveTextContent('23 of them are hidden by the current filter:');
    expect(warning).not.toHaveTextContent('bootnode-1');
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));

    await user.click(within(bulk).getByRole('button', { name: 'Forget' }));
    expect(within(screen.getByRole('dialog')).getByRole('alert')).toHaveTextContent('23 of them are hidden');
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Cancel' }));

    // "Drop hidden" narrows the selection to what is on screen; the request then carries only that.
    await user.click(within(bulk).getByRole('button', { name: 'Drop them' }));
    expect(bulk).toHaveTextContent('1 host selected');
    expect(bulk).not.toHaveTextContent('hidden by the filter');

    await user.click(within(bulk).getByRole('button', { name: 'Forget' }));
    const confirm = screen.getByRole('dialog');
    expect(within(confirm).queryByRole('alert')).not.toBeInTheDocument();
    await user.click(within(confirm).getByRole('button', PRIMARY));

    await vi.waitFor(() => {
      expect(h.calls.filter(c => c.method === 'POST')).toHaveLength(1);
    });
    const post = h.calls.find(c => c.method === 'POST');
    expect(post?.path).toBe('/api/v1/actions/forget');
    expect(post?.body).toStrictEqual({ hosts: ['bootnode-1'], reason: '' });
  });

  it('opens a host in the drawer, linkable, and steps through the list with the arrow keys', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    await user.click(within(table).getByRole('button', { name: 'Open lighthouse-geth-2' }));
    const drawer = await screen.findByRole('complementary', { name: 'Host lighthouse-geth-2' });
    expect(window.location.search).toBe('?host=lighthouse-geth-2');
    expect(rowOf(table, 'lighthouse-geth-2')).toHaveAttribute('aria-current', 'true');
    // The table stays.
    expect(table).toBeInTheDocument();
    await within(drawer).findByRole('tablist');
    // The details come over the stream, which is reopened to ask for them.
    expect(h.calls.at(-1)?.path).toBe('/api/v1/events?host=lighthouse-geth-2');
    expect(h.calls.every(c => c.path.startsWith('/api/v1/events'))).toBe(true);

    await user.keyboard('{ArrowDown}');
    expect(await screen.findByRole('complementary', { name: 'Host lighthouse-geth-3' })).toBeInTheDocument();
    await user.keyboard('{ArrowUp}{ArrowUp}');
    expect(await screen.findByRole('complementary', { name: 'Host lighthouse-geth-1' })).toBeInTheDocument();
    expect(window.location.search).toBe('?host=lighthouse-geth-1');

    await user.keyboard('{Escape}');
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument();
    expect(window.location.search).toBe('');
  });

  it('opens the host the URL names', async () => {
    window.history.replaceState(null, '', '/?host=rpc-1');
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<HostsTab />, h);

    expect(await screen.findByRole('complementary', { name: 'Host rpc-1' })).toBeInTheDocument();
  });
});

describe('HostsTab, open mode', () => {
  it('shows everything without a token', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<HostsTab />, h);

    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    expect(columnNames(table)).toEqual([...PUBLIC_COLUMNS, ...PRIVATE_COLUMNS]);
    expect(h.calls.every(c => c.authorization === null)).toBe(true);
    expect(screen.queryByText(/Log in for host details/)).not.toBeInTheDocument();
  });
});

describe('HostsTab rows', () => {
  it('opens a host by a click anywhere on its row, but not on its checkbox', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    await user.click(within(table).getByRole('checkbox', { name: 'Select rpc-1' }));
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument();

    await user.click(within(rowOf(table, 'rpc-1')).getByText('geth:master'));
    const actions = await screen.findByRole('group', { name: 'Actions for rpc-1' });
    expect(within(actions).getByRole('button', { name: 'Roll…' })).toBeInTheDocument();
    expect(within(table).getByRole('button', { name: 'Open rpc-1' })).toHaveAttribute('aria-expanded', 'true');
  });

  it('filters by the state the table shows', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });
    const filter = screen.getByRole('searchbox', { name: 'Filter hosts' });
    const hosts = () =>
      within(table)
        .getAllByRole('row')
        .slice(1)
        .map(r => r.querySelector('td:nth-child(2) button')?.textContent);

    await user.type(filter, 'silent');
    expect(hosts()).toEqual(['prysm-geth-3']);

    await user.clear(filter);
    await user.type(filter, 'verifying');
    // The state, and the waiting reasons that mention it.
    expect(hosts()).toEqual(['lighthouse-geth-2', 'nimbus-nethermind-2', 'nimbus-nethermind-3']);

    await user.clear(filter);
    await user.type(filter, 'approval');
    expect(hosts()).toHaveLength(4);

    // The raw slot names of the API are not what the table says.
    await user.clear(filter);
    await user.type(filter, 'stale');
    expect(screen.getByText(/No host matches/)).toBeInTheDocument();
  });
});

describe('HostsTab, a large fleet', () => {
  it('shows a page of hosts at a time, and selects every host the filter shows', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true, fleet: 'large' });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    expect(within(table).getAllByRole('row')).toHaveLength(1 + 100);
    const pages = screen.getByRole('group', { name: 'Pages' });
    expect(pages).toHaveTextContent(/^1–100 of \d+/);

    await user.click(within(pages).getByRole('button', { name: 'Next page' }));
    expect(pages).toHaveTextContent(/^101–200 of/);

    await user.click(within(table).getByRole('checkbox', { name: 'Select all hosts the filter shows, on every page' }));
    const total = Number(/of (\d+)/.exec(pages.textContent)?.[1]);
    expect(total).toBeGreaterThan(200);
    expect(screen.getByRole('group', { name: 'Actions on the selected hosts' })).toHaveTextContent(
      `${total} hosts selected`
    );

    // A new filter starts at the first page.
    await user.type(screen.getByRole('searchbox', { name: 'Filter hosts' }), 'geth');
    expect(screen.queryByRole('group', { name: 'Pages' })?.textContent ?? '1–').toMatch(/^1–/);
  });
});

describe('HostsTab, host actions on the selection', () => {
  it('starts an action on the selected hosts and shows the hosts it was not sent to', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    await user.click(within(table).getByRole('checkbox', { name: 'Select prysm-erigon-1' }));
    await user.click(within(table).getByRole('checkbox', { name: 'Select tooling-1' }));
    const bulk = screen.getByRole('group', { name: 'Actions on the selected hosts' });
    await user.click(within(bulk).getByRole('button', { name: 'Run host action…' }));

    // Each host offers another action: the choice says on how many of them, and none is picked.
    const dialog = screen.getByRole('dialog', { name: 'Run a host action' });
    const radios = within(dialog).getAllByRole('radio');
    expect(radios.map(r => r.closest('label')?.textContent)).toEqual([
      expect.stringMatching(/^docker-prune.*available on 1 of 2 selectedRemove unused Docker images$/) as string,
      expect.stringMatching(/^security-upgrades.*available on 1 of 2 selected/) as string,
    ]);
    expect(radios.some(r => (r as HTMLInputElement).checked)).toBe(false);
    expect(within(dialog).getByRole('button', { name: 'Run … on 2 hosts' })).toBeDisabled();
    // The gate is open: nothing to override.
    expect(within(dialog).queryByRole('checkbox', { name: /Roll despite network state/ })).not.toBeInTheDocument();

    await user.click(within(dialog).getByRole('radio', { name: /^security-upgrades/ }));
    expect(within(dialog).getByRole('note')).toHaveTextContent('1 selected host does not offer security-upgrades');

    await user.click(within(dialog).getByText('Advanced options'));
    await user.click(within(dialog).getByRole('checkbox', { name: /Pause after the canary/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Run security-upgrades on 2 hosts' }));

    await vi.waitFor(() => {
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });
    const post = h.calls.find(c => c.method === 'POST');
    expect(post?.path).toBe('/api/v1/actions/host-action');
    expect(post?.body).toStrictEqual({
      action: 'security-upgrades',
      hosts: ['prysm-erigon-1', 'tooling-1'],
      pauseAfterCanary: true,
      reason: '',
    });

    const notice = screen.getByRole('status');
    expect(notice).toHaveTextContent('action security-upgrades on 1 host(s)');
    expect(within(notice).getByRole('list', { name: 'Hosts not sent' })).toHaveTextContent(
      'tooling-1: the host does not report the action security-upgrades'
    );
  });

  it('runs despite a closed network gate only when asked to', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    h.controller.setGateFailing(true);
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    await user.click(within(table).getByRole('checkbox', { name: 'Select prysm-erigon-1' }));
    await user.click(
      within(screen.getByRole('group', { name: 'Actions on the selected hosts' })).getByRole('button', {
        name: 'Run host action…',
      })
    );
    const dialog = screen.getByRole('dialog', { name: 'Run a host action on prysm-erigon-1' });
    await user.click(within(dialog).getByRole('radio', { name: /^security-upgrades/ }));

    const ignore = within(dialog).getByRole('checkbox', { name: /Roll despite network state/ });
    expect(ignore).not.toBeChecked();
    expect(ignore.closest('label')).toHaveTextContent('The network gate is closed:');
    await user.click(ignore);
    await user.click(within(dialog).getByRole('button', { name: 'Run security-upgrades on 1 host' }));

    await vi.waitFor(() => {
      expect(h.calls.find(c => c.method === 'POST')).toBeDefined();
    });
    expect(h.calls.find(c => c.method === 'POST')?.body).toStrictEqual({
      action: 'security-upgrades',
      hosts: ['prysm-erigon-1'],
      ignoreGate: true,
      reason: '',
    });
  });
});
