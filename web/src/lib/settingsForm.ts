// The settings form model: text inputs in, a validated fleet.Settings out.
// The rules mirror fleet.Settings.Validate. The fields and their places on
// the page are declared in settingsLayout.

import type { Policy, Settings } from '../api/types';
import { formatGoDuration, normalizeGoDuration, parseGoDuration } from './goDuration';
import { FIELDS, POLICIES, type FieldSpec, type SettingKey } from './settingsLayout';

export { FIELDS, POLICIES, POLICY_LABEL, POLICY_MEANING, type FieldSpec, type SettingKey } from './settingsLayout';

/** fieldSpec returns the spec of a setting. */
export function fieldSpec(key: SettingKey): FieldSpec {
  const spec = FIELDS.find(f => f.key === key);
  if (!spec) throw new Error(`no field for the setting ${key}`);
  return spec;
}

/** isPolicyMap: the setting is a policy per name, held as JSON text. */
export function isPolicyMap(spec: FieldSpec): boolean {
  return spec.kind === 'kinds' || spec.kind === 'teams' || spec.kind === 'actions';
}

/** valueText renders a stored or typed value for a person: with its unit, a switch as on or off. */
export function valueText(spec: FieldSpec, value: string): string {
  if (spec.kind === 'switch') return value === 'true' ? 'on' : 'off';
  if (spec.kind === 'actions') {
    const auto = autoActions(parsePolicies(value) ?? {});
    return auto.length === 0 ? 'every action manual' : `${auto.join(', ')} auto`;
  }
  if (spec.kind === 'kinds') {
    const set = Object.entries(parsePolicies(value) ?? {}).sort(([a], [b]) => a.localeCompare(b));
    return set.length === 0 ? 'every kind inherits' : set.map(([name, p]) => `${name} ${p}`).join(', ');
  }
  if (spec.kind === 'teams') {
    const set = Object.entries(parseTeamPolicies(value) ?? {})
      .sort(([a], [b]) => a.localeCompare(b))
      .flatMap(([kind, teams]) =>
        Object.entries(teams)
          .sort(([a], [b]) => a.localeCompare(b))
          .map(([team, p]) => `${team} ${kind} ${p}`)
      );
    return set.length === 0 ? 'every team inherits' : set.join(', ');
  }
  return spec.unit ? `${value} ${spec.unit}` : value;
}

/** shortValue is valueText without the unit, for where the unit stands next to it anyway. */
export function shortValue(spec: FieldSpec, value: string): string {
  return spec.kind === 'switch' || isPolicyMap(spec) ? valueText(spec, value) : value;
}

/** policiesText is the form text of a policy per name: the JSON object, sorted by name. */
export function policiesText(policies: Record<string, Policy>): string {
  return JSON.stringify(Object.fromEntries(Object.entries(policies).sort(([a], [b]) => a.localeCompare(b))));
}

/**
 * kindRows are the kinds the policy per kind offers: those of the fleet, then
 * those with a policy that no container has now, and other (unlabelled) last.
 */
export function kindRows(fleet: readonly string[], ...policies: Record<string, Policy>[]): string[] {
  const named = policies.flatMap(p => Object.keys(p)).sort();
  return [...new Set([...fleet, ...named])].filter(k => k !== 'other').concat('other');
}

/** TeamKinds is a team and the container kinds it runs ('' for unlabelled). */
export interface TeamKinds {
  name: string;
  kinds: readonly string[];
}

/** TeamPolicies is the image policy per kind and team (Settings.policyClients). */
export type TeamPolicies = Record<string, Record<string, Policy>>;

/**
 * teamRows lists the teams of each kind: those that run it (unlabelled
 * containers under other), then those with a policy under it that run none
 * now, sorted by name.
 */
export function teamRows(teams: readonly TeamKinds[], ...policies: TeamPolicies[]): Map<string, string[]> {
  const byKind = new Map<string, Set<string>>();
  const add = (kind: string, team: string) => byKind.set(kind, (byKind.get(kind) ?? new Set()).add(team));
  for (const team of teams) for (const kind of team.kinds) add(kind || 'other', team.name);
  for (const p of policies)
    for (const [kind, named] of Object.entries(p)) for (const team of Object.keys(named)) add(kind, team);
  return new Map([...byKind].map(([kind, names]) => [kind, [...names].sort()]));
}

