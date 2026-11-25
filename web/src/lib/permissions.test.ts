// The table of pkg/controller/userapi/actions_test.go, for the page's copy of the
// rule: every action against every kind of login, and what the team twins
// cover.

import { describe, expect, it } from 'vitest';

import type { ActionCall } from '../api/client';
import {
  FEATURE,
  TEAM_PREFIX,
  TWINNED,
  can,
  hasFeature,
  needsActionHosts,
  needsOwnership,
  type LeaseWork,
  type Login,
  type OwnedHost,
} from './permissions';
import type { Need, RolloutKind } from '../api/types';
import type { PermissionCall } from './permissions';

const MIXED = 'lighthouse-geth-1';
const TEKU = 'teku-geth-1';
const UNOWNED = 'bootnode-1';
const FRESH = 'fresh-1';
const GHOST = 'ghost-1';

const owned = (name: string, team?: string) => ({ name, managed: true, team });

/** The fleet of the controller's table. */
const HOSTS: OwnedHost[] = [
  {
    name: MIXED,
    containers: [
      owned('geth', 'geth'),
      owned('lighthouse', 'lighthouse'),
      owned('lighthouse-validator', 'lighthouse'),
      { name: 'debug', managed: false },
    ],
  },
  { name: TEKU, containers: [owned('geth', 'geth'), owned('teku', 'teku')] },
  { name: UNOWNED, containers: [{ name: 'bootnode', managed: true }] },
  // Registered, never synced.
  { name: FRESH, containers: null },
];

const imageNeed = (container: string): Need => ({
  id: `image/${container}`,
  kind: 'image',
  image: { container, image: '', toDigest: '' },
});

/** lease-mixed rolls lighthouse, lease-unowned bootnode, lease-reboot geth and reboots. */
const LEASES: Record<string, LeaseWork> = {
  'lease-mixed': { host: MIXED, needs: [imageNeed('lighthouse')] },
  'lease-unowned': { host: UNOWNED, needs: [imageNeed('bootnode')] },
  'lease-reboot': { host: MIXED, needs: [imageNeed('geth'), { id: 'reboot', kind: 'reboot' }] },
};

/**
 * r-lighthouse rolls the client lighthouse, r-roll is a manual roll of lighthouse's containers, r-aborted an
 * aborted rollout of lighthouse, r-1 a manual roll for no team, r-reboot a reboot; others are unknown.
 */
const ROLLOUT_CLIENTS: Record<string, string> = {
  'r-lighthouse': 'lighthouse',
  'r-roll': 'lighthouse',
  'r-aborted': 'lighthouse',
  'r-1': '',
  'r-reboot': '',
};
const ROLLOUT_KINDS: Record<string, RolloutKind> = {
  'r-lighthouse': 'group',
  'r-roll': 'manual',
  'r-aborted': 'group',
  'r-1': 'manual',
  'r-reboot': 'reboot',
};

const OWNERSHIP = {
  hosts: HOSTS,
  clients: [{ name: 'lighthouse', hosts: 1 }],
  leaseWork: (id: string) => LEASES[id],
  rolloutClient: (id: string) => ROLLOUT_CLIENTS[id],
  rolloutKind: (id: string) => ROLLOUT_KINDS[id],
};

/** login builds a login from the features a grant gives; "all" is a grant like upgradoor:all. */
function login(features: readonly string[] | 'all', teams: readonly string[] = [], service = 'upgradoor'): Login {
  return {
    open: false,
    service,
    hasFeature: feature => features === 'all' || features.includes(feature),
    teams,
  };
}

const OPEN: Login = { open: true, service: 'upgradoor', hasFeature: () => false, teams: [] };

const QUIET = { staggered: false, force: false, ignoreBudget: false, reason: '' };

const roll = (hosts: string[], containers: string[] = [], flags: Partial<typeof QUIET & { all: boolean }> = {}) =>
  ({ action: 'roll', body: { hosts, all: false, containers, ...QUIET, ...flags } }) satisfies ActionCall;
const reboot = (hosts: string[], flags: Partial<typeof QUIET & { all: boolean }> = {}) =>
  ({ action: 'reboot', body: { hosts, all: false, ...QUIET, ...flags } }) satisfies ActionCall;
const hold = (action: 'hold' | 'unhold' | 'forget', hosts: string[], holders?: string[]) =>
  ({ action, body: { hosts, reason: '', ...(holders ? { holders } : {}) } }) satisfies ActionCall;
const scope = (action: 'pause' | 'resume' | 'abort', rollout: string) =>
  ({ action, body: { rollout, reason: '' } }) satisfies ActionCall;
const rolloutHosts = (action: 'skip' | 'retry', hosts: string[]) =>
  ({ action, body: { rollout: 'r-roll', hosts, reason: '' } }) satisfies ActionCall;
const endLease = (lease: string) => ({ action: 'end-lease', body: { lease, reason: '' } }) satisfies ActionCall;

interface Sample {
  name: string;
  call: PermissionCall;
  feature: string;
  twin: boolean;
}

