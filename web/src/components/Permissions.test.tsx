// What the views offer to a staff login, a team member, a reader and a login
// whose auth client cannot say what it may do; and what happens when the
// controller refuses anyway.

import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { MOCK_LOGINS, type MockProfile } from '../mock/mockController';
import { HostsTab } from '../tabs/HostsTab';
import { RolloutsTab } from '../tabs/RolloutsTab';
import { SettingsTab } from '../tabs/SettingsTab';
import { createHarness, renderWithApp, PRIMARY } from '../test/harness';

/** openHost renders the Hosts tab as the given login and opens one host. */
async function openHost(profile: MockProfile, host: string) {
  const user = userEvent.setup();
  const h = await createHarness({ mode: 'auth', loggedIn: true, profile });
  renderWithApp(<HostsTab />, h);

  const table = await screen.findByRole('table', { name: 'Hosts' });
  await within(table).findByRole('columnheader', { name: /Kernel/ });
  await user.click(within(table).getByRole('button', { name: `Expand ${host}` }));

  const actions = await screen.findByRole('group', { name: `Actions for ${host}` });
  const button = (name: string) => within(actions).getByRole('button', { name });

  return { h, user, table, actions, button };
}

describe('the host actions', () => {
  it('are all offered to a staff login', async () => {
    const { button } = await openHost('staff', 'rpc-1');

    for (const name of ['Roll…', 'Reboot…', 'Hold…', 'Forget…']) {
      expect(button(name)).toBeEnabled();
      expect(button(name)).not.toHaveAttribute('title');
    }

    const bulk = screen.getByRole('group', { name: 'Fleet actions' });
    expect(within(bulk).getByRole('button', { name: 'Roll all hosts…' })).toBeEnabled();
  });

  it('are offered to a team member on a host of the team, except what needs admin', async () => {
    const { button, actions } = await openHost('team', 'rpc-1');

    // Ownership arrives with the host list.
    await waitFor(() => {
      expect(button('Reboot…')).toBeEnabled();
    });
    expect(button('Roll…')).toBeEnabled();
    expect(button('Hold…')).toBeEnabled();

    expect(button('Forget…')).toBeDisabled();
    expect(button('Forget…')).toHaveAttribute('title', 'Needs upgradoor:admin');

    expect(actions).toHaveTextContent('owners: geth, lighthouse');

    const bulk = screen.getByRole('group', { name: 'Fleet actions' });
    expect(within(bulk).getByRole('button', { name: 'Roll all hosts…' })).toBeDisabled();
    expect(within(bulk).getByRole('button', { name: 'Reboot all hosts…' })).toBeDisabled();
  });

  it('are disabled, and say why, on a host of another team', async () => {
    const { button } = await openHost('team', 'tooling-1');

    await waitFor(() => {
      expect(button('Hold…')).toHaveAttribute(
        'title',
        'upgradoor:team_hold does not cover host tooling-1 (owners: none)'
      );
    });

    for (const name of ['Roll…', 'Reboot…', 'Hold…', 'Forget…']) expect(button(name)).toBeDisabled();
  });

  it('are disabled for a reader, who still sees everything', async () => {
    const { button, table } = await openHost('reader', 'rpc-1');

    expect(button('Reboot…')).toBeDisabled();
    expect(button('Reboot…')).toHaveAttribute(
      'title',
      'Needs upgradoor:reboot or upgradoor:team_reboot for a team that owns this host'
    );
    expect(button('Roll…')).toBeDisabled();
    expect(button('Hold…')).toBeDisabled();
    expect(button('Forget…')).toBeDisabled();

    // The owners chip of the list.
    expect(within(table).getAllByTitle('Owners: the teams that own a container of this host')[0]).toHaveTextContent(
      /\w+ · \w+/
    );
    expect(await screen.findByRole('table', { name: 'Containers' })).toHaveTextContent('Team');
  });

  it('are disabled when the auth client cannot say what the login may do', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    // A client from before features and teams.
    if (h.client) {
      h.client.hasFeature = undefined;
      h.client.getTeams = undefined;
    }
    renderWithApp(<HostsTab />, h);

    const table = await screen.findByRole('table', { name: 'Hosts' });
    await within(table).findByRole('columnheader', { name: /Kernel/ });
    await user.click(within(table).getByRole('button', { name: 'Expand rpc-1' }));

    const actions = await screen.findByRole('group', { name: 'Actions for rpc-1' });
    for (const name of ['Roll…', 'Reboot…', 'Hold…', 'Forget…']) {
      expect(within(actions).getByRole('button', { name })).toBeDisabled();
    }
  });
});

