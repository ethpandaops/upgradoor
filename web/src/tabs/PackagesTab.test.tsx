import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { createHarness, renderWithApp } from '../test/harness';
import { PAGE_SIZE, PackagesTab } from './PackagesTab';

/** names are the package names of the page on screen, in order. */
function names(): string[] {
  const table = screen.getByRole('table', { name: 'Packages' });
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
    const user = userEvent.setup();
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
});