/** teamPoliciesText is the form text of the policy per kind and team: sorted, without empty kinds. */
export function teamPoliciesText(policies: TeamPolicies): string {
  return JSON.stringify(
    Object.fromEntries(
      Object.entries(policies)
        .filter(([, teams]) => Object.keys(teams).length > 0)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([kind, teams]) => [kind, JSON.parse(policiesText(teams)) as Record<string, Policy>])
    )
  );
}

/** parseTeamPolicies reads the form text of the policy per kind and team; undefined when it is not one. */
export function parseTeamPolicies(text: string): TeamPolicies | undefined {
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof v !== 'object' || v === null || Array.isArray(v)) return undefined;
  const out: TeamPolicies = {};
  for (const [kind, teams] of Object.entries(v as Record<string, unknown>)) {
    const parsed = parsePolicies(JSON.stringify(teams));
    if (!parsed) return undefined;
    out[kind] = parsed;
  }
  return out;
}

/** kindPolicy is what a kind's image rollouts answer to without a team policy: its own, else policyImage. */
export function kindPolicy(form: SettingsForm, kind: string): Policy {
  return parsePolicies(form.policyKinds)?.[kind] ?? (form.policyImage as Policy);
}

/** autoActions are the actions whose policy is auto, sorted: an action that is not listed is manual. */
function autoActions(policies: Record<string, Policy>): string[] {
  return Object.keys(policies)
    .filter(name => policies[name] === 'auto')
    .sort();
}

/** parsePolicies reads the form text of a policy per name; undefined when it is not an object of name to policy. */
export function parsePolicies(text: string): Record<string, Policy> | undefined {
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (typeof v !== 'object' || v === null || Array.isArray(v)) return undefined;
  const entries = Object.entries(v as Record<string, unknown>);
  if (!entries.every(([, p]) => (POLICIES as readonly unknown[]).includes(p))) return undefined;
  return Object.fromEntries(entries) as Record<string, Policy>;
}

/**
 * errorField finds the setting a validation error of the controller is about:
 * its messages start with the setting's name ("minPeers must be at least 1"),
 * other settings may follow.
 */
export function errorField(message: string): SettingKey | undefined {
  const text = message.toLowerCase();
  let best: { key: SettingKey; at: number } | undefined;
  for (const f of FIELDS) {
    const at = text.search(new RegExp(`(^|[^a-z])${f.key.toLowerCase()}([^a-z]|$)`));
    if (at >= 0 && (!best || at < best.at)) best = { key: f.key, at };
  }
  return best?.key;
}

/** SettingsForm holds every field as the text (or switch state) shown in its input. */
export type SettingsForm = Record<SettingKey, string>;

/** displayValue renders one setting the way its input shows it. */
export function displayValue(settings: Settings, key: SettingKey): string {
  const v = settings[key];
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (key === 'policyClients') return teamPoliciesText(v as TeamPolicies);
  if (typeof v === 'object') return policiesText(v as Record<string, Policy>);
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
    case 'share': {
      // ".5" and "0." are numbers too.
      if (!/^(\d+(\.\d*)?|\.\d+)$/.test(text)) return 'must be a number between 0 and 1';
      const n = Number(text);
      return n < 0 || n > 1 ? 'must be between 0 and 1' : null;
    }
    case 'count': {
      if (text === '') return 'must be a whole number';
      if (!/^\d+$/.test(text)) return 'must be a whole number';
      const min = spec.min ?? 0;
      if (Number(text) < min) return `must be at least ${min}`;
      return spec.max !== undefined && Number(text) > spec.max ? `must be at most ${spec.max}` : null;
    }
    case 'duration': {
      const ms = parseGoDuration(text);
      if (ms === null) return 'must be a duration like 30m, 90s or 1h30m';
      const min = spec.min ?? 0;
      if (ms < min) return `must be at least ${formatGoDuration(min)}`;
      return spec.max !== undefined && ms > spec.max ? `must be at most ${formatGoDuration(spec.max)}` : null;
    }
    case 'policy':
      return (POLICIES as readonly string[]).includes(text) ? null : 'must be auto, manual or paused';
    case 'switch':
      return text === 'true' || text === 'false' ? null : 'must be on or off';
    case 'actions':
      return parsePolicies(text) ? null : 'must be an object of action to auto or manual';
    case 'kinds': {
      const policies = parsePolicies(text);
      if (!policies) return 'must be an object of kind to auto or manual';
      return 'none' in policies ? 'none is not a kind' : null;
    }
    case 'teams':
      return parseTeamPolicies(text) ? null : 'must be an object of kind to an object of team to auto or manual';
  }
}

