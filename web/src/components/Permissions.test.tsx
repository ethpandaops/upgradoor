// What the views offer to a staff login, a team member, a reader and a login
// whose auth client cannot say what it may do; and what happens when the
// controller refuses anyway.

import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { AuditEntry } from '../api/types';
import { MOCK_LOGINS, MockController, type MockProfile } from '../mock/mockController';
import { HostsTab } from '../tabs/HostsTab';
import { RolloutsTab } from '../tabs/RolloutsTab';
import { SettingsTab } from '../tabs/SettingsTab';
import { createHarness, renderWithApp } from '../test/harness';

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

    const bulk = screen.getByRole('group', { name: 'Bulk actions' });
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

    const bulk = screen.getByRole('group', { name: 'Bulk actions' });
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
    expect(await screen.findByRole('table', { name: 'Containers' })).toHaveTextContent('Owners');
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
    const confirm = within(dialog).getByRole('button', { name: 'Confirm' });

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
    await user.click(within(dialog).getByRole('button', { name: 'Confirm' }));

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

  it('shows the settings to a reader and does not let them be saved', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true, profile: 'reader' });
    renderWithApp(<SettingsTab />, h);

    const form = await screen.findByRole('form', { name: 'Settings' });
    const input = within(form).getByLabelText('Ready epochs');
    await user.clear(input);
    await user.type(input, '2');

    const save = screen.getByRole('button', { name: 'Save' });
    expect(save).toBeDisabled();
    expect(save).toHaveAttribute('title', 'Needs upgradoor:admin');
    expect(screen.getByRole('status')).toHaveTextContent('Read only: Needs upgradoor:admin.');

    await user.type(input, '{Enter}');
    expect(h.userCalls().some(c => c.method === 'PUT')).toBe(false);
  });
});

describe('the mock controller', () => {
  const send = (mock: MockController, profile: MockProfile, method: string, path: string, body?: unknown) =>
    mock.fetch(`/api/v1/${path}`, {
      method,
      headers: { Authorization: `Bearer ${MOCK_LOGINS[profile].token}` },
      body: body === undefined ? undefined : JSON.stringify(body),
    });

  it('applies the rule of the controller and records what allowed an action', async () => {
    const mock = new MockController({ auth: true });
    const roll = (containers: string[]) => ({
      hosts: ['rpc-1'],
      all: false,
      containers,
      staggered: false,
      force: false,
      ignoreBudget: false,
      reason: '',
    });

    expect((await send(mock, 'reader', 'GET', 'hosts')).status).toBe(200);
    expect((await send(mock, 'reader', 'POST', 'actions/hold', { hosts: ['rpc-1'], reason: '' })).status).toBe(403);
    expect((await send(mock, 'team', 'POST', 'actions/pause', { rollout: '', reason: '' })).status).toBe(403);

    const refused = await send(mock, 'team', 'POST', 'actions/roll', roll(['geth']));
    expect(refused.status).toBe(403);
    expect(await refused.json()).toEqual({
      error: 'forbidden: upgradoor:team_roll does not cover container geth on host rpc-1 (owners: geth)',
    });

    expect((await send(mock, 'team', 'POST', 'actions/hold', { hosts: ['rpc-1'], reason: 'mine' })).status).toBe(200);

    const audit = (await (await send(mock, 'reader', 'GET', 'audit?action=action.hold')).json()) as {
      entries: AuditEntry[];
    };
    expect(audit.entries[0]?.detail).toMatchObject({
      authorizedBy: 'team_hold',
      teams: ['lighthouse'],
      reason: 'mine',
    });
    expect(audit.entries.filter(e => e.target === 'rpc-1')).toHaveLength(1);

    // The configuration names the service the features belong to.
    expect(await (await mock.fetch('/api/v1/config')).json()).toMatchObject({ authService: 'upgradoor' });
  });
});

