import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';

import { createHarness, renderWithApp } from '../test/harness';
import { AuditTab } from './AuditTab';

const log = () => screen.getByRole('list', { name: 'Audit log' });
const auditCalls = (calls: { path: string }[]) =>
  calls.filter(c => c.path.startsWith('/api/v1/audit')).map(c => c.path);

describe('AuditTab', () => {
  beforeEach(() => {
    window.location.hash = '#audit';
  });

  it("opens on what people did, and shows the scheduler's entries on request", async () => {
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<AuditTab />, h);

    await screen.findByRole('list', { name: 'Audit log' });
    expect(auditCalls(h.calls)[0]).toBe('/api/v1/audit?who=people&limit=50&actors=1');
    expect(within(log()).queryByText('Lease granted')).not.toBeInTheDocument();
    expect(within(log()).getAllByText('Changed the settings').length).toBeGreaterThan(0);

    const who = screen.getByRole('group', { name: 'Who acted' });
    expect(within(who).getByRole('button', { name: /^People \d+/ })).toHaveAttribute('aria-pressed', 'true');

    await user.click(within(who).getByRole('button', { name: 'Scheduler' }));
    await waitFor(() => {
      expect(within(log()).getAllByText('Lease granted').length).toBeGreaterThan(0);
    });
    expect(window.location.hash).toBe('#audit/?who=scheduler');
    expect(within(log()).queryByText('Changed the settings')).not.toBeInTheDocument();
  });

  it('lands on a linked filter and narrows to a target from an entry', async () => {
    window.location.hash = '#audit/?who=all&group=settings';
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<AuditTab />, h);

    await screen.findByRole('list', { name: 'Audit log' });
    expect(auditCalls(h.calls)[0]).toBe('/api/v1/audit?group=settings&limit=50&actors=1');
    expect(screen.getByRole('button', { name: 'Remove filter Settings' })).toBeInTheDocument();
    expect(within(log()).getByText('Paused the scheduler')).toBeInTheDocument();

    // The detail of a settings change is a table of what changed, by the names of the settings.
    const row = within(log()).getAllByText('Changed the settings')[0]?.closest('li') as HTMLElement;
    await user.click(within(row).getByRole('button', { name: /Show the detail of entry/ }));
    const changes = within(row).getByRole('table', { name: 'Changes' });
    expect(changes).toHaveTextContent('Pause after canary: images');
    await user.click(within(row).getByRole('button', { name: 'Raw' }));
    expect(within(row).getByText(/"action": "settings.update"/)).toBeInTheDocument();

    // A host named in an entry narrows the page to everything about it.
    await user.click(screen.getByRole('button', { name: 'Remove filter Settings' }));
    await waitFor(() => {
      expect(within(log()).getAllByText('Held the host').length).toBeGreaterThan(0);
    });
    const hold = within(log()).getAllByText('Held the host')[0]?.closest('li') as HTMLElement;
    await user.click(within(hold).getByTitle('Every entry about grandine-erigon-2'));
    await waitFor(() => {
      expect(window.location.hash).toBe('#audit/?who=all&target=grandine-erigon-2');
    });
    await waitFor(() => {
      expect(within(log()).getAllByText('Agent took the instruction').length).toBeGreaterThan(0);
    });
  });

  it('says when nothing matches and offers everyone', async () => {
    window.location.hash = '#audit/?q=nothing-like-this';
    const user = userEvent.setup();
    const h = await createHarness({ mode: 'open' });
    renderWithApp(<AuditTab />, h);

    expect(await screen.findByText('No audit entry matches the filters.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Reset' }));
    await screen.findByRole('list', { name: 'Audit log' });
    expect(window.location.hash).toBe('#audit');
  });
});
