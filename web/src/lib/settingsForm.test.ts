import { describe, expect, it } from 'vitest';

import { defaultSettings } from '../mock/fixture';
import type { SettingsBody } from '../api/types';
import golden from '../api/wire.golden.json';
import {
  FIELDS,
  changedSettings,
  errorField,
  fieldSpec,
  fromForm,
  kindPolicy,
  kindRows,
  teamRows,
  sameValue,
  toForm,
  validateField,
  validateForm,
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

describe('the baseline grace and the pull limit', () => {
  it('take a grace between ready stable for and the readiness timeout', () => {
    const form = toForm(defaultSettings());
    expect(form.baselineGrace).toBe('15m');
    expect(validateForm(form).baselineGrace).toBeUndefined();

    expect(validateForm({ ...form, baselineGrace: '45m' }).baselineGrace).toBe(
      'must be between Ready stable for and Readiness timeout'
    );
    expect(validateForm({ ...form, baselineGrace: '30s' }).baselineGrace).toBe(
      'must be between Ready stable for and Readiness timeout'
    );
    expect(validateForm({ ...form, baselineGrace: 'soon' }).baselineGrace).toBe(
      'must be a duration like 30m, 90s or 1h30m'
    );
    expect(fromForm({ ...form, baselineGrace: '20m' }).baselineGrace).toBe('20m');
  });

  it('take 0 to 10000 pulls at once, 0 for no bound', () => {
    const pulls = fieldSpec('maxConcurrentPulls');
    for (const text of ['0', '10', '10000']) expect(validateField(pulls, text), text).toBeNull();
    for (const text of ['-1', '10001', '2.5', '']) expect(validateField(pulls, text), text).not.toBeNull();
    expect(fromForm({ ...toForm(defaultSettings()), maxConcurrentPulls: '0' }).maxConcurrentPulls).toBe(0);
  });

  it('find the setting a message names first', () => {
    expect(errorField('baselineGrace must be between readyStableFor and readinessTimeout')).toBe('baselineGrace');
  });
});

describe('the settings form', () => {
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

describe('the policy per team', () => {
  const teams = fieldSpec('policyClients');

  it('lists the teams of each kind, and those with a policy under it', () => {
    const rows = teamRows(
      [
        { name: 'lighthouse', kinds: ['cl', 'vc', 'cl'] },
        { name: 'geth', kinds: ['el'] },
        { name: 'exporter', kinds: [''] },
      ],
      { cl: { gone: 'manual' }, el: { geth: 'auto' } }
    );
    expect(Object.fromEntries(rows)).toEqual({
      cl: ['gone', 'lighthouse'],
      vc: ['lighthouse'],
      el: ['geth'],
      other: ['exporter'],
    });
  });

  it('inherits the kind, which inherits the image policy', () => {
    const form = { ...toForm(defaultSettings()), policyImage: 'manual', policyKinds: '{"cl":"auto"}' };
    expect(kindPolicy(form, 'cl')).toBe('auto');
    expect(kindPolicy(form, 'el')).toBe('manual');
  });

  it('is sent as a whole map and says what is set', () => {
    const stored = defaultSettings();
    const form = { ...toForm(stored), policyClients: '{"vc":{},"cl":{"teku":"auto","lighthouse":"manual"}}' };
    expect(changedSettings(form, stored)).toEqual({ policyClients: { cl: { lighthouse: 'manual', teku: 'auto' } } });
    expect(sameValue(teams, '{"el":{}}', '{}')).toBe(true);
    expect(valueText(teams, '{}')).toBe('every team inherits');
    expect(valueText(teams, form.policyClients)).toBe('lighthouse cl manual, teku cl auto');
    expect(validateField(teams, '{"el":{"geth":"later"}}')).toBe(
      'must be an object of kind to an object of team to auto or manual'
    );
    expect(validateField(teams, '{"geth":"manual"}')).not.toBeNull();
  });
});
