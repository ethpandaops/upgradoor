import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createHarness, renderWithApp } from '../test/harness';
import { SettingsTab } from './SettingsTab';

describe('ActionPolicies', () => {
  it('lists the actions the hosts report with their policy, and saves a change with the others', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    renderWithApp(<SettingsTab />, h);

    const group = await screen.findByRole('group', { name: 'Policy: host actions' });
    const pressed = (action: string) =>
      within(within(group).getByRole('group', { name: `Policy of the action ${action}` })).getByRole('button', {
        pressed: true,
      });
    await vi.waitFor(() => {
      expect(
        within(group)
          .getAllByRole('listitem')
          .map(li => li.textContent)
      ).toEqual(['docker-pruneautomanual', 'prune-beacon-dbautomanual', 'security-upgradesautomanual']);
    });
    expect(pressed('security-upgrades')).toHaveTextContent('manual');
    expect(group).toHaveTextContent('every action manual');

    await user.click(
      within(within(group).getByRole('group', { name: 'Policy of the action docker-prune' })).getByRole('button', {
        name: 'auto',
      })
    );
    expect(pressed('docker-prune')).toHaveTextContent('auto');
    expect(screen.getByRole('list', { name: 'Changes to save' })).toHaveTextContent(
      'Policy: host actions every action manual → docker-prune auto'
    );

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await vi.waitFor(() => {
      expect(h.calls.find(c => c.method === 'PUT')).toBeDefined();
    });
    expect(h.calls.find(c => c.method === 'PUT')?.body).toStrictEqual({
      actionPolicies: { 'docker-prune': 'auto', 'security-upgrades': 'manual' },
    });
    expect(h.controller.data.settings.actionPolicies).toStrictEqual({
      'docker-prune': 'auto',
      'security-upgrades': 'manual',
    });
  });
});