type FormErrors = Partial<Record<SettingKey, string>>;

/** validateForm returns the problems of every field, keyed by setting; baselineGrace also against its neighbours. */
export function validateForm(form: SettingsForm): FormErrors {
  const errors: FormErrors = {};
  for (const f of FIELDS) {
    const err = validateField(f, form[f.key]);
    if (err) errors[f.key] = err;
  }

  const grace = parseGoDuration(form.baselineGrace);
  const stable = parseGoDuration(form.readyStableFor);
  const timeout = parseGoDuration(form.readinessTimeout);
  if (!errors.baselineGrace && grace !== null && stable !== null && timeout !== null) {
    if (grace < stable || grace > timeout)
      errors.baselineGrace = 'must be between Ready stable for and Readiness timeout';
  }

  return errors;
}

/** fromForm builds the PUT body from a valid form. Call validateForm first. */
export function fromForm(form: SettingsForm): Settings {
  const duration = (key: SettingKey) => formatGoDuration(parseGoDuration(form[key]) ?? 0);

  return {
    maxUnavailablePercent: Number(form.maxUnavailablePercent),
    maxLeasedPercent: Number(form.maxLeasedPercent),
    maxIndependentPercent: Number(form.maxIndependentPercent),
    minParticipationPercent: Number(form.minParticipationPercent),
    maxFinalityLagEpochs: Number(form.maxFinalityLagEpochs),
    readyAttestations: Number(form.readyAttestations),
    readyStableFor: duration('readyStableFor'),
    minPeers: Number(form.minPeers),
    leaseTtl: duration('leaseTtl'),
    hostStaleAfter: duration('hostStaleAfter'),
    readinessTimeout: duration('readinessTimeout'),
    baselineGrace: duration('baselineGrace'),
    maxConcurrentPulls: Number(form.maxConcurrentPulls),
    policyImage: form.policyImage as Policy,
    policyPackages: form.policyPackages as Policy,
    policyReboot: form.policyReboot as Policy,
    pauseAfterCanaryImage: form.pauseAfterCanaryImage === 'true',
    pauseAfterCanaryPackages: form.pauseAfterCanaryPackages === 'true',
    pauseAfterCanaryReboot: form.pauseAfterCanaryReboot === 'true',
    readyPeerShare: Number(form.readyPeerShare),
    lookupInterval: duration('lookupInterval'),
    paused: form.paused === 'true',
    actionPolicies: parsePolicies(form.actionPolicies) ?? {},
    policyKinds: parsePolicies(form.policyKinds) ?? {},
    policyClients: JSON.parse(teamPoliciesText(parseTeamPolicies(form.policyClients) ?? {})) as TeamPolicies,
  };
}

/** sameValue compares an input with a stored value: numerically, as durations, or as text. */
export function sameValue(spec: FieldSpec, a: string, b: string): boolean {
  const x = a.trim();
  const y = b.trim();
  if (x === y) return true;

  if (spec.kind === 'actions') {
    const px = parsePolicies(x);
    const py = parsePolicies(y);
    return px !== undefined && py !== undefined && autoActions(px).join() === autoActions(py).join();
  }
  if (spec.kind === 'teams') {
    const px = parseTeamPolicies(x);
    const py = parseTeamPolicies(y);
    return px !== undefined && py !== undefined && teamPoliciesText(px) === teamPoliciesText(py);
  }
  if (spec.kind === 'kinds') {
    const px = parsePolicies(x);
    const py = parsePolicies(y);
    return px !== undefined && py !== undefined && policiesText(px) === policiesText(py);
  }

  if (spec.kind === 'duration') {
    const mx = parseGoDuration(x);
    return mx !== null && mx === parseGoDuration(y);
  }
  if (spec.kind === 'percent' || spec.kind === 'share' || spec.kind === 'count') {
    return x !== '' && y !== '' && Number(x) === Number(y);
  }
  return false;
}

/** changedSettings is the PUT body of a valid form: the settings whose input differs from the stored ones, and no others. */
export function changedSettings(form: SettingsForm, stored: Settings): Partial<Settings> {
  const all = fromForm(form);
  return Object.fromEntries(dirtyKeys(form, stored).map(key => [key, all[key]]));
}

/** dirtyKeys lists the fields whose input differs from the stored settings. */
export function dirtyKeys(form: SettingsForm, stored: Settings): SettingKey[] {
  return FIELDS.filter(f => !sameValue(f, form[f.key], displayValue(stored, f.key))).map(f => f.key);
}
