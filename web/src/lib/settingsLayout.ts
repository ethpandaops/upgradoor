// The settings page, declared: its sections in display order, each with the
// settings it holds. The page renders from SECTIONS alone.
//
// Where a new setting goes: into the fields of the one section whose subject
// it governs (a rollout kind's own section, or the scheduler's limits and
// checks), never into the page's JSX. A section's master is its main control,
// shown in the section header. A field with a parent is a policy per name
// whose entries either inherit the parent's value or have their own; the
// parent is a setting of the same section. settingsLayout.test.ts fails for a
// setting the controller serves that is not placed exactly once.

import type { Policy, Settings } from '../api/types';

/** SettingKey is a setting of the page. */
export type SettingKey = keyof Settings;

/**
 * kinds, teams and actions are policies per name (container kind, team,
 * host action), held as the JSON text of an object of name to policy.
 */
export type FieldKind =
  | 'percent'
  | 'share'
  | 'count'
  | 'duration'
  | 'policy'
  | 'switch'
  | 'kinds'
  | 'teams'
  | 'actions';

export interface FieldSpec {
  key: SettingKey;
  /** label is short: it names the control. */
  label: string;
  kind: FieldKind;
  /** help is one line under the label: what the setting is for. */
  help: string;
  /** hint is the long description behind the info icon. */
  hint: string;
  /**
   * parent is the setting a name without an entry of its own inherits from:
   * policyKinds inherit policyImage, policyClients the policy of their kind.
   * It is in the same section.
   */
  parent?: SettingKey;
  /** range states the valid values in words. */
  range?: string;
  /** unit is shown next to the control. */
  unit?: string;
  /** min and max are the valid bounds: numbers, or milliseconds for a duration. */
  min?: number;
  max?: number;
  /** step is the step of a number control. */
  step?: number;
  /** byAction: the setting is changed by its own action (pause and resume), not saved with the form. */
  byAction?: boolean;
}

export interface SectionSpec {
  id: string;
  title: string;
  /** about says in a line what the section governs. */
  about: string;
  /** master is the section's main control, in its header. */
  master?: FieldSpec;
  /** fields are the section's rows, in order. */
  fields: readonly FieldSpec[];
  /** clientPolicyNote: the section explains that teams also set their policy on the Clients tab. */
  clientPolicyNote?: boolean;
}

/** AreaSpec is a titled run of sections. */
export interface AreaSpec {
  id: string;
  title: string;
  sections: readonly SectionSpec[];
}

export const POLICIES: readonly Policy[] = ['auto', 'manual'];

/** What each policy does, for the info text and the choices' tooltips. */
export const POLICY_MEANING: Record<Policy, string> = {
  auto: 'roll out by itself',
  manual: 'queue, and wait for a person to approve the rollout',
};

/** How each policy is labelled as a choice. */
export const POLICY_LABEL: Record<Policy, string> = { auto: 'Automatic', manual: 'Manual' };

const POLICY_TEXT = POLICIES.map(p => `${p}: ${POLICY_MEANING[p]}`).join('; ');

const CANARY_HINT =
  'Pause a rollout of this kind when its first host (the canary) is ready, until a person resumes it. It applies under either policy: a manual rollout pauses after its canary once it is approved.';

const canary = (key: SettingKey, kind: string, what: string): FieldSpec => ({
  key,
  label: `Pause after canary: ${kind}`,
  kind: 'switch',
  help: `Hold ${what} rollout after its first host is back, until someone resumes it.`,
  hint: CANARY_HINT,
});

const policy = (key: SettingKey, label: string, what: string): FieldSpec => ({
  key,
  label,
  kind: 'policy',
  help: `What happens when hosts report ${what}.`,
  hint: `What the scheduler does with a new rollout of this kind. ${POLICY_TEXT}.`,
});

