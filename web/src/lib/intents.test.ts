import { describe, expect, it } from 'vitest';

import { describeCall } from './actionRequests';
import { endLeaseIntent } from './intents';

describe('endLeaseIntent', () => {
  it('describes the end of a lease as a request that ends it as failed', () => {
    const intent = endLeaseIntent('ls-1', 'host-a');
    if (intent.kind !== 'call') throw new Error('not a call intent');
    expect(intent.title).toBe('End the lease of host-a');

    const call = intent.make('why');
    expect(call).toStrictEqual({ action: 'end-lease', body: { lease: 'ls-1', reason: 'why' } });

    const text = describeCall(call).join(' ');
    expect(text).toContain('completes the step in progress');
    expect(text).toContain('No hold is set');
    expect(text).toContain('stays open');
    expect(text).toContain('then it ends as failed');
  });
});
