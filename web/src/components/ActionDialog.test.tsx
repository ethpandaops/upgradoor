import { cleanup, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { ActionResult } from '../api/types';
import { buildHoldRequest, buildRolloutHostsRequest, buildScopeRequest } from '../lib/actionRequests';
import { createHarness, renderWithApp, type Harness, PRIMARY } from '../test/harness';
import { offlineIntent } from '../lib/intents';
import { ActionDialog, type ActionIntent } from './ActionDialog';

async function open(intent: ActionIntent) {
  const h = await createHarness({ mode: 'auth', loggedIn: true });
  const onDone = vi.fn<(r: ActionResult) => void>();
  const onClose = vi.fn();
  renderWithApp(<ActionDialog intent={intent} onClose={onClose} onDone={onDone} />, h);
  return { h, onDone, onClose, user: userEvent.setup(), dialog: screen.getByRole('dialog') };
}

/** posted returns the one action request the dialog sent. */
function posted(h: Harness): { path: string; body: unknown; authorization: string | null } {
  const posts = h.calls.filter(c => c.method === 'POST');
  expect(posts).toHaveLength(1);
  const call = posts[0];
  if (!call) throw new Error('no POST recorded');
  return { path: call.path, body: call.body, authorization: call.authorization };
}

describe('ActionDialog: roll', () => {
  const intent: ActionIntent = {
    kind: 'roll',
    hosts: ['lighthouse-geth-4', 'lighthouse-geth-3'],
    all: false,
    containerChoices: ['geth', 'lighthouse', 'lighthouse-validator'],
  };

  it('sends the default staggered roll of a selection as scheduler.RollRequest', async () => {
    const { h, user, onDone } = await open(intent);

    await user.click(screen.getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(onDone).toHaveBeenCalledTimes(1);
    });

    const call = posted(h);
    expect(call.path).toBe('/api/v1/actions/roll');
    expect(call.authorization).toBe('Bearer mock-token');
    expect(call.body).toStrictEqual({
      hosts: ['lighthouse-geth-3', 'lighthouse-geth-4'],
      all: false,
      containers: [],
      staggered: true,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      reason: '',
    });
    expect(onDone.mock.calls[0]?.[0].hosts).toEqual(['lighthouse-geth-3', 'lighthouse-geth-4']);
  });

  it('sends containers, force, ignoreBudget and the reason exactly as chosen', async () => {
    const { h, user, onDone, dialog } = await open(intent);

    await user.click(within(dialog).getByRole('checkbox', { name: /^Force/ }));
    await user.click(within(dialog).getByRole('checkbox', { name: 'lighthouse' }));
    await user.click(within(dialog).getByRole('checkbox', { name: 'geth' }));
    await user.click(within(dialog).getByRole('checkbox', { name: /^Ignore budget/ }));
    await user.type(within(dialog).getByRole('textbox'), '  hotfix for the sync bug ');

    const summary = within(dialog).getByLabelText('What will happen');
    expect(summary).toHaveTextContent('Roll 2 containers (geth, lighthouse) on 2 hosts');
    expect(summary).toHaveTextContent('IGNORE BUDGET');

    await user.click(screen.getByRole('button', { name: 'Confirm, ignoring the budget' }));
    await waitFor(() => {
      expect(onDone).toHaveBeenCalledTimes(1);
    });

    expect(posted(h).body).toStrictEqual({
      hosts: ['lighthouse-geth-3', 'lighthouse-geth-4'],
      all: false,
      containers: ['geth', 'lighthouse'],
      staggered: true,
      force: true,
      ignoreBudget: true,
      ignoreGate: false,
      reason: 'hotfix for the sync bug',
    });
  });

  it('"now" never sends force, even if it was ticked before', async () => {
    const { h, user, onDone, dialog } = await open(intent);

    await user.click(within(dialog).getByRole('checkbox', { name: /^Force/ }));
    await user.click(within(dialog).getByRole('radio', { name: /^Now/ }));
    expect(within(dialog).getByRole('checkbox', { name: /^Force/ })).toBeDisabled();

    await user.click(screen.getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(onDone).toHaveBeenCalledTimes(1);
    });

    expect(posted(h).body).toMatchObject({ staggered: false, force: false, ignoreBudget: false });
  });

  it('defaults a single host to "now"', async () => {
    const single = await open({ kind: 'roll', hosts: ['rpc-1'], all: false, containerChoices: [] });
    expect(within(single.dialog).getByRole('radio', { name: /^Now/ })).toBeChecked();
    await single.user.click(screen.getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(single.onDone).toHaveBeenCalledTimes(1);
    });
    expect(posted(single.h).body).toStrictEqual({
      hosts: ['rpc-1'],
      all: false,
      containers: [],
      staggered: false,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      reason: '',
    });
  });

  it('rolls the whole fleet with all=true and no host list', async () => {
    const { h, user, onDone, dialog } = await open({ kind: 'roll', hosts: [], all: true, containerChoices: [] });

    expect(within(dialog).getByRole('radio', { name: /^Staggered/ })).toBeChecked();
    expect(within(dialog).getByLabelText('What will happen')).toHaveTextContent('every host of the fleet');

    await user.click(screen.getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(onDone).toHaveBeenCalledTimes(1);
    });

    expect(posted(h).body).toStrictEqual({
      hosts: [],
      all: true,
      containers: [],
      staggered: true,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      reason: '',
    });
  });
});

