// What the login may do, decided the way the controller decides it
// (pkg/controller/authz/authz.go and the requirements in userapi/actions.go).
//
// This is for what the page offers: a button the login cannot use is
// disabled and says why. The controller verifies the token and stays the
// enforcement; a wrong answer here costs a 403, never an action.

import type { ActionCall } from '../api/client';

/** The features of the controller's service, as a grant of the authenticatoor spells them. */
export const FEATURE = {
  /** roll: the action roll. Twin team_roll: containers a team of the login owns. */
  roll: 'roll',
  /** reboot: the action reboot. Twin team_reboot: hosts on which a team of the login owns a container. */
  reboot: 'reboot',
  /** hold: the actions hold and unhold. Twin team_hold, which leaves a hold of staff alone. */
  hold: 'hold',
  /** recover: skip, retry with hosts (of a rollout that is not aborted), end-lease. Twin team_recover. */
  recover: 'recover',
  /** supervise: pause, resume and abort of one rollout, retry without hosts, every retry of an aborted rollout. No twin. */
  supervise: 'supervise',
  /** client_policy: the image policy of one client. Twin team_client_policy: a client whose containers a team of the login all owns. */
  client_policy: 'client_policy',
  /** image_override: the action image-override. Twin team_image_override: a container a team of the login owns. */
  image_override: 'image_override',
  /** flag_override: the action flag-override. Twin team_flag_override: a container a team of the login owns. */
  flag_override: 'flag_override',
  /** admin: forget, the global pause, the settings, the flags force and ignoreBudget, the selector all. No twin. */
  admin: 'admin',
} as const;

type Feature = (typeof FEATURE)[keyof typeof FEATURE];

/** TEAM_PREFIX turns a feature into its team twin. */
export const TEAM_PREFIX = 'team_';

/** TWINNED are the features that have a team twin. */
export const TWINNED: readonly Feature[] = [
  FEATURE.roll,
  FEATURE.reboot,
  FEATURE.hold,
  FEATURE.recover,
  FEATURE.client_policy,
  FEATURE.image_override,
  FEATURE.flag_override,
];

/** Login is what the page knows about who is logged in. */
export interface Login {
  /** open: the controller runs without an auth provider and allows everything. */
  open: boolean;
  /** service is the controller's name in the token's "services" claim (config.authService). */
  service: string;
  /**
   * hasFeature is the auth client's default-deny check for one feature of
   * service. It answers false when nobody is logged in and when the client
   * is too old to know.
   */
  hasFeature: (feature: string) => boolean;
  /** teams are the token's teams. */
  teams: readonly string[];
}

/** OwnedContainer is the part of a container the rule reads. */
interface OwnedContainer {
  name: string;
  managed: boolean;
  owners?: readonly string[] | null;
}

/** OwnedHost is the part of a host the rule reads; a HostListing is one. */
export interface OwnedHost {
  name: string;
  containers?: readonly OwnedContainer[] | null;
  /** holdWanted and holdBy: a hold that was not set through a team feature is not a team's to lift. */
  holdWanted?: boolean;
  holdBy?: string;
}

/** OwnedClient is the part of a client the rule reads; a ClientView is one. */
export interface OwnedClient {
  name: string;
  /** hosts is how many hosts run the client: a client nobody runs has no container and no owner. */
  hosts: number;
  /** allOwned are the teams that own every container of the client. */
  allOwned?: readonly string[] | null;
  /** owners are the teams that own a container of the client. */
  owners?: readonly string[] | null;
}

/** Ownership is what the page knows about who owns what. */
export interface Ownership {
  /** hosts are all hosts the controller knows. */
  hosts: readonly OwnedHost[];
  /** clients are the clients of the fleet, for the image policy of one. */
  clients?: readonly OwnedClient[];
  /** leaseHost names the host of a lease; a lease it does not know has no owner. */
  leaseHost?: (lease: string) => string | undefined;
  /**
   * rolloutState names the state of a rollout. A retry of an aborted rollout
   * reopens its cause for every host and is no recovery of hosts. A rollout
   * the page does not know is taken as not aborted; the controller decides.
   */
  rolloutState?: (rollout: string) => string | undefined;
}

