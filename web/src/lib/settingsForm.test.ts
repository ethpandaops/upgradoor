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
  sameValue,
  toForm,
  validateField,
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

describe('the groups of the settings page', () => {
  // The settings as the controller answers them (written by TestWireFiles in
  // pkg/controller/api): a setting the controller gains and the page does not
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

    // The groups that stand side by side have the same number of lines, so their cards line up.
    const lines = GROUPS.filter(g => !g.wide).map(g => g.keys.length);
    expect(new Set(lines).size).toBe(1);
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
