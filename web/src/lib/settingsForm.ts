// The settings form model: text inputs in, a validated fleet.Settings out.
// The rules mirror fleet.Settings.Validate.

import type { Policy, Settings } from '../api/types';
import { formatGoDuration, normalizeGoDuration, parseGoDuration } from './goDuration';

/**
 * ELSEWHERE are the settings the controller serves that this form does not
 * edit: the image policy per client is changed on the Clients tab, with the
 * client-policy action. The Settings tab shows them read-only.
 */
export const ELSEWHERE = ['policyClients'] as const;

/** SettingKey is a setting the form edits. */
export type SettingKey = Exclude<keyof Settings, (typeof ELSEWHERE)[number]>;

/** FormSettings are the settings the form edits. */
type FormSettings = Pick<Settings, SettingKey>;

/** actions: the policy per host action, as the JSON text of an object of action to policy. */
type FieldKind = 'percent' | 'share' | 'count' | 'duration' | 'policy' | 'switch' | 'actions';

export interface FieldSpec {
  key: SettingKey;
  /** label is short: it stands next to the control. */
  label: string;
  kind: FieldKind;
  /** hint is the long description behind the info icon: what the setting does. */
  hint: string;
  /** range states the valid values in words; it is part of the info text and of the control. */
  range?: string;
  /** unit is shown next to the control. */
  unit?: string;
  /** min and max are the valid bounds: numbers, or milliseconds for a duration. */
  min?: number;
  max?: number;
  /** step is the step of a number control. */
  step?: number;
}

export const POLICIES: readonly Policy[] = ['auto', 'manual'];

/** What each policy does, for the info text and the choices' tooltips. */
export const POLICY_MEANING: Record<Policy, string> = {
  auto: 'roll out by itself',
  manual: 'queue, and wait for a person to approve the rollout',
};

const POLICY_HINT = `What the scheduler does with a new rollout of this kind. ${POLICIES.map(p => `${p}: ${POLICY_MEANING[p]}`).join('; ')}.`;

const CANARY_HINT =
  'Pause a rollout of this kind when its first host (the canary) is ready, until a person resumes it. Also for a staggered manual action of the kind.';