/** One request per kind of action, on targets the team lighthouse covers. */
const SAMPLES: Sample[] = [
  { name: 'roll', call: roll([MIXED], ['lighthouse']), feature: FEATURE.manage, twin: true },
  {
    name: 'roll with the flags',
    call: roll([MIXED], ['lighthouse'], { force: true, ignoreBudget: true }),
    feature: FEATURE.manage,
    twin: true,
  },
  {
    name: 'client-policy',
    call: { action: 'client-policy', body: { client: 'lighthouse', policy: 'manual', reason: '' } },
    feature: FEATURE.manage,
    twin: true,
  },
  {
    name: 'image-override',
    call: { action: 'image-override', body: { host: MIXED, container: 'lighthouse', image: 'a/b:c', reason: '' } },
    feature: FEATURE.manage_image,
    twin: true,
  },
  {
    name: 'flag-override',
    call: {
      action: 'flag-override',
      body: { host: MIXED, container: 'lighthouse', addFlags: ['--x'], removeFlags: [], reason: '' },
    },
    feature: FEATURE.manage_flags,
    twin: true,
  },
  { name: 'reboot', call: reboot([MIXED]), feature: FEATURE.manage, twin: false },
  {
    name: 'mark-offline',
    call: { action: 'mark-offline', body: { hosts: [MIXED], reason: 'x' } },
    feature: FEATURE.manage,
    twin: false,
  },
  { name: 'hold', call: hold('hold', [MIXED]), feature: FEATURE.manage, twin: true },
  { name: 'unhold', call: hold('unhold', [MIXED]), feature: FEATURE.manage, twin: true },
  { name: 'skip', call: rolloutHosts('skip', [MIXED]), feature: FEATURE.manage, twin: true },
  { name: 'retry hosts', call: rolloutHosts('retry', [MIXED]), feature: FEATURE.manage, twin: true },
  { name: 'end-lease', call: endLease('lease-mixed'), feature: FEATURE.manage, twin: true },
  { name: 'pause one', call: scope('pause', 'r-lighthouse'), feature: FEATURE.manage, twin: true },
  { name: 'resume one', call: scope('resume', 'r-lighthouse'), feature: FEATURE.manage, twin: true },
  {
    name: 'ignore-gate',
    call: { action: 'ignore-gate', body: { rollout: 'r-lighthouse', reason: '' } },
    feature: FEATURE.manage,
    twin: true,
  },
  {
    name: 'ignore-budget',
    call: { action: 'ignore-budget', body: { rollout: 'r-lighthouse', reason: '' } },
    feature: FEATURE.manage,
    twin: true,
  },
  {
    name: 'approve-lease',
    call: { action: 'approve-lease', body: { rollout: 'r-lighthouse', host: MIXED, reason: '' } },
    feature: FEATURE.manage,
    twin: true,
  },
  { name: 'abort', call: scope('abort', 'r-lighthouse'), feature: FEATURE.manage, twin: true },
  { name: 'retry rollout', call: rolloutHosts('retry', []), feature: FEATURE.manage, twin: true },
  { name: 'pause all', call: scope('pause', ''), feature: FEATURE.admin, twin: false },
  { name: 'resume all', call: scope('resume', ''), feature: FEATURE.admin, twin: false },
  { name: 'forget', call: hold('forget', [MIXED]), feature: FEATURE.admin, twin: false },
  { name: 'settings', call: { action: 'settings' }, feature: FEATURE.admin, twin: false },
];

const ALL_TWINS = TWINNED.map(f => TEAM_PREFIX + f);

describe('can, every action against every kind of login', () => {
  for (const s of SAMPLES) {
    const twin = TEAM_PREFIX + s.feature;
    const lacks = `Needs upgradoor:${s.feature}`;
    // admin includes every feature, which is then named.
    const withAdmin = s.feature;

    const cases: { name: string; login: Login; by?: string; reason?: RegExp | string }[] = [
      { name: 'open mode', login: OPEN, by: 'open' },
      { name: 'no features', login: login([], ['lighthouse']), reason: lacks },
      { name: 'plain feature', login: login([s.feature]), by: s.feature },
      { name: 'plain feature, no owning team', login: login([s.feature], ['besu']), by: s.feature },
      {
        name: 'team twin, owning team',
        login: login([twin], ['lighthouse']),
        ...(s.twin ? { by: twin } : { reason: lacks }),
      },
      {
        name: 'team twin, other teams',
        login: login([twin], ['besu', 'reth']),
        reason: s.twin ? `upgradoor:${twin} does not cover ` : lacks,
      },
      {
        name: 'team twin, no teams',
        login: login([twin]),
        reason: s.twin ? `upgradoor:${twin} does not cover ` : lacks,
      },
      { name: 'plain and twin, other teams', login: login([s.feature, twin], ['besu']), by: s.feature },
      { name: 'every feature', login: login('all', ['besu']), by: withAdmin },
      {
        name: 'the team features, owning team',
        login: login(ALL_TWINS, ['lighthouse']),
        ...(s.twin ? { by: twin } : { reason: lacks }),
      },
      {
        name: 'the team features, other teams',
        login: login(ALL_TWINS, ['besu']),
        reason: s.twin ? `upgradoor:${twin} does not cover ` : lacks,
      },
      { name: 'admin includes every feature', login: login([FEATURE.admin], ['lighthouse']), by: withAdmin },
      {
        name: 'every other feature',
        login: login(Object.values(FEATURE).filter(f => f !== s.feature)),
        ...(s.feature === FEATURE.admin ? { reason: lacks } : { by: withAdmin }),
      },
      {
        name: 'every other feature but admin',
        login: login(Object.values(FEATURE).filter(f => f !== s.feature && f !== FEATURE.admin)),
        reason: lacks,
      },
    ];

    for (const c of cases) {
      it(`${s.name}: ${c.name}`, () => {
        const verdict = can(c.login, s.call, OWNERSHIP);

        if (c.by !== undefined) {
          expect(verdict).toMatchObject({ allowed: true, reason: '', by: c.by });
          return;
        }

        expect(verdict.allowed).toBe(false);
        expect(verdict.by).toBe('');
        expect(verdict.reason.startsWith(c.reason as string)).toBe(true);
      });
    }
  }
});