describe('ActionDialog: despite the network state', () => {
  const intent: ActionIntent = { kind: 'reboot', hosts: ['teku-besu-1'], all: false };

  it('offers to pass the gate only while it is closed, with its reasons, and sends ignoreGate', async () => {
    const h = await createHarness({ mode: 'auth', loggedIn: true });
    h.controller.setGateFailing(true);
    const onDone = vi.fn<(r: ActionResult) => void>();
    const user = userEvent.setup();
    renderWithApp(<ActionDialog intent={intent} onClose={vi.fn()} onDone={onDone} />, h);

    const despite = await screen.findByRole('checkbox', { name: /^Roll despite network state/ });
    expect(despite.closest('label')).toHaveTextContent(/The network gate is closed: .+/);
    await user.click(despite);
    await user.click(screen.getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(onDone).toHaveBeenCalledTimes(1);
    });
    expect(posted(h).body).toMatchObject({ ignoreGate: true });
  });

  it('does not offer it while the gate is open', async () => {
    await open(intent);
    expect(screen.queryByRole('checkbox', { name: /^Roll despite network state/ })).not.toBeInTheDocument();
  });
});

describe('ActionDialog: reboot', () => {
  it('sends scheduler.RebootRequest without a containers field', async () => {
    const { h, user, onDone, dialog } = await open({
      kind: 'reboot',
      hosts: ['teku-besu-2', 'teku-besu-1'],
      all: false,
    });

    expect(within(dialog).queryByText(/Containers/)).not.toBeInTheDocument();
    await user.type(within(dialog).getByRole('textbox'), 'kernel 6.12.57');
    await user.click(screen.getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(onDone).toHaveBeenCalledTimes(1);
    });

    const call = posted(h);
    expect(call.path).toBe('/api/v1/actions/reboot');
    expect(call.body).toStrictEqual({
      hosts: ['teku-besu-1', 'teku-besu-2'],
      all: false,
      staggered: true,
      force: false,
      ignoreBudget: false,
      ignoreGate: false,
      reason: 'kernel 6.12.57',
    });
  });
});

describe('ActionDialog: simple actions', () => {
  it.each([
    {
      name: 'hold',
      intent: {
        kind: 'call',
        title: 'Hold 1 host',
        make: reason => ({ action: 'hold', body: buildHoldRequest(['rpc-1'], reason) }),
      } satisfies ActionIntent,
      path: '/api/v1/actions/hold',
      body: { hosts: ['rpc-1'], reason: 'because' },
    },
    {
      name: 'unhold',
      intent: {
        kind: 'call',
        title: 'Unhold 1 host',
        make: reason => ({ action: 'unhold', body: buildHoldRequest(['rpc-1'], reason) }),
      } satisfies ActionIntent,
      path: '/api/v1/actions/unhold',
      body: { hosts: ['rpc-1'], reason: 'because' },
    },
    {
      name: 'forget',
      intent: {
        kind: 'call',
        title: 'Forget 1 host',
        danger: true,
        make: reason => ({ action: 'forget', body: buildHoldRequest(['tooling-1'], reason) }),
      } satisfies ActionIntent,
      path: '/api/v1/actions/forget',
      body: { hosts: ['tooling-1'], reason: 'because' },
    },
    {
      name: 'global pause',
      intent: {
        kind: 'call',
        title: 'Pause everything',
        make: reason => ({ action: 'pause', body: buildScopeRequest('', reason) }),
      } satisfies ActionIntent,
      path: '/api/v1/actions/pause',
      body: { rollout: '', reason: 'because' },
    },
    {
      name: 'global resume',
      intent: {
        kind: 'call',
        title: 'Resume everything',
        make: reason => ({ action: 'resume', body: buildScopeRequest('', reason) }),
      } satisfies ActionIntent,
      path: '/api/v1/actions/resume',
      body: { rollout: '', reason: 'because' },
    },
  ])('$name sends its exact body', async ({ intent, path, body }) => {
    const { h, user, onDone } = await open(intent);

    await user.type(screen.getByRole('textbox'), 'because');
    await user.click(screen.getByRole('button', PRIMARY));
    await waitFor(() => {
      expect(onDone).toHaveBeenCalledTimes(1);
    });

    const call = posted(h);
    expect(call.path).toBe(path);
    expect(call.body).toStrictEqual(body);
  });

  it('sends skip, retry and abort for a rollout', async () => {
    const probe = await createHarness({ mode: 'open' });
    const rollouts = probe.controller.data.rollouts;
    const halted = rollouts.find(r => r.state === 'halted');
    if (!halted) throw new Error('the fixture has no halted rollout');

    const cases: { intent: ActionIntent; path: string; body: unknown }[] = [
      {
        intent: {
          kind: 'call',
          title: 'Skip',
          make: reason => ({
            action: 'skip',
            body: buildRolloutHostsRequest(halted.id, ['nimbus-nethermind-2'], reason),
          }),
        },
        path: '/api/v1/actions/skip',
        body: { rollout: halted.id, hosts: ['nimbus-nethermind-2'], reason: '' },
      },
      {
        intent: {
          kind: 'call',
          title: 'Retry',
          make: reason => ({ action: 'retry', body: buildRolloutHostsRequest(halted.id, [], reason) }),
        },
        path: '/api/v1/actions/retry',
        body: { rollout: halted.id, hosts: [], reason: '' },
      },
      {
        intent: {
          kind: 'call',
          title: 'Abort',
          danger: true,
          make: reason => ({ action: 'abort', body: buildScopeRequest(halted.id, reason) }),
        },
        path: '/api/v1/actions/abort',
        body: { rollout: halted.id, reason: '' },
      },
    ];

    for (const c of cases) {
      const { h, user, onDone } = await open(c.intent);
      await user.click(screen.getByRole('button', PRIMARY));
      await waitFor(() => {
        expect(onDone).toHaveBeenCalledTimes(1);
      });
      const call = posted(h);
      expect(call.path).toBe(c.path);
      expect(call.body).toStrictEqual(c.body);
      cleanup();
    }
  });
});