/** Every setting, by key. The page places them through GROUPS. */
export const FIELDS: readonly FieldSpec[] = [
  {
    key: 'paused',
    label: 'Global pause',
    kind: 'switch',
    hint: 'Nothing new is granted while this is on. Open leases finish. Use it before planned work on the devnet.',
  },
  {
    key: 'maxUnavailablePercent',
    label: 'Max unavailable',
    kind: 'percent',
    unit: '%',
    min: 0,
    max: 100,
    step: 0.5,
    range: '0 to 100 %',
    hint: 'Share of the tracked validator weight that may be unavailable at once: hosts being updated, and validators that are not attesting.',
  },
  {
    key: 'maxLeasedPercent',
    label: 'Max hosts at once',
    kind: 'percent',
    unit: '%',
    min: 0,
    max: 100,
    step: 0.5,
    range: 'above 0, up to 100 %',
    hint: 'Share of the hosts that may be worked on at once (node and slot work), at least one. Max unavailable limits the validator weight offline; this limits the number of hosts.',
  },
  {
    key: 'maxIndependentPercent',
    label: 'Max independent',
    kind: 'percent',
    unit: '%',
    min: 0,
    max: 100,
    step: 0.5,
    range: '0 to 100 %',
    hint: 'Share of the hosts that may update independent containers (exporters, sidecars) at once, at least one. That work has no gate and no validator budget, and counts for no other limit.',
  },
  {
    key: 'lookupInterval',
    label: 'Lookup interval',
    kind: 'duration',
    min: 30_000,
    range: 'a duration like 5m, 30s to 24h',
    hint: 'How often the controller looks a tag up at its registry. It tells every host on the tag what it points at.',
  },
  {
    key: 'minParticipationPercent',
    label: 'Min participation',
    kind: 'percent',
    unit: '%',
    min: 0,
    max: 100,
    step: 0.5,
    range: '0 to 100 %',
    hint: 'No new lease while the participation of the tracked validators is below this.',
  },
  {
    key: 'maxFinalityLagEpochs',
    label: 'Max finality lag',
    kind: 'count',
    unit: 'epochs',
    min: 2,
    range: 'at least 2 (a healthy network lags 2)',
    hint: 'No new lease while finality lags more than this many epochs.',
  },
  {
    key: 'readyAttestations',
    label: 'Ready attestations',
    kind: 'count',
    unit: 'slots',
    min: 1,
    max: 32,
    range: '1 to 32',
    hint: 'For how many distinct slots after its operation a host’s validators must have attestations included on the canonical chain before its slot is released, next to the node at head with peers. A host with fewer validators needs one slot per validator.',
  },
  {
    key: 'readyStableFor',
    label: 'Ready stable for',
    kind: 'duration',
    min: 0,
    max: 30 * 60_000,
    range: 'a duration like 1m or 90s, 0 to 30m',
    hint: 'How long the node of a host without validators must stay at head with peers after its operation before its slot is released. 0 releases at its first healthy sync.',
  },
  {
    key: 'minPeers',
    label: 'Min peers',
    kind: 'count',
    unit: 'peers',
    min: 1,
    range: 'at least 1',
    hint: 'Peers each of the execution and the consensus client must have for the node to count as back. The controller asks for at most half of the other nodes.',
  },
  {
    key: 'readyPeerShare',
    label: 'Ready peer share',
    kind: 'share',
    unit: '× median',
    min: 0,
    max: 1,
    step: 0.05,
    range: '0 to 1; 0 is off',
    hint: 'Share of the fleet’s median peer count that each client must have before a host’s slot is released, next to Min peers: EL against EL, CL against CL, of the hosts that hold no lease, are not broken and are at head with peers. Not applied with fewer than three such hosts. Start with 0.25: a client with a lower peer limit than the others never passes a share above what that limit allows.',
  },
  {
    key: 'readinessTimeout',
    label: 'Readiness timeout',
    kind: 'duration',
    min: 60_000,
    range: 'a duration like 30m or 1h30m, at least 1m',
    hint: 'How long to wait for readiness after the agent reports done before the slot has failed.',
  },
  {
    key: 'leaseTtl',
    label: 'Lease TTL',
    kind: 'duration',
    min: 60_000,
    range: 'a duration like 30m or 1h30m, at least 1m',
    hint: 'Time an agent has to start a granted lease, and the silence after which a running lease has failed.',
  },
  {
    key: 'hostStaleAfter',
    label: 'Host stale after',
    kind: 'duration',
    min: 30_000,
    range: 'a duration like 90s or 2m, at least 30s',
    hint: 'Sync silence after which a host counts as stale and is no longer granted a lease.',
  },
  { key: 'policyImage', label: 'Policy: images', kind: 'policy', hint: POLICY_HINT },
  { key: 'policyPackages', label: 'Policy: packages', kind: 'policy', hint: POLICY_HINT },
  { key: 'policyReboot', label: 'Policy: reboots', kind: 'policy', hint: POLICY_HINT },
  { key: 'pauseAfterCanaryImage', label: 'Pause after canary: images', kind: 'switch', hint: CANARY_HINT },
  { key: 'pauseAfterCanaryPackages', label: 'Pause after canary: packages', kind: 'switch', hint: CANARY_HINT },
  { key: 'pauseAfterCanaryReboot', label: 'Pause after canary: reboots', kind: 'switch', hint: CANARY_HINT },
  {
    key: 'actionPolicies',
    label: 'Policy: host actions',
    kind: 'actions',
    hint: `What the scheduler does when hosts report a host action due. ${POLICIES.map(p => `${p}: ${POLICY_MEANING[p]}`).join('; ')}. An action without a policy is manual; an action a person starts needs no approval.`,
  },
];

