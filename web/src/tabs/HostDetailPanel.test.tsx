import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { list } from '../api/types';
import { useActions } from '../components/ActionDialog';
import { createHarness, renderWithApp } from '../test/harness';
import { HostDrawer } from './HostDetailPanel';

function Panel({ name, onClose = () => undefined }: { name: string; onClose?: () => void }) {
  const actions = useActions();
  return (
    <>
      <HostDrawer name={name} onClose={onClose} onAction={actions.request} />
      {actions.dialog}
    </>
  );
}

async function openDrawer(name: string, opts: Parameters<typeof createHarness>[0] = { mode: 'open' }) {
  const user = userEvent.setup();
  const h = await createHarness(opts);
  renderWithApp(<Panel name={name} />, h);
  const drawer = await screen.findByRole('complementary', { name: `Host ${name}` });
  await within(drawer).findByRole('tablist');
  const tab = (label: string) => user.click(within(drawer).getByRole('tab', { name: label }));
  return { h, user, drawer, tab };
}

describe('HostDrawer header', () => {
  it('names the host, its state and its facts, with the host actions', async () => {
    const { drawer } = await openDrawer('lighthouse-geth-2');

    const heading = within(drawer).getByRole('heading', { name: 'lighthouse-geth-2' });
    expect(heading.parentElement).toHaveTextContent('lighthouse-geth-2verifying');
    expect(within(drawer).getByLabelText('Facts')).toHaveTextContent(/lighthouse.*EL CL VC.*agent .*seen/);

    const actions = within(drawer).getByRole('group', { name: 'Actions for lighthouse-geth-2' });
    expect(
      within(actions)
        .getAllByRole('button')
        .map(b => b.textContent)
    ).toEqual(['Roll…', 'Reboot…', 'Hold…', 'Mark offline…', 'Forget…', 'Actions']);
  });
});

describe('HostDrawer overview', () => {
  it('shows the current lease, the validators and the open needs', async () => {
    const { drawer } = await openDrawer('lighthouse-geth-2');

    const now = within(drawer).getByRole('region', { name: 'Now' });
    expect(now).toHaveTextContent('verifying');
    expect(now).toHaveTextContent('before leaseready');
    expect(within(drawer).getByRole('region', { name: 'Node' })).toHaveTextContent('128 assigned, 128 active');

    const resources = within(drawer).getByRole('region', { name: 'System resources' });
    expect(resources).toHaveTextContent(/\d+ cores · load [\d.]+ \/ [\d.]+ \/ [\d.]+/);
    expect(resources).toHaveTextContent(/GiB of \d+ GiB/);
    expect(within(resources).getByRole('meter', { name: 'CPU busy' })).toBeInTheDocument();
  });

  it('says what a host without a lease is not ready for', async () => {
    const { drawer } = await openDrawer('nimbus-nethermind-1');

    expect(within(drawer).getByRole('region', { name: 'Now' })).toHaveTextContent('not readyEL is not reachable');
  });

  it('says which image of a host could not be pulled, and why', async () => {
    const { drawer } = await openDrawer('lighthouse-geth-4');

    const needs = within(drawer).getByRole('list', { name: 'Open needs' });
    const failed = within(needs).getAllByText('pull failed');
    expect(failed).toHaveLength(2);
    expect(failed[0]).toHaveAttribute('title', expect.stringMatching(/^image pull failed: toomanyrequests/));
  });

  it('folds the upgradable packages away and offers the security updates next to the counts', async () => {
    const { drawer, user } = await openDrawer('prysm-erigon-1');

    await user.click(within(drawer).getByText('Upgradable packages (3)'));
    const upgradable = within(drawer).getByRole('table', { name: 'Upgradable packages' });
    expect(upgradable).toHaveTextContent('openssl');
    expect(within(upgradable).getAllByText('security')).toHaveLength(3);

    const shortcut = within(drawer).getByRole('button', { name: 'Run security-upgrades on prysm-erigon-1' });
    expect(shortcut).toHaveTextContent('Update now');
    expect(shortcut).toHaveAttribute('title', expect.stringContaining('security updates are due'));
    await user.click(shortcut);
    expect(await screen.findByRole('dialog', { name: 'Run security-upgrades on prysm-erigon-1' })).toBeInTheDocument();
  });
});