describe('ActionDialog: behaviour', () => {
  it('states what will happen, shows a failed action and stays open', async () => {
    const { h, user, onDone, dialog } = await open({
      kind: 'call',
      title: 'Abort',
      danger: true,
      make: reason => ({ action: 'abort', body: buildScopeRequest('ro-does-not-exist', reason) }),
    });

    expect(within(dialog).getByLabelText('What will happen')).toHaveTextContent(
      'Abort rollout ro-does-not-exist. A manual rollout is closed'
    );

    await user.click(screen.getByRole('button', PRIMARY));

    expect(await within(dialog).findByRole('alert')).toHaveTextContent('unknown rollout ro-does-not-exist (HTTP 404)');
    expect(onDone).not.toHaveBeenCalled();
    expect(posted(h).path).toBe('/api/v1/actions/abort');
    expect(screen.getByRole('button', PRIMARY)).toBeEnabled();
  });

  it('warns about targets that the view hides, and only then', async () => {
    const base: ActionIntent = { kind: 'reboot', hosts: ['geth-1', 'geth-2', 'besu-1'], all: false };

    const shown = await open(base);
    expect(within(shown.dialog).queryByRole('alert')).not.toBeInTheDocument();
    cleanup();

    const { dialog } = await open({ ...base, hiddenHosts: ['geth-1', 'geth-2'] });
    expect(within(dialog).getByRole('alert')).toHaveTextContent(
      '2 of them are hidden by the current filter: geth-1, geth-2'
    );
  });

  it('closes on Escape and on Cancel without sending anything', async () => {
    const { h, user, onClose } = await open({ kind: 'reboot', hosts: ['rpc-1'], all: false });

    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalledTimes(2);
    expect(h.calls.filter(c => c.method === 'POST')).toEqual([]);
  });

  it('focuses the reason field and keeps Tab inside the dialog', async () => {
    const { user, dialog } = await open({
      kind: 'call',
      title: 'Hold 1 host',
      make: reason => ({ action: 'hold', body: buildHoldRequest(['rpc-1'], reason) }),
    });

    expect(within(dialog).getByRole('textbox')).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', PRIMARY)).toHaveFocus();
    await user.tab();
    expect(within(dialog).getByRole('textbox')).toHaveFocus();
  });
});

describe('ActionDialog: mark offline', () => {
  it('needs a reason and sends the end that was picked', async () => {
    const { h, user, dialog } = await open(offlineIntent(true, ['teku-besu-1']));

    expect(screen.getByRole('button', { name: 'Mark offline' })).toBeDisabled();
    await user.type(within(dialog).getByRole('textbox'), 'client kept broken for debugging');
    await user.click(within(dialog).getByRole('button', { name: '6h' }));
    const before = Date.now();
    await user.click(screen.getByRole('button', { name: 'Mark offline' }));

    await waitFor(() => {
      expect(h.calls.some(c => c.method === 'POST')).toBe(true);
    });
    const { path, body } = posted(h);
    expect(path).toBe('/api/v1/actions/mark-offline');
    const sent = body as { hosts: string[]; reason: string; until: string };
    expect(sent.hosts).toEqual(['teku-besu-1']);
    expect(sent.reason).toBe('client kept broken for debugging');
    expect(Date.parse(sent.until) - before).toBeGreaterThan(6 * 3_600_000 - 60_000);
  });
});
