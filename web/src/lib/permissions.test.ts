// The table of pkg/controller/userapi/actions_test.go, for the page's copy of the
// rule: every action against every kind of login, and what the team twins
// cover.

import { describe, expect, it } from 'vitest';

import type { ActionCall } from '../api/client';
import { FEATURE, TEAM_PREFIX, TWINNED, can, needsOwnership, type Login, type OwnedHost } from './permissions';
import type { PermissionCall } from './permissions';

const MIXED = 'lighthouse-geth-1';
const TEKU = 'teku-geth-1';
const UNOWNED = 'bootnode-1';
const FRESH = 'fresh-1';
const GHOST = 'ghost-1';

const owned = (name: string, ...owners: string[]) => ({ name, managed: true, owners });

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
  { name: TEKU, containers: [owned('geth', 'geth'), owned('teku', 'nimbus', 'teku')] },
  { name: UNOWNED, containers: [{ name: 'bootnode', managed: true }] },
  // Registered, never synced.
  { name: FRESH, containers: null },
];

const LEASES: Record<string, string> = { 'lease-mixed': MIXED, 'lease-unowned': UNOWNED };

/** r-lighthouse rolls the client lighthouse, r-1 no client; others are unknown. */
const ROLLOUT_CLIENTS: Record<string, string> = { 'r-lighthouse': 'lighthouse', 'r-1': '' };

const OWNERSHIP = {
  hosts: HOSTS,
  clients: [{ name: 'lighthouse', hosts: 1, allOwned: ['lighthouse'], owners: ['lighthouse'] }],
  leaseHost: (id: string) => LEASES[id],
  rolloutClient: (id: string) => ROLLOUT_CLIENTS[id],
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
const hold = (action: 'hold' | 'unhold' | 'forget', hosts: string[]) =>
  ({ action, body: { hosts, reason: '' } }) satisfies ActionCall;
const scope = (action: 'pause' | 'resume' | 'abort', rollout: string) =>
  ({ action, body: { rollout, reason: '' } }) satisfies ActionCall;
const rolloutHosts = (action: 'skip' | 'retry', hosts: string[]) =>
  ({ action, body: { rollout: 'r-1', hosts, reason: '' } }) satisfies ActionCall;
const endLease = (lease: string) => ({ action: 'end-lease', body: { lease, reason: '' } }) satisfies ActionCall;

interface Sample {
  name: string;
  call: PermissionCall;
  feature: string;
  twin: boolean;
}

/** One request per kind of action, on targets the team lighthouse covers. */
const SAMPLES: Sample[] = [
  { name: 'roll', call: roll([MIXED], ['lighthouse']), feature: FEATURE.roll, twin: true },
  { name: 'reboot', call: reboot([MIXED]), feature: FEATURE.reboot, twin: true },
  { name: 'hold', call: hold('hold', [MIXED]), feature: FEATURE.hold, twin: true },
  { name: 'unhold', call: hold('unhold', [MIXED]), feature: FEATURE.hold, twin: true },
  { name: 'skip', call: rolloutHosts('skip', [MIXED]), feature: FEATURE.recover, twin: true },
  { name: 'retry hosts', call: rolloutHosts('retry', [MIXED]), feature: FEATURE.recover, twin: true },
  { name: 'end-lease', call: endLease('lease-mixed'), feature: FEATURE.recover, twin: true },
  { name: 'pause one', call: scope('pause', 'r-lighthouse'), feature: FEATURE.supervise, twin: true },
  { name: 'resume one', call: scope('resume', 'r-lighthouse'), feature: FEATURE.supervise, twin: true },
  { name: 'abort', call: scope('abort', 'r-1'), feature: FEATURE.supervise, twin: false },
  { name: 'retry rollout', call: rolloutHosts('retry', []), feature: FEATURE.supervise, twin: false },
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
      { name: 'every feature', login: login('all', ['besu']), by: s.feature },
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
      {
        name: 'admin alone implies nothing',
        login: login([FEATURE.admin], ['lighthouse']),
        ...(s.feature === FEATURE.admin ? { by: FEATURE.admin } : { reason: lacks }),
      },
      {
        name: 'every other feature',
        login: login(Object.values(FEATURE).filter(f => f !== s.feature)),
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
    ['one host', reboot([MIXED]), 'Needs upgradoor:reboot or upgradoor:team_reboot for a team that owns this host'],
    [
      'several hosts',
      hold('hold', [MIXED, TEKU]),
      'Needs upgradoor:hold or upgradoor:team_hold for a team that owns these hosts',
    ],
    [
      'a roll of the host',
      roll([MIXED]),
      'Needs upgradoor:roll or upgradoor:team_roll for a team that owns every managed container of the host',
    ],
    [
      'a roll of containers',
      roll([MIXED], ['geth']),
      'Needs upgradoor:roll or upgradoor:team_roll for a team that owns these containers',
    ],
    [
      'a lease',
      endLease('lease-mixed'),
      'Needs upgradoor:recover or upgradoor:team_recover for a team that owns the host of this lease',
    ],
    ['no twin', scope('abort', 'r-1'), 'Needs upgradoor:supervise'],
    ['the settings', { action: 'settings' }, 'Needs upgradoor:admin'],
  ])('%s', (_name, call, reason) => {
    expect(can(nobody, call, OWNERSHIP)).toEqual({ allowed: false, reason, by: '', staggered: false });
  });

  it('names the configured service', () => {
    expect(can(login([], [], 'upgradoor-dev'), reboot([MIXED]), OWNERSHIP).reason).toBe(
      'Needs upgradoor-dev:reboot or upgradoor-dev:team_reboot for a team that owns this host'
    );
    expect(can(login(['team_reboot'], ['teku'], 'upgradoor-dev'), reboot([MIXED]), OWNERSHIP).reason).toBe(
      'upgradoor-dev:team_reboot does not cover host lighthouse-geth-1 (owners: geth, lighthouse)'
    );
  });
});

