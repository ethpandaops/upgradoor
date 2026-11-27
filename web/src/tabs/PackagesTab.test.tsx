import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createHarness, fakeClock, renderWithApp } from '../test/harness';
import { PAGE_SIZE, PackagesTab } from './PackagesTab';

/** names are the package names of the page on screen, in order. */
function names(): string[] {
  const table = screen.getByRole('table');
  return within(table)
    .getAllByRole('row')
    .slice(1)
    .flatMap(row => {
      const cell = row.querySelector('td[rowspan] span');
      return cell?.textContent ? [cell.textContent] : [];
    });
}

const pager = () => screen.getByRole('group', { name: 'Pages' });
const packageCalls = (calls: { path: string }[]) => calls.filter(c => c.path.startsWith('/api/v1/packages'));

describe('PackagesTab', () => {
  it('lists every package without a filter, a page at a time', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);

    await screen.findByRole('table', { name: 'Packages' });
    const first = names();
    expect(first).toHaveLength(PAGE_SIZE);
    expect([...first].sort()).toEqual(first);
    expect(pager()).toHaveTextContent(/^1–25 of \d+ packages/);
    expect(within(pager()).getByRole('button', { name: /Previous/ })).toBeDisabled();

    await user.click(within(pager()).getByRole('button', { name: /Next/ }));
    await waitFor(() => {
      expect(pager()).toHaveTextContent(/^26–50 of/);
      expect(names()[0]).not.toBe(first[0]);
    });
    const second = names();
    expect(second.filter(n => first.includes(n))).toEqual([]);
    expect((second[0] ?? '') > (first[first.length - 1] ?? '')).toBe(true);

    await user.click(within(pager()).getByRole('button', { name: /Previous/ }));
    await waitFor(() => {
      expect(names()).toEqual(first);
    });

    // One request per page shown, each for a page, none for everything.
    expect(packageCalls(h.calls).map(c => c.path)).toEqual([
      `/api/v1/packages?limit=${PAGE_SIZE}`,
      expect.stringMatching(new RegExp(`^/api/v1/packages\\?limit=${PAGE_SIZE}&after=`)) as string,
      `/api/v1/packages?limit=${PAGE_SIZE}`,
    ]);
  });

  it('filters as the user types and starts the filtered list at its first page', async () => {
    const user = fakeClock();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.click(within(pager()).getByRole('button', { name: /Next/ }));
    await waitFor(() => {
      expect(pager()).toHaveTextContent(/^26–50 of/);
    });

    await user.type(screen.getByRole('searchbox', { name: 'Filter packages' }), 'openssl');

    await waitFor(() => {
      expect(names()).toEqual(['openssl']);
    });
    expect(pager()).toHaveTextContent('1–1 of 1 package');
    expect(screen.getByRole('table', { name: 'Packages' })).toHaveTextContent('mixed');
    expect(within(pager()).getByRole('button', { name: /Next/ })).toBeDisabled();

    // Not one request per key: the typing is debounced.
    expect(
      packageCalls(h.calls)
        .filter(c => c.path.includes('q='))
        .map(c => c.path)
    ).toEqual([`/api/v1/packages?q=openssl&limit=${PAGE_SIZE}`]);

    await user.click(screen.getAllByRole('button', { name: /^Show hosts on openssl/ })[0] as HTMLElement);
    expect(screen.getByRole('table', { name: 'Packages' })).toHaveTextContent(/lighthouse-geth-\d/);
  });

  it('is where the user left it after another tab was shown', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    const first = renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.type(screen.getByRole('searchbox', { name: 'Filter packages' }), 'lib');
    await waitFor(() => {
      expect(names().every(n => n.includes('lib'))).toBe(true);
    });
    await user.click(within(pager()).getByRole('button', { name: /Next/ }));
    await waitFor(() => {
      expect(pager()).toHaveTextContent(/^26–50 of/);
    });
    const page = names();

    first.unmount();
    renderWithApp(<PackagesTab />, h);

    expect(screen.getByRole('searchbox', { name: 'Filter packages' })).toHaveValue('lib');
    await waitFor(() => {
      expect(names()).toEqual(page);
    });
    expect(pager()).toHaveTextContent(/^26–50 of/);
  });

  it('lists the images of the containers in a list of its own', async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<PackagesTab />, h);
    await screen.findByRole('table', { name: 'Packages' });

    await user.click(screen.getByRole('button', { name: 'Docker images' }));

    const table = await screen.findByRole('table', { name: 'Images' });
    expect(screen.queryByRole('table', { name: 'Packages' })).toBeNull();
    expect(pager()).toHaveTextContent(/^1–\d+ of \d+ images/);
    expect(table).toHaveTextContent('latest');
    expect(table).toHaveTextContent('registry checked');

    await user.type(screen.getByRole('searchbox', { name: 'Filter images' }), 'geth');
    await waitFor(() => {
      expect(names().length).toBeGreaterThan(0);
      expect(names().every(n => n.includes('geth'))).toBe(true);
    });
    expect(h.calls.map(c => c.path)).toContain(`/api/v1/images?q=geth&limit=${PAGE_SIZE}`);

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