describe('can, the tooltip of a login that has neither the feature nor its twin', () => {
  const nobody = login([]);

  it.each<[string, PermissionCall, string]>([
    [
      'one host',
      hold('hold', [MIXED]),
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns this host',
    ],
    ['no twin, a reboot', reboot([MIXED]), 'Needs upgradoor:manage'],
    [
      'several hosts',
      hold('hold', [MIXED, TEKU]),
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns these hosts',
    ],
    [
      'a roll of the host',
      roll([MIXED]),
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns every managed container of the host',
    ],
    [
      'a roll of containers',
      roll([MIXED], ['geth']),
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns these containers',
    ],
    [
      'a lease',
      endLease('lease-mixed'),
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns every container this lease rolls',
    ],
    ['no twin', reboot([MIXED]), 'Needs upgradoor:manage'],
    [
      'a rollout',
      scope('abort', 'r-1'),
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns this rollout',
    ],
    ['the settings', { action: 'settings' }, 'Needs upgradoor:admin'],
  ])('%s', (_name, call, reason) => {
    expect(can(nobody, call, OWNERSHIP)).toEqual({ allowed: false, reason, by: '' });
  });

  it('names the configured service', () => {
    expect(can(login([], [], 'upgradoor-dev'), hold('hold', [MIXED]), OWNERSHIP).reason).toBe(
      'Needs upgradoor-dev:manage or upgradoor-dev:team_manage for a team that owns this host'
    );
    expect(can(login(['team_manage'], ['teku'], 'upgradoor-dev'), hold('hold', [MIXED]), OWNERSHIP).reason).toBe(
      'upgradoor-dev:team_manage does not cover host lighthouse-geth-1 (owners: geth, lighthouse)'
    );
  });
});