describe('can, what the team twins cover', () => {
  const teamRoll = (...teams: string[]) => login(['team_roll'], teams);
  const teamReboot = (...teams: string[]) => login(['team_reboot'], teams);
  const teamRecover = (...teams: string[]) => login(['team_recover'], teams);

  const NONE = 'needs a target that one of your teams owns, and the request names none';

  // [name, login, call, what allows it | why not]
  const cases: [string, Login, PermissionCall, { by: string } | { reason: string }][] = [
    // A roll with named containers: every one of them.
    ['roll, own container', teamRoll('lighthouse'), roll([MIXED], ['lighthouse']), { by: 'team_roll' }],
    [
      'roll, own containers',
      teamRoll('lighthouse'),
      roll([MIXED], ['lighthouse', 'lighthouse-validator']),
      { by: 'team_roll' },
    ],
    [
      'roll, mixed ownership',
      teamRoll('lighthouse'),
      roll([MIXED], ['lighthouse', 'geth']),
      { reason: 'upgradoor:team_roll does not cover container geth on host lighthouse-geth-1 (owners: geth)' },
    ],
    [
      'roll, mixed ownership, in both teams',
      teamRoll('geth', 'lighthouse'),
      roll([MIXED], ['lighthouse', 'geth']),
      { by: 'team_roll' },
    ],
    ['roll, a container of two teams', teamRoll('nimbus'), roll([TEKU], ['teku']), { by: 'team_roll' }],
    [
      'roll, a container of two teams, a third team',
      teamRoll('lighthouse'),
      roll([TEKU], ['teku']),
      { reason: 'upgradoor:team_roll does not cover container teku on host teku-geth-1 (owners: nimbus, teku)' },
    ],
    ['roll, one container on two hosts', teamRoll('geth'), roll([MIXED, TEKU], ['geth']), { by: 'team_roll' }],
    [
      'roll, a container one host does not have',
      teamRoll('geth'),
      roll([MIXED, UNOWNED], ['geth']),
      { reason: 'upgradoor:team_roll does not cover container geth on host bootnode-1 (owners: none)' },
    ],
    [
      'roll, unknown container',
      teamRoll('geth', 'lighthouse'),
      roll([MIXED], ['nethermind']),
      { reason: 'upgradoor:team_roll does not cover container nethermind on host lighthouse-geth-1 (owners: none)' },
    ],
    [
      'roll, unmanaged container',
      teamRoll('geth', 'lighthouse'),
      roll([MIXED], ['debug']),
      { reason: 'upgradoor:team_roll does not cover container debug on host lighthouse-geth-1 (owners: none)' },
    ],
    ['roll, unknown container, plain feature', login(['roll']), roll([MIXED], ['nethermind']), { by: 'roll' }],

    // A roll without containers: every managed container of the host.
    [
      'roll a host, one team of two',
      teamRoll('lighthouse'),
      roll([MIXED]),
      { reason: 'upgradoor:team_roll does not cover container geth on host lighthouse-geth-1 (owners: geth)' },
    ],
    ['roll a host, both teams', teamRoll('geth', 'lighthouse'), roll([MIXED]), { by: 'team_roll' }],
    [
      'roll hosts, second not covered',
      teamRoll('geth', 'lighthouse'),
      roll([MIXED, TEKU]),
      { reason: 'upgradoor:team_roll does not cover container teku on host teku-geth-1 (owners: nimbus, teku)' },
    ],
    [
      'roll a host nobody owns',
      teamRoll('geth'),
      roll([UNOWNED]),
      { reason: 'upgradoor:team_roll does not cover container bootnode on host bootnode-1 (owners: none)' },
    ],
    [
      'roll a host that never synced',
      teamRoll('geth'),
      roll([FRESH]),
      { reason: 'upgradoor:team_roll does not cover host fresh-1 (owners: none)' },
    ],
    [
      'roll an unknown host',
      teamRoll('geth'),
      roll([GHOST]),
      { reason: 'upgradoor:team_roll does not cover host ghost-1 (owners: none)' },
    ],
    [
      'roll an unknown host, named container',
      teamRoll('geth'),
      roll([GHOST], ['geth']),
      { reason: 'upgradoor:team_roll does not cover container geth on host ghost-1 (owners: none)' },
    ],
    ['roll an unknown host, plain feature', login(['roll']), roll([GHOST]), { by: 'roll' }],
    ['roll, no target', teamRoll('geth'), roll([]), { reason: `upgradoor:team_roll ${NONE}` }],

    // Host-level actions: one owned container is enough.
    ['reboot, the CL team', teamReboot('lighthouse'), reboot([MIXED]), { by: 'team_reboot' }],
    ['reboot, the EL team', teamReboot('geth'), reboot([MIXED]), { by: 'team_reboot' }],
    [
      'reboot, a team without a container there',
      teamReboot('teku'),
      reboot([MIXED]),
      { reason: 'upgradoor:team_reboot does not cover host lighthouse-geth-1 (owners: geth, lighthouse)' },
    ],
    ['reboot two hosts, the EL team', teamReboot('geth'), reboot([MIXED, TEKU]), { by: 'team_reboot' }],
    [
      'reboot two hosts, second not covered',
      teamReboot('lighthouse'),
      reboot([MIXED, TEKU]),
      { reason: 'upgradoor:team_reboot does not cover host teku-geth-1 (owners: geth, nimbus, teku)' },
    ],
    [
      'reboot a host nobody owns',
      teamReboot('geth'),
      reboot([UNOWNED]),
      { reason: 'upgradoor:team_reboot does not cover host bootnode-1 (owners: none)' },
    ],
    [
      'reboot a host that never synced',
      teamReboot('geth'),
      reboot([FRESH]),
      { reason: 'upgradoor:team_reboot does not cover host fresh-1 (owners: none)' },
    ],
    [
      'reboot an unknown host',
      teamReboot('geth'),
      reboot([GHOST]),
      { reason: 'upgradoor:team_reboot does not cover host ghost-1 (owners: none)' },
    ],
    ['reboot an unknown host, plain feature', login(['reboot']), reboot([GHOST]), { by: 'reboot' }],
    ['hold, no target', login(['team_hold'], ['geth']), hold('hold', []), { reason: `upgradoor:team_hold ${NONE}` }],
    [
      'the twin of another feature',
      teamReboot('geth'),
      hold('hold', [MIXED]),
      { reason: 'Needs upgradoor:hold or upgradoor:team_hold for a team that owns this host' },
    ],

    // The flags and the selector all need admin on top.
    [
      'reboot with force, no admin',
      login(['reboot']),
      reboot([MIXED], { force: true }),
      { reason: 'Needs upgradoor:admin (for force)' },
    ],
    ['reboot with force, admin', login(['reboot', 'admin']), reboot([MIXED], { force: true }), { by: 'reboot,admin' }],
    [
      'reboot with force, admin without reboot',
      login(['admin']),
      reboot([MIXED], { force: true }),
      { reason: 'Needs upgradoor:reboot or upgradoor:team_reboot for a team that owns this host' },
    ],
    [
      'reboot with force, team twin and admin',
      login(['team_reboot', 'admin'], ['geth']),
      reboot([MIXED], { force: true }),
      { by: 'team_reboot,admin' },
    ],
    [
      'reboot with force, team twin without admin',
      teamReboot('geth'),
      reboot([MIXED], { force: true }),
      { reason: 'Needs upgradoor:admin (for force)' },
    ],
    [
      'reboot with force, team twin and admin, not covered',
      login(['team_reboot', 'admin'], ['geth']),
      reboot([UNOWNED], { force: true }),
      { reason: 'upgradoor:team_reboot does not cover host bootnode-1 (owners: none)' },
    ],
    ['reboot with force, everything', login('all'), reboot([MIXED], { force: true }), { by: 'reboot,admin' }],
    [
      'roll with ignoreBudget, no admin',
      teamRoll('lighthouse'),
      roll([MIXED], ['lighthouse'], { ignoreBudget: true }),
      { reason: 'Needs upgradoor:admin (for ignoreBudget)' },
    ],
    [
      'roll with ignoreBudget, admin',
      login(['team_roll', 'admin'], ['lighthouse']),
      roll([MIXED], ['lighthouse'], { ignoreBudget: true }),
      { by: 'team_roll,admin' },
    ],
    ['roll, staggered is no flag', login(['roll']), roll([MIXED], [], { staggered: true }), { by: 'roll' }],
    ['roll all, plain', login(['roll']), roll([], [], { all: true }), { reason: 'Needs upgradoor:admin (for all)' }],
    ['roll all, admin', login(['roll', 'admin']), roll([], [], { all: true }), { by: 'roll,admin' }],
    [
      'roll all, team twin and admin: not every host is covered',
      login(['team_roll', 'admin'], ['geth', 'lighthouse']),
      roll([], [], { all: true }),
      { reason: 'upgradoor:team_roll does not cover container teku on host teku-geth-1 (owners: nimbus, teku)' },
    ],
    ['reboot all, plain', login(['reboot']), reboot([], { all: true }), { reason: 'Needs upgradoor:admin (for all)' }],
    ['reboot all, admin', login(['reboot', 'admin']), reboot([], { all: true }), { by: 'reboot,admin' }],

    // One rollout, or everything.
    [
      'pause one rollout with admin only',
      login(['admin']),
      scope('pause', 'r-1'),
      {
        reason:
          "Needs upgradoor:supervise or upgradoor:team_supervise for a team that owns every container of this rollout's client",
      },
    ],
    [
      'pause everything with rollouts only',
      login(['supervise']),
      scope('pause', ''),
      { reason: 'Needs upgradoor:admin' },
    ],
    [
      'abort with the team features',
      login(ALL_TWINS, ['geth']),
      scope('abort', 'r-1'),
      { reason: 'Needs upgradoor:supervise' },
    ],

    // Retry: hosts are a recovery, none is the whole rollout.
    ['retry hosts, team', teamRecover('geth'), rolloutHosts('retry', [MIXED]), { by: 'team_recover' }],
    [
      'retry hosts, team, not covered',
      teamRecover('geth'),
      rolloutHosts('retry', [MIXED, UNOWNED]),
      { reason: 'upgradoor:team_recover does not cover host bootnode-1 (owners: none)' },
    ],
    [
      'retry the rollout, team',
      teamRecover('geth'),
      rolloutHosts('retry', []),
      { reason: 'Needs upgradoor:supervise' },
    ],
    [
      'retry the rollout, recover',
      login(['recover']),
      rolloutHosts('retry', []),
      { reason: 'Needs upgradoor:supervise' },
    ],
    [
      'retry hosts, rollouts',
      login(['supervise']),
      rolloutHosts('retry', [MIXED]),
      { reason: 'Needs upgradoor:recover or upgradoor:team_recover for a team that owns this host' },
    ],
    [
      'skip, team, not covered',
      teamRecover('lighthouse'),
      rolloutHosts('skip', [TEKU]),
      { reason: 'upgradoor:team_recover does not cover host teku-geth-1 (owners: geth, nimbus, teku)' },
    ],

    // A lease stands for its host.
    ['end a lease of an owned host', teamRecover('geth'), endLease('lease-mixed'), { by: 'team_recover' }],
    [
      "end a lease of another team's host",
      teamRecover('teku'),
      endLease('lease-mixed'),
      { reason: 'upgradoor:team_recover does not cover host lighthouse-geth-1 (owners: geth, lighthouse)' },
    ],
    [
      'end a lease of a host nobody owns',
      teamRecover('geth'),
      endLease('lease-unowned'),
      { reason: 'upgradoor:team_recover does not cover host bootnode-1 (owners: none)' },
    ],
    [
      'end an unknown lease',
      teamRecover('geth'),
      endLease('lease-ghost'),
      { reason: 'upgradoor:team_recover does not cover lease lease-ghost (owners: none)' },
    ],
    ['end an unknown lease, plain feature', login(['recover']), endLease('lease-ghost'), { by: 'recover' }],
  ];

  it.each(cases)('%s', (_name, who, call, want) => {
    const verdict = can(who, call, OWNERSHIP);

    if ('by' in want) {
      expect(verdict).toMatchObject({ allowed: true, reason: '', by: want.by });
    } else {
      expect(verdict).toEqual({ allowed: false, reason: want.reason, by: '', staggered: false });
    }
  });
});