/** PermissionCall is an action call, or saving the settings. */
export type PermissionCall = ActionCall | { action: 'settings' };

export interface Verdict {
  allowed: boolean;
  /** reason says what is missing, for a tooltip; empty when allowed. */
  reason: string;
  /** by is what allows it, as the audit log will record it: "roll", "team_roll", "reboot,admin", "open". */
  by: string;
  /**
   * staggered: the controller runs this roll or reboot staggered, whatever
   * the request says. That is so for a request allowed through a team
   * feature by a login without admin: "now" skips the order and the wait for
   * readiness.
   */
  staggered: boolean;
}

/** Targets is what a request acts on, for the team twin of its feature. */
type Targets =
  | { rule: 'host'; hosts: readonly string[]; all: boolean; hold?: 'set' | 'lift' }
  | {
      rule: 'container';
      hosts: readonly string[];
      all: boolean;
      containers: readonly string[];
      /** one: the request is about exactly one container (an override). */
      one?: boolean;
    }
  | { rule: 'lease'; lease: string }
  | { rule: 'client'; client: string }
  /** someOf: a request on many hosts of a client, judged host by host; a team that owns a container of the client opens it. */
  | { rule: 'someOf'; client: string };

interface Requirement {
  feature: Feature;
  /** team is set when the feature has a twin. */
  team?: Targets;
  /** admin names the flag that needs admin on top; empty when none does. */
  admin: string;
  /** teamStaggered: through the twin alone the request is run staggered. */
  teamStaggered?: boolean;
  /** teamNow: the request is not staggered, which through the twin takes admin. */
  teamNow?: boolean;
}

function adminReason(force: boolean, ignoreBudget: boolean, all: boolean): string {
  if (force) return 'force';
  if (ignoreBudget) return 'ignoreBudget';
  if (all) return 'all';
  return '';
}

/** requirement is what each request needs: the one table of this page, the mirror of actions.go. */
function requirement(call: PermissionCall, ownership: Ownership): Requirement {
  switch (call.action) {
    case 'roll': {
      const b = call.body;
      return {
        feature: FEATURE.roll,
        team: { rule: 'container', hosts: b.hosts, all: b.all, containers: b.containers },
        admin: adminReason(b.force, b.ignoreBudget, b.all),
        teamStaggered: true,
        teamNow: !b.staggered,
      };
    }
    case 'reboot': {
      const b = call.body;
      return {
        feature: FEATURE.reboot,
        team: { rule: 'host', hosts: b.hosts, all: b.all },
        admin: adminReason(b.force, b.ignoreBudget, b.all),
        teamStaggered: true,
        teamNow: !b.staggered,
      };
    }
    case 'hold':
      return {
        feature: FEATURE.hold,
        team: { rule: 'host', hosts: call.body.hosts, all: false, hold: 'set' },
        admin: '',
      };
    case 'unhold':
      return {
        feature: FEATURE.hold,
        team: { rule: 'host', hosts: call.body.hosts, all: false, hold: 'lift' },
        admin: '',
      };
    case 'mark-offline':
    case 'unmark-offline':
      // The controller lets team_hold change only a mark set through it.
      return { feature: FEATURE.hold, team: { rule: 'host', hosts: call.body.hosts, all: false }, admin: '' };
    case 'forget':
      return { feature: FEATURE.admin, admin: '' };
    case 'end-lease':
      return { feature: FEATURE.recover, team: { rule: 'lease', lease: call.body.lease }, admin: '' };
    case 'pause':
    case 'resume':
      // Without a rollout it is the global switch.
      return { feature: call.body.rollout === '' ? FEATURE.admin : FEATURE.supervise, admin: '' };
    case 'abort':
      return { feature: FEATURE.supervise, admin: '' };
    case 'skip':
      return { feature: FEATURE.recover, team: { rule: 'host', hosts: call.body.hosts, all: false }, admin: '' };
    case 'retry':
      // Without hosts it reopens the rollout. So does every retry of an
      // aborted rollout, whatever hosts it names. Otherwise it recovers the
      // named hosts.
      return call.body.hosts.length === 0 || ownership.rolloutState?.(call.body.rollout) === 'aborted'
        ? { feature: FEATURE.supervise, admin: '' }
        : { feature: FEATURE.recover, team: { rule: 'host', hosts: call.body.hosts, all: false }, admin: '' };
    case 'client-policy':
      // The policy of a client is its team's when the team owns every container of it.
      return { feature: FEATURE.client_policy, team: { rule: 'client', client: call.body.client }, admin: '' };
    case 'image-override':
    case 'flag-override':
      // The rule of a roll of exactly that container on that host.
      return {
        feature: call.action === 'image-override' ? FEATURE.image_override : FEATURE.flag_override,
        team: {
          rule: 'container',
          hosts: [call.body.host],
          all: false,
          containers: [call.body.container],
          one: true,
        },
        admin: '',
      };
    case 'client-override':
      // Each host alone, by the rule of the single-host override: the controller lists the hosts the teams do not cover.
      return {
        feature: call.body.part === 'image' ? FEATURE.image_override : FEATURE.flag_override,
        team: { rule: 'someOf', client: call.body.client },
        admin: '',
      };
    case 'settings':
      // The settings carry the global pause, the budget, the gate and the policies.
      return { feature: FEATURE.admin, admin: '' };
  }
}

