import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { App, visibleTabs } from './App';
import { ApiClient } from './api/client';
import { AuthStore } from './auth/authStore';
import { fakeAuthenticatoor } from './mock/install';
import { MockController, type MockProfile } from './mock/mockController';
import { PUBLIC_PATHS, recordingFetch, type RecordedCall } from './test/harness';

const TABS = ['Overview', 'Hosts', 'Clients', 'Rollouts', 'Packages', 'Settings', 'Data sources', 'Audit'];

function boot(opts: { auth: boolean; loggedIn?: boolean; unreachable?: boolean; profile?: MockProfile }) {
  const controller = new MockController({ auth: opts.auth });
  const calls: RecordedCall[] = [];

  if (opts.auth && !opts.unreachable) {
    window.ethpandaops = { authenticatoor: fakeAuthenticatoor(opts.loggedIn ?? false, opts.profile ?? 'staff') };
  }

  const auth = new AuthStore(() => Promise.reject(new Error('cannot load client-v2.js')));
  const api = new ApiClient({ fetch: recordingFetch(controller.fetch, calls), tokens: auth });
  render(<App api={api} auth={auth} />);

  const userCalls = () => calls.filter(c => !PUBLIC_PATHS.includes(c.path.split('?')[0] ?? ''));
  return { calls, userCalls, user: userEvent.setup() };
}

// The tabs of the page; a view may have tabs of its own.
const tabNames = () =>
  within(screen.getByRole('tablist', { name: 'Views' }))
    .getAllByRole('tab')
    .map(t => t.textContent);