describe('needsOwnership', () => {
  it('is true only for a login that acts through a team feature', () => {
    expect(needsOwnership(OPEN)).toBe(false);
    expect(needsOwnership(login([]))).toBe(false);
    expect(needsOwnership(login('all'))).toBe(false);
    expect(needsOwnership(login(['roll', 'team_roll']))).toBe(false);
    expect(needsOwnership(login(['supervise', 'admin']))).toBe(false);
    expect(needsOwnership(login(['team_hold']))).toBe(true);
    expect(needsOwnership(login(['roll', 'team_reboot']))).toBe(true);
  });
});

describe('can, what the review of the first version found', () => {
  const teamRecover = (...teams: string[]) => login(['team_recover'], teams);
  const retry = (rollout: string, hosts: string[]) =>
    ({ action: 'retry', body: { rollout, hosts, reason: '' } }) satisfies ActionCall;

  it('takes a retry with hosts for a recovery, whatever the state of the rollout', () => {
    // An aborted rollout is refused by the controller: an abort is final.
    expect(can(teamRecover('lighthouse'), retry('r-aborted', [MIXED]), OWNERSHIP).by).toBe('team_recover');
    expect(can(teamRecover('lighthouse'), retry('r-1', [MIXED]), OWNERSHIP).by).toBe('team_recover');
    expect(can(login(['supervise']), retry('r-1', [MIXED]), OWNERSHIP).allowed).toBe(false);
    expect(can(login(['supervise']), retry('r-1', []), OWNERSHIP).by).toBe('supervise');
  });

  it("lets a client's team approve and pause the client's rollouts, not abort them", () => {
    const team = login(['team_supervise'], ['lighthouse']);

    expect(can(team, scope('resume', 'r-lighthouse'), OWNERSHIP).by).toBe('team_supervise');
    expect(can(team, scope('pause', 'r-lighthouse'), OWNERSHIP).by).toBe('team_supervise');
    expect(can(team, scope('abort', 'r-lighthouse'), OWNERSHIP).reason).toBe('Needs upgradoor:supervise');
    expect(can(login(['team_supervise'], ['geth']), scope('resume', 'r-lighthouse'), OWNERSHIP).reason).toBe(
      'upgradoor:team_supervise does not cover client lighthouse (owners of all of it: lighthouse)'
    );
    expect(can(team, scope('resume', 'r-1'), OWNERSHIP).reason).toBe(
      'upgradoor:team_supervise does not cover rollout r-1: it is for no client'
    );
    expect(can(team, scope('resume', 'r-ghost'), OWNERSHIP).reason).toBe(
      'upgradoor:team_supervise does not cover rollout r-ghost (owners: none)'
    );
  });

  it('leaves a hold of staff alone on the team path', () => {
    const held = (holdBy?: string): OwnedHost[] => [
      { name: MIXED, holdWanted: true, holdBy, containers: [owned('geth', 'geth'), owned('lighthouse', 'lighthouse')] },
    ];
    const twin = login(['team_hold'], ['lighthouse']);

    for (const holdBy of ['staff', undefined, '']) {
      expect(can(twin, hold('unhold', [MIXED]), { hosts: held(holdBy) })).toEqual({
        allowed: false,
        reason: 'upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by staff)',
        by: '',
        staggered: false,
      });
      expect(can(twin, hold('hold', [MIXED]), { hosts: held(holdBy) }).reason).toBe(
        'upgradoor:team_hold does not change the hold on host lighthouse-geth-1 (set by staff)'
      );
    }

    // A team's hold: the team that set it, not another owner, not a stranger.
    const theirs = held('team:lighthouse');
    expect(can(twin, hold('unhold', [MIXED]), { hosts: theirs }).by).toBe('team_hold');
    expect(can(login(['team_hold'], ['geth']), hold('unhold', [MIXED]), { hosts: theirs }).reason).toBe(
      'upgradoor:team_hold does not lift the hold on host lighthouse-geth-1 (set by team lighthouse)'
    );
    expect(can(login(['team_hold'], ['teku']), hold('unhold', [MIXED]), { hosts: theirs }).reason).toBe(
      'upgradoor:team_hold does not cover host lighthouse-geth-1 (owners: geth, lighthouse)'
    );

    // The plain feature lifts any; other host-level actions do not look at the hold.
    expect(can(login(['hold']), hold('unhold', [MIXED]), { hosts: held('staff') }).by).toBe('hold');
    expect(can(login(['team_reboot'], ['geth']), reboot([MIXED]), { hosts: held('staff') }).by).toBe('team_reboot');
  });

  it('says that a roll or reboot through a team feature alone is run staggered', () => {
    const lighthouse = ['lighthouse'];
    const own = roll([MIXED], ['lighthouse']);

    expect(can(login(['team_roll'], lighthouse), own, OWNERSHIP).staggered).toBe(true);
    expect(can(login(['team_reboot'], lighthouse), reboot([MIXED]), OWNERSHIP).staggered).toBe(true);
    expect(can(login(['team_roll', 'admin'], lighthouse), own, OWNERSHIP).staggered).toBe(false);
    expect(can(login(['roll', 'team_roll'], lighthouse), own, OWNERSHIP).staggered).toBe(false);
    expect(can(login('all'), roll([MIXED]), OWNERSHIP).staggered).toBe(false);
    expect(can(OPEN, roll([MIXED]), OWNERSHIP).staggered).toBe(false);
    expect(can(login(['team_hold'], lighthouse), hold('hold', [MIXED]), OWNERSHIP).staggered).toBe(false);
  });

  it('takes no teams for no owners, and a team for its exact name', () => {
    expect(can(login(['team_reboot']), reboot([UNOWNED]), OWNERSHIP).reason).toBe(
      'upgradoor:team_reboot does not cover host bootnode-1 (owners: none)'
    );
    expect(can(login(['team_roll'], []), roll([UNOWNED], ['bootnode']), OWNERSHIP).reason).toBe(
      'upgradoor:team_roll does not cover container bootnode on host bootnode-1 (owners: none)'
    );
    expect(can(login(['team_reboot'], ['']), reboot([FRESH]), OWNERSHIP).allowed).toBe(false);
    expect(can(login(['team_reboot'], ['Lighthouse', 'GETH', 'light']), reboot([MIXED]), OWNERSHIP).reason).toBe(
      'upgradoor:team_reboot does not cover host lighthouse-geth-1 (owners: geth, lighthouse)'
    );
    expect(can(login(['team_roll'], ['Lighthouse']), roll([MIXED], ['lighthouse']), OWNERSHIP).allowed).toBe(false);
  });

  it('bounds the names a reason repeats', () => {
    const name = 'bad\u0007\u001b[2J' + 'h'.repeat(4000);
    const reason = can(login(['team_hold'], ['geth']), hold('hold', [name]), OWNERSHIP).reason;

    expect(reason).toBe(`upgradoor:team_hold does not cover host bad??[2J${'h'.repeat(128 - 8)} (owners: none)`);
  });

  it('has no feature whose name starts with another: a prefix grant of one gives that one', () => {
    const names = [...Object.values(FEATURE), ...ALL_TWINS];

    for (const a of Object.values(FEATURE)) {
      expect(names.filter(b => b.startsWith(a))).toEqual([a]);
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
        { name: 'lighthouse', hosts: 2, owners: ['lighthouse'] },
        { name: 'teku', hosts: 2, owners: ['nimbus', 'teku'] },
      ],
    };

    expect(can(login([FEATURE.image_override]), group('teku', 'image'), ownership)).toMatchObject({
      allowed: true,
      by: 'image_override',
    });
    expect(can(login([FEATURE.image_override]), group('teku', 'flags'), ownership).allowed).toBe(false);
    // The controller judges each host and lists the ones the team does not cover.
    expect(
      can(login([TEAM_PREFIX + FEATURE.flag_override], ['teku']), group('teku', 'flags'), ownership)
    ).toMatchObject({ allowed: true, by: 'team_flag_override' });
    expect(
      can(login([TEAM_PREFIX + FEATURE.flag_override], ['teku']), group('lighthouse', 'flags'), ownership)
    ).toEqual(
      expect.objectContaining({
        allowed: false,
        reason: 'upgradoor:team_flag_override does not cover any container of client lighthouse (owners: lighthouse)',
      })
    );
  });

  it('covers the policy of a client for a team that owns every container of it', () => {
    const policy = (client: string) =>
      ({ action: 'client-policy', body: { client, policy: 'manual', reason: '' } }) satisfies ActionCall;
    // lighthouse owns its client throughout; on one host the teku container is nimbus's alone.
    const clients = [
      { name: 'lighthouse', hosts: 2, allOwned: ['lighthouse'] },
      { name: 'teku', hosts: 2, allOwned: ['nimbus'] },
      { name: 'geth', hosts: 3, allOwned: [] },
      { name: 'retired', hosts: 0, allOwned: ['lighthouse'] },
    ];
    const ownership = { ...OWNERSHIP, clients };
    const twin = TEAM_PREFIX + FEATURE.client_policy;

    expect(TWINNED).toContain(FEATURE.client_policy);
    expect(can(login([FEATURE.client_policy]), policy('geth'), OWNERSHIP)).toMatchObject({
      allowed: true,
      by: 'client_policy',
    });
    expect(can(login([twin], ['lighthouse']), policy('lighthouse'), ownership)).toMatchObject({
      allowed: true,
      by: 'team_client_policy',
      staggered: false,
    });
    expect(can(login([twin], ['teku', 'nimbus']), policy('teku'), ownership).allowed).toBe(true);

    expect(can(login([twin], ['teku']), policy('teku'), ownership).reason).toBe(
      'upgradoor:team_client_policy does not cover client teku (owners of all of it: nimbus)'
    );
    for (const client of ['geth', 'retired', 'unknown']) {
      expect(can(login([twin], ['lighthouse']), policy(client), ownership).reason, client).toBe(
        `upgradoor:team_client_policy does not cover client ${client} (owners of all of it: none)`
      );
    }
    // Without the client table nobody owns a client; without a team neither.
    expect(can(login([twin], ['lighthouse']), policy('lighthouse'), { ...OWNERSHIP, clients: [] }).allowed).toBe(false);
    expect(can(login([twin]), policy('lighthouse'), ownership).allowed).toBe(false);

    expect(can(login(['admin', 'roll', 'supervise']), policy('geth'), ownership).reason).toBe(
      'Needs upgradoor:client_policy or upgradoor:team_client_policy for a team that owns every container of this client'
    );
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
      [FEATURE.image_override, image],
      [FEATURE.flag_override, flags],
    ] as const) {
      const twin = TEAM_PREFIX + feature;
      expect(TWINNED).toContain(feature);

      expect(can(login([feature]), call(MIXED, 'geth'), OWNERSHIP)).toMatchObject({ allowed: true, by: feature });
      expect(can(login([twin], lighthouse), call(MIXED, 'lighthouse'), OWNERSHIP)).toMatchObject({
        allowed: true,
        by: twin,
        staggered: false,
      });
      // The rule of a roll of one container: another team's, an unmanaged one and one the host does not have.
      expect(can(login([twin], lighthouse), call(MIXED, 'geth'), OWNERSHIP).reason).toBe(
        `upgradoor:${twin} does not cover container geth on host ${MIXED} (owners: geth)`
      );
      for (const container of ['debug', 'nope']) {
        expect(can(login([twin], lighthouse), call(MIXED, container), OWNERSHIP).allowed, container).toBe(false);
      }
      expect(can(login([twin], lighthouse), call(GHOST, 'lighthouse'), OWNERSHIP).allowed).toBe(false);

      // One feature does not give the other, and neither does roll or admin.
      const others = [
        FEATURE.roll,
        FEATURE.admin,
        feature === FEATURE.image_override ? FEATURE.flag_override : FEATURE.image_override,
      ];
      expect(can(login(others), call(MIXED, 'geth'), OWNERSHIP).reason).toBe(
        `Needs upgradoor:${feature} or upgradoor:${twin} for a team that owns this container`
      );
    }
  });

  it('says that the controller decides when several teams of the login own parts of a client', () => {
    const policy = {
      action: 'client-policy',
      body: { client: 'teku', policy: 'manual', reason: '' },
    } satisfies ActionCall;
    const clients = [{ name: 'teku', hosts: 2, allOwned: [], owners: ['nimbus', 'teku'] }];
    const twin = TEAM_PREFIX + FEATURE.client_policy;

    const both = can(login([twin], ['teku', 'nimbus']), policy, { ...OWNERSHIP, clients });
    expect(both.allowed).toBe(false);
    expect(both.reason).toMatch(
      /owners of all of it: none\); your teams nimbus, teku own parts of it.*the controller decides/
    );
    // One team that owns a part does not cover it, and nothing is left open.
    expect(can(login([twin], ['teku']), policy, { ...OWNERSHIP, clients }).reason).toBe(
      'upgradoor:team_client_policy does not cover client teku (owners of all of it: none)'
    );
  });

  it('names admin for "now" through a team feature, and only then', () => {
    const lighthouse = ['lighthouse'];
    const now = roll([MIXED], ['lighthouse']);
    const staggered = roll([MIXED], ['lighthouse'], { staggered: true });

    expect(can(login(['team_roll', 'admin'], lighthouse), now, OWNERSHIP).by).toBe('team_roll,admin');
    expect(can(login(['team_roll', 'admin'], lighthouse), staggered, OWNERSHIP).by).toBe('team_roll');
    expect(can(login(['team_roll'], lighthouse), now, OWNERSHIP).by).toBe('team_roll');
    expect(can(login(['roll', 'admin'], lighthouse), now, OWNERSHIP).by).toBe('roll');
    expect(can(login(['team_reboot', 'admin'], lighthouse), reboot([MIXED]), OWNERSHIP).by).toBe('team_reboot,admin');
    expect(can(login(['team_hold', 'admin'], lighthouse), hold('hold', [MIXED]), OWNERSHIP).by).toBe('team_hold');

    // Named once when a flag needs it anyway.
    const forced = roll([MIXED], ['lighthouse'], { staggered: true, force: true });
    expect(can(login(['team_roll', 'admin'], lighthouse), forced, OWNERSHIP).by).toBe('team_roll,admin');
    expect(
      can(login(['team_roll', 'admin'], lighthouse), roll([MIXED], ['lighthouse'], { ignoreBudget: true }), OWNERSHIP)
        .by
    ).toBe('team_roll,admin');
  });
});
