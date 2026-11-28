import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { DEFAULT_PAGE_SIZE } from '../lib/packagesView';
import { createHarness, fakeClock, renderWithApp } from '../test/harness';
import { PackagesTab } from './PackagesTab';

/** names are the package names of the page on screen, in order. */
function names(): string[] {
  const table = screen.getByRole('table', { name: 'Packages' });
  return within(table)
    .getAllByRole('button', { name: /^(Show|Hide) the hosts of / })
    .map(b => (b.getAttribute('aria-label') ?? '').replace(/^(Show|Hide) the hosts of /, ''));
}

/** imageNames are the image references of the page on screen, in order. */
function imageNames(): string[] {
  const table = screen.getByRole('table', { name: 'Images' });
  return within(table)
    .getAllByRole('row')
    .slice(1)
    .flatMap(row => {
      const cell = row.querySelector('td[rowspan] span');
      return cell?.textContent ? [cell.textContent] : [];
    });
}

const pager = () => screen.getByRole('group', { name: 'Pages' });
const show = () => screen.getByRole('group', { name: 'Show' });
const packageCalls = (calls: { path: string }[]) =>
  calls.filter(c => c.path.startsWith('/api/v1/packages')).map(c => c.path);

describe('PackagesTab', () => {
  it('lists every package without a filter, a page at a time', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);

    await screen.findByRole('table', { name: 'Packages' });
    const first = names();
    expect(first).toHaveLength(DEFAULT_PAGE_SIZE);
    expect([...first].sort()).toEqual(first);
    expect(pager()).toHaveTextContent(/^1–100 of 155 packages · page 1 of 2/);
    expect(within(pager()).getByRole('button', { name: /Previous/ })).toBeDisabled();

    await user.click(within(pager()).getByRole('button', { name: /Next/ }));
    await waitFor(() => {
      expect(pager()).toHaveTextContent(/^101–155 of/);
    });
    const second = names();
    expect(second.filter(n => first.includes(n))).toEqual([]);
    expect(window.location.hash).toMatch(/^#packages\/\?page=1&after=/);

    await user.click(within(pager()).getByRole('button', { name: /Previous/ }));
    await waitFor(() => {
      expect(names()).toEqual(first);
    });

    // One request per page shown, compact, none for everything.
    expect(packageCalls(h.calls)).toEqual([
      `/api/v1/packages?limit=${DEFAULT_PAGE_SIZE}&compact=1`,
      expect.stringMatching(new RegExp(`^/api/v1/packages\\?limit=${DEFAULT_PAGE_SIZE}&after=.*&compact=1$`)) as string,
      `/api/v1/packages?limit=${DEFAULT_PAGE_SIZE}&compact=1`,
    ]);
  });

  it('takes the page size from the user and keeps it in the URL', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.selectOptions(screen.getByRole('combobox', { name: 'Page size' }), '250');

    await waitFor(() => {
      expect(names()).toHaveLength(155);
    });
    expect(pager()).toHaveTextContent(/^1–155 of 155 packages‹/);
    expect(within(pager()).getByRole('button', { name: /Next/ })).toBeDisabled();
    expect(window.location.hash).toBe('#packages/?size=250');
    expect(packageCalls(h.calls)).toContain('/api/v1/packages?limit=250&compact=1');
  });

  it('narrows to the packages that differ or are outdated, with the count of each', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    expect(within(show()).getByRole('button', { name: /^All/ })).toHaveTextContent('All155');
    const differing = within(show()).getByRole('button', { name: /^Differing/ });
    expect(differing).toHaveTextContent(/^Differing\d+$/);
    const count = Number(differing.textContent.replace(/\D/g, ''));

    await user.click(differing);
    await waitFor(() => {
      expect(names()).toHaveLength(count);
    });
    expect(differing).toHaveAttribute('aria-pressed', 'true');
    expect(names()).toContain('openssl');
    expect(window.location.hash).toBe('#packages/?show=differing');

    await user.click(within(show()).getByRole('button', { name: /^Security/ }));
    await waitFor(() => {
      expect(names()).toContain('openssl');
    });
    const table = screen.getByRole('table', { name: 'Packages' });
    expect(within(table).getAllByText('security').length).toBe(names().length);
    expect(packageCalls(h.calls)).toContain(`/api/v1/packages?limit=${DEFAULT_PAGE_SIZE}&show=security&compact=1`);
  });

  it('sorts by a column on the controller', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.click(screen.getByRole('button', { name: /^Latest available/ }));

    await waitFor(() => {
      expect(screen.getByRole('columnheader', { name: /Latest available/ })).toHaveAttribute('aria-sort', 'descending');
    });
    await waitFor(() => {
      expect(names()[0]).not.toBe('apt');
    });
    expect(packageCalls(h.calls)).toContain(`/api/v1/packages?limit=${DEFAULT_PAGE_SIZE}&sort=outdated&compact=1`);

    // The active column again: back to the names.
    await user.click(screen.getByRole('button', { name: /^Latest available/ }));
    await waitFor(() => {
      expect(names()[0]).toBe('apt');
    });
  });

  it('filters as the user types and shows the hosts of a package on demand', async () => {
    const user = fakeClock();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.type(screen.getByRole('searchbox', { name: 'Filter packages' }), 'openssl');

    await waitFor(() => {
      expect(names()).toEqual(['openssl']);
    });
    expect(pager()).toHaveTextContent('1–1 of 1 package');
    const table = screen.getByRole('table', { name: 'Packages' });
    expect(table).toHaveTextContent(/\+1 version on 2/);
    expect(table).toHaveTextContent(/2\/\d+ behind/);

    // Not one request per key: the typing is debounced.
    expect(packageCalls(h.calls).filter(p => p.includes('q='))).toEqual([
      `/api/v1/packages?q=openssl&limit=${DEFAULT_PAGE_SIZE}&compact=1`,
    ]);

    await user.click(screen.getByRole('button', { name: 'Show the hosts of openssl' }));
    await waitFor(() => {
      expect(table).toHaveTextContent(/lighthouse-geth-\d/);
    });
    expect(table).toHaveTextContent(/all can upgrade to/);
    expect(packageCalls(h.calls)).toContain('/api/v1/packages?q=openssl&limit=1');
  });

  it('opens at the place a link names and offers to clear a filter that selects nothing', async () => {
    const user = userEvent.setup();
    window.location.hash = '#packages/?q=nothing-like-this&show=outdated';
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);

    expect(await screen.findByText('No package with an upgrade available matches “nothing-like-this”.')).toBeVisible();
    expect(screen.getByRole('searchbox', { name: 'Filter packages' })).toHaveValue('nothing-like-this');

    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    await screen.findByRole('table', { name: 'Packages' });
    expect(window.location.hash).toBe('#packages');
  });

  it('is where the user left it after another tab was shown', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    const first = renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.selectOptions(screen.getByRole('combobox', { name: 'Page size' }), '50');
    await user.type(screen.getByRole('searchbox', { name: 'Filter packages' }), 'lib');
    await waitFor(() => {
      expect(names().every(n => n.includes('lib'))).toBe(true);
    });
    await user.click(within(pager()).getByRole('button', { name: /Next/ }));
    await waitFor(() => {
      expect(pager()).toHaveTextContent(/^51–/);
    });
    const page = names();

    first.unmount();
    window.location.hash = '#packages';
    renderWithApp(<PackagesTab />, h);

    expect(screen.getByRole('searchbox', { name: 'Filter packages' })).toHaveValue('lib');
    await waitFor(() => {
      expect(names()).toEqual(page);
    });
    expect(pager()).toHaveTextContent(/^51–/);
    expect(window.location.hash).toMatch(/^#packages\/\?q=lib&size=50&page=1&after=/);
  });

  it('lists the images of the containers in a list of its own', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.click(screen.getByRole('button', { name: 'Docker images' }));

    const table = await screen.findByRole('table', { name: 'Images' });
    expect(screen.queryByRole('table', { name: 'Packages' })).toBeNull();
    expect(screen.queryByRole('group', { name: 'Show' })).toBeNull();
    expect(pager()).toHaveTextContent(/^1–\d+ of \d+ images/);
    expect(table).toHaveTextContent('latest');
    expect(table).toHaveTextContent('registry checked');
    expect(window.location.hash).toBe('#packages/images');

    await user.type(screen.getByRole('searchbox', { name: 'Filter images' }), 'geth');
    await waitFor(() => {
      expect(imageNames().length).toBeGreaterThan(0);
      expect(imageNames().every(n => n.includes('geth'))).toBe(true);
    });
    expect(h.calls.map(c => c.path)).toContain(`/api/v1/images?q=geth&limit=${DEFAULT_PAGE_SIZE}`);

    // Each list keeps its own filter.
    await user.click(screen.getByRole('button', { name: 'APT packages' }));
    await screen.findByRole('table', { name: 'Packages' });
    expect(screen.getByRole('searchbox', { name: 'Filter packages' })).toHaveValue('');

    await user.click(screen.getByRole('button', { name: 'Docker images' }));
    expect(screen.getByRole('searchbox', { name: 'Filter images' })).toHaveValue('geth');
  });

  it('asks for a registry lookup of every tag', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);

    await user.click(await screen.findByRole('button', { name: 'Docker images' }));
    await screen.findByRole('table', { name: 'Images' });

    await user.click(screen.getByRole('button', { name: 'Check registries now' }));

    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent(
        /^Looking up \d+ tags.* Reading the source branches of 4 images\.$/
      );
    });
    expect(h.calls.filter(c => c.path === '/api/v1/images/refresh')).toHaveLength(1);

    // Within the half minute the controller says when it looks up again.
    await user.click(screen.getByRole('button', { name: 'Check registries now' }));

    await waitFor(() => {
      expect(screen.getByRole('status')).toHaveTextContent(
        /^Every tag was looked up within the last half minute\. The next lookup on request is possible at /
      );
    });
  });
});