/** MAX_ECHO bounds a name a reason repeats, like the controller does. */
const MAX_ECHO = 128;

/** echo makes a name safe to repeat: printable ASCII, at most MAX_ECHO characters. */
function echo(name: string): string {
  return name.slice(0, MAX_ECHO).replace(/[^\x20-\x7e]/g, '?');
}

function ownerList(owners: readonly string[]): string {
  return owners.length === 0 ? 'none' : echo(owners.join(', '));
}

/** ownedBy reports whether one of the teams is among the owners; names are compared exactly. */
function ownedBy(teams: readonly string[], owners: readonly string[]): boolean {
  return owners.some(o => teams.includes(o));
}

function managedContainers(host: OwnedHost | undefined): OwnedContainer[] {
  return (host?.containers ?? []).filter(c => c.managed);
}

/** uncoveredOn applies the target rule to one host; empty when the host is covered. */
function uncoveredOn(teams: readonly string[], name: string, host: OwnedHost | undefined, t: Targets): string {
  const containers = managedContainers(host);
  const all = [...new Set(containers.flatMap(c => c.owners ?? []))].sort();
  const onHost = `does not cover host ${echo(name)} (owners: ${ownerList(all)})`;
  const onContainer = (container: string, owners: readonly string[]) =>
    `does not cover container ${echo(container)} on host ${echo(name)} (owners: ${ownerList(owners)})`;

  if (t.rule !== 'container') {
    // The any-owner rule of host-level actions.
    if (!ownedBy(teams, all)) return onHost;

    // A hold that was not set through the twin is not the twin's to lift,
    // nor to set again as its own.
    if (t.rule === 'host' && t.hold && host?.holdWanted === true && host.holdBy !== 'team') {
      return `does not ${t.hold === 'lift' ? 'lift' : 'change'} the hold on host ${echo(name)} (set by staff)`;
    }

    return '';
  }

  if (t.containers.length === 0) {
    // A roll of the whole host touches every managed container.
    if (containers.length === 0) return onHost;
    const foreign = containers.find(c => !ownedBy(teams, c.owners ?? []));
    return foreign ? onContainer(foreign.name, foreign.owners ?? []) : '';
  }

  for (const wanted of t.containers) {
    const c = containers.find(x => x.name === wanted);
    if (!c) return onContainer(wanted, []);
    if (!ownedBy(teams, c.owners ?? [])) return onContainer(c.name, c.owners ?? []);
  }

  return '';
}

/**
 * uncoveredClient applies the rule of a client: a team of the login owns
 * every container of it. The page reads that off the client's row (allOwned)
 * and cannot name the container that is somebody else's; the controller does.
 */