describe('HostDrawer holds', () => {
  it('lists the holds the host reports and says its open needs wait for them', async () => {
    const { h, drawer } = await openDrawer('lighthouse-geth-3');
    await h.api.action({ action: 'hold', body: { hosts: ['lighthouse-geth-3'], reason: 'disk swap' } });

    const now = within(drawer).getByRole('region', { name: 'Now' });
    expect(await within(now).findByText('held by staff: they wait until the hold is lifted')).toBeInTheDocument();
    expect(now).toHaveTextContent('staffdisk swap');
    expect(within(now).getByRole('button', { name: 'Lift…' })).toBeInTheDocument();
  });

  it("shows the host's own hold without a way to lift it here", async () => {
    const { drawer } = await openDrawer('lodestar-reth-3');

    const now = within(drawer).getByRole('region', { name: 'Now' });
    expect(now).toHaveTextContent('on the hostdebugging reth OOM, do not touch (pk)');
    expect(within(now).queryByRole('button', { name: 'Lift…' })).not.toBeInTheDocument();
  });
});

describe('HostDrawer without details', () => {
  it('leaves out the sections of what the login may not read', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    const name = 'prysm-geth-1';
    const host = h.controller.data.hosts.get(name);
    if (!host) throw new Error(`no ${name} in the fixture`);

    h.controller.data.hosts.set(name, {
      ...host,
      reduced: true,
      agentVersion: '',
      certHash: '',
      remoteAddr: '',
      inventory: undefined,
      actions: [],
      node: {},
      os: {
        id: '',
        versionId: '',
        prettyName: '',
        arch: '',
        runningKernel: '',
        newestKernel: '',
        rebootRequired: false,
      },
      pending: { upgradable: 0, security: 0, blocked: 0 },
      resources: undefined,
      containers: list(host.containers)
        .filter(c => c.managed)
        .map(c => (c.name === 'geth' ? c : { ...c, id: '', command: undefined, stats: undefined, restartCount: 0 })),
    });

    renderWithApp(<Panel name={name} />, h);
    const drawer = await screen.findByRole('complementary', { name: `Host ${name}` });
    const tabs = await within(drawer).findByRole('tablist');

    expect(within(tabs).queryByRole('tab', { name: /Actions/ })).not.toBeInTheDocument();
    expect(within(drawer).getByLabelText('Facts')).not.toHaveTextContent('agent');
    expect(within(drawer).queryByRole('region', { name: 'Packages' })).not.toBeInTheDocument();
    expect(within(drawer).queryByRole('region', { name: 'System resources' })).not.toBeInTheDocument();
    const system = within(drawer).getByRole('region', { name: 'System' });
    expect(system).not.toHaveTextContent(/kernel|certificate|first seen/);
    expect(system).toHaveTextContent('allows');

    await user.click(within(tabs).getByRole('tab', { name: /Containers/ }));
    await user.click(within(drawer).getByText('prysm', { selector: 'summary span' }));
    expect(within(drawer).queryByRole('list', { name: 'Command line of prysm' })).not.toBeInTheDocument();
    const prysm = within(drawer).getByLabelText('Container prysm').closest('li') as HTMLElement;
    expect(prysm).not.toHaveTextContent('restarts');
    const geth = within(drawer).getByLabelText('Container geth').closest('li') as HTMLElement;
    expect(geth).toHaveTextContent('restarts');
    expect(within(drawer).getByLabelText('Container geth')).toHaveTextContent(/CPU\s*\d+ GiB/);
    expect(within(drawer).getByLabelText('Container prysm')).not.toHaveTextContent('CPU');
  });
});