describe('App in public mode (auth configured, logged out)', () => {
  it('offers only the public views and never calls an authenticated endpoint', async () => {
    const { calls, userCalls, user } = boot({ auth: true, loggedIn: false });

    await screen.findByRole('region', { name: 'Network gate' });
    expect(screen.getByRole('button', { name: 'Login' })).toBeInTheDocument();

    // What needs a login is not in the menu.
    expect(tabNames()).toEqual(['Overview', 'Hosts']);

    await user.click(screen.getByRole('tab', { name: 'Hosts' }));
    await screen.findByRole('table', { name: 'Hosts' });

    expect(userCalls()).toEqual([]);
    expect(calls.length).toBeGreaterThan(0);
    expect(calls.every(c => c.authorization === null)).toBe(true);
  });

  it('opens the overview for the URL of a tab that is not offered', async () => {
    window.location.hash = '#settings';
    const { userCalls } = boot({ auth: true, loggedIn: false });

    await screen.findByRole('region', { name: 'Network gate' });

    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.queryByRole('form', { name: 'Settings' })).not.toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    await waitFor(() => {
      expect(window.location.hash).toBe('#overview');
    });
    expect(userCalls()).toEqual([]);
  });

  it('shows the private views after a login and takes them away again after logout', async () => {
    const { userCalls, user } = boot({ auth: true, loggedIn: false });
    await screen.findByRole('region', { name: 'Network gate' });
    expect(screen.queryByRole('region', { name: 'In progress' })).not.toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Login' }));

    expect(await screen.findByText('mock-user@example.org')).toBeInTheDocument();
    await screen.findByRole('region', { name: 'In progress' });
    // The mock logs in staff: every tab, the settings included.
    expect(tabNames()).toEqual(TABS);

    await user.click(screen.getByRole('tab', { name: 'Hosts' }));
    await screen.findByRole('columnheader', { name: /Kernel/ });
    expect(userCalls().length).toBeGreaterThan(0);
    expect(userCalls().every(c => c.authorization === 'Bearer mock-token')).toBe(true);

    await user.click(screen.getByRole('tab', { name: 'Rollouts' }));
    expect(await screen.findByRole('tab', { name: 'Open rollouts 5' })).toHaveAttribute('aria-selected', 'true');

    // The login ends while a private tab is shown: the overview, and no error.
    await user.click(screen.getByRole('button', { name: 'Logout' }));

    await waitFor(() => {
      expect(tabNames()).toEqual(['Overview', 'Hosts']);
    });
    expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('aria-selected', 'true');
    expect(await screen.findByRole('region', { name: 'Network gate' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(within(screen.getByRole('banner')).getByRole('button', { name: 'Login' })).toBeInTheDocument();
  });

  it('reports an unreachable auth provider and stays public', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const { userCalls } = boot({ auth: true, unreachable: true });

    await screen.findByRole('region', { name: 'Network gate' });

    expect(screen.getByText('auth provider unreachable')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Login' })).not.toBeInTheDocument();
    expect(tabNames()).toEqual(['Overview', 'Hosts']);
    expect(userCalls()).toEqual([]);
  });
});

describe('the menu of a login', () => {
  // Who the mock's fake auth client is logged in as: staff holds every
  // feature, a team member the team features, a reader none. Packages
  // needs details, Audit needs audit.
  const cases: [MockProfile, string[]][] = [
    ['staff', ['Overview', 'Hosts', 'Clients', 'Rollouts', 'Packages', 'Settings', 'Data sources', 'Audit']],
    ['team', ['Overview', 'Hosts', 'Clients', 'Rollouts']],
    ['reader', ['Overview', 'Hosts', 'Clients', 'Rollouts']],
  ];

  it.each(cases)('offers a %s login the tabs it can use', async (profile, tabs) => {
    window.location.hash = '#settings';
    boot({ auth: true, loggedIn: true, profile });

    await screen.findByText('mock-user@example.org');
    await waitFor(() => {
      expect(tabNames()).toEqual(tabs);
    });

    if (tabs.includes('Settings')) {
      // Only who may change the settings is shown them.
      expect(await screen.findByRole('form', { name: 'Settings' })).toBeInTheDocument();
    } else {
      expect(await screen.findByRole('region', { name: 'In progress' })).toBeInTheDocument();
      expect(screen.getByRole('tab', { name: 'Overview' })).toHaveAttribute('aria-selected', 'true');
      expect(screen.queryByRole('form', { name: 'Settings' })).not.toBeInTheDocument();
    }
  });

  it('decides by what the login may do', () => {
    expect(visibleTabs({ priv: false, admin: false, hostDetails: true, audit: true })).toEqual(['overview', 'hosts']);
    expect(visibleTabs({ priv: true, admin: false, hostDetails: true, audit: true })).toEqual([
      'overview',
      'hosts',
      'clients',
      'rollouts',
      'packages',
      'audit',
    ]);
    expect(visibleTabs({ priv: true, admin: false, hostDetails: false, audit: true })).not.toContain('packages');
    expect(visibleTabs({ priv: true, admin: false, hostDetails: true, audit: false })).not.toContain('audit');
    expect(visibleTabs({ priv: true, admin: true, hostDetails: true, audit: true })).toHaveLength(8);
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
    expect(await screen.findByRole('list', { name: 'Audit log' })).toBeInTheDocument();
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
    expect(window.location.hash).toBe('#clients');
    await user.keyboard('{ArrowRight}');

    const rollouts = screen.getByRole('tab', { name: 'Rollouts' });
    expect(rollouts).toHaveAttribute('aria-selected', 'true');
    expect(rollouts).toHaveFocus();
    expect(window.location.hash).toBe('#rollouts');

    await user.keyboard('{End}');
    expect(window.location.hash).toBe('#audit');
  });

  it('opens the Rollouts tab at the list a link names', async () => {
    window.location.hash = '#rollouts/leases/recent';
    const { user } = boot({ auth: false });

    expect(await screen.findByRole('table', { name: 'Recent leases' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Rollouts' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('tab', { name: /^Leases \d+$/ })).toHaveAttribute('aria-selected', 'true');
    expect(window.location.hash).toBe('#rollouts/leases/recent');

    await user.click(screen.getByRole('tab', { name: /^Finished rollouts/ }));
    expect(window.location.hash).toBe('#rollouts/finished');
    expect(screen.getByRole('tab', { name: 'Rollouts' })).toHaveAttribute('aria-selected', 'true');
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
