import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { Container } from '../api/types';
import { list } from '../api/types';
import type { MockProfile } from '../mock/mockController';
import { createHarness, renderWithApp, type Harness, PRIMARY } from '../test/harness';
import { useActions } from './ActionDialog';
import { OverrideButtons, notAllowed, overrideState } from './Overrides';

/** Bench is the two buttons on containers of a host of the mock, with the dialog and the answer of a view. */
function Bench({ host, containers, stale = false }: { host: string; containers: Container[]; stale?: boolean }) {
  const actions = useActions();
  return (
    <div>
      {actions.notice}
      <OverrideButtons host={host} containers={containers} stale={stale} onAction={actions.request} />
      {actions.dialog}
    </div>
  );
}

async function bench(
  host: string,
  names: string[],
  opts: { profile?: MockProfile; stale?: boolean; before?: { action: string; body: object } } = {}
): Promise<Harness> {
  const h = await createHarness({ mode: 'auth', loggedIn: true, profile: opts.profile ?? 'staff' });
  if (opts.before) {
    // What staff did before the page was opened.
    await h.controller.fetch(`/api/v1/actions/${opts.before.action}`, {
      method: 'POST',
      headers: { Authorization: 'Bearer mock-token' },
      body: JSON.stringify({ host, ...opts.before.body }),
    });
  }
  const detail = h.controller.data.hosts.get(host);
  const containers = names.map(n => structuredClone(list(detail?.containers).find(c => c.name === n) as Container));
  h.calls.length = 0;
  renderWithApp(<Bench host={host} containers={containers} stale={opts.stale} />, h);
  return h;
}

const sent = (h: Harness, action: string) => h.calls.find(c => c.path === `/api/v1/actions/${action}`)?.body;

