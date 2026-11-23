import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { ApiError } from '../api/client';
import { list } from '../api/types';
import { hasUnsavedWork } from '../hooks/useUnsavedWork';
import { createHarness, renderWithApp } from '../test/harness';
import { SettingsTab } from './SettingsTab';

async function openSettings() {
  const h = await createHarness({ mode: 'auth', loggedIn: true });
  renderWithApp(<SettingsTab />, h);
  const form = await screen.findByRole('form', { name: 'Settings' });
  return { h, form, user: userEvent.setup() };
}

/** rowOf is the line of a setting: its control and the note below it. */
function rowOf(form: HTMLElement, label: string): HTMLElement {
  const row = within(form).getByLabelText(label).closest('div.border-b, div.border');
  if (!(row instanceof HTMLElement)) throw new Error(`no row for ${label}`);
  return row;
}

const group = (name: string) => screen.getByRole('region', { name });
const policy = (form: HTMLElement, label: string) =>
  within(within(form).getByRole('group', { name: label })).getByRole('button', { pressed: true });

describe('SettingsTab', () => {
  it('shows the stored settings in groups, with the unit at the control', async () => {
    const { form, h } = await openSettings();

    expect(within(group('Budget and network gate')).getByLabelText('Max unavailable')).toHaveValue(10);
    expect(group('Budget and network gate')).toHaveTextContent('%');
    expect(within(group('Budget and network gate')).getByLabelText('Min participation')).toHaveValue(90);
    expect(within(group('Budget and network gate')).getByLabelText('Max finality lag')).toHaveValue(3);
    expect(rowOf(form, 'Max finality lag')).toHaveTextContent('epochs');

    const readiness = group('Readiness after an operation');
    expect(within(readiness).getByLabelText('Ready attestations')).toHaveValue(2);
    expect(rowOf(form, 'Ready attestations')).toHaveTextContent('slots');
    expect(within(readiness).getByLabelText('Ready stable for')).toHaveValue('1m');
    expect(within(readiness).getByLabelText('Min peers')).toHaveValue(3);
    expect(within(group('Timeouts')).getByLabelText('Readiness timeout')).toHaveValue('30m');
    // Off by default; the fixture has it on at a quarter, which the line says.
    expect(within(readiness).getByLabelText('Ready peer share')).toHaveValue(0.25);
    expect(rowOf(form, 'Ready peer share')).toHaveTextContent('default 0');
    expect(rowOf(form, 'Ready peer share')).toHaveTextContent('× median');
    expect(rowOf(form, 'Ready attestations')).not.toHaveTextContent('default');

    expect(within(group('Timeouts')).getByLabelText('Lease TTL')).toHaveValue('30m');
    expect(within(group('Timeouts')).getByLabelText('Host stale after')).toHaveValue('2m');

    // A kind of rollout is one line: its policy as a choice, the canary switch next to it.
    const policies = group('Scheduler');
    expect(policy(form, 'Policy: images')).toHaveTextContent('auto');
    expect(policy(form, 'Policy: reboots')).toHaveTextContent('manual');
    expect(within(policies).getByRole('switch', { name: 'Pause after canary: images' })).toBeChecked();
    expect(within(policies).getByRole('switch', { name: 'Pause after canary: packages' })).not.toBeChecked();

    expect(within(group('Scheduler')).getByRole('switch', { name: 'Global pause' })).not.toBeChecked();

    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    expect(within(form).getByRole('status')).toHaveTextContent('No unsaved changes.');
    expect(screen.queryByRole('list', { name: 'Changes to save' })).not.toBeInTheDocument();
    expect(h.calls.filter(c => c.method === 'PUT')).toEqual([]);
  });

  it('offers a policy per kind of the fleet and other, and saves the whole map', async () => {
    const { form, h, user } = await openSettings();

    const kinds = within(form).getByRole('group', { name: 'Policy per kind' });
    const rows = within(kinds)
      .getAllByRole('listitem')
      .map(li => li.firstChild?.textContent);
    expect(rows).toEqual([...list((await h.api.settings()).kinds), 'other (unlabelled)']);
    expect(rows).toContain('snooper-el');
    expect(within(group('Images')).getByRole('group', { name: 'Image policy of the kind cl' })).toHaveTextContent(
      'Inherit (automatic)'
    );
    expect(policy(form, 'Image policy of the kind cl')).toHaveTextContent('Inherit (automatic)');

    await user.click(
      within(within(kinds).getByRole('group', { name: 'Image policy of the kind cl' })).getByRole('button', {
        name: 'Manual',
      })
    );
    await user.click(
      within(within(kinds).getByRole('group', { name: 'Image policy of the kind other' })).getByRole('button', {
        name: 'Automatic',
      })
    );
    expect(kinds).toHaveTextContent('changed');
    expect(kinds).toHaveTextContent('default every kind inherits');

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => {
      expect(h.calls.filter(c => c.method === 'PUT')).toHaveLength(1);
    });
    expect(h.calls.find(c => c.method === 'PUT')?.body).toStrictEqual({
      policyKinds: { cl: 'manual', other: 'auto' },
    });
    // The answer of the save is the new stored state: nothing is changed any more.
    await waitFor(() => {
      expect(within(screen.getByRole('form', { name: 'Settings' })).getByRole('status')).toHaveTextContent(
        'No unsaved changes.'
      );
    });
    expect(policy(screen.getByRole('form', { name: 'Settings' }), 'Image policy of the kind cl')).toHaveTextContent(
      'Manual'
    );
  });

  it('keeps the long description behind the info icon', async () => {
    const { form, user } = await openSettings();

    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();

    // A click (or a tap) opens it; it says what the setting does, its range and its default.
    const info = within(form).getByRole('button', { name: 'About Max finality lag' });
    await user.click(info);
    const tip = screen.getByRole('tooltip');
    expect(tip).toHaveTextContent('No new lease while finality lags more than this many epochs.');
    expect(tip).toHaveTextContent('Valid: at least 2');
    expect(tip).toHaveTextContent('Default: 3 epochs');
    expect(info).toHaveAttribute('aria-expanded', 'true');

    await user.keyboard('{Escape}');
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();

    // Keyboard focus opens it too.
    act(() => {
      within(form).getByRole('button', { name: 'About Lease TTL' }).focus();
    });
    expect(screen.getByRole('tooltip')).toHaveTextContent('Time an agent has to start a granted lease');
  });

  it('saves only what changed, and says what will change before it does', async () => {
    const { form, h, user } = await openSettings();

    const maxUnavailable = within(form).getByLabelText('Max unavailable');
    await user.clear(maxUnavailable);
    await user.type(maxUnavailable, '12.5');
    const ttl = within(form).getByLabelText('Lease TTL');
    await user.clear(ttl);
    await user.type(ttl, '45m');
    await user.click(
      within(within(form).getByRole('group', { name: 'Policy: packages' })).getByRole('button', { name: 'manual' })
    );
    await user.click(within(form).getByRole('switch', { name: 'Global pause' }));

    expect(within(rowOf(form, 'Max unavailable')).getByText('changed')).toBeInTheDocument();
    expect(within(rowOf(form, 'Ready attestations')).queryByText('changed')).not.toBeInTheDocument();
    expect(within(form).getByRole('status')).toHaveTextContent('4 unsaved changes');

    const changes = within(screen.getByRole('list', { name: 'Changes to save' }))
      .getAllByRole('listitem')
      .map(li => li.textContent);
    expect(changes).toEqual([
      'Global pause off → on',
      'Max unavailable 10 % → 12.5 %',
      'Lease TTL 30m → 45m',
      'Policy: packages auto → manual',
    ]);

    await user.click(screen.getByRole('button', { name: 'Save 4 changes' }));

    await waitFor(() => {
      expect(h.calls.filter(c => c.method === 'PUT')).toHaveLength(1);
    });
    const put = h.calls.find(c => c.method === 'PUT');
    expect(put?.path).toBe('/api/v1/settings');
    expect(put?.authorization).toBe('Bearer mock-token');
    expect(put?.body).toStrictEqual({
      paused: true,
      maxUnavailablePercent: 12.5,
      leaseTtl: '45m',
      policyPackages: 'manual',
    });

    // The form now shows the stored answer and is clean again.
    await waitFor(() => {
      expect(within(screen.getByRole('form', { name: 'Settings' })).getByRole('status')).toHaveTextContent(
        'No unsaved changes.'
      );
    });
    const saved = screen.getByRole('form', { name: 'Settings' });
    expect(within(saved).getByLabelText('Max unavailable')).toHaveValue(12.5);
    expect(within(saved).getByLabelText('Lease TTL')).toHaveValue('45m');
    expect(within(saved).getByRole('switch', { name: 'Global pause' })).toBeChecked();
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();

    // The controller stored it, kept what was not named, and audited the change.
    expect((await h.api.settings()).settings).toMatchObject({
      maxUnavailablePercent: 12.5,
      leaseTtl: '45m',
      paused: true,
      readyPeerShare: 0.25,
    });
    const audit = await h.api.audit({ action: 'settings.' });
    expect(audit.entries?.[0]?.detail).toMatchObject({
      before: { maxUnavailablePercent: 10 },
      after: { maxUnavailablePercent: 12.5 },
    });

    // A value that differs from its default says so and can be reset to it.
    expect(rowOf(saved, 'Max unavailable')).toHaveTextContent('default 10');
    await user.click(within(saved).getByRole('button', { name: 'Reset Max unavailable to its default' }));
    expect(within(saved).getByLabelText('Max unavailable')).toHaveValue(10);
    expect(within(saved).getByRole('status')).toHaveTextContent('1 unsaved change.');
  });

  it('validates fields and blocks saving', async () => {
    const { form, h, user } = await openSettings();

    const cases: [string, string, string][] = [
      ['Max unavailable', '150', 'must be between 0 and 100'],
      ['Max finality lag', '1', 'must be at least 2'],
      ['Ready attestations', '1.5', 'must be a whole number'],
      ['Ready attestations', '0', 'must be at least 1'],
      ['Ready attestations', '33', 'must be at most 32'],
      ['Ready stable for', '31m', 'must be at most 30m'],
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

    // The controls state their range themselves.
    expect(within(form).getByLabelText('Max unavailable')).toHaveAttribute('max', '100');
    expect(within(form).getByLabelText('Max finality lag')).toHaveAttribute('min', '2');

    // An empty number is not a number.
    await user.clear(within(form).getByLabelText('Min participation'));
    expect(within(rowOf(form, 'Min participation')).getByRole('alert')).toHaveTextContent('must be a number');

    expect(screen.getByRole('button', { name: /^Save/ })).toBeDisabled();
    expect(within(form).getByRole('status')).toHaveTextContent('Fix the marked fields to save.');
    // A reload of the page would lose the edits.
    expect(hasUnsavedWork()).toBe(true);

    await user.click(screen.getByRole('button', { name: 'Discard changes' }));
    expect(within(form).getByLabelText('Max unavailable')).toHaveValue(10);
    expect(hasUnsavedWork()).toBe(false);
    expect(within(form).queryByRole('alert')).not.toBeInTheDocument();
    expect(h.calls.filter(c => c.method === 'PUT')).toEqual([]);
  });

  it('treats an equivalent duration as unchanged', async () => {
    const { form, user } = await openSettings();

    const ttl = within(form).getByLabelText('Lease TTL');
    await user.clear(ttl);
    await user.type(ttl, '1800s');

    expect(within(form).getByRole('status')).toHaveTextContent('No unsaved changes.');
  });

  it('shows what the controller says about a field at that field', async () => {
    const { form, h, user } = await openSettings();

    const input = within(form).getByLabelText('Min peers');
    await user.clear(input);
    await user.type(input, '7');
    vi.spyOn(h.api, 'putSettings').mockRejectedValue(new ApiError(400, 'minPeers must be at most 5 on this fleet'));

    await user.click(screen.getByRole('button', { name: 'Save' }));

    expect(await within(rowOf(form, 'Min peers')).findByRole('alert')).toHaveTextContent(
      'minPeers must be at most 5 on this fleet'
    );
    expect(input).toHaveAttribute('aria-invalid', 'true');
    expect(within(rowOf(form, 'Ready attestations')).queryByRole('alert')).not.toBeInTheDocument();

    // It was about the value that was sent: another value clears it.
    await user.clear(input);
    await user.type(input, '5');
    expect(within(rowOf(form, 'Min peers')).queryByRole('alert')).not.toBeInTheDocument();
  });

  it('shows a failed save and keeps the edit', async () => {
    const { form, h, user } = await openSettings();

    const input = within(form).getByLabelText('Ready attestations');
    await user.clear(input);
    await user.type(input, '3');

    // The token is no longer accepted when the save is sent: the controller answers 401.
    h.auth.getToken = () => Promise.resolve('stale-token');

    await user.click(screen.getByRole('button', { name: 'Save' }));

    const alerts = await screen.findAllByRole('alert');
    expect(alerts.some(a => /Could not save the settings: .*HTTP 401/.test(a.textContent))).toBe(true);
    expect(within(form).getByLabelText('Ready attestations')).toHaveValue(3);
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();
  });
});
