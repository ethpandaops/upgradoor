import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

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
      expect.stringMatching(/^gethgeth · ELgroup executionno override/) as string,
      expect.stringMatching(
        /^prysmprysm · CLgroup consensusapplied.*by alice@example.org.*add --verbosity=debug/
      ) as string,
      expect.stringMatching(/^prysm-validatorprysm · VCgroup consensusno override/) as string,
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

  it('names the roll group of every managed container, tooling of a group of its own too', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="rpc-1" />, h);

    const table = await screen.findByRole('table', { name: 'Containers' });
    const group = (name: string) => {
      const cells = within(table)
        .getAllByRole('row')
        .map(row => within(row).queryAllByRole('cell'))
        .find(row => row[0]?.textContent === name);
      return cells?.[2]?.textContent;
    };
    expect(group('geth')).toBe('execution');
    expect(group('lighthouse')).toBe('consensus');
    expect(group('nginx')).toBe('proxy');
    expect(group('node-exporter')).toBe('–');
    // The proxy has no client: it is not among the containers that may be overridden.
    expect(screen.queryByRole('group', { name: 'Overrides of nginx' })).not.toBeInTheDocument();
  });

  it('says where a host stands with the images its needs wait for', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="lighthouse-geth-4" />, h);

    // One host pulls at a time: the one ahead of it in the queue has the window.
    expect(await screen.findByText('waiting for a pull window')).toBeInTheDocument();
    const needs = (await screen.findByRole('heading', { name: /^Open needs/ })).closest('section') as HTMLElement;
    expect(within(needs).getAllByText('not pulled')).toHaveLength(2);
  });

  it('lists no client section for a host without client containers', async () => {
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<Panel name="tooling-1" />, h);

    await screen.findByRole('heading', { name: /^Containers/ });
    expect(screen.queryByRole('heading', { name: 'Clients, overrides and command lines' })).not.toBeInTheDocument();
  });
});
