import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { App } from './App';
import { ApiClient } from './api/client';
import { AuthStore } from './auth/authStore';
import { fakeAuthenticatoor } from './mock/install';
import { MockController } from './mock/mockController';
import { PUBLIC_PATHS, recordingFetch, type RecordedCall } from './test/harness';

const TABS = ['Rollouts', 'Packages', 'Settings', 'Audit'];

function boot(opts: { auth: boolean; loggedIn?: boolean; unreachable?: boolean }) {
  const controller = new MockController({ auth: opts.auth });
  const calls: RecordedCall[] = [];

  if (opts.auth && !opts.unreachable) {
    window.ethpandaops = { authenticatoor: fakeAuthenticatoor(opts.loggedIn ?? false) };
  }

  const auth = new AuthStore(() => Promise.reject(new Error('cannot load client-v2.js')));
  const api = new ApiClient({ fetch: recordingFetch(controller.fetch, calls), tokens: auth });
  render(<App api={api} auth={auth} />);

  const userCalls = () => calls.filter(c => !PUBLIC_PATHS.includes(c.path.split('?')[0] ?? ''));
  return { calls, userCalls, user: userEvent.setup() };
}

describe('App in public mode (auth configured, logged out)', () => {
  it('never calls an authenticated endpoint, whatever tab is opened', async () => {
    const { calls, userCalls, user } = boot({ auth: true, loggedIn: false });

    await screen.findByRole('region', { name: 'Network gate' });
    expect(screen.getByRole('button', { name: 'Login' })).toBeInTheDocument();

    for (const name of TABS) {
      await user.click(screen.getByRole('tab', { name: new RegExp(name) }));
      expect(screen.getByRole('tabpanel')).toHaveTextContent('only served to logged-in users');
    }

    await user.click(screen.getByRole('tab', { name: 'Hosts' }));
    await screen.findByRole('table', { name: 'Hosts' });

    expect(userCalls()).toEqual([]);
    expect(calls.length).toBeGreaterThan(0);
    expect(calls.every(c => c.authorization === null)).toBe(true);
  });

  it('shows the private views after a login and hides them again after logout', async () => {
    const { userCalls, user } = boot({ auth: true, loggedIn: false });
    await screen.findByRole('region', { name: 'Network gate' });
    expect(screen.queryByRole('region', { name: 'In progress' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Login' }));

    expect(await screen.findByText('mock-user@example.org')).toBeInTheDocument();
    await screen.findByRole('region', { name: 'In progress' });
    await user.click(screen.getByRole('tab', { name: 'Hosts' }));
    await screen.findByRole('columnheader', { name: /Kernel/ });
    expect(userCalls().length).toBeGreaterThan(0);
    expect(userCalls().every(c => c.authorization === 'Bearer mock-token')).toBe(true);

    await user.click(screen.getByRole('tab', { name: 'Rollouts' }));
    expect(await screen.findByRole('heading', { name: /Open rollouts \(3\)/ })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Logout' }));

    expect(await screen.findByRole('tabpanel')).toHaveTextContent('only served to logged-in users');
    expect(within(screen.getByRole('banner')).getByRole('button', { name: 'Login' })).toBeInTheDocument();
    expect(within(screen.getByRole('tabpanel')).getByRole('button', { name: 'Login' })).toBeInTheDocument();
  });

  it('reports an unreachable auth provider and stays public', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const { userCalls, user } = boot({ auth: true, unreachable: true });

    await screen.findByRole('region', { name: 'Network gate' });

    expect(screen.getByText('auth provider unreachable')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Login' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('tab', { name: /Audit/ }));
    expect(screen.getByRole('alert')).toHaveTextContent('The auth provider is unreachable');
    expect(userCalls()).toEqual([]);
  });
});

describe('App in open mode (no auth provider)', () => {
  it('shows everything without a login button', async () => {
    const { calls, user } = boot({ auth: false });

    await screen.findByRole('region', { name: 'In progress' });

    expect(screen.getByText('open access')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Login' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Logout' })).not.toBeInTheDocument();
    expect(document.title).toBe('upgradoor · mock-devnet-0');

    await user.click(screen.getByRole('tab', { name: 'Audit' }));
    expect(await screen.findByRole('table', { name: 'Audit log' })).toBeInTheDocument();
    expect(calls.every(c => c.authorization === null)).toBe(true);
  });

  it('keeps the tab in the URL hash and moves between tabs with the arrow keys', async () => {
    const { user } = boot({ auth: false });
    await screen.findByRole('region', { name: 'Network gate' });

    const overview = screen.getByRole('tab', { name: 'Overview' });
    expect(overview).toHaveAttribute('aria-selected', 'true');

    overview.focus();
    await user.keyboard('{ArrowRight}');
    expect(window.location.hash).toBe('#hosts');
    await screen.findByRole('table', { name: 'Hosts' });
    await user.keyboard('{ArrowRight}');

    const rollouts = screen.getByRole('tab', { name: 'Rollouts' });
    expect(rollouts).toHaveAttribute('aria-selected', 'true');
    expect(rollouts).toHaveFocus();
    expect(window.location.hash).toBe('#rollouts');

    await user.keyboard('{End}');
    expect(window.location.hash).toBe('#audit');
  });

  it('switches and persists the theme', async () => {
    const { user } = boot({ auth: false });
    await screen.findByRole('region', { name: 'Network gate' });

    await user.click(screen.getByRole('button', { name: /Theme: system/ }));
    expect(localStorage.getItem('upgradoor.theme')).toBe('light');
    expect(document.documentElement).not.toHaveClass('dark');

    await user.click(screen.getByRole('button', { name: /Theme: light/ }));
    expect(localStorage.getItem('upgradoor.theme')).toBe('dark');
    expect(document.documentElement).toHaveClass('dark');

    await user.click(screen.getByRole('button', { name: /Theme: dark/ }));
    expect(localStorage.getItem('upgradoor.theme')).toBeNull();
  });
});

describe('Header', () => {
  it('holds the views and keeps its corner free once the panda menu is on the page', async () => {
    boot({ auth: false });
    await screen.findByRole('region', { name: 'Network gate' });

    const banner = screen.getByRole('banner');
    expect(within(banner).getByRole('tablist', { name: 'Views' })).toBeInTheDocument();
    // Dark in both themes.
    expect(banner).toHaveClass('dark');
    expect(banner).toHaveClass('pl-4');

    const menu = document.createElement('div');
    menu.id = 'panda-menu-root';
    document.body.appendChild(menu);
    try {
      await waitFor(() => {
        expect(banner).toHaveClass('pl-18');
      });
      expect(banner).not.toHaveClass('pl-4');
    } finally {
      menu.remove();
    }

    await waitFor(() => {
      expect(banner).toHaveClass('pl-4');
    });
  });
});

describe('App start', () => {
  it('shows an error with a retry when the controller is unreachable', async () => {
    let fail = true;
    const controller = new MockController();
    const api = new ApiClient({
      fetch: (input, init) => (fail ? Promise.reject(new TypeError('Failed to fetch')) : controller.fetch(input, init)),
    });
    render(<App api={api} auth={new AuthStore()} />);

    expect(await screen.findByRole('alert')).toHaveTextContent('Could not reach the controller');

    fail = false;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }));

    await waitFor(() => {
      expect(screen.getByRole('tablist', { name: 'Views' })).toBeInTheDocument();
    });
    expect(within(screen.getByRole('banner')).getByText('mock-devnet-0')).toBeInTheDocument();
  });
});
