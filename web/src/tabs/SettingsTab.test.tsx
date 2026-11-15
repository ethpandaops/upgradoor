import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createHarness, renderWithApp } from '../test/harness';
import { SettingsTab } from './SettingsTab';

async function openSettings() {
  const h = await createHarness({ mode: 'auth', loggedIn: true });
  renderWithApp(<SettingsTab />, h);
  const form = await screen.findByRole('form', { name: 'Settings' });
  return { h, form, user: userEvent.setup() };
}

function rowOf(form: HTMLElement, label: string): HTMLElement {
  const row = within(form).getByLabelText(label).closest('tr');
  if (!row) throw new Error(`no row for ${label}`);
  return row;
}

describe('SettingsTab', () => {
  it('shows the stored settings with their defaults', async () => {
    const { form, h } = await openSettings();

    expect(within(form).getByLabelText('Max unavailable')).toHaveValue('10');
    expect(within(form).getByLabelText('Min participation')).toHaveValue('90');
    expect(within(form).getByLabelText('Max finality lag')).toHaveValue('3');
    expect(within(form).getByLabelText('Ready epochs')).toHaveValue('1');
    expect(within(form).getByLabelText('Min peers')).toHaveValue('3');
    expect(within(form).getByLabelText('Lease TTL')).toHaveValue('30m');
    expect(within(form).getByLabelText('Host stale after')).toHaveValue('2m');
    expect(within(form).getByLabelText('Readiness timeout')).toHaveValue('30m');
    expect(within(form).getByLabelText('Policy: images')).toHaveValue('auto');
    expect(within(form).getByLabelText('Policy: reboots')).toHaveValue('manual');
    // Off by default; the fixture has it on at a quarter.
    expect(within(form).getByLabelText('Ready peer share')).toHaveValue('0.25');
    expect(rowOf(form, 'Ready peer share')).toHaveTextContent('use default');
    // One switch per kind; the fixture has the one for images on.
    expect(within(form).getByLabelText('Pause after canary: images')).toBeChecked();
    expect(within(form).getByLabelText('Pause after canary: packages')).not.toBeChecked();
    expect(within(form).getByLabelText('Pause after canary: reboots')).not.toBeChecked();
    expect(rowOf(form, 'Pause after canary: images')).toHaveTextContent('off');
    expect(within(form).getByLabelText('Global pause')).not.toBeChecked();

    // The Default column.
    expect(rowOf(form, 'Lease TTL')).toHaveTextContent('30m');
    expect(rowOf(form, 'Policy: reboots')).toHaveTextContent('manual');
    expect(rowOf(form, 'Global pause')).toHaveTextContent('off');

    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(screen.getByRole('status')).toHaveTextContent('No unsaved changes.');
    expect(h.calls.filter(c => c.method === 'PUT')).toEqual([]);
  });

  it('round-trips an edit: dirty state, PUT body, stored answer', async () => {
    const { form, h, user } = await openSettings();

    const maxUnavailable = within(form).getByLabelText('Max unavailable');
    await user.clear(maxUnavailable);
    await user.type(maxUnavailable, '12.5');
    const ttl = within(form).getByLabelText('Lease TTL');
    await user.clear(ttl);
    await user.type(ttl, '45m');
    await user.selectOptions(within(form).getByLabelText('Policy: packages'), 'manual');
    await user.click(within(form).getByLabelText('Global pause'));

    expect(within(rowOf(form, 'Max unavailable')).getByText('changed')).toBeInTheDocument();
    expect(within(rowOf(form, 'Lease TTL')).getByText('changed')).toBeInTheDocument();
    expect(within(rowOf(form, 'Ready epochs')).queryByText('changed')).not.toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('4 unsaved changes');

    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(h.calls.filter(c => c.method === 'PUT')).toHaveLength(1);
    });
    const put = h.calls.find(c => c.method === 'PUT');
    expect(put?.path).toBe('/api/v1/settings');
    expect(put?.authorization).toBe('Bearer mock-token');
    expect(put?.body).toStrictEqual({
      maxUnavailablePercent: 12.5,
      minParticipationPercent: 90,
      maxFinalityLagEpochs: 3,
      readyEpochs: 1,
      minPeers: 3,
      leaseTtl: '45m',
      hostStaleAfter: '2m',
      readinessTimeout: '30m',
      policyImage: 'auto',
      policyPackages: 'manual',
      policyReboot: 'manual',
      pauseAfterCanaryImage: true,
      pauseAfterCanaryPackages: false,
      pauseAfterCanaryReboot: false,
      readyPeerShare: 0.25,
      paused: true,
    });

    // The form now shows the stored answer and is clean again.
    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent('No unsaved changes.');
    });
    const saved = screen.getByRole('form', { name: 'Settings' });
    expect(within(saved).getByLabelText('Max unavailable')).toHaveValue('12.5');
    expect(within(saved).getByLabelText('Lease TTL')).toHaveValue('45m');
    expect(within(saved).getByLabelText('Global pause')).toBeChecked();
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();

    // The controller stored it and audited the change.
    expect((await h.api.settings()).settings).toMatchObject({
      maxUnavailablePercent: 12.5,
      leaseTtl: '45m',
      paused: true,
    });
    const audit = await h.api.audit({ action: 'settings.' });
    expect(audit.entries?.[0]?.detail).toMatchObject({
      before: { maxUnavailablePercent: 10 },
      after: { maxUnavailablePercent: 12.5 },
    });

    // A value that differs from its default can be reset to it.
    await user.click(within(saved).getByRole('button', { name: 'Reset Max unavailable to its default' }));
    expect(within(saved).getByLabelText('Max unavailable')).toHaveValue('10');
    expect(screen.getByRole('status')).toHaveTextContent('1 unsaved change.');
  });

  it('validates fields and blocks saving', async () => {
    const { form, h, user } = await openSettings();

    const cases: [string, string, string][] = [
      ['Max unavailable', '150', 'must be between 0 and 100'],
      ['Min participation', 'abc', 'must be a number between 0 and 100'],
      ['Max finality lag', '1', 'must be at least 2'],
      ['Ready epochs', '1.5', 'must be a whole number'],
      ['Min peers', '0', 'must be at least 1'],
      ['Ready peer share', '1.5', 'must be between 0 and 1'],
      ['Lease TTL', '30', 'must be a duration like 30m, 90s or 1h30m'],
      ['Host stale after', '10s', 'must be at least 30s'],
      ['Readiness timeout', '30s', 'must be at least 1m'],
    ];

    for (const [label, value, message] of cases) {
      const input = within(form).getByLabelText(label);
      await user.clear(input);
      await user.type(input, value);
      expect(within(rowOf(form, label)).getByRole('alert')).toHaveTextContent(message);
      expect(input).toHaveAttribute('aria-invalid', 'true');
    }

    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(screen.getByRole('status')).toHaveTextContent('Fix the marked fields to save.');

    await user.click(screen.getByRole('button', { name: 'Discard changes' }));
    expect(within(form).getByLabelText('Max unavailable')).toHaveValue('10');
    expect(within(form).queryByRole('alert')).not.toBeInTheDocument();
    expect(h.calls.filter(c => c.method === 'PUT')).toEqual([]);
  });

  it('treats an equivalent duration as unchanged', async () => {
    const { form, user } = await openSettings();

    const ttl = within(form).getByLabelText('Lease TTL');
    await user.clear(ttl);
    await user.type(ttl, '1800s');

    expect(screen.getByRole('status')).toHaveTextContent('No unsaved changes.');
  });

  it('shows a failed save and keeps the edit', async () => {
    const { form, h, user } = await openSettings();

    const input = within(form).getByLabelText('Ready epochs');
    await user.clear(input);
    await user.type(input, '2');

    // The session ends before the save: the controller answers 401.
    await h.client?.logout();
    h.auth.getToken = () => Promise.resolve('stale-token');

    await user.click(screen.getByRole('button', { name: 'Save' }));

    const alerts = await screen.findAllByRole('alert');
    expect(alerts.some(a => /Could not save the settings: .*HTTP 401/.test(a.textContent))).toBe(true);
    expect(within(form).getByLabelText('Ready epochs')).toHaveValue('2');
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
  });
});
