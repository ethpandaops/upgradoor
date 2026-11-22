import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { useActions } from '../components/ActionDialog';
import { createHarness, renderWithApp } from '../test/harness';
import { HostDetailPanel } from './HostDetailPanel';

function Panel({ name }: { name: string }) {
  const actions = useActions();
  return (
    <>
      <HostDetailPanel name={name} onAction={actions.request} />
      {actions.dialog}
    </>
  );
}

describe('HostDetailPanel, the containers of a client', () => {
  it('shows the client, the override and, on demand, the command line of each, with the override actions', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="prysm-geth-1" />, h);

    const section = (await screen.findByRole('heading', { name: 'Clients, overrides and command lines' })).closest(
      'section'
    ) as HTMLElement;
    const rows = within(section)
      .getAllByRole('listitem')
      .filter(li => li.querySelector('details'));
    expect(rows.map(li => li.textContent.split('command line')[0])).toEqual([
      expect.stringMatching(/^gethgeth · ELnodeno override/) as string,
      expect.stringMatching(/^prysmprysm · CLnodeapplied.*by alice@example.org.*add --verbosity=debug/) as string,
      expect.stringMatching(/^prysm-validatorprysm · VCnodeno override/) as string,
    ]);

    // The command line is there and closed until it is asked for.
    const command = within(section).getByRole('list', { name: 'Command line of prysm' });
    expect(command.closest('details')).not.toHaveAttribute('open');
    expect(command).toHaveTextContent('--verbosity=debug');

    // The same two actions as on the Clients tab, for this one container.
    await user.click(
      within(within(section).getByRole('group', { name: 'Overrides of geth' })).getByRole('button', {
        name: 'Override the image on this host',
      })
    );
    const dialog = await screen.findByRole('dialog', { name: 'Image override on prysm-geth-1' });
    expect(within(dialog).queryByRole('combobox')).not.toBeInTheDocument();
    expect(dialog).toHaveTextContent('Containergeth');
  });

  it('names the team of every labelled container; a kind without a team has none', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="rpc-1" />, h);

    const table = await screen.findByRole('table', { name: 'Containers' });
    const cell = (name: string, i: number) => {
      const cells = within(table)
        .getAllByRole('row')
        .map(row => within(row).queryAllByRole('cell'))
        .find(row => row[0]?.textContent === name);
      return cells?.[i]?.textContent;
    };
    expect(cell('geth', 2)).toBe('geth');
    expect(cell('lighthouse', 2)).toBe('lighthouse');
    expect(cell('snooper', 1)).toContain('SNOOPER-EL');
    expect(cell('snooper', 2)).toBe('–');
    expect(cell('node-exporter', 2)).toBe('–');
    // The snooper has no team: it is not among the containers that may be overridden.
    expect(screen.queryByRole('group', { name: 'Overrides of snooper' })).not.toBeInTheDocument();
  });

  it('says which image of a host could not be pulled, and why', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="lighthouse-geth-4" />, h);

    const needs = (await screen.findByRole('heading', { name: /^Open needs/ })).closest('section') as HTMLElement;
    const failed = within(needs).getAllByText('pull failed');
    expect(failed).toHaveLength(2);
    expect(failed[0]).toHaveAttribute('title', expect.stringMatching(/^image pull failed: toomanyrequests/));
  });

  it('lists no client section for a host without client containers', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="tooling-1" />, h);

    await screen.findByRole('heading', { name: /^Containers/ });
    expect(screen.queryByRole('heading', { name: 'Clients, overrides and command lines' })).not.toBeInTheDocument();
  });
});

describe('HostDetailPanel, host actions', () => {
  it('lists only the actions the login may start, and starts one on the host', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'team' });
    renderWithApp(<Panel name="lighthouse-geth-1" />, h);

    // The team login is in lighthouse, which the prune action names; it lacks the feature of the security updates.
    await user.click(await screen.findByRole('button', { name: 'Host actions of lighthouse-geth-1' }));
    const menu = screen.getByRole('menu');
    expect(
      within(menu)
        .getAllByRole('menuitem')
        .map(i => i.textContent)
    ).toEqual([expect.stringMatching(/^prune-beacon-db/) as string]);
    expect(screen.queryByRole('button', { name: /^Run security-upgrades/ })).not.toBeInTheDocument();

    await user.click(within(menu).getByRole('menuitem'));
    const dialog = await screen.findByRole('dialog', { name: 'Run prune-beacon-db on lighthouse-geth-1' });
    expect(within(dialog).queryByText('Advanced options')).not.toBeInTheDocument();
    await user.type(within(dialog).getByRole('textbox'), 'db too large');
    await user.click(within(dialog).getByRole('button', { name: 'Start' }));

    await vi.waitFor(() => {
      expect(h.calls.find(c => c.method === 'POST')).toBeDefined();
    });
    const post = h.calls.find(c => c.method === 'POST');
    expect(post?.path).toBe('/api/v1/actions/host-action');
    expect(post?.body).toStrictEqual({
      action: 'prune-beacon-db',
      hosts: ['lighthouse-geth-1'],
      reason: 'db too large',
    });
  });

  it('offers the security updates next to the package counts, highlighted when they are due', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="prysm-erigon-1" />, h);

    const shortcut = await screen.findByRole('button', { name: 'Run security-upgrades on prysm-erigon-1' });
    expect(shortcut).toHaveTextContent('Update now');
    expect(shortcut).toHaveAttribute('title', expect.stringContaining('security updates are due'));

    await user.click(shortcut);
    expect(await screen.findByRole('dialog', { name: 'Run security-upgrades on prysm-erigon-1' })).toBeInTheDocument();
  });
});
