import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { shortDigest } from '../lib/format';
import { LIGHTHOUSE_IMAGE, currentDigest } from '../mock/fixture';
import { createHarness, renderWithApp, PRIMARY } from '../test/harness';
import { HostsTab } from './HostsTab';

const PUBLIC_COLUMNS = ['Host', 'Execution', 'Consensus', 'State'];
// The selection checkbox column has no text; the expander column has a screen-reader label.
const LEADING_COLUMNS = ['Expand'];
const PRIVATE_COLUMNS = ['Attesting', 'Kernel', 'Uptime', 'Updates', 'Seen'];

function columnNames(table: HTMLElement): string[] {
  return within(table)
    .getAllByRole('columnheader')
    .map(th => th.textContent.replace(/[▲▼]/g, '').trim())
    .filter(Boolean);
}

function rowOf(table: HTMLElement, host: string): HTMLElement {
  const cell = within(table).getByText(host, { selector: 'span' });
  const row = cell.closest('tr');
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
    expect(within(behind).getByTitle(LIGHTHOUSE_IMAGE)).toBeInTheDocument();
    expect(within(behind).getByText('pending')).toBeInTheDocument();

    const current = rowOf(table, 'lighthouse-geth-1');
    const digest = currentDigest(LIGHTHOUSE_IMAGE);
    const button = within(current).getByRole('button', { name: `digest ${digest}, click to copy` });
    expect(button).toHaveTextContent(shortDigest(digest));
    expect(shortDigest(digest)).toHaveLength(12);
    expect(button).toHaveAttribute('title', expect.stringContaining(digest));
    expect(within(current).getByText('up to date')).toBeInTheDocument();

    expect(within(rowOf(table, 'grandine-erigon-2')).getByText('pending')).toBeInTheDocument();
  });
});

describe('HostsTab, logged in', () => {
  it('adds the authenticated columns to the same table', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);

    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    expect(columnNames(table)).toEqual([...LEADING_COLUMNS, ...PUBLIC_COLUMNS, ...PRIVATE_COLUMNS]);
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
    expect(within(reboot).getByText(/^queued #\d+$/)).toBeInTheDocument();
    expect(reboot).toHaveTextContent('waiting for approval (policy manual)');

    const failed = rowOf(table, 'nimbus-nethermind-1');
    expect(failed).toHaveTextContent('0%0/128');
    expect(rowOf(table, 'prysm-erigon-1')).toHaveTextContent('3 security');
    expect(within(failed).getByText('failed')).toBeInTheDocument();

    const stale = rowOf(table, 'prysm-geth-3');
    expect(within(stale).getByText('silent')).toBeInTheDocument();
    expect(stale).toHaveTextContent('the agent stopped syncing');

    expect(within(rowOf(table, 'lighthouse-geth-2')).getByText('verifying')).toBeInTheDocument();
    expect(within(rowOf(table, 'lodestar-reth-3')).getByText('held')).toBeInTheDocument();
    expect(within(rowOf(table, 'lodestar-reth-3')).getByText('local hold')).toBeInTheDocument();
    expect(within(rowOf(table, 'prysm-geth-1')).getByText('up to date')).toBeInTheDocument();
    expect(rowOf(table, 'teku-reth-1')).toHaveTextContent('debian 13');
    expect(rowOf(table, 'lighthouse-geth-3')).toHaveTextContent(/budget [\d.]+% of 10%/);

    // A host whose last lease could not pull its image waits to try again.
    expect(rowOf(table, 'lighthouse-geth-4')).toHaveTextContent('waiting to pull its image again after a failed pull');
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

  it('expands a row to containers, needs, packages, leases and history', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<HostsTab />, h);
    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    await user.click(screen.getByRole('button', { name: 'Expand lighthouse-geth-2' }));

    const containers = await screen.findByRole('table', { name: 'Containers' });
    expect(within(containers).getAllByRole('row')).toHaveLength(1 + 4);
    expect(within(containers).getByText('lighthouse-validator')).toBeInTheDocument();
    expect(within(containers).getByText('unmanaged')).toBeInTheDocument();

    expect(screen.getByRole('table', { name: 'Leases of lighthouse-geth-2' })).toHaveTextContent('verifying');
    expect(screen.getByRole('table', { name: 'History of lighthouse-geth-2' })).toHaveTextContent('lease.granted');
    // The details come over the stream, which is reopened to ask for them.
    expect(h.calls.at(-1)?.path).toBe('/api/v1/events?host=lighthouse-geth-2');
    expect(h.calls.every(c => c.path.startsWith('/api/v1/events'))).toBe(true);

    // The validators the inventory assigns, and how many are active.
    expect(screen.getByRole('group', { name: 'Actions for lighthouse-geth-2' }).parentElement).toHaveTextContent(
      /128 assigned, 128 active/
    );

    await user.click(screen.getByRole('button', { name: 'Expand prysm-erigon-1' }));
    const upgradable = await screen.findByRole('table', { name: 'Upgradable packages' });
    expect(upgradable).toHaveTextContent('openssl');
    expect(within(upgradable).getAllByText('security')).toHaveLength(3);
  });
});

describe('HostsTab, open mode', () => {
  it('shows everything without a token', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<HostsTab />, h);

    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });

    expect(columnNames(table)).toEqual([...LEADING_COLUMNS, ...PUBLIC_COLUMNS, ...PRIVATE_COLUMNS]);
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
    expect(screen.queryByRole('group', { name: 'Actions for rpc-1' })).not.toBeInTheDocument();

    await user.click(within(table).getByText('rpc-1'));
    const actions = await screen.findByRole('group', { name: 'Actions for rpc-1' });
    expect(within(actions).getByRole('button', { name: 'Roll…' })).toBeInTheDocument();
    expect(within(table).getByRole('button', { name: 'Collapse rpc-1' })).toHaveAttribute('aria-expanded', 'true');
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
        .map(r => r.querySelector('td:nth-child(3) span span')?.textContent);

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
    await user.click(within(bulk).getByRole('button', { name: 'Action…' }));

    // Each host offers another action: the choice says on how many of them.
    const dialog = screen.getByRole('dialog', { name: 'Run an action' });
    const choice = within(dialog).getByRole('combobox');
    expect(
      within(choice)
        .getAllByRole('option')
        .map(o => o.textContent)
    ).toEqual(['docker-prune (1 of 2)', 'security-upgrades (1 of 2)']);
    await user.selectOptions(choice, 'security-upgrades');
    expect(within(dialog).getByRole('note')).toHaveTextContent('1 selected host does not offer security-upgrades');

    await user.click(within(dialog).getByText('Advanced options'));
    await user.click(within(dialog).getByRole('checkbox', { name: /Pause after the canary/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Start' }));

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
});
