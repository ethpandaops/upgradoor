// The settings form model: text inputs in, a validated fleet.Settings out.
// The rules mirror fleet.Settings.Validate.

import type { Policy, Settings } from '../api/types';
import { formatGoDuration, normalizeGoDuration, parseGoDuration } from './goDuration';

export type SettingKey = keyof Settings;

export type FieldKind = 'percent' | 'count' | 'duration' | 'policy' | 'switch';

export interface FieldSpec {
  key: SettingKey;
  label: string;
  kind: FieldKind;
  /** hint explains the setting and its valid range. */
  hint: string;
  /** min is the smallest valid value: a number, or milliseconds for a duration. */
  min?: number;
}

export const POLICIES: readonly Policy[] = ['auto', 'manual', 'paused'];

const POLICY_HINT = 'auto: roll out by itself · manual: queue and wait for approval · paused: queue, grant nothing';

/** The settings in display order. */
export const FIELDS: readonly FieldSpec[] = [
  {
    key: 'maxUnavailablePercent',
    label: 'Max unavailable',
    kind: 'percent',
    hint: 'Share of tracked validator weight that may be unavailable at once (0–100 %). Also bounds how many hosts hold a lease at once.',
  },
  {
    key: 'minParticipationPercent',
    label: 'Min participation',
    kind: 'percent',
    hint: 'Network gate: no new lease while participation is below this (0–100 %). Also the share of a host’s validators that must attest again before its slot is released.',
  },
  {
    key: 'maxFinalityLagEpochs',
    label: 'Max finality lag',
    kind: 'count',
    min: 2,
    hint: 'Network gate: no new lease while finality lags more than this many epochs. At least 2 (a healthy network lags 2).',
  },
  {
    key: 'readyEpochs',
    label: 'Ready epochs',
    kind: 'count',
    min: 0,
    hint: 'How long a host must be healthy after its operation before its slot is released: the node at head with peers for this many epochs, and its validators attesting in this many complete epochs. 0 releases at the first healthy sync.',
  },
  {
    key: 'minPeers',
    label: 'Min peers',
    kind: 'count',
    min: 1,
    hint: 'Peers each of the execution and the consensus client must have for the node to count as back. At least 1; the controller asks for at most half of the other nodes.',
  },
  {
    key: 'leaseTtl',
    label: 'Lease TTL',
    kind: 'duration',
    min: 60_000,
    hint: 'Time an agent has to start a granted lease, and the silence after which a running lease has failed. At least 1m.',
  },
  {
    key: 'hostStaleAfter',
    label: 'Host stale after',
    kind: 'duration',
    min: 30_000,
    hint: 'Sync silence after which a host counts as stale and is no longer granted. At least 30s.',
  },
  {
    key: 'readinessTimeout',
    label: 'Readiness timeout',
    kind: 'duration',
    min: 60_000,
    hint: 'How long to wait for readiness after the agent reports done before the slot has failed. At least 1m.',
  },
  { key: 'policyImage', label: 'Policy: images', kind: 'policy', hint: POLICY_HINT },
  { key: 'policyPackages', label: 'Policy: packages', kind: 'policy', hint: POLICY_HINT },
  { key: 'policyReboot', label: 'Policy: reboots', kind: 'policy', hint: POLICY_HINT },
  {
    key: 'paused',
    label: 'Global pause',
    kind: 'switch',
    hint: 'Nothing new is granted while set. Open leases finish.',
  },
];

/** SettingsForm holds every field as the text (or switch state) shown in its input. */
export type SettingsForm = Record<SettingKey, string>;

/** displayValue renders one setting the way its input shows it. */
export function displayValue(settings: Settings, key: SettingKey): string {
  const v = settings[key];
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (typeof v === 'number') return String(v);
  const spec = FIELDS.find(f => f.key === key);
  return spec?.kind === 'duration' ? normalizeGoDuration(v) : v;
}

/** toForm turns settings into form values. */
export function toForm(settings: Settings): SettingsForm {
  const form = {} as SettingsForm;
  for (const f of FIELDS) form[f.key] = displayValue(settings, f.key);
  return form;
}

/** validateField returns the problem with one input, or null. */
export function validateField(spec: FieldSpec, raw: string): string | null {
  const text = raw.trim();

  switch (spec.kind) {
    case 'percent': {
      if (text === '' || !/^\d+(\.\d+)?$/.test(text)) return 'must be a number between 0 and 100';
      const n = Number(text);
      return n < 0 || n > 100 ? 'must be between 0 and 100' : null;
    }
    case 'count': {
      if (!/^\d+$/.test(text)) return 'must be a whole number';
      const min = spec.min ?? 0;
      return Number(text) < min ? `must be at least ${min}` : null;
    }
    case 'duration': {
      const ms = parseGoDuration(text);
      if (ms === null) return 'must be a duration like 30m, 90s or 1h30m';
      const min = spec.min ?? 0;
      return ms < min ? `must be at least ${formatGoDuration(min)}` : null;
    }
    case 'policy':
      return (POLICIES as readonly string[]).includes(text) ? null : 'must be auto, manual or paused';
    case 'switch':
      return text === 'true' || text === 'false' ? null : 'must be on or off';
  }
}

export type FormErrors = Partial<Record<SettingKey, string>>;

/** validateForm returns the problems of every field, keyed by setting. */
export function validateForm(form: SettingsForm): FormErrors {
  const errors: FormErrors = {};
  for (const f of FIELDS) {
    const err = validateField(f, form[f.key]);
    if (err) errors[f.key] = err;
  }
  return errors;
}

/** fromForm builds the PUT body from a valid form. Call validateForm first. */
export function fromForm(form: SettingsForm): Settings {
  const duration = (key: SettingKey) => formatGoDuration(parseGoDuration(form[key]) ?? 0);

  return {
    maxUnavailablePercent: Number(form.maxUnavailablePercent),
    minParticipationPercent: Number(form.minParticipationPercent),
    maxFinalityLagEpochs: Number(form.maxFinalityLagEpochs),
    readyEpochs: Number(form.readyEpochs),
    minPeers: Number(form.minPeers),
    leaseTtl: duration('leaseTtl'),
    hostStaleAfter: duration('hostStaleAfter'),
    readinessTimeout: duration('readinessTimeout'),
    policyImage: form.policyImage as Policy,
    policyPackages: form.policyPackages as Policy,
    policyReboot: form.policyReboot as Policy,
    paused: form.paused === 'true',
  };
}

/** sameValue compares an input with a stored value: numerically, as durations, or as text. */
export function sameValue(spec: FieldSpec, a: string, b: string): boolean {
  const x = a.trim();
  const y = b.trim();
  if (x === y) return true;

  if (spec.kind === 'duration') {
    const mx = parseGoDuration(x);
    return mx !== null && mx === parseGoDuration(y);
  }
  if (spec.kind === 'percent' || spec.kind === 'count') {
    return x !== '' && y !== '' && Number(x) === Number(y);
  }
  return false;
}

/** dirtyKeys lists the fields whose input differs from the stored settings. */
export function dirtyKeys(form: SettingsForm, stored: Settings): SettingKey[] {
  return FIELDS.filter(f => !sameValue(f, form[f.key], displayValue(stored, f.key))).map(f => f.key);
}