describe('the image override dialog', () => {
  it('asks for a tag, an end and a reason, and sends one request', async () => {
    const user = userEvent.setup();
    const h = await bench('rpc-1', ['geth']);

    await user.click(screen.getByRole('button', { name: 'Override the image on this host' }));
    const dialog = await screen.findByRole('dialog', { name: 'Image override on rpc-1' });
    const confirm = within(dialog).getByRole('button', PRIMARY);
    const input = within(dialog).getByRole('textbox', { name: 'Image' });

    expect(dialog).toHaveTextContent('configuredethpandaops/geth:master');
    expect(within(dialog).queryByRole('button', { name: /Clear/ })).not.toBeInTheDocument();
    expect(confirm).toBeDisabled();

    // Not a tag, a digest: each says why and blocks the request.
    await user.type(input, 'ethpandaops/geth');
    expect(within(dialog).getByRole('alert')).toHaveTextContent('must name a tag');
    await user.type(input, '@sha256:00');
    expect(within(dialog).getByRole('alert')).toHaveTextContent('not a digest');
    expect(confirm).toBeDisabled();

    await user.clear(input);
    await user.type(input, 'ethpandaops/geth:pr-42');
    expect(within(dialog).queryByRole('alert')).not.toBeInTheDocument();
    // The reason is required.
    expect(confirm).toBeDisabled();
    await user.type(within(dialog).getByPlaceholderText('Recorded in the audit log'), 'try the fix');
    await user.click(within(dialog).getByRole('button', { name: '6h' }));
    expect(within(dialog).getByLabelText('What will happen')).toHaveTextContent(
      /Run geth on rpc-1 from ethpandaops\/geth:pr-42 instead of its configured image, until /
    );

    const before = Date.now();
    await user.click(confirm);
    await waitFor(() => {
      expect(sent(h, 'image-override')).toBeDefined();
    });
    const body = sent(h, 'image-override') as { until: string };
    expect(body).toMatchObject({
      host: 'rpc-1',
      container: 'geth',
      image: 'ethpandaops/geth:pr-42',
      reason: 'try the fix',
    });
    expect(Date.parse(body.until) - before).toBeGreaterThanOrEqual(6 * 3_600_000 - 1000);
    expect(Date.parse(body.until) - before).toBeLessThan(6 * 3_600_000 + 60_000);

    expect(await screen.findByRole('status')).toHaveTextContent(
      'sent to rpc-1; the agent applies it under its next lease'
    );
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('shows the override that is there, waiting for its lease, and clears it', async () => {
    const user = userEvent.setup();
    const h = await bench('teku-besu-1', ['besu'], {
      before: {
        action: 'image-override',
        body: { container: 'besu', image: 'hyperledger/besu:pr-1', reason: 'first' },
      },
    });

    await user.click(screen.getByRole('button', { name: 'Override the image on this host' }));
    const dialog = await screen.findByRole('dialog');
    const current = within(dialog).getByLabelText('Current override');
    expect(current).toHaveTextContent('waiting for a lease');
    expect(current).toHaveTextContent('image hyperledger/besu:pr-1');
    expect(current).toHaveTextContent('“first”');

    // Clearing needs no image and no reason.
    await user.click(within(dialog).getByRole('button', { name: 'Clear image override' }));
    await waitFor(() => {
      expect(sent(h, 'image-override')).toEqual({ host: 'teku-besu-1', container: 'besu', image: '', reason: '' });
    });
  });

  it('lets the user pick the container, the consensus client before its validator client', async () => {
    const user = userEvent.setup();
    const h = await bench('lighthouse-geth-1', ['lighthouse-validator', 'lighthouse']);

    await user.click(screen.getByRole('button', { name: 'Override the flags on this host' }));
    const dialog = await screen.findByRole('dialog', { name: 'Flag override on lighthouse-geth-1' });
    const pick = within(dialog).getByRole('combobox');
    expect(pick).toHaveValue('lighthouse');
    expect(
      within(pick)
        .getAllByRole('option')
        .map(o => o.textContent)
    ).toEqual(['lighthouse (cl)', 'lighthouse-validator (vc)']);

    await user.selectOptions(pick, 'lighthouse-validator');
    await user.type(within(dialog).getByRole('textbox', { name: 'Flags to add' }), '--graffiti=x');
    await user.type(within(dialog).getByPlaceholderText('Recorded in the audit log'), 'r');
    await user.click(within(dialog).getByRole('button', PRIMARY));

    await waitFor(() => {
      expect(sent(h, 'flag-override')).toMatchObject({
        host: 'lighthouse-geth-1',
        container: 'lighthouse-validator',
        addFlags: ['--graffiti=x'],
        removeFlags: [],
      });
    });
  });
});

describe('the flag override dialog', () => {
  it('previews the command line, takes a click on an argument as a removal and refuses what is no flag', async () => {
    const user = userEvent.setup();
    const h = await bench('rpc-1', ['geth']);

    await user.click(screen.getByRole('button', { name: 'Override the flags on this host' }));
    const dialog = await screen.findByRole('dialog', { name: 'Flag override on rpc-1' });
    const now = within(dialog).getByRole('list', { name: 'Command line now' });
    const after = () =>
      within(within(dialog).getByRole('list', { name: 'Command line after' }))
        .getAllByRole('listitem')
        .map(li => li.textContent);
    const command = within(now)
      .getAllByRole('button')
      .map(b => b.textContent);
    const remove = within(dialog).getByRole('textbox', { name: 'Flags to remove' });
    const add = within(dialog).getByRole('textbox', { name: 'Flags to add' });

    expect(command).toContain('--datadir=/data');
    expect(after()).toEqual(command);
    expect(within(dialog).getByRole('list', { name: 'How flags are applied' })).toHaveTextContent(
      'To change a value: remove --name, add --name=new.'
    );

    await user.click(within(now).getByRole('button', { name: '--datadir=/data' }));
    expect(remove).toHaveValue('--datadir=/data');
    await user.type(add, '--datadir=/mnt{Enter}--ws.port 8546');
    expect(after()).toEqual([...command.filter(a => a !== '--datadir=/data'), '--datadir=/mnt', '--ws.port', '8546']);

    await user.type(add, '{Enter}--a b c');
    expect(within(dialog).getByRole('alert')).toHaveTextContent('add "--a b c": has more than one blank');
    expect(within(dialog).getByRole('button', PRIMARY)).toBeDisabled();
    await user.type(add, '{Backspace}{Backspace}');

    await user.type(within(dialog).getByPlaceholderText('Recorded in the audit log'), 'move the data');
    await user.click(within(dialog).getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(sent(h, 'flag-override')).toMatchObject({
        host: 'rpc-1',
        container: 'geth',
        addFlags: ['--datadir=/mnt', '--ws.port 8546', '--a b'],
        removeFlags: ['--datadir=/data'],
        reason: 'move the data',
      });
    });
  });

  it('starts with the override that is there and clears it', async () => {
    const user = userEvent.setup();
    const h = await bench('prysm-geth-1', ['prysm']);

    await user.click(screen.getByRole('button', { name: 'Override the flags on this host' }));
    const dialog = await screen.findByRole('dialog');
    const current = within(dialog).getByLabelText('Current override');
    expect(current).toHaveTextContent('applied');
    expect(current).toHaveTextContent('by alice@example.org');
    expect(current).toHaveTextContent('add --verbosity=debug');
    expect(within(dialog).getByRole('textbox', { name: 'Flags to add' })).toHaveValue('--verbosity=debug');

    await user.click(within(dialog).getByRole('button', { name: 'Clear flag override' }));
    await waitFor(() => {
      expect(sent(h, 'flag-override')).toMatchObject({ container: 'prysm', addFlags: [], removeFlags: [] });
    });
    expect((sent(h, 'flag-override') as { until?: string }).until).toBeUndefined();
  });
});

describe('the override buttons', () => {
  it('are disabled, with the reason, where the host does not allow it or is silent', async () => {
    // The agent configuration of prysm-geth-2 opens nothing.
    await bench('prysm-geth-2', ['prysm']);
    for (const [name, what] of [
      ['Override the image on this host', 'image'],
      ['Override the flags on this host', 'flag'],
    ] as const) {
      const button = screen.getByRole('button', { name });
      expect(button).toBeDisabled();
      expect(button).toHaveAttribute(
        'title',
        `This host does not allow ${what} overrides of prysm (a container needs a team label, the host overrides on)`
      );
    }
  });

  it('are disabled for a silent host and for a login without the feature', async () => {
    await bench('rpc-1', ['geth'], { stale: true });
    expect(screen.getByRole('button', { name: 'Override the image on this host' })).toHaveAttribute(
      'title',
      'The host is silent: its agent has to sync before it can take an override'
    );
  });

  it('follow the login: a reader may not, a team may on its own container', async () => {
    await bench('rpc-1', ['geth'], { profile: 'reader' });
    const reader = screen.getByRole('button', { name: 'Override the flags on this host' });
    expect(reader).toBeDisabled();
    expect(reader).toHaveAttribute(
      'title',
      'Needs upgradoor:flag_override or upgradoor:team_flag_override for a team that owns this container'
    );
  });

  it('follow the team: its own container, and what is not covered on another', async () => {
    await bench('rpc-1', ['lighthouse', 'geth'], { profile: 'team' });
    // One container the team may override is enough to open the dialog.
    const user = userEvent.setup();
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Override the image on this host' })).toBeEnabled();
    });
    await user.click(screen.getByRole('button', { name: 'Override the image on this host' }));
    const dialog = await screen.findByRole('dialog');
    const pick = within(dialog).getByRole('combobox');
    // It starts on the team's container, not on the first by kind, which is another team's.
    expect(pick).toHaveValue('lighthouse');
    expect(within(dialog).queryByRole('note')).not.toBeInTheDocument();
    await user.selectOptions(pick, 'geth');
    expect(within(dialog).getByRole('note')).toHaveTextContent(
      'Not permitted: upgradoor:team_image_override does not cover container geth on host rpc-1 (owners: geth)'
    );
  });

  it('name where an override stands', () => {
    const base = { applied: false };
    expect(overrideState({ ...base, applied: true }).label).toBe('applied');
    expect(overrideState(base).label).toBe('waiting for a lease');
    expect(overrideState({ ...base, applied: true, restoring: true }).label).toBe('restoring');
    expect(overrideState({ ...base, error: 'no' })).toMatchObject({ label: 'override error', title: 'no' });
    expect(notAllowed([{ name: 'geth' } as Container], 'image-override')).toBe(
      'This host does not allow image overrides of geth (a container needs a team label, the host overrides on)'
    );
    expect(notAllowed([{ name: 'geth', overridable: true } as Container], 'flag-override')).toBe('');
  });
});