/** fieldSpec returns the spec of a setting. */
export function fieldSpec(key: SettingKey): FieldSpec {
  const spec = FIELDS.find(f => f.key === key);
  if (!spec) throw new Error(`no field for the setting ${key}`);
  return spec;
}

/**
 * RolloutKindSpec is one kind of rollout with its two settings: what happens
 * to a new rollout of the kind, and whether it pauses after its canary. The
 * policy group of the page is this list; a kind whose policy moves elsewhere
 * is one entry less here.
 */
export interface RolloutKindSpec {
  id: string;
  label: string;
  /** what names the kind in a sentence. */
  what: string;
  policy: SettingKey;
  canary: SettingKey;
}

export const ROLLOUT_KINDS: readonly RolloutKindSpec[] = [
  { id: 'image', label: 'Images', what: 'a new image digest', policy: 'policyImage', canary: 'pauseAfterCanaryImage' },
  {
    id: 'packages',
    label: 'Packages',
    what: 'an upgrade of the blocked packages',
    policy: 'policyPackages',
    canary: 'pauseAfterCanaryPackages',
  },
  {
    id: 'reboot',
    label: 'Reboots',
    what: 'a required reboot',
    policy: 'policyReboot',
    canary: 'pauseAfterCanaryReboot',
  },
];

/** GroupSpec is one card of the settings page: settings that govern one thing. */
export interface GroupSpec {
  id: string;
  title: string;
  /** about says in a line what the group governs. */
  about: string;
  /** keys are the settings shown one per line. */
  keys: readonly SettingKey[];
  /** wide: the group is a strip over the whole width, its settings side by side. */
  wide?: boolean;
  /** kinds: the group also shows the rollout kinds, each with its policy and its canary switch. */
  kinds?: readonly RolloutKindSpec[];
  /** actionPolicies: the group also shows the policy of each host action, after the kinds. */
  actionPolicies?: boolean;
}

/**
 * The groups of the settings page, in display order. Every setting is in
 * exactly one. The first two are strips over the whole width; the three below
 * them stand side by side, stretched to one height.
 */
export const GROUPS: readonly GroupSpec[] = [
  {
    id: 'scheduler',
    title: 'Scheduler',
    about:
      'The switch for everything, how many hosts may update independent containers at once, and what happens to a new rollout of each kind.',
    wide: true,
    keys: ['paused', 'maxIndependentPercent'],
    kinds: ROLLOUT_KINDS,
    actionPolicies: true,
  },
  {
    id: 'images',
    title: 'Image lookups',
    about: 'How often the controller asks a registry where a tag points.',
    wide: true,
    keys: ['lookupInterval'],
  },
  {
    id: 'limits',
    title: 'Budget and network gate',
    about: 'How much of the fleet may be down at once, and when the network is healthy enough to take a host down.',
    keys: ['maxUnavailablePercent', 'maxLeasedPercent', 'minParticipationPercent', 'maxFinalityLagEpochs'],
  },
  {
    id: 'readiness',
    title: 'Readiness after an operation',
    about: 'When a host counts as back, so the next one may go.',
    keys: ['readyAttestations', 'readyStableFor', 'minPeers', 'readyPeerShare'],
  },
  {
    id: 'timeouts',
    title: 'Timeouts',
    about: 'How long the scheduler waits for an agent, and for a host to become ready.',
    keys: ['leaseTtl', 'hostStaleAfter', 'readinessTimeout'],
  },
];

/** groupKeys lists the settings a group places. */
export function groupKeys(group: GroupSpec): SettingKey[] {
  return [
    ...group.keys,
    ...(group.kinds ?? []).flatMap(k => [k.policy, k.canary]),
    ...(group.actionPolicies ? (['actionPolicies'] as const) : []),
  ];
}

/** valueText renders a stored or typed value for a person: with its unit, a switch as on or off. */
export function valueText(spec: FieldSpec, value: string): string {
  if (spec.kind === 'switch') return value === 'true' ? 'on' : 'off';
  if (spec.kind === 'actions') {
    const auto = autoActions(parseActionPolicies(value) ?? {});
    return auto.length === 0 ? 'every action manual' : `${auto.join(', ')} auto`;
  }
  return spec.unit ? `${value} ${spec.unit}` : value;
}

