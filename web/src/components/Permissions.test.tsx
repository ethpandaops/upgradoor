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
  await within(table).findByRole('columnheader', { name: /Attesting/ });
  await user.click(within(table).getByRole('button', { name: `Open ${host}` }));

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
  });

  it('are offered to a team member on a host of the team, except what needs admin or reboot', async () => {
    const { button, actions } = await openHost('team', 'rpc-1');

    // Ownership arrives with the host list.
    await waitFor(() => {
      expect(button('Roll…')).toBeEnabled();
    });
    // A team feature never reboots a host: the action is not offered.
    expect(within(actions).queryByRole('button', { name: 'Reboot…' })).not.toBeInTheDocument();
    expect(button('Hold…')).toBeEnabled();

    expect(button('Forget…')).toBeDisabled();
    expect(button('Forget…')).toHaveAttribute('title', 'Needs upgradoor:admin');

    expect(screen.getByLabelText('Facts')).toHaveTextContent('geth, lighthouse');
  });

  it('are disabled, and say why, on a host of another team', async () => {
    const { button } = await openHost('team', 'tooling-1');

    await waitFor(() => {
      expect(button('Hold…')).toHaveAttribute(
        'title',
        'upgradoor:team_manage does not cover host tooling-1 (owners: none)'
      );
    });

    for (const name of ['Roll…', 'Hold…', 'Forget…']) expect(button(name)).toBeDisabled();
  });

  it('are disabled for a reader, who still sees everything', async () => {
    const { actions, button, table, user } = await openHost('reader', 'rpc-1');

    expect(within(actions).queryByRole('button', { name: 'Reboot…' })).not.toBeInTheDocument();
    expect(button('Roll…')).toBeDisabled();
    expect(button('Hold…')).toBeDisabled();
    expect(button('Forget…')).toBeDisabled();

    // The clients of the list, each named in its column.
    const row = within(table).getByRole('button', { name: 'Open lighthouse-geth-1' }).closest('tr') as HTMLElement;
    expect(within(row).getByText('geth')).toBeInTheDocument();
    expect(within(row).getByText('lighthouse')).toBeInTheDocument();
    await user.click(screen.getByRole('tab', { name: 'Containers' }));
    expect(screen.getByRole('list', { name: 'Containers' })).toHaveTextContent('lighthouse');
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
    await within(table).findByRole('columnheader', { name: /Attesting/ });
    await user.click(within(table).getByRole('button', { name: 'Open rpc-1' }));

    const actions = await screen.findByRole('group', { name: 'Actions for rpc-1' });
    for (const name of ['Roll…', 'Hold…', 'Forget…']) {
      expect(within(actions).getByRole('button', { name })).toBeDisabled();
    }
  });
});

describe('the holds of a host', () => {
  it("are listed one per holder, and a team member lifts the team's own", async () => {
    const { button, user, h } = await openHost('team', 'rpc-1');

    await waitFor(() => {
      expect(button('Hold…')).toBeEnabled();
    });
    await user.click(button('Hold…'));
    await user.click(within(screen.getByRole('dialog')).getByRole('button', PRIMARY));

    const drawer = screen.getByRole('complementary', { name: 'Host rpc-1' });
    const lift = await within(drawer).findByRole('button', { name: 'Lift…' });
    expect(within(drawer).getByText('lighthouse', { selector: 'span' })).toBeInTheDocument();

    await waitFor(() => {
      expect(lift).toBeEnabled();
    });
    await user.click(lift);
    await user.click(within(screen.getByRole('dialog')).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(h.userCalls().find(c => c.path === '/api/v1/actions/unhold')?.body).toMatchObject({
        hosts: ['rpc-1'],
        holders: ['lighthouse'],
      });
    });
  });
});

describe('the roll dialog of a team member', () => {
  it('starts with the containers of the team and greys out the others', async () => {
    const { button, user, h } = await openHost('team', 'rpc-1');

    await waitFor(() => {
      expect(button('Roll…')).toBeEnabled();
    });
    await user.click(button('Roll…'));

    const dialog = await screen.findByRole('dialog');
    const lighthouse = within(dialog).getByRole('checkbox', { name: 'lighthouse' });
    const geth = within(dialog).getByRole('checkbox', { name: /^geth/ });
    const confirm = within(dialog).getByRole('button', PRIMARY);

    expect(lighthouse).toBeChecked();
    expect(geth).not.toBeChecked();
    // Another team's container is greyed out with its owner.
    expect(geth).toBeDisabled();
    expect(geth.closest('label')).toHaveTextContent('team geth');
    expect(within(dialog).getByRole('group', { name: /Containers/ })).toHaveTextContent('(tick the ones to roll)');
    expect(confirm).toBeEnabled();
    expect(within(dialog).queryByRole('note')).toBeNull();

    // Force and "ignore budget" are the team's for its own containers.
    expect(within(dialog).getByRole('checkbox', { name: /Ignore budget/ })).toBeEnabled();

    // Nothing ticked would roll every container: it is not sent, and the dialog says what to do.
    await user.click(lighthouse);
    expect(confirm).toBeDisabled();
    expect(within(dialog).getByRole('note')).toHaveTextContent(
      'Not permitted: tick at least one container your teams own'
    );

    await user.click(lighthouse);
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
      'Action failed: forbidden: Needs upgradoor:manage or upgradoor:team_manage for a team that owns this host (HTTP 403)'
    );
    expect(h.auth.getState().loggedIn).toBe(true);
  });
});

describe('rollouts and settings', () => {
  it('are read only for a team member, apart from what the team owns', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'team' });
    renderWithApp(<RolloutsTab />, h);

    const pause = await screen.findByRole('button', { name: 'Pause all…' });
    expect(pause).toBeDisabled();
    expect(pause).toHaveAttribute('title', 'Needs upgradoor:admin');

    // A team aborts its own rollouts only.
    for (const abort of await screen.findAllByRole('button', { name: 'Abort…' })) {
      if (abort.hasAttribute('disabled')) {
        expect(abort.getAttribute('title')).toMatch(/^upgradoor:team_manage does not cover rollout /);
      }
    }
  });

  it('offers the rollout actions to a staff login', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<RolloutsTab />, h);

    expect(await screen.findByRole('button', { name: 'Pause all…' })).toBeEnabled();
    for (const abort of await screen.findAllByRole('button', { name: 'Abort…' })) expect(abort).toBeEnabled();
  });

  it('does not show the settings to a reader', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'reader' });
    renderWithApp(<SettingsTab />, h);

    // The menu does not offer the tab to a reader; the page itself, opened
    // some other way, is refused the settings.
    expect(await screen.findByText(/lacks the feature upgradoor:admin/)).toBeInTheDocument();
    expect(screen.queryByRole('form', { name: 'Settings' })).not.toBeInTheDocument();
    expect(h.userCalls().some(c => c.method === 'PUT')).toBe(false);
  });
});

describe('when a roll runs', () => {
  it('offers "now" to a team member, and the roll is sent as asked', async () => {
    const { button, user, h } = await openHost('team', 'rpc-1');

    await waitFor(() => {
      expect(button('Roll…')).toBeEnabled();
    });
    await user.click(button('Roll…'));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeEnabled();
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeChecked();

    await user.click(within(dialog).getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(screen.queryByRole('dialog')).toBeNull();
    });

    expect(h.userCalls().find(c => c.path === '/api/v1/actions/roll')?.body).toMatchObject({ staggered: false });
  });

  it('offers "now" to staff', async () => {
    const { button, user } = await openHost('staff', 'rpc-1');
    await user.click(button('Roll…'));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeEnabled();
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeChecked();
  });
});