describe('can, what the team twins cover', () => {
  const team = (...teams: string[]) => login(['team_manage'], teams);

  const NONE = 'needs a target that one of your teams owns, and the request names none';

  // [name, login, call, what allows it | why not]
  const cases: [string, Login, PermissionCall, { by: string } | { reason: string }][] = [
    // A roll with named containers: every one of them.
    ['roll, own container', team('lighthouse'), roll([MIXED], ['lighthouse']), { by: 'team_manage' }],
    [
      'roll, own containers',
      team('lighthouse'),
      roll([MIXED], ['lighthouse', 'lighthouse-validator']),
      { by: 'team_manage' },
    ],
    [
      'roll, mixed ownership',
      team('lighthouse'),
      roll([MIXED], ['lighthouse', 'geth']),
      { reason: 'upgradoor:team_manage does not cover container geth on host lighthouse-geth-1 (owners: geth)' },
    ],
    [
      'roll, mixed ownership, in both teams',
      team('geth', 'lighthouse'),
      roll([MIXED], ['lighthouse', 'geth']),
      { by: 'team_manage' },
    ],
    ['roll, a container of its team', team('teku'), roll([TEKU], ['teku']), { by: 'team_manage' }],
    [
      'roll, a container of another team',
      team('lighthouse'),
      roll([TEKU], ['teku']),
      { reason: 'upgradoor:team_manage does not cover container teku on host teku-geth-1 (owners: teku)' },
    ],
    ['roll, one container on two hosts', team('geth'), roll([MIXED, TEKU], ['geth']), { by: 'team_manage' }],
    [
      'roll, a container one host does not have',
      team('geth'),
      roll([MIXED, UNOWNED], ['geth']),
      { reason: 'upgradoor:team_manage does not cover container geth on host bootnode-1 (owners: none)' },
    ],
    [
      'roll, unknown container',
      team('geth', 'lighthouse'),
      roll([MIXED], ['nethermind']),
      { reason: 'upgradoor:team_manage does not cover container nethermind on host lighthouse-geth-1 (owners: none)' },
    ],
    [
      'roll, unmanaged container',
      team('geth', 'lighthouse'),
      roll([MIXED], ['debug']),
      { reason: 'upgradoor:team_manage does not cover container debug on host lighthouse-geth-1 (owners: none)' },
    ],
    ['roll, unknown container, plain feature', login(['manage']), roll([MIXED], ['nethermind']), { by: 'manage' }],

    // A roll without containers: every managed container of the host.
    [
      'roll a host, one team of two',
      team('lighthouse'),
      roll([MIXED]),
      { reason: 'upgradoor:team_manage does not cover container geth on host lighthouse-geth-1 (owners: geth)' },
    ],
    ['roll a host, both teams', team('geth', 'lighthouse'), roll([MIXED]), { by: 'team_manage' }],
    [
      'roll hosts, second not covered',
      team('geth', 'lighthouse'),
      roll([MIXED, TEKU]),
      { reason: 'upgradoor:team_manage does not cover container teku on host teku-geth-1 (owners: teku)' },
    ],
    [
      'roll a host nobody owns',
      team('geth'),
      roll([UNOWNED]),
      { reason: 'upgradoor:team_manage does not cover container bootnode on host bootnode-1 (owners: none)' },
    ],
    [
      'roll a host that never synced',
      team('geth'),
      roll([FRESH]),
      { reason: 'upgradoor:team_manage does not cover host fresh-1 (owners: none)' },
    ],
    [
      'roll an unknown host',
      team('geth'),
      roll([GHOST]),
      { reason: 'upgradoor:team_manage does not cover host ghost-1 (owners: none)' },
    ],
    [
      'roll an unknown host, named container',
      team('geth'),
      roll([GHOST], ['geth']),
      { reason: 'upgradoor:team_manage does not cover container geth on host ghost-1 (owners: none)' },
    ],
    ['roll an unknown host, plain feature', login(['manage']), roll([GHOST]), { by: 'manage' }],
    ['roll, no target', team('geth'), roll([]), { reason: `upgradoor:team_manage ${NONE}` }],

    // Host-level actions: one owned container is enough.
    ['hold, the CL team', team('lighthouse'), hold('hold', [MIXED]), { by: 'team_manage' }],
    ['hold, the EL team', team('geth'), hold('hold', [MIXED]), { by: 'team_manage' }],
    [
      'hold, a team without a container there',
      team('teku'),
      hold('hold', [MIXED]),
      { reason: 'upgradoor:team_manage does not cover host lighthouse-geth-1 (owners: geth, lighthouse)' },
    ],
    ['hold two hosts, the EL team', team('geth'), hold('hold', [MIXED, TEKU]), { by: 'team_manage' }],
    [
      'hold two hosts, second not covered',
      team('lighthouse'),
      hold('hold', [MIXED, TEKU]),
      { reason: 'upgradoor:team_manage does not cover host teku-geth-1 (owners: geth, teku)' },
    ],
    [
      'hold a host nobody owns',
      team('geth'),
      hold('hold', [UNOWNED]),
      { reason: 'upgradoor:team_manage does not cover host bootnode-1 (owners: none)' },
    ],
    [
      'hold a host that never synced',
      team('geth'),
      hold('hold', [FRESH]),
      { reason: 'upgradoor:team_manage does not cover host fresh-1 (owners: none)' },
    ],
    [
      'hold an unknown host',
      team('geth'),
      hold('hold', [GHOST]),
      { reason: 'upgradoor:team_manage does not cover host ghost-1 (owners: none)' },
    ],
    ['reboot an unknown host, plain feature', login(['manage']), reboot([GHOST]), { by: 'manage' }],
    [
      'hold, no target',
      login(['team_manage'], ['geth']),
      hold('hold', []),
      { reason: `upgradoor:team_manage ${NONE}` },
    ],
    [
      'the twin of another feature',
      login(['team_manage_image', 'team_details'], ['geth']),
      hold('hold', [MIXED]),
      { reason: 'Needs upgradoor:manage or upgradoor:team_manage for a team that owns this host' },
    ],

    // The flags and the selector all need nothing more; the twin applies its rule to every host.
    ['reboot with force, plain', login(['manage']), reboot([MIXED], { force: true }), { by: 'manage' }],
    ['reboot with force, admin includes manage', login(['admin']), reboot([MIXED], { force: true }), { by: 'manage' }],
    ['reboot, the team features', login(ALL_TWINS, ['geth']), reboot([MIXED]), { reason: 'Needs upgradoor:manage' }],
    [
      'roll with force and ignoreBudget, team',
      team('lighthouse'),
      roll([MIXED], ['lighthouse'], { force: true, ignoreBudget: true }),
      { by: 'team_manage' },
    ],
    [
      'roll with force, team, another container',
      team('lighthouse'),
      roll([MIXED], ['geth'], { force: true }),
      { reason: 'upgradoor:team_manage does not cover container geth on host lighthouse-geth-1 (owners: geth)' },
    ],
    ['roll, staggered is no flag', login(['manage']), roll([MIXED], [], { staggered: true }), { by: 'manage' }],
    ['roll all, plain', login(['manage']), roll([], [], { all: true }), { by: 'manage' }],
    [
      'roll all, team: every host',
      team('lighthouse'),
      roll([], ['lighthouse'], { all: true }),
      { reason: 'upgradoor:team_manage does not cover container lighthouse on host teku-geth-1 (owners: none)' },
    ],
    [
      'roll all, team twin and admin: admin includes manage',
      login(['team_manage', 'admin'], ['geth', 'lighthouse']),
      roll([], [], { all: true }),
      { by: 'manage' },
    ],
    ['reboot all, plain', login(['manage']), reboot([], { all: true }), { by: 'manage' }],

    // One rollout, or everything.
    ['pause one rollout with admin only', login(['admin']), scope('pause', 'r-1'), { by: 'manage' }],
    ['pause everything with rollouts only', login(['manage']), scope('pause', ''), { reason: 'Needs upgradoor:admin' }],
    [
      'abort a rollout for no team with the team features',
      login(ALL_TWINS, ['geth']),
      scope('abort', 'r-1'),
      { reason: 'upgradoor:team_manage does not cover rollout r-1: it is for no team' },
    ],

    // Retry: hosts are a recovery, none is the whole rollout.
    ['retry hosts, team', team('lighthouse'), rolloutHosts('retry', [MIXED]), { by: 'team_manage' }],
    [
      'retry hosts, another team',
      team('geth'),
      rolloutHosts('retry', [MIXED]),
      { reason: 'upgradoor:team_manage does not cover rollout r-roll (owners: lighthouse)' },
    ],
    [
      'retry hosts of a reboot, team',
      team('geth', 'lighthouse'),
      { action: 'retry', body: { rollout: 'r-reboot', hosts: [MIXED], reason: '' } },
      { reason: 'upgradoor:team_manage does not cover rollout r-reboot: it rolls no containers (reboot)' },
    ],
    [
      'retry hosts of a manual roll for no team',
      team('geth', 'lighthouse'),
      { action: 'retry', body: { rollout: 'r-1', hosts: [MIXED], reason: '' } },
      { reason: 'upgradoor:team_manage does not cover rollout r-1: it is for no team' },
    ],
    ['retry the rollout, team', team('lighthouse'), rolloutHosts('retry', []), { by: 'team_manage' }],
    [
      'retry the rollout, another team',
      team('geth'),
      rolloutHosts('retry', []),
      { reason: 'upgradoor:team_manage does not cover rollout r-roll (owners: lighthouse)' },
    ],
    ['retry the rollout, plain', login(['manage']), rolloutHosts('retry', []), { by: 'manage' }],
    ['retry hosts, plain', login(['manage']), rolloutHosts('retry', [MIXED]), { by: 'manage' }],

    // A lease is the team's when every need of it rolls the team's container.
    ['end a lease of the team', team('lighthouse'), endLease('lease-mixed'), { by: 'team_manage' }],
    [
      "end a lease of another team's roll",
      team('geth'),
      endLease('lease-mixed'),
      {
        reason:
          'upgradoor:team_manage does not cover container lighthouse on host lighthouse-geth-1 (owners: lighthouse)',
      },
    ],
    [
      'end a lease that reboots as well',
      team('geth', 'lighthouse'),
      endLease('lease-reboot'),
      { reason: 'upgradoor:team_manage does not cover lease lease-reboot: it carries a reboot' },
    ],
    [
      'end a lease of a host nobody owns',
      team('geth'),
      endLease('lease-unowned'),
      { reason: 'upgradoor:team_manage does not cover container bootnode on host bootnode-1 (owners: none)' },
    ],
    [
      'end an unknown lease',
      team('geth'),
      endLease('lease-ghost'),
      { reason: 'upgradoor:team_manage does not cover lease lease-ghost (owners: none)' },
    ],
    ['end an unknown lease, plain feature', login(['manage']), endLease('lease-ghost'), { by: 'manage' }],
  ];

  it.each(cases)('%s', (_name, who, call, want) => {
    const verdict = can(who, call, OWNERSHIP);

    if ('by' in want) {
      expect(verdict).toMatchObject({ allowed: true, reason: '', by: want.by });
    } else {
      expect(verdict).toEqual({ allowed: false, reason: want.reason, by: '' });
    }
  });
});

