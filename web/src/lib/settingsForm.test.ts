import { describe, expect, it } from 'vitest';

import { defaultSettings } from '../mock/fixture';
import { FIELDS, fromForm, sameValue, toForm, validateField, type FieldSpec } from './settingsForm';

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