describe('HostDrawer containers', () => {
  it('lists one line per container and opens it to the override and the command line', async () => {
    const { drawer, tab, user } = await openDrawer('prysm-geth-1');
    await tab('Containers');

    const rows = Array.from(within(drawer).getByRole('list', { name: 'Containers' }).children);
    expect(rows.map(r => r.querySelector('summary')?.textContent)).toEqual([
      expect.stringMatching(/^gethEL/) as string,
      expect.stringMatching(/^prysmCL.*override$/) as string,
      expect.stringMatching(/^prysm-validatorVC/) as string,
      expect.stringMatching(/^node-exporterunmanaged/) as string,
    ]);

    await user.click(within(drawer).getByText('prysm', { selector: 'summary span' }));
    const prysm = rows[1] as HTMLElement;
    expect(prysm).toHaveTextContent(/applied.*by alice@example.org.*add --verbosity=debug/);
    const command = within(prysm).getByRole('list', { name: 'Command line of prysm' });
    expect(command.closest('details')).not.toHaveAttribute('open');
    expect(command).toHaveTextContent('--verbosity=debug');

    await user.click(within(drawer).getByText('geth', { selector: 'summary span' }));
    await user.click(
      within(within(drawer).getByRole('group', { name: 'Overrides of geth' })).getByRole('button', {
        name: 'Override the image on this host',
      })
    );
    const dialog = await screen.findByRole('dialog', { name: 'Image override on prysm-geth-1' });
    expect(dialog).toHaveTextContent('Containergeth');
  });

  it('offers overrides only for containers with a team', async () => {
    const { drawer, tab } = await openDrawer('rpc-1');
    await tab('Containers');

    const list = within(drawer).getByRole('list', { name: 'Containers' });
    expect(within(list).getByText('SNOOPER-EL')).toBeInTheDocument();
    expect(within(drawer).getByRole('group', { name: 'Overrides of geth' })).toBeInTheDocument();
    expect(within(drawer).queryByRole('group', { name: 'Overrides of snooper' })).not.toBeInTheDocument();
  });
});

describe('HostDrawer actions', () => {
  it('lists only the actions the login may start in the menu, and starts one on the host', async () => {
    const { h, user, drawer } = await openDrawer('lighthouse-geth-1', {
      mode: 'auth',
      loggedIn: true,
      profile: 'team',
    });

    // The team login is in lighthouse, which the prune action names; it lacks the feature of the security updates.
    await user.click(within(drawer).getByRole('button', { name: 'Host actions of lighthouse-geth-1' }));
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
    expect(within(dialog).getByRole('radio', { name: /prune-beacon-db/ })).toBeChecked();
    await user.type(within(dialog).getByRole('textbox'), 'db too large');
    await user.click(within(dialog).getByRole('button', { name: 'Run prune-beacon-db on 1 host' }));

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

  it('shows every action the host reports with its last run', async () => {
    const { drawer, tab, user } = await openDrawer('prysm-erigon-1');
    await tab('Actions');

    const action = within(drawer).getByRole('listitem', { name: 'Action security-upgrades' });
    expect(action).toHaveTextContent('Install the pending security updates');
    expect(action).toHaveTextContent('no run in the recent leases');

    await user.click(within(action).getByRole('button', { name: 'Run…' }));
    const dialog = await screen.findByRole('dialog', { name: 'Run security-upgrades on prysm-erigon-1' });
    await user.click(within(dialog).getByRole('button', { name: 'Run security-upgrades on 1 host' }));

    await vi.waitFor(() => {
      expect(within(drawer).getByRole('listitem', { name: 'Action security-upgrades' })).toHaveTextContent('exit 0');
    });
  });
});

describe('HostDrawer history', () => {
  it('shows the leases and the events of the host', async () => {
    const { drawer, tab } = await openDrawer('lighthouse-geth-2');
    await tab('History');

    expect(within(drawer).getByRole('table', { name: 'Leases of lighthouse-geth-2' })).toHaveTextContent('verifying');
    expect(within(drawer).getByRole('table', { name: 'History of lighthouse-geth-2' })).toHaveTextContent(
      'lease.granted'
    );
  });
});

describe('HostDrawer keys', () => {
  it('closes on Escape, but not while a dialog is open', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    const onClose = vi.fn();
    renderWithApp(<Panel name="rpc-1" onClose={onClose} />, h);
    const drawer = await screen.findByRole('complementary', { name: 'Host rpc-1' });

    await user.click(await within(drawer).findByRole('button', { name: 'Hold…' }));
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    expect(onClose).not.toHaveBeenCalled();

    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledOnce();
  });
});
