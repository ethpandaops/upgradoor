import { describe, expect, it } from 'vitest';

import { defaultSettings } from '../mock/fixture';
import type { SettingsBody } from '../api/types';
import golden from '../api/wire.golden.json';
import {
  ELSEWHERE,
  FIELDS,
  GROUPS,
  ROLLOUT_KINDS,
  changedSettings,
  errorField,
  fieldSpec,
  fromForm,
  groupKeys,
  kindRows,
  sameValue,
  toForm,
  validateField,
  valueText,
  type FieldSpec,
} from './settingsForm';

const share = FIELDS.find(f => f.key === 'readyPeerShare') as FieldSpec;

describe('the ready peer share field', () => {
  it('takes a share the way people type it', () => {
    for (const text of ['0', '0.25', '.5', '0.', '1', ' 0.4 ']) {
      expect(validateField(share, text), text).toBeNull();
    }

    for (const text of ['', '.', '1.5', '-0.1', 'half', '0,5', '1e-1']) {
      expect(validateField(share, text), text).not.toBeNull();
    }
  });

  it('sends .5 as the number 0.5 and sees it as the same value', () => {
    const form = { ...toForm(defaultSettings()), readyPeerShare: '.5' };

    expect(fromForm(form).readyPeerShare).toBe(0.5);
    expect(sameValue(share, '.5', '0.5')).toBe(true);
  });

  it('is off by default', () => {
    expect(defaultSettings().readyPeerShare).toBe(0);
    expect(toForm(defaultSettings()).readyPeerShare).toBe('0');
  });
});

describe('the readiness fields', () => {
  const attestations = fieldSpec('readyAttestations');
  const stable = fieldSpec('readyStableFor');

  it('take 1 to 32 slots and 0 to 30m', () => {
    for (const text of ['1', '2', '32']) expect(validateField(attestations, text), text).toBeNull();
    for (const text of ['0', '33', '1.5']) expect(validateField(attestations, text), text).not.toBeNull();

    for (const text of ['0', '0s', '90s', '30m']) expect(validateField(stable, text), text).toBeNull();
    for (const text of ['30m1s', '1h', 'soon']) expect(validateField(stable, text), text).not.toBeNull();
  });

  it('send 0 as a Go duration', () => {
    const form = { ...toForm(defaultSettings()), readyStableFor: '0' };
    expect(fromForm(form).readyStableFor).toBe('0s');
  });
});

describe('the groups of the settings page', () => {
  // The settings as the controller answers them (written by TestWireFiles in
  // pkg/controller/userapi): a setting the controller gains and the page does not
  // place fails here.
  const served = Object.keys((golden.rest.settings as { settings: object }).settings).sort();

  it('place every setting the controller has exactly once', () => {
    const placed: string[] = GROUPS.flatMap(groupKeys);

    // A setting is a field of the form, or it is named as edited elsewhere
    // (the image policy per client, on the Clients tab); never both, never neither.
    expect([...placed, ...ELSEWHERE].sort()).toEqual(served);
    expect(new Set(placed).size).toBe(placed.length);
    expect([...FIELDS.map(f => f.key), ...ELSEWHERE].sort()).toEqual(served);
  });

  it('put the canary switch of a kind next to its policy', () => {
    for (const kind of ROLLOUT_KINDS) {
      expect(fieldSpec(kind.policy).kind).toBe('policy');
      expect(fieldSpec(kind.canary).kind).toBe('switch');
    }
    expect(GROUPS.filter(g => g.kinds)).toHaveLength(1);
  });

  it('find the setting a message of the controller is about', () => {
    expect(errorField('minPeers must be at least 1')).toBe('minPeers');
    expect(errorField('leaseTTL must be at least 1m')).toBe('leaseTtl');
    expect(errorField('pauseAfterCanaryImage must be a boolean')).toBe('pauseAfterCanaryImage');
    expect(errorField('a valid bearer token is required')).toBeUndefined();
  });

  it('send only the settings that changed', () => {
    const stored = (golden.rest.settings as unknown as SettingsBody).settings;
    const form = { ...toForm(stored), minPeers: '5', leaseTtl: '1800s' };

    expect(changedSettings(form, stored)).toEqual({ minPeers: 5 });
  });
});

describe('the policy per kind', () => {
  const kinds = fieldSpec('policyKinds');

  it('offers the kinds of the fleet, those with a policy, and other last', () => {
    expect(kindRows(['el', 'cl', 'snooper'], { blobber: 'manual', other: 'auto' }, { cl: 'manual' })).toEqual([
      'el',
      'cl',
      'snooper',
      'blobber',
      'other',
    ]);
    expect(kindRows([])).toEqual(['other']);
  });

  it('sends the whole map when it changed, and sees another order as the same', () => {
    const stored = { ...defaultSettings(), policyKinds: { cl: 'manual' as const, other: 'auto' as const } };
    const form = { ...toForm(stored), policyKinds: '{"other":"auto","cl":"manual","vc":"manual"}' };

    expect(changedSettings(form, stored)).toEqual({ policyKinds: { cl: 'manual', other: 'auto', vc: 'manual' } });
    expect(sameValue(kinds, '{"other":"auto","cl":"manual"}', toForm(stored).policyKinds)).toBe(true);
  });

  it('says what is set, and refuses what is no kind to policy', () => {
    expect(valueText(kinds, '{}')).toBe('every kind inherits');
    expect(valueText(kinds, '{"cl":"manual","other":"auto"}')).toBe('cl manual, other auto');
    expect(validateField(kinds, '{"none":"manual"}')).toBe('none is not a kind');
    expect(validateField(kinds, '{"cl":"sometimes"}')).toBe('must be an object of kind to auto or manual');
    expect(validateField(kinds, '{"cl":"manual"}')).toBeNull();
  });
});