export const AREAS: readonly AreaSpec[] = [
  {
    id: 'rollouts',
    title: 'Rollouts',
    sections: [
      {
        id: 'pause',
        title: 'Pause all',
        about:
          'The emergency switch above everything below: it holds every lease the controller would grant. Open leases finish.',
        master: {
          key: 'paused',
          label: 'Global pause',
          kind: 'switch',
          byAction: true,
          help: 'Holds every lease the controller would grant.',
          hint: 'Nothing new is granted while it is on. Open leases finish. Use it before planned work on the devnet. It is set with the pause and resume actions.',
        },
        fields: [],
      },
      {
        id: 'images',
        title: 'Image rollouts',
        about: 'New image digests of the containers.',
        master: policy('policyImage', 'Policy: images', 'a new image digest'),
        fields: [
          {
            key: 'policyKinds',
            label: 'Kind policies',
            kind: 'kinds',
            parent: 'policyImage',
            help: 'Each container kind inherits the image policy, or has its own.',
            hint: `The image policy of the containers of a kind; other is the unlabelled containers. A kind that inherits follows Policy: images. ${POLICY_TEXT}.`,
          },
          {
            key: 'policyClients',
            label: 'Team policies',
            kind: 'teams',
            parent: 'policyKinds',
            help: 'Each team inherits the policy of its kind, or has its own.',
            hint: `The image policy of a team's containers of one kind. A team with its own policy for a kind answers to it; a team that inherits follows the policy of the kind. ${POLICY_TEXT}.`,
          },
          canary('pauseAfterCanaryImage', 'images', 'an image'),
          {
            key: 'lookupInterval',
            label: 'Lookup interval',
            kind: 'duration',
            min: 30_000,
            range: 'a duration like 5m, 30s to 24h',
            help: 'How often a tag is looked up at its registry.',
            hint: 'How often the controller looks a tag up at its registry. It tells every host on the tag what it points at.',
          },
          {
            key: 'maxConcurrentPulls',
            label: 'Max image pulls',
            kind: 'count',
            unit: 'leases',
            min: 0,
            max: 10_000,
            range: '0 to 10000; 0 is no bound',
            help: 'Leases with image work granted or pulling at once; 0 is no bound.',
            hint: 'Leases with image work granted or pulling at once; 0 is no bound.',
          },
        ],
        clientPolicyNote: true,
      },
      {
        id: 'packages',
        title: 'Package upgrades',
        about: 'Upgrades of the packages the hosts report as blocked.',
        master: policy('policyPackages', 'Policy: packages', 'an upgrade of the blocked packages'),
        fields: [canary('pauseAfterCanaryPackages', 'packages', 'a package')],
      },
      {
        id: 'reboots',
        title: 'Reboots',
        about: 'Reboots the hosts report as required.',
        master: policy('policyReboot', 'Policy: reboots', 'a required reboot'),
        fields: [canary('pauseAfterCanaryReboot', 'reboots', 'a reboot')],
      },
      {
        id: 'actions',
        title: 'Host actions',
        about: 'Actions the hosts report as due. An action without a policy is manual.',
        fields: [
          {
            key: 'actionPolicies',
            label: 'Action policies',
            kind: 'actions',
            help: 'An action a person starts needs no approval.',
            hint: `What the scheduler does when hosts report a host action due. ${POLICY_TEXT}. An action without a policy is manual; an action a person starts needs no approval.`,
          },
        ],
      },
    ],
  },
  {
    id: 'limits',
    title: 'Limits and checks',
    sections: [
      {
        id: 'budget',
        title: 'Budget',
        about: 'How much of the fleet may be worked on at once.',
        fields: [
          {
            key: 'maxUnavailablePercent',
            label: 'Max unavailable',
            kind: 'percent',
            unit: '%',
            min: 0,
            max: 100,
            step: 0.5,
            range: '0 to 100 %',
            help: 'Share of the validator weight that may be down at once.',
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
            help: 'Share of the hosts with node work at once, at least one.',
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
            help: 'Share of the hosts updating exporters and sidecars at once.',
            hint: 'Share of the hosts that may update independent containers (exporters, sidecars) at once, at least one. That work has no gate and no validator budget, and counts for no other limit.',
          },
        ],
      },
      {
        id: 'gate',
        title: 'Network gate',
        about: 'When the network is healthy enough to take a host down.',
        fields: [
          {
            key: 'minParticipationPercent',
            label: 'Min participation',
            kind: 'percent',
            unit: '%',
            min: 0,
            max: 100,
            step: 0.5,
            range: '0 to 100 %',
            help: 'No new lease while participation is below this.',
            hint: 'No new lease while the participation of the tracked validators is below this.',
          },
          {
            key: 'maxFinalityLagEpochs',
            label: 'Max finality lag',
            kind: 'count',
            unit: 'epochs',
            min: 2,
            range: 'at least 2 (a healthy network lags 2)',
            help: 'No new lease while finality lags more than this.',
            hint: 'No new lease while finality lags more than this many epochs.',
          },
        ],
      },
      {
        id: 'readiness',
        title: 'Readiness after an operation',
        about: 'When a host counts as back, so the next one may go.',
        fields: [
          {
            key: 'readyAttestations',
            label: 'Ready attestations',
            kind: 'count',
            unit: 'slots',
            min: 1,
            max: 32,
            range: '1 to 32',
            help: 'Slots with included attestations before a host is back.',
            hint: 'For how many distinct slots after its operation a host’s validators must have attestations included on the canonical chain before its slot is released, next to the node at head with peers. A host with fewer validators needs one slot per validator.',
          },
          {
            key: 'readyStableFor',
            label: 'Ready stable for',
            kind: 'duration',
            min: 0,
            max: 30 * 60_000,
            range: 'a duration like 1m or 90s, 0 to 30m',
            help: 'How long a host without validators stays healthy first.',
            hint: 'How long the node of a host without validators must stay at head with peers after its operation before its slot is released. 0 releases at its first healthy sync.',
          },
          {
            key: 'minPeers',
            label: 'Min peers',
            kind: 'count',
            unit: 'peers',
            min: 1,
            range: 'at least 1',
            help: 'Peers each client needs for the node to count as back.',
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
            help: 'Share of the fleet’s median peer count each client needs; 0 is off.',
            hint: 'Share of the fleet’s median peer count that each client must have before a host’s slot is released, next to Min peers: EL against EL, CL against CL, of the hosts that hold no lease, are not broken and are at head with peers. Not applied with fewer than three such hosts. Start with 0.25: a client with a lower peer limit than the others never passes a share above what that limit allows.',
          },
        ],
      },
      {
        id: 'timeouts',
        title: 'Timeouts',
        about: 'How long the scheduler waits for an agent, and for a host to become ready.',
        fields: [
          {
            key: 'leaseTtl',
            label: 'Lease TTL',
            kind: 'duration',
            min: 60_000,
            range: 'a duration like 30m or 1h30m, at least 1m',
            help: 'Time to start a granted lease, and the silence that fails a running one.',
            hint: 'Time an agent has to start a granted lease, and the silence after which a running lease has failed.',
          },
          {
            key: 'hostStaleAfter',
            label: 'Host stale after',
            kind: 'duration',
            min: 30_000,
            range: 'a duration like 90s or 2m, at least 30s',
            help: 'Sync silence after which a host gets no lease.',
            hint: 'Sync silence after which a host counts as stale and is no longer granted a lease.',
          },
          {
            key: 'readinessTimeout',
            label: 'Readiness timeout',
            kind: 'duration',
            min: 60_000,
            range: 'a duration like 30m or 1h30m, at least 1m',
            help: 'Wait for readiness after the agent is done before the slot fails.',
            hint: 'How long to wait for readiness after the agent reports done before the slot has failed.',
          },
          {
            key: 'baselineGrace',
            label: 'Baseline grace',
            kind: 'duration',
            min: 0,
            range: 'a duration like 15m, from Ready stable for to Readiness timeout',
            help: 'Wait for a host that was not ready before its lease.',
            hint: 'How long a host that was not ready before its lease waits for the full readiness check before it is released as no worse than before.',
          },
        ],
      },
    ],
  },
];

/** SECTIONS are the sections of every area, in display order. */
export const SECTIONS: readonly SectionSpec[] = AREAS.flatMap(a => a.sections);

/** sectionFields lists a section's settings: its master first. */
export function sectionFields(section: SectionSpec): FieldSpec[] {
  return section.master ? [section.master, ...section.fields] : [...section.fields];
}

/** FIELDS is every setting of the page, in display order. */
export const FIELDS: readonly FieldSpec[] = SECTIONS.flatMap(sectionFields);