describe('what the review of the first version found, in the page and the mock', () => {
  const send = (mock: MockController, profile: MockProfile, path: string, body: unknown) =>
    mock.fetch(`/api/v1/${path}`, {
      method: 'POST',
      headers: { Authorization: `Bearer ${MOCK_LOGINS[profile].token}` },
      body: JSON.stringify(body),
    });
  const error = async (res: Response) => ((await res.json()) as { error: string }).error;

  it('does not offer "now" to a team member, and the roll is sent staggered', async () => {
    const { button, user, h } = await openHost('team', 'rpc-1');

    await waitFor(() => {
      expect(button('Roll…')).toBeEnabled();
    });
    await user.click(button('Roll…'));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('radio', { name: /Now/ })).toBeDisabled();
    expect(within(dialog).getByRole('radio', { name: /Staggered/ })).toBeChecked();

    await user.click(within(dialog).getByRole('button', { name: 'Confirm' }));
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

  it('runs a team roll staggered in the mock, whatever is asked', async () => {
    const mock = new MockController({ auth: true });
    const body = {
      hosts: ['rpc-1'],
      all: false,
      containers: ['lighthouse'],
      staggered: false,
      force: false,
      ignoreBudget: false,
      reason: '',
    };

    const res = await send(mock, 'team', 'actions/roll', body);
    expect(res.status).toBe(200);

    const id = ((await res.json()) as { rollout: string }).rollout;
    expect(mock.data.rollouts.find(r => r.id === id)?.force).toBe(false);

    const staff = await send(mock, 'staff', 'actions/roll', body);
    const staffId = ((await staff.json()) as { rollout: string }).rollout;
    expect(mock.data.rollouts.find(r => r.id === staffId)?.force).toBe(true);
  });

  it('lets a team lift its own hold and not a hold of staff', async () => {
    const mock = new MockController({ auth: true });
    const hold = { hosts: ['rpc-1'], reason: 'mine' };
    const host = () => mock.data.hosts.get('rpc-1');

    expect((await send(mock, 'team', 'actions/hold', hold)).status).toBe(200);
    expect(host()?.holdBy).toBe('team');
    expect((await send(mock, 'team', 'actions/unhold', hold)).status).toBe(200);
    expect(host()?.holdWanted).toBe(false);

    expect((await send(mock, 'staff', 'actions/hold', hold)).status).toBe(200);
    expect(host()?.holdBy).toBe('staff');

    const refused = await send(mock, 'team', 'actions/unhold', hold);
    expect(refused.status).toBe(403);
    expect(await error(refused)).toBe(
      'forbidden: upgradoor:team_hold does not lift the hold on host rpc-1 (set by staff)'
    );
    expect((await send(mock, 'team', 'actions/hold', hold)).status).toBe(403);
    expect(host()?.holdWanted).toBe(true);
    expect(host()?.holdBy).toBe('staff');

    expect((await send(mock, 'staff', 'actions/unhold', hold)).status).toBe(200);
  });

  it('retries hosts, not rollouts, with the recovery feature', async () => {
    const mock = new MockController({ auth: true });
    const aborted = mock.data.rollouts.find(r => r.state === 'aborted');
    const halted = mock.data.rollouts.find(r => r.state === 'halted');
    if (!aborted || !halted) throw new Error('the fixture has no aborted or halted rollout');

    // An aborted rollout: reopening its cause needs supervise, and takes no hosts.
    const refused = await send(mock, 'team', 'actions/retry', { rollout: aborted.id, hosts: ['rpc-1'], reason: '' });
    expect(refused.status).toBe(403);
    expect(await error(refused)).toBe('forbidden: Needs upgradoor:supervise');

    const half = await send(mock, 'staff', 'actions/retry', { rollout: aborted.id, hosts: ['rpc-1'], reason: '' });
    expect(half.status).toBe(409);
    expect(aborted.state).toBe('aborted');

    // A halted rollout: a host that did not fail lifts no halt.
    const bystander = (halted.hosts ?? []).find(m => m.state !== 'failed');
    if (!bystander) throw new Error('the halted rollout has only failed members');

    const nothing = await send(mock, 'staff', 'actions/retry', {
      rollout: halted.id,
      hosts: [bystander.host],
      reason: '',
    });
    expect(nothing.status).toBe(409);
    expect(await error(nothing)).toContain('no host could be retried');
    expect(halted.state).toBe('halted');

    // A second host failed in it (a lease that was open when the rollout halted).
    bystander.state = 'failed';

    // The failed hosts, one by one: the halt is lifted with the last.
    const failed = (halted.hosts ?? []).filter(m => m.state === 'failed').map(m => m.host);
    expect(failed.length).toBeGreaterThan(1);

    for (const [i, name] of failed.entries()) {
      const res = await send(mock, 'staff', 'actions/retry', { rollout: halted.id, hosts: [name], reason: '' });
      expect(res.status).toBe(200);
      expect(halted.state).toBe(i === failed.length - 1 ? 'active' : 'halted');
    }
  });

  it('needs admin to save the settings', async () => {
    const mock = new MockController({ auth: true });
    const updates = () => mock.data.audit.filter(e => e.action === 'settings.update').length;
    const before = updates();
    const loaded = await mock.fetch('/api/v1/settings', {
      headers: { Authorization: `Bearer ${MOCK_LOGINS.reader.token}` },
    });
    const { settings } = (await loaded.json()) as { settings: unknown };

    const put = (profile: MockProfile) =>
      mock.fetch('/api/v1/settings', {
        method: 'PUT',
        headers: { Authorization: `Bearer ${MOCK_LOGINS[profile].token}` },
        body: JSON.stringify(settings),
      });

    expect((await put('team')).status).toBe(403);
    expect((await put('reader')).status).toBe(403);
    expect((await put('staff')).status).toBe(200);

    // The settings as they are: nothing changes and nothing is audited.
    expect(updates()).toBe(before);
  });

  it('does not audit an unhold of a host that is not held', async () => {
    const mock = new MockController({ auth: true });
    const before = mock.data.audit.length;

    const res = await send(mock, 'staff', 'actions/unhold', { hosts: ['rpc-1'], reason: '' });
    expect(res.status).toBe(200);
    expect(((await res.json()) as { hosts: string[] }).hosts).toEqual([]);
    expect(mock.data.audit).toHaveLength(before);
  });

  it('keeps a halt whose failed host was skipped when a waiting host is skipped and retried', async () => {
    const mock = new MockController({ auth: true });
    const halted = mock.data.rollouts.find(r => r.state === 'halted');
    if (!halted) throw new Error('the fixture has no halted rollout');

    const members = halted.hosts ?? [];
    const waiting = members.find(m => m.state === 'pending' || m.state === 'expected');
    if (!waiting) throw new Error('the halted rollout has no waiting member');

    // The failed hosts are skipped, as their team would: nothing that could
    // be retried now has failed.
    for (const m of members) if (m.state === 'failed') m.state = 'skipped';

    const res = await send(mock, 'staff', 'actions/retry', { rollout: halted.id, hosts: [waiting.host], reason: '' });
    expect(res.status).toBe(409);
    expect(halted.state).toBe('halted');

    // The rollout as a whole: lifted.
    expect((await send(mock, 'staff', 'actions/retry', { rollout: halted.id, hosts: [], reason: '' })).status).toBe(
      200
    );
    expect(halted.state).toBe('active');
  });
});