function uncoveredClient(teams: readonly string[], name: string, ownership: Ownership): string {
  const client = ownership.clients?.find(c => c.name === name);
  const all = client && client.hosts > 0 ? [...(client.allOwned ?? [])].sort() : [];
  if (ownedBy(teams, all)) return '';

  const reason = `does not cover client ${echo(name)} (owners of all of it: ${ownerList(all)})`;

  // The controller asks whether every container is owned by a team of the
  // login. Several teams that each own a part satisfy that, and a client's
  // row does not say whether they do: the page offers nothing it cannot
  // show to be allowed, and says who decides.
  const parts = (client?.owners ?? []).filter(o => teams.includes(o));
  return parts.length > 1
    ? `${reason}; your teams ${echo(parts.join(', '))} own parts of it, which the page cannot add up: the controller decides, ask it through the API`
    : reason;
}

/** uncovered names the first target no team of the login owns; empty when all are covered. */
function uncovered(teams: readonly string[], t: Targets, ownership: Ownership): string {
  let names: string[];

  if (t.rule === 'client') return uncoveredClient(teams, t.client, ownership);
  if (t.rule === 'someOf') {
    const owners = [...(ownership.clients?.find(c => c.name === t.client)?.owners ?? [])].sort();
    return ownedBy(teams, owners)
      ? ''
      : `does not cover any container of client ${echo(t.client)} (owners: ${ownerList(owners)})`;
  }

  if (t.rule === 'lease') {
    const host = ownership.leaseHost?.(t.lease);
    if (host === undefined) return `does not cover lease ${echo(t.lease)} (owners: none)`;
    names = [host];
  } else {
    names = t.all ? [...t.hosts, ...ownership.hosts.map(h => h.name)] : [...t.hosts];
  }

  if (names.length === 0) return 'needs a target that one of your teams owns, and the request names none';

  const byName = new Map(ownership.hosts.map(h => [h.name, h]));

  for (const name of names) {
    const reason = uncoveredOn(teams, name, byName.get(name), t);
    if (reason !== '') return reason;
  }

  return '';
}

function targetWords(t: Targets): string {
  switch (t.rule) {
    case 'lease':
      return 'the host of this lease';
    case 'client':
      return 'every container of this client';
    case 'someOf':
      return 'the containers of this client on the hosts';
    case 'container':
      if (t.one) return 'this container';
      return t.containers.length === 0 ? 'every managed container of the host' : 'these containers';
    case 'host':
      return t.hosts.length === 1 ? 'this host' : 'these hosts';
  }
}

const denied = (reason: string): Verdict => ({ allowed: false, reason, by: '', staggered: false });

/**
 * can decides whether the login may make a call. The rule is the
 * controller's: the plain feature allows it on any target; the team twin
 * allows it when a team of the login owns every target; a request with force,
 * ignoreBudget or all needs admin as well.
 */
export function can(login: Login, call: PermissionCall, ownership: Ownership): Verdict {
  if (login.open) return { allowed: true, reason: '', by: 'open', staggered: false };

  const need = requirement(call, ownership);
  const feature = `${login.service}:${need.feature}`;
  let by: string = need.feature;
  let viaTeam = false;

  if (!login.hasFeature(need.feature)) {
    const twin = TEAM_PREFIX + need.feature;

    if (!need.team) return denied(`Needs ${feature}`);

    if (!login.hasFeature(twin)) {
      return denied(`Needs ${feature} or ${login.service}:${twin} for a team that owns ${targetWords(need.team)}`);
    }

    const reason = uncovered(login.teams, need.team, ownership);
    if (reason !== '') return denied(`${login.service}:${twin} ${reason}`);

    by = twin;
    viaTeam = true;
  }

  const admin = login.hasFeature(FEATURE.admin);

  if (need.admin !== '') {
    if (!admin) return denied(`Needs ${login.service}:${FEATURE.admin} (for ${need.admin})`);
    by += `,${FEATURE.admin}`;
  } else if (viaTeam && admin && need.teamNow === true) {
    // "Now" through a team feature is what admin permits: it is named for it.
    by += `,${FEATURE.admin}`;
  }

  return { allowed: true, reason: '', by, staggered: viaTeam && !admin && need.teamStaggered === true };
}

/** needsOwnership reports whether some verdict of this login depends on who owns what. */
export function needsOwnership(login: Login): boolean {
  return !login.open && TWINNED.some(f => !login.hasFeature(f) && login.hasFeature(TEAM_PREFIX + f));
}