describe('needsOwnership', () => {
  it('is true only for a login that acts through a team feature', () => {
    expect(needsOwnership(OPEN)).toBe(false);
    expect(needsOwnership(login([]))).toBe(false);
    expect(needsOwnership(login('all'))).toBe(false);
    expect(needsOwnership(login(['manage', 'team_manage']))).toBe(false);
    expect(needsOwnership(login(['manage', 'admin']))).toBe(false);
    expect(needsOwnership(login(['team_manage']))).toBe(true);
    expect(needsOwnership(login(['manage', 'team_manage_image']))).toBe(true);
    expect(needsOwnership(login(['team_details']))).toBe(false);
  });
});

describe('needsActionHosts', () => {
  it('is true for a login without manage', () => {
    expect(needsActionHosts(OPEN)).toBe(false);
    expect(needsActionHosts(login('all'))).toBe(false);
    expect(needsActionHosts(login(['manage']))).toBe(false);
    expect(needsActionHosts(login([]))).toBe(true);
    expect(needsActionHosts(login(['admin', 'team_manage']))).toBe(false);
    expect(needsActionHosts(login(['team_manage']))).toBe(true);
  });
});

describe('can, what the review of the first version found', () => {
  const teamRecover = (...teams: string[]) => login(['team_manage'], teams);
  const retry = (rollout: string, hosts: string[]) =>
    ({ action: 'retry', body: { rollout, hosts, reason: '' } }) satisfies ActionCall;

  it('takes a retry with hosts for a recovery, whatever the state of the rollout', () => {
    // An aborted rollout is refused by the controller: an abort is final.
    expect(can(teamRecover('lighthouse'), retry('r-aborted', [MIXED]), OWNERSHIP).by).toBe('team_manage');
    expect(can(teamRecover('lighthouse'), retry('r-roll', [MIXED]), OWNERSHIP).by).toBe('team_manage');
    expect(can(login(['manage']), retry('r-1', [MIXED]), OWNERSHIP).by).toBe('manage');
    expect(can(login(['manage']), retry('r-1', []), OWNERSHIP).by).toBe('manage');
  });

  it('lets a team approve, pause and abort its rollouts', () => {
    const team = login(['team_manage'], ['lighthouse']);

    expect(can(team, scope('resume', 'r-lighthouse'), OWNERSHIP).by).toBe('team_manage');
    expect(can(team, scope('pause', 'r-lighthouse'), OWNERSHIP).by).toBe('team_manage');
    expect(can(team, scope('abort', 'r-lighthouse'), OWNERSHIP).by).toBe('team_manage');
    expect(can(login(['team_manage'], ['geth']), scope('resume', 'r-lighthouse'), OWNERSHIP).reason).toBe(
      'upgradoor:team_manage does not cover rollout r-lighthouse (owners: lighthouse)'
    );
    expect(can(team, scope('resume', 'r-1'), OWNERSHIP).reason).toBe(
      'upgradoor:team_manage does not cover rollout r-1: it is for no team'
    );
    expect(can(team, scope('resume', 'r-ghost'), OWNERSHIP).reason).toBe(
      'upgradoor:team_manage does not cover rollout r-ghost (owners: none)'
    );
  });

  it('lets a login that may run the action on every member host approve an action rollout', () => {
    const action = (teams: string[]) => [{ name: 'security-upgrades', impact: 'node' as const, teams }];
    const hosts: OwnedHost[] = [
      { name: MIXED, actions: action(['lighthouse']) },
      { name: TEKU, actions: action(['lighthouse', 'teku']) },
      { name: UNOWNED, actions: action(['ops']) },
    ];
    const ownership = (members: string[]) => ({
      hosts,
      rolloutAction: (id: string) => (id === 'r-action' ? { action: 'security-upgrades', hosts: members } : undefined),
    });
    const team = login([], ['lighthouse']);

    expect(can(team, scope('resume', 'r-action'), ownership([MIXED, TEKU]))).toEqual({
      allowed: true,
      reason: '',
      by: 'team',
    });
    expect(
      can(team, { action: 'ignore-gate', body: { rollout: 'r-action', reason: '' } }, ownership([MIXED, TEKU])).by
    ).toBe('team');
    expect(
      can(team, { action: 'ignore-budget', body: { rollout: 'r-action', reason: '' } }, ownership([MIXED, TEKU])).by
    ).toBe('team');

    // One member the team may not run the action on, or one that does not report it, keeps manage.
    for (const members of [[MIXED, UNOWNED], [MIXED, FRESH], []]) {
      expect(can(team, scope('resume', 'r-action'), ownership(members)).reason).toBe(
        'Needs upgradoor:manage or upgradoor:team_manage for a team that owns this rollout; ' +
          'or the feature or a team of the action security-upgrades on every host of the rollout'
      );
    }

    // Pause and abort, and any other rollout, stay manage's.
    expect(can(team, scope('pause', 'r-action'), ownership([MIXED])).allowed).toBe(false);
    expect(can(team, scope('abort', 'r-action'), ownership([MIXED])).allowed).toBe(false);
    expect(can(team, scope('resume', 'r-lighthouse'), { ...OWNERSHIP, ...ownership([MIXED]) }).reason).toBe(
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns this rollout'
    );
    expect(can(login(['manage']), scope('resume', 'r-action'), ownership([UNOWNED])).by).toBe('manage');
  });

  it("lifts only the holds of the login's teams on the team path", () => {
    const hosts: OwnedHost[] = [
      {
        name: MIXED,
        holders: ['staff', 'geth', 'lighthouse'],
        containers: [owned('geth', 'geth'), owned('lighthouse', 'lighthouse')],
      },
    ];
    const twin = login(['team_manage'], ['lighthouse']);

    // The team's own holds, by default or by name; never staff's or another team's.
    expect(can(twin, hold('unhold', [MIXED]), { hosts }).by).toBe('team_manage');
    expect(can(twin, hold('unhold', [MIXED], ['lighthouse']), { hosts }).by).toBe('team_manage');
    expect(can(twin, hold('hold', [MIXED]), { hosts }).by).toBe('team_manage');
    for (const holder of ['staff', 'geth']) {
      expect(can(twin, hold('unhold', [MIXED], [holder]), { hosts })).toEqual({
        allowed: false,
        reason: `upgradoor:team_manage does not lift the hold of ${holder} on host lighthouse-geth-1`,
        by: '',
      });
    }
    expect(can(login(['team_manage'], ['teku']), hold('unhold', [MIXED]), { hosts }).reason).toBe(
      'upgradoor:team_manage does not cover host lighthouse-geth-1 (owners: geth, lighthouse)'
    );

    // The plain feature lifts any hold it names; mark-offline has no twin.
    expect(can(login(['manage']), hold('unhold', [MIXED], ['staff', 'geth']), { hosts }).by).toBe('manage');
    expect(can(twin, { action: 'unmark-offline', body: { hosts: [MIXED], reason: '' } }, { hosts }).reason).toBe(
      'Needs upgradoor:manage'
    );
  });

  it('takes no teams for no owners, and a team for its exact name', () => {
    expect(can(login(['team_manage']), hold('hold', [UNOWNED]), OWNERSHIP).reason).toBe(
      'upgradoor:team_manage does not cover host bootnode-1 (owners: none)'
    );
    expect(can(login(['team_manage'], []), roll([UNOWNED], ['bootnode']), OWNERSHIP).reason).toBe(
      'upgradoor:team_manage does not cover container bootnode on host bootnode-1 (owners: none)'
    );
    expect(can(login(['team_manage'], ['']), hold('hold', [FRESH]), OWNERSHIP).allowed).toBe(false);
    expect(can(login(['team_manage'], ['Lighthouse', 'GETH', 'light']), hold('hold', [MIXED]), OWNERSHIP).reason).toBe(
      'upgradoor:team_manage does not cover host lighthouse-geth-1 (owners: geth, lighthouse)'
    );
    expect(can(login(['team_manage'], ['Lighthouse']), roll([MIXED], ['lighthouse']), OWNERSHIP).allowed).toBe(false);
  });

  it('bounds the names a reason repeats', () => {
    const name = 'bad\u0007\u001b[2J' + 'h'.repeat(4000);
    const reason = can(login(['team_manage'], ['geth']), hold('hold', [name]), OWNERSHIP).reason;

    expect(reason).toBe(`upgradoor:team_manage does not cover host bad??[2J${'h'.repeat(128 - 8)} (owners: none)`);
  });

  it('has no feature whose name starts with another, but manage: its prefix grant gives the three manage features', () => {
    const names = [...Object.values(FEATURE), ...ALL_TWINS, TEAM_PREFIX + FEATURE.details];

    for (const a of Object.values(FEATURE)) {
      const want = a === FEATURE.manage ? [FEATURE.manage, FEATURE.manage_image, FEATURE.manage_flags] : [a];
      expect(names.filter(b => b.startsWith(a))).toEqual(want);
    }
  });

  it('opens an override of a client on many hosts to a team that owns a container of it', () => {
    const group = (client: string, part: 'image' | 'flags') =>
      ({
        action: 'client-override',
        body: { client, kind: 'cl', hosts: [MIXED, TEKU], part, reason: '' },
      }) satisfies ActionCall;
    const ownership = {
      ...OWNERSHIP,
      clients: [
        { name: 'lighthouse', hosts: 2 },
        { name: 'teku', hosts: 2 },
      ],
    };

    expect(can(login([FEATURE.manage_image]), group('teku', 'image'), ownership)).toMatchObject({
      allowed: true,
      by: 'manage_image',
    });
    expect(can(login([FEATURE.manage_image]), group('teku', 'flags'), ownership).allowed).toBe(false);
    // The controller judges each host and lists the ones the team does not cover.
    expect(can(login([TEAM_PREFIX + FEATURE.manage_flags], ['teku']), group('teku', 'flags'), ownership)).toMatchObject(
      { allowed: true, by: 'team_manage_flags' }
    );
    expect(can(login([TEAM_PREFIX + FEATURE.manage_flags], ['teku']), group('lighthouse', 'flags'), ownership)).toEqual(
      expect.objectContaining({
        allowed: false,
        reason: 'upgradoor:team_manage_flags does not cover any container of client lighthouse (owners: lighthouse)',
      })
    );
  });

  it('covers the policy of a client for its team', () => {
    const policy = (client: string) =>
      ({ action: 'client-policy', body: { client, policy: 'manual', reason: '' } }) satisfies ActionCall;
    const clients = [
      { name: 'lighthouse', hosts: 2 },
      { name: 'teku', hosts: 2 },
      { name: 'retired', hosts: 0 },
    ];
    const ownership = { ...OWNERSHIP, clients };
    const twin = TEAM_PREFIX + FEATURE.manage;

    expect(TWINNED).toContain(FEATURE.manage);
    expect(can(login([FEATURE.manage]), policy('geth'), OWNERSHIP)).toMatchObject({
      allowed: true,
      by: 'manage',
    });
    expect(can(login([twin], ['lighthouse']), policy('lighthouse'), ownership)).toMatchObject({
      allowed: true,
      by: 'team_manage',
    });
    expect(can(login([twin], ['nimbus']), policy('teku'), ownership).reason).toBe(
      'upgradoor:team_manage does not cover client teku (owners: teku)'
    );
    for (const client of ['retired', 'unknown']) {
      expect(can(login([twin], [client]), policy(client), ownership).reason, client).toBe(
        `upgradoor:team_manage does not cover client ${client} (owners: none)`
      );
    }
    // Without the client table nobody owns a client; without a team neither.
    expect(can(login([twin], ['lighthouse']), policy('lighthouse'), { ...OWNERSHIP, clients: [] }).allowed).toBe(false);
    expect(can(login([twin]), policy('lighthouse'), ownership).allowed).toBe(false);

    expect(can(login(['manage_image', 'manage_flags', 'details']), policy('geth'), ownership).reason).toBe(
      'Needs upgradoor:manage or upgradoor:team_manage for a team that owns every container of this client'
    );
    expect(can(login(['admin']), policy('geth'), ownership).by).toBe('manage');
    expect(can(OPEN, policy('geth'), OWNERSHIP).allowed).toBe(true);
  });

  it('covers an override for a team that owns exactly that container on that host', () => {
    const image = (host: string, container: string) =>
      ({ action: 'image-override', body: { host, container, image: 'a/b:c', reason: '' } }) satisfies ActionCall;
    const flags = (host: string, container: string) =>
      ({
        action: 'flag-override',
        body: { host, container, addFlags: ['--x'], removeFlags: [], reason: '' },
      }) satisfies ActionCall;
    const lighthouse = ['lighthouse'];

    for (const [feature, call] of [
      [FEATURE.manage_image, image],
      [FEATURE.manage_flags, flags],
    ] as const) {
      const twin = TEAM_PREFIX + feature;
      expect(TWINNED).toContain(feature);

      expect(can(login([feature]), call(MIXED, 'geth'), OWNERSHIP)).toMatchObject({ allowed: true, by: feature });
      expect(can(login([twin], lighthouse), call(MIXED, 'lighthouse'), OWNERSHIP)).toMatchObject({
        allowed: true,
        by: twin,
      });
      // The rule of a roll of one container: another team's, an unmanaged one and one the host does not have.
      expect(can(login([twin], lighthouse), call(MIXED, 'geth'), OWNERSHIP).reason).toBe(
        `upgradoor:${twin} does not cover container geth on host ${MIXED} (owners: geth)`
      );
      for (const container of ['debug', 'nope']) {
        expect(can(login([twin], lighthouse), call(MIXED, container), OWNERSHIP).allowed, container).toBe(false);
      }
      expect(can(login([twin], lighthouse), call(GHOST, 'lighthouse'), OWNERSHIP).allowed).toBe(false);

      // One feature does not give the other, and neither does roll.
      const others = [FEATURE.manage, feature === FEATURE.manage_image ? FEATURE.manage_flags : FEATURE.manage_image];
      expect(can(login(others), call(MIXED, 'geth'), OWNERSHIP).reason).toBe(
        `Needs upgradoor:${feature} or upgradoor:${twin} for a team that owns this container`
      );
    }
  });

  it('names the one feature that allows the request, whatever its flags', () => {
    const lighthouse = ['lighthouse'];
    const now = roll([MIXED], ['lighthouse']);
    const forced = roll([MIXED], ['lighthouse'], { staggered: true, force: true, ignoreBudget: true });

    expect(can(login(['team_manage'], lighthouse), now, OWNERSHIP).by).toBe('team_manage');
    expect(can(login(['team_manage'], lighthouse), forced, OWNERSHIP).by).toBe('team_manage');
    expect(can(login(['team_manage', 'admin'], lighthouse), now, OWNERSHIP).by).toBe('manage');
    expect(can(login(['manage'], lighthouse), forced, OWNERSHIP).by).toBe('manage');
    expect(can(login(['team_manage', 'admin'], lighthouse), hold('hold', [MIXED]), OWNERSHIP).by).toBe('manage');
  });
});

describe('hasFeature, the implications of the controller', () => {
  it('gives admin every feature and a plain feature its twin, nothing else', () => {
    for (const f of [FEATURE.details, FEATURE.manage, FEATURE.manage_image, FEATURE.manage_flags]) {
      expect(hasFeature(login([FEATURE.admin]), f), f).toBe(true);
      expect(hasFeature(login([FEATURE.admin]), TEAM_PREFIX + f), f).toBe(true);
      expect(hasFeature(login([f]), TEAM_PREFIX + f), f).toBe(true);
      expect(hasFeature(login([TEAM_PREFIX + f]), f), f).toBe(false);
    }

    for (const f of [FEATURE.details, FEATURE.manage_image, FEATURE.manage_flags]) {
      expect(hasFeature(login([FEATURE.manage]), f), f).toBe(false);
      expect(hasFeature(login([FEATURE.manage, 'team_manage']), TEAM_PREFIX + f), f).toBe(false);
    }
    expect(hasFeature(login(['details', 'manage', 'manage_image', 'manage_flags']), FEATURE.admin)).toBe(false);
  });
});
