import { describe, expect, it } from 'vitest';

import { describeCall } from './actionRequests';
import { endLeaseIntent, holdIntent } from './intents';

describe('endLeaseIntent', () => {
  it('describes the end of a lease as a request that ends it as cancelled', () => {
    const intent = endLeaseIntent({ id: 'ls-1', host: 'host-a', needs: [] });
    if (intent.kind !== 'call') throw new Error('not a call intent');
    expect(intent.title).toBe('End the lease of host-a');

    const call = intent.make('why');
    expect(call).toStrictEqual({ action: 'end-lease', body: { lease: 'ls-1', reason: 'why' } });

    const text = describeCall(call).join(' ');
    expect(text).toContain('completes the step in progress');
    expect(text).toContain('No hold is set');
    expect(text).toContain('stays open');
    expect(text).toContain('then it ends as cancelled');
  });
});

describe('holdIntent', () => {
  it('names the holds an unhold lifts', () => {
    const intent = holdIntent('unhold', ['host-a'], [], ['geth']);
    if (intent.kind !== 'call') throw new Error('not a call intent');
    expect(intent.title).toBe('Unhold host-a (hold of geth)');
    expect(intent.make('done')).toStrictEqual({
      action: 'unhold',
      body: { hosts: ['host-a'], reason: 'done', holders: ['geth'] },
    });

    const plain = holdIntent('hold', ['host-a']);
    if (plain.kind !== 'call') throw new Error('not a call intent');
    expect(plain.make('')).toStrictEqual({ action: 'hold', body: { hosts: ['host-a'], reason: '' } });
  });
});