describe('the roll dialog of a team member', () => {
  it('starts with the containers of the team and refuses the others before sending', async () => {
    const { button, user, h } = await openHost('team', 'rpc-1');

    await waitFor(() => {
      expect(button('Roll…')).toBeEnabled();
    });
    await user.click(button('Roll…'));

    const dialog = await screen.findByRole('dialog');
    const lighthouse = within(dialog).getByRole('checkbox', { name: 'lighthouse' });
    const geth = within(dialog).getByRole('checkbox', { name: 'geth' });
    const confirm = within(dialog).getByRole('button', PRIMARY);

    expect(lighthouse).toBeChecked();
    expect(geth).not.toBeChecked();
    expect(confirm).toBeEnabled();
    expect(within(dialog).queryByRole('note')).toBeNull();

    // Force and "ignore budget" need admin.
    expect(within(dialog).getByRole('checkbox', { name: /Force/ })).toBeDisabled();
    expect(within(dialog).getByRole('checkbox', { name: /Ignore budget/ })).toBeDisabled();

    await user.click(geth);
    expect(confirm).toBeDisabled();
    expect(within(dialog).getByRole('note')).toHaveTextContent(
      'Not permitted: upgradoor:team_roll does not cover container geth on host rpc-1 (owners: geth)'
    );

    await user.click(geth);
    await user.click(confirm);

    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });

    const sent = h.userCalls().find(c => c.path === '/api/v1/actions/roll');
    expect(sent?.body).toMatchObject({ hosts: ['rpc-1'], containers: ['lighthouse'], force: false });
    expect(sent?.authorization).toBe(`Bearer ${MOCK_LOGINS.team.token}`);
  });
});

describe('a refusal by the controller', () => {
  it('is shown where an action error is shown', async () => {
    const { button, user, h } = await openHost('staff', 'rpc-1');

    // The page believes the login is staff; the token it sends is a reader's.
    h.auth.getToken = () => Promise.resolve(MOCK_LOGINS.reader.token);

    await user.click(button('Hold…'));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', PRIMARY));

    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'Action failed: forbidden: Needs upgradoor:hold or upgradoor:team_hold for a team that owns this host (HTTP 403)'
    );
    expect(h.auth.getState().loggedIn).toBe(true);
  });
});

describe('rollouts and settings', () => {
  it('are read only for a team member, apart from the hosts of the team', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'team' });
    renderWithApp(<RolloutsTab />, h);

    const pause = await screen.findByRole('button', { name: 'Pause all…' });
    expect(pause).toBeDisabled();
    expect(pause).toHaveAttribute('title', 'Needs upgradoor:admin');

    for (const abort of await screen.findAllByRole('button', { name: 'Abort…' })) {
      expect(abort).toBeDisabled();
      expect(abort).toHaveAttribute('title', 'Needs upgradoor:supervise');
    }
  });

  it('offers the rollout actions to a staff login', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<RolloutsTab />, h);

    expect(await screen.findByRole('button', { name: 'Pause all…' })).toBeEnabled();
    for (const abort of await screen.findAllByRole('button', { name: 'Abort…' })) expect(abort).toBeEnabled();
  });

  it('shows the settings to a reader read only', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'reader' });
    renderWithApp(<SettingsTab />, h);

    // The menu does not offer the tab to a reader; the page itself, opened
    // some other way, offers nothing to change either.
    const form = await screen.findByRole('form', { name: 'Settings' });
    expect(within(form).getByLabelText('Ready epochs')).toBeDisabled();
    expect(within(form).getByRole('switch', { name: 'Global pause' })).toBeDisabled();

    const save = screen.getByRole('button', { name: 'Save' });
    expect(save).toBeDisabled();
    expect(save).toHaveAttribute('title', 'Needs upgradoor:admin');
    expect(screen.getByRole('status')).toHaveTextContent('Read only: Needs upgradoor:admin.');
    expect(h.userCalls().some(c => c.method === 'PUT')).toBe(false);
  });
});

describe('when a roll runs', () => {
  it('does not offer "now" to a team member, and the roll is sent staggered', async () => {
    const { button, user, h } = await openHost('team', 'rpc-1');

    await waitFor(() => {
      expect(button('Roll…')).toBeEnabled();
    });
    await user.click(button('Roll…'));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeDisabled();
    expect(within(dialog).getByRole('radio', { name: /Staggered/ })).toBeChecked();

    await user.click(within(dialog).getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });

    expect(h.userCalls().find(c => c.path === '/api/v1/actions/roll')?.body).toMatchObject({ staggered: true });
  });

  it('offers "now" to staff', async () => {
    const { button, user } = await openHost('staff', 'rpc-1');
    await user.click(button('Roll…'));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeEnabled();
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeChecked();
  });
});
