import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';

import { MOCK_LOGINS, MockController } from '../mock/mockController';
import { createHarness, renderWithApp } from '../test/harness';
import { SSHLoginsTab } from './SSHLoginsTab';

const sshCalls = (calls: { path: string }[]) =>
  calls.filter(c => c.path.startsWith('/api/v1/ssh-logins')).map(c => c.path);

describe('SSHLoginsTab', () => {
  beforeEach(() => {
    window.location.hash = '#ssh';
  });

  it('opens on the fleet view: an Ansible run is one row that unfolds into its hosts', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<SSHLoginsTab />, h);

    const table = await screen.findByRole('table', { name: 'SSH logins across the fleet' });
    expect(sshCalls(h.calls)[0]).toBe('/api/v1/ssh-logins?view=fleet&limit=50');

    // A GitHub identity links to its profile.
    const github = within(table).getAllByRole('link', { name: 'github:pk910' })[0];
    expect(github).toHaveAttribute('href', 'https://github.com/pk910');
    // A key the host's key map does not know shows by its fingerprint.
    expect(within(table).getAllByText('unknown key').length).toBeGreaterThan(0);
    // A session logged in now says so instead of an end.
    expect(within(table).getAllByText('logged in now').length).toBeGreaterThan(0);

    const mass = within(table)
      .getAllByRole('row')
      .find(r => /\d+ hosts/.test(r.textContent));
    if (!mass) throw new Error('no row across hosts');
    await user.click(within(mass).getAllByRole('button', { name: 'Show the hosts' })[0] as HTMLElement);

    const hosts = await screen.findByRole('table', { name: /^Hosts of the logins of/ });
    expect(within(hosts).getAllByRole('row').length).toBeGreaterThan(2);
    expect(sshCalls(h.calls).some(p => p.includes('view=groups') && p.includes('identity=github'))).toBe(true);
  });

  it('keeps the view and the filters in the URL', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<SSHLoginsTab />, h);
    await screen.findByRole('table', { name: 'SSH logins across the fleet' });

    await user.click(screen.getByRole('button', { name: 'Per host' }));
    const groups = await screen.findByRole('table', { name: 'SSH login groups' });
    expect(window.location.hash).toBe('#ssh/?view=groups');

    await user.click(screen.getByRole('checkbox', { name: 'Unknown keys only' }));
    await waitFor(() => {
      expect(window.location.hash).toBe('#ssh/?view=groups&unknown=1');
    });
    await waitFor(() => {
      expect(within(groups).queryAllByRole('link', { name: /^github:/ })).toHaveLength(0);
    });
    expect(within(groups).getAllByText('unknown key').length).toBeGreaterThan(0);
    expect(sshCalls(h.calls).at(-1)).toBe('/api/v1/ssh-logins?view=groups&limit=50&unknown=1');
  });

  it('lands on a linked filter', async () => {
    window.location.hash = '#ssh/?view=groups&open=1';
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<SSHLoginsTab />, h);

    const groups = await screen.findByRole('table', { name: 'SSH login groups' });
    expect(within(groups).getAllByRole('row')).toHaveLength(2);
    expect(within(groups).getByText('logged in now')).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Logged in now' })).toBeChecked();
  });
});

describe('GET /api/v1/ssh-logins of the mock', () => {
  it('pages both views and refuses a login without the feature audit', async () => {
    const m = new MockController();
    const get = async (q: string) => {
      const res = await m.fetch(`/api/v1/ssh-logins?${q}`);
      return { status: res.status, body: (await res.json()) as Record<string, unknown> };
    };

    const first = await get('view=groups&limit=5');
    expect((first.body.groups as unknown[]).length).toBe(5);
    expect(first.body.next).not.toBe('');
    const second = await get(`view=groups&limit=5&cursor=${String(first.body.next)}`);
    expect((second.body.groups as { id: number }[])[0]?.id).not.toBe((first.body.groups as { id: number }[])[0]?.id);

    const fleet = await get('');
    const clusters = fleet.body.clusters as { hosts: number; identity?: string }[];
    expect(clusters.some(c => c.hosts > 10 && c.identity === 'github:pk910')).toBe(true);
  });

  it('answers 403 to a login without audit', async () => {
    const m = new MockController({ auth: true });
    const as = (token: string) => m.fetch('/api/v1/ssh-logins', { headers: { Authorization: `Bearer ${token}` } });
    expect((await as(MOCK_LOGINS.reader.token)).status).toBe(403);
    expect((await as(MOCK_LOGINS.staff.token)).status).toBe(200);
  });
});
