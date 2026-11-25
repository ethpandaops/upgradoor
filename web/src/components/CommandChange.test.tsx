import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { CommandChange } from './CommandChange';

const enr = (n: number) => `enr:-${String(n).repeat(80)}`;

describe('CommandChange', () => {
  it('says only that the command line changes without a delta (an older agent)', () => {
    render(<CommandChange flags label="Change" />);
    expect(screen.getByText('command line changed')).toBeInTheDocument();
    expect(screen.queryByRole('list')).not.toBeInTheDocument();
  });

  it('names the image the container ran', () => {
    render(<CommandChange fromImage="ethpandaops/geth:v1" label="Change" />);
    expect(screen.getByText('ethpandaops/geth:v1').parentElement).toHaveTextContent('from ethpandaops/geth:v1');
  });

  it('folds away a changed flag and its value', async () => {
    const user = userEvent.setup();
    render(
      <CommandChange
        flags
        command={{ added: ['--verbosity=4', '--metrics'], removed: ['--verbosity=3'] }}
        label="Change"
      />
    );

    const changes = screen.getByRole('list', { name: 'Change' });
    expect(changes.closest('details')).not.toHaveAttribute('open');
    await user.click(screen.getByText(/^command line changed/));
    expect(changes.closest('details')).toHaveAttribute('open');

    expect(
      within(changes)
        .getAllByRole('listitem')
        .map(li => li.textContent)
    ).toEqual(['--verbosity3→4', '+--metrics']);
  });

  it('shows the items a list gains and loses, long ones clipped until expanded', async () => {
    const user = userEvent.setup();
    render(
      <CommandChange
        flags
        command={{
          added: [`--bootnodes=${[enr(1), enr(3)].join(',')}`],
          removed: [`--bootnodes=${[enr(1), enr(2)].join(',')}`],
          truncated: true,
        }}
        label="Change"
      />
    );

    const items = screen.getByRole('list', { name: 'Items of --bootnodes' });
    expect(screen.getByText('2 → 2 items')).toBeInTheDocument();
    const [removed, added] = within(items).getAllByRole('listitem');
    expect(removed).toHaveTextContent(`−${enr(2).slice(0, 48)}…`);
    expect(added).toHaveTextContent(`+${enr(3).slice(0, 48)}…`);
    expect(within(added as HTMLElement).getByTitle(enr(3))).toBeInTheDocument();

    await user.click(within(added as HTMLElement).getByRole('button', { name: `all ${enr(3).length} characters` }));
    expect(added).toHaveTextContent(enr(3));
    expect(screen.getByText(/some arguments are cut or left out/)).toBeInTheDocument();
  });
});