/** shortValue is valueText without the unit, for where the unit stands next to it anyway. */
export function shortValue(spec: FieldSpec, value: string): string {
  return spec.kind === 'switch' || spec.kind === 'actions' ? valueText(spec, value) : value;
}

/** actionPoliciesText is the form text of the action policies: the JSON object, sorted by action. */
export function actionPoliciesText(policies: Record<string, Policy>): string {
  return JSON.stringify(Object.fromEntries(Object.entries(policies).sort(([a], [b]) => a.localeCompare(b))));
}

/** autoActions are the actions whose policy is auto, sorted: an action that is not listed is manual. */
function autoActions(policies: Record<string, Policy>): string[] {
  return Object.keys(policies)
    .filter(name => policies[name] === 'auto')
    .sort();
}

/** parseActionPolicies reads the form text of the action policies; undefined when it is not an object of action to policy. */
export function parseActionPolicies(text: string): Record<string, Policy> | undefined {
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
 * its messages start with the setting's name ("minPeers must be at least 1").
 */
export function errorField(message: string): SettingKey | undefined {
  const text = message.toLowerCase();
  return [...FIELDS]
    .sort((a, b) => b.key.length - a.key.length)
    .find(f => new RegExp(`(^|[^a-z])${f.key.toLowerCase()}([^a-z]|$)`).test(text))?.key;
}

/** SettingsForm holds every field as the text (or switch state) shown in its input. */
export type SettingsForm = Record<SettingKey, string>;

/** displayValue renders one setting the way its input shows it. */
export function displayValue(settings: FormSettings, key: SettingKey): string {
  const v = settings[key];
  if (typeof v === 'boolean') return v ? 'true' : 'false';
  if (typeof v === 'object') return actionPoliciesText(v);
  if (typeof v === 'number') return String(v);
  const spec = FIELDS.find(f => f.key === key);
  return spec?.kind === 'duration' ? normalizeGoDuration(v) : v;
}

/** toForm turns settings into form values. */
export function toForm(settings: FormSettings): SettingsForm {
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
      return parseActionPolicies(text) ? null : 'must be an object of action to auto or manual';
  }
}

type FormErrors = Partial<Record<SettingKey, string>>;

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
export function fromForm(form: SettingsForm): FormSettings {
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
    policyImage: form.policyImage as Policy,
    policyPackages: form.policyPackages as Policy,
    policyReboot: form.policyReboot as Policy,
    pauseAfterCanaryImage: form.pauseAfterCanaryImage === 'true',
    pauseAfterCanaryPackages: form.pauseAfterCanaryPackages === 'true',
    pauseAfterCanaryReboot: form.pauseAfterCanaryReboot === 'true',
    readyPeerShare: Number(form.readyPeerShare),
    lookupInterval: duration('lookupInterval'),
    paused: form.paused === 'true',
    actionPolicies: parseActionPolicies(form.actionPolicies) ?? {},
  };
}

/** sameValue compares an input with a stored value: numerically, as durations, or as text. */
export function sameValue(spec: FieldSpec, a: string, b: string): boolean {
  const x = a.trim();
  const y = b.trim();
  if (x === y) return true;

  if (spec.kind === 'actions') {
    const px = parseActionPolicies(x);
    const py = parseActionPolicies(y);
    return px !== undefined && py !== undefined && autoActions(px).join() === autoActions(py).join();
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
export function changedSettings(form: SettingsForm, stored: FormSettings): Partial<FormSettings> {
  const all = fromForm(form);
  return Object.fromEntries(dirtyKeys(form, stored).map(key => [key, all[key]]));
}

/** dirtyKeys lists the fields whose input differs from the stored settings. */
export function dirtyKeys(form: SettingsForm, stored: FormSettings): SettingKey[] {
  return FIELDS.filter(f => !sameValue(f, form[f.key], displayValue(stored, f.key))).map(f => f.key);
}
