// What the login may do, decided the way the controller decides it
// (pkg/controller/authz/authz.go and the requirements in userapi/actions.go).
//
// This is for what the page offers: a button the login cannot use is
// disabled and says why. The controller verifies the token and stays the
// enforcement; a wrong answer here costs a 403, never an action.

import type { ActionCall } from '../api/client';
import type { ActionInfo, Need, RolloutKind } from '../api/types';
import { approvalBasis, hostActionVerdict, type RolloutAction } from './hostActions';

/** The features of the controller's service, as a grant of the authenticatoor spells them. */
export const FEATURE = {
  /**
   * details: the hosts in full (system, packages, unmanaged containers, internals, host actions) and the Packages
   * tab. Twin team_details: hosts on which a team of the login owns a container. Not an action.
   */
  details: 'details',
  /**
   * manage: roll, reboot, hold, unhold, mark-offline, unmark-offline, skip, retry, end-lease, client-policy, the
   * decisions on one rollout (pause, resume, ignore-gate, ignore-budget, approve-lease, abort), the image refresh, the flags force
   * and ignoreBudget, the selector all, a roll now, and a host action that names no feature. Twin team_manage: the
   * same on what a team of the login owns (its containers, rollouts, leases and members that only roll them, its
   * client, its own hold); never reboot, mark-offline, unmark-offline or the image refresh.
   */
  manage: 'manage',
  /** manage_image: image-override, client-override of the image. Twin team_manage_image: a container a team of the login owns. */
  manage_image: 'manage_image',
  /** manage_flags: flag-override, client-override of the flags. Twin team_manage_flags: a container a team of the login owns. */
  manage_flags: 'manage_flags',
  /** admin: forget, the global pause, arming the deployment window, the settings, the data sources. Includes every other feature. No twin. */
  admin: 'admin',
} as const;

type Feature = (typeof FEATURE)[keyof typeof FEATURE];

/** TEAM_PREFIX turns a feature into its team twin. */
export const TEAM_PREFIX = 'team_';

/**
 * hasFeature reports whether the login holds a feature, as the controller decides it (authz.Implied): granted,
 * through admin (every other feature) or through its plain form (a plain feature includes its twin).
 */
export function hasFeature(login: Pick<Login, 'hasFeature'>, feature: string): boolean {
  if (login.hasFeature(feature) || login.hasFeature(FEATURE.admin)) return true;
  return feature.startsWith(TEAM_PREFIX) && login.hasFeature(feature.slice(TEAM_PREFIX.length));
}

/** TWINNED are the action features that have a team twin. */
export const TWINNED: readonly Feature[] = [FEATURE.manage, FEATURE.manage_image, FEATURE.manage_flags];

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
  /** team is the container's team label: its only owner; none makes it staff's. */
  team?: string;
}

/** OwnedHost is the part of a host the rule reads; a HostListing is one. */
export interface OwnedHost {
  name: string;
  containers?: readonly OwnedContainer[] | null;
  /** holders are the holders of the host's controller holds: "staff" or a team. */
  holders?: readonly string[] | null;
  /** actions are the host actions the host reports, for the rule of host-action. */
  actions?: readonly ActionInfo[] | null;
}

/** OwnedClient is the part of a client the rule reads; a ClientView is one. */
export interface OwnedClient {
  name: string;
  /** hosts is how many hosts run the client: a client nobody runs has no container and no owner. */
  hosts: number;
}

/** Ownership is what the page knows about who owns what. */
export interface Ownership {
  /** hosts are all hosts the controller knows. */
  hosts: readonly OwnedHost[];
  /** clients are the clients of the fleet, for the image policy of one. */
  clients?: readonly OwnedClient[];
  /** leaseWork is the host and the needs of a lease; a lease it does not know has no owner. */
  leaseWork?: (lease: string) => LeaseWork | undefined;
  /** rolloutKind is the kind of a rollout; undefined for one the page does not know. */
  rolloutKind?: (rollout: string) => RolloutKind | undefined;
  /**
   * rolloutClient names the client a rollout is for: empty for a rollout for
   * no client, undefined for one the page does not know.
   */
  rolloutClient?: (rollout: string) => string | undefined;
  /** rolloutAction is the action and members of an action rollout; undefined for any other. */
  rolloutAction?: (rollout: string) => RolloutAction | undefined;
}

/** LeaseWork is what a lease does: its host and needs. */
export interface LeaseWork {
  host: string;
  needs: readonly Need[];
}

/** CallContext is what the page knows of the lease or rollout a call is about. */
export interface CallContext {
  /** lease is what the lease an end-lease call is about does. */
  lease?: LeaseWork;
  /** rolloutClient is the team of the rollout a call is about ("" for none). */
  rolloutClient?: string;
  /** rolloutKind is the kind of the rollout a skip or retry is about. */
  rolloutKind?: RolloutKind;
  /** rolloutAction is the action and members of an action rollout: who may run it everywhere may approve it. */
  rolloutAction?: RolloutAction;
}

/** PermissionCall is an action call, or saving the settings. */
export type PermissionCall = ActionCall | { action: 'settings' };

export interface Verdict {
  allowed: boolean;
  /** reason says what is missing, for a tooltip; empty when allowed. */
  reason: string;
  /** by is what allows it, as the audit log will record it: "manage", "team_manage", "open". */
  by: string;
}

/** Targets is what a request acts on, for the team twin of its feature. */
type Targets =
  /** host: one owned container of each host; holders are the holds an unhold names. */
  | { rule: 'host'; hosts: readonly string[]; all: boolean; holders?: readonly string[] }
  | {
      rule: 'container';
      hosts: readonly string[];
      all: boolean;
      containers: readonly string[];
      /** one: the request is about exactly one container (an override). */
      one?: boolean;
    }
  /** lease: every need of the lease rolls a container the login's teams own. */
  | { rule: 'lease'; lease: string }
  /** members: the hosts as members of the rollout, which only rolls containers of the login's teams. */
  | { rule: 'members'; rollout: string; hosts: readonly string[] }
  | { rule: 'client'; client: string }
  /** rollout: the client the rollout is for, by the rule of a client. */
  | { rule: 'rollout'; rollout: string }
  /** someOf: a request on many hosts of a client, judged host by host; a team that owns a container of the client opens it. */
  | { rule: 'someOf'; client: string };

interface Requirement {
  feature: Feature;
  /** team is set when the twin allows the request on the targets. */
  team?: Targets;
}

/** requirement is what each request needs: the one table of this page, the mirror of actions.go. */
function requirement(call: Exclude<PermissionCall, { action: 'host-action' }>): Requirement {
  switch (call.action) {
    case 'roll': {
      const b = call.body;
      return {
        feature: FEATURE.manage,
        team: { rule: 'container', hosts: b.hosts, all: b.all, containers: b.containers },
      };
    }
    case 'reboot':
    case 'mark-offline':
    case 'unmark-offline':
      return { feature: FEATURE.manage };
    case 'hold':
      return { feature: FEATURE.manage, team: { rule: 'host', hosts: call.body.hosts, all: false } };
    case 'unhold':
      return {
        feature: FEATURE.manage,
        team: { rule: 'host', hosts: call.body.hosts, all: false, holders: call.body.holders ?? [] },
      };
    case 'forget':
    case 'arm-deploy-window':
      return { feature: FEATURE.admin };
    case 'end-lease':
      return { feature: FEATURE.manage, team: { rule: 'lease', lease: call.body.lease } };
    case 'pause':
    case 'resume':
    case 'ignore-gate':
    case 'ignore-budget':
    case 'approve-lease':
      // Without a rollout it is the global switch.
      return call.body.rollout === ''
        ? { feature: FEATURE.admin }
        : { feature: FEATURE.manage, team: { rule: 'rollout', rollout: call.body.rollout } };
    case 'abort':
      return { feature: FEATURE.manage, team: { rule: 'rollout', rollout: call.body.rollout } };
    case 'skip':
    case 'retry':
      // A retry without hosts reopens the rollout; otherwise both recover the named members.
      return call.action === 'retry' && call.body.hosts.length === 0
        ? { feature: FEATURE.manage, team: { rule: 'rollout', rollout: call.body.rollout } }
        : { feature: FEATURE.manage, team: { rule: 'members', rollout: call.body.rollout, hosts: call.body.hosts } };
    case 'client-policy':
      // The policy of a client is its team's when the team owns every container of it.
      return { feature: FEATURE.manage, team: { rule: 'client', client: call.body.client } };
    case 'image-override':
    case 'flag-override':
      // The rule of a roll of exactly that container on that host.
      return {
        feature: call.action === 'image-override' ? FEATURE.manage_image : FEATURE.manage_flags,
        team: {
          rule: 'container',
          hosts: [call.body.host],
          all: false,
          containers: [call.body.container],
          one: true,
        },
      };
    case 'client-override':
      // Each host alone, by the rule of the single-host override: the controller lists the hosts the teams do not cover.
      return {
        feature: call.body.part === 'image' ? FEATURE.manage_image : FEATURE.manage_flags,
        team: { rule: 'someOf', client: call.body.client },
      };
    case 'settings':
      // The settings carry the global pause, the budget, the gate and the policies.
      return { feature: FEATURE.admin };
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

/** ownersOf is a container's owners: its team, if it has one. */
function ownersOf(c: OwnedContainer): string[] {
  return c.team ? [c.team] : [];
}

function managedContainers(host: OwnedHost | undefined): OwnedContainer[] {
  return (host?.containers ?? []).filter(c => c.managed);
}

/** STAFF is the holder of staff's hold. */
export const STAFF = 'staff';

/** ROLL_NEEDS are the kinds of need that roll a container. */
const ROLL_NEEDS: readonly string[] = ['image', 'override', 'spec'];

/** needContainer is the container a need is about. */
function needContainer(n: Need): string {
  return n.image?.container ?? n.override?.container ?? n.spec?.container ?? '';
}

/** uncoveredOn applies the target rule to one host; empty when the host is covered. */
function uncoveredOn(teams: readonly string[], name: string, host: OwnedHost | undefined, t: Targets): string {
  const containers = managedContainers(host);
  const all = [...new Set(containers.flatMap(ownersOf))].sort();
  const onHost = `does not cover host ${echo(name)} (owners: ${ownerList(all)})`;
  const onContainer = (container: string, owners: readonly string[]) =>
    `does not cover container ${echo(container)} on host ${echo(name)} (owners: ${ownerList(owners)})`;

  if (t.rule !== 'container') {
    // The any-owner rule of host-level actions.
    if (!ownedBy(teams, all)) return onHost;

    // An unhold names only holds of the login's teams on the host.
    if (t.rule === 'host') {
      const foreign = (t.holders ?? []).find(h => h === STAFF || !teams.includes(h) || !all.includes(h));
      if (foreign !== undefined) return `does not lift the hold of ${echo(foreign)} on host ${echo(name)}`;
    }

    return '';
  }

  if (t.containers.length === 0) {
    // A roll of the whole host touches every managed container.
    if (containers.length === 0) return onHost;
    const foreign = containers.find(c => !ownedBy(teams, ownersOf(c)));
    return foreign ? onContainer(foreign.name, ownersOf(foreign)) : '';
  }

  for (const wanted of t.containers) {
    const c = containers.find(x => x.name === wanted);
    if (!c) return onContainer(wanted, []);
    if (!ownedBy(teams, ownersOf(c))) return onContainer(c.name, ownersOf(c));
  }

  return '';
}

/** clientOwners is the owner of a client: the client is the team of its containers; none while nobody runs it. */
function clientOwners(name: string, ownership: Ownership): string[] {
  const client = ownership.clients?.find(c => c.name === name);
  return client && client.hosts > 0 ? [name] : [];
}

/** uncoveredClient applies the rule of a client: the client is one of the login's teams. */
function uncoveredClient(teams: readonly string[], name: string, ownership: Ownership): string {
  const owners = clientOwners(name, ownership);
  return ownedBy(teams, owners) ? '' : `does not cover client ${echo(name)} (owners: ${ownerList(owners)})`;
}

/** uncoveredLease applies the rule of a recovery through team_manage to a lease: it only rolls the teams' containers. */
function uncoveredLease(teams: readonly string[], lease: string, ownership: Ownership): string {
  const work = ownership.leaseWork?.(lease);
  if (!work) return `does not cover lease ${echo(lease)} (owners: none)`;
  if (work.needs.length === 0) return `does not cover lease ${echo(lease)}: it carries no need`;

  const host = ownership.hosts.find(h => h.name === work.host);

  for (const n of work.needs) {
    if (!ROLL_NEEDS.includes(n.kind)) return `does not cover lease ${echo(lease)}: it carries a ${echo(n.kind)}`;
    const reason = uncoveredOn(teams, work.host, host, {
      rule: 'container',
      hosts: [work.host],
      all: false,
      containers: [needContainer(n)],
    });
    if (reason !== '') return reason;
  }

  return '';
}

/**
 * uncoveredMembers applies the rule of a recovery through team_manage to members of a rollout. The page does not know a manual
 * roll's directives nor a host's own needs: a manual roll is judged by the team it records, a host's own roll by
 * every managed container of the host. The controller judges the members' leases.
 */
function uncoveredMembers(
  teams: readonly string[],
  t: Extract<Targets, { rule: 'members' }>,
  ownership: Ownership
): string {
  const kind = ownership.rolloutKind?.(t.rollout);
  if (kind === undefined) return `does not cover rollout ${echo(t.rollout)} (owners: none)`;
  if (t.hosts.length === 0) return 'needs a target that one of your teams owns, and the request names none';

  switch (kind) {
    case 'group':
    case 'manual': {
      const team = ownership.rolloutClient?.(t.rollout) ?? '';
      if (team === '') return `does not cover rollout ${echo(t.rollout)}: it is for no team`;
      return ownedBy(teams, [team]) ? '' : `does not cover rollout ${echo(t.rollout)} (owners: ${ownerList([team])})`;
    }
    case 'local': {
      const byName = new Map(ownership.hosts.map(h => [h.name, h]));
      for (const name of t.hosts) {
        const reason = uncoveredOn(teams, name, byName.get(name), {
          rule: 'container',
          hosts: [name],
          all: false,
          containers: [],
        });
        if (reason !== '') return reason;
      }
      return '';
    }
    default:
      return `does not cover rollout ${echo(t.rollout)}: it rolls no containers (${echo(kind)})`;
  }
}

/** uncovered names the first target no team of the login owns; empty when all are covered. */
function uncovered(teams: readonly string[], t: Targets, ownership: Ownership): string {
  if (t.rule === 'client') return uncoveredClient(teams, t.client, ownership);
  if (t.rule === 'rollout') {
    const team = ownership.rolloutClient?.(t.rollout);
    if (team === undefined) return `does not cover rollout ${echo(t.rollout)} (owners: none)`;
    if (team === '') return `does not cover rollout ${echo(t.rollout)}: it is for no team`;
    return ownedBy(teams, [team]) ? '' : `does not cover rollout ${echo(t.rollout)} (owners: ${echo(team)})`;
  }
  if (t.rule === 'someOf') {
    const owners = clientOwners(t.client, ownership);
    return ownedBy(teams, owners)
      ? ''
      : `does not cover any container of client ${echo(t.client)} (owners: ${ownerList(owners)})`;
  }

  if (t.rule === 'lease') return uncoveredLease(teams, t.lease, ownership);
  if (t.rule === 'members') return uncoveredMembers(teams, t, ownership);

  const names = t.all ? [...t.hosts, ...ownership.hosts.map(h => h.name)] : [...t.hosts];

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
      return 'every container this lease rolls';
    case 'members':
      return 'every container the rollout rolls on these hosts';
    case 'client':
      return 'every container of this client';
    case 'rollout':
      return 'this rollout';
    case 'someOf':
      return 'the containers of this client on the hosts';
    case 'container':
      if (t.one) return 'this container';
      return t.containers.length === 0 ? 'every managed container of the host' : 'these containers';
    case 'host':
      return t.hosts.length === 1 ? 'this host' : 'these hosts';
  }
}

const denied = (reason: string): Verdict => ({ allowed: false, reason, by: '' });

/**
 * can decides whether the login may make a call. The rule is the
 * controller's: the plain feature allows it on any target; the team twin
 * allows it when a team of the login owns every target.
 */
export function can(login: Login, call: PermissionCall, ownership: Ownership): Verdict {
  if (login.open) return { allowed: true, reason: '', by: 'open' };

  // A host action has a rule of its own, per host: see hostActions.ts.
  if (call.action === 'host-action') return hostActionVerdict(login, call.body, ownership.hosts);

  if (isApproval(call)) return approvalVerdict(login, call, ownership);

  return featureVerdict(login, call, ownership);
}

/** ApprovalCall is a person's decision on one rollout: resume (which approves), ignore-gate, ignore-budget, approve-lease. */
type ApprovalCall = Extract<PermissionCall, { action: 'resume' | 'ignore-gate' | 'ignore-budget' | 'approve-lease' }>;

function isApproval(call: PermissionCall): call is ApprovalCall {
  return (
    (call.action === 'resume' ||
      call.action === 'ignore-gate' ||
      call.action === 'ignore-budget' ||
      call.action === 'approve-lease') &&
    call.body.rollout !== ''
  );
}

/**
 * approvalVerdict decides a person's decision on one rollout: manage or its
 * twin, or, for an action rollout, the action on every member host.
 */
function approvalVerdict(login: Login, call: ApprovalCall, ownership: Ownership): Verdict {
  const ra = hasFeature(login, FEATURE.manage) ? undefined : ownership.rolloutAction?.(call.body.rollout);
  if (!ra) return featureVerdict(login, call, ownership);

  const by = approvalBasis(login, ra, ownership.hosts);
  if (by !== '') return { allowed: true, reason: '', by };

  const verdict = featureVerdict(login, call, ownership);
  if (verdict.allowed) return verdict;

  return denied(
    `${verdict.reason}; or the feature or a team of the action ${echo(ra.action)} on every host of the rollout`
  );
}

/** featureVerdict applies the call's feature and its team twin. */
function featureVerdict(
  login: Login,
  call: Exclude<PermissionCall, { action: 'host-action' }>,
  ownership: Ownership
): Verdict {
  const need = requirement(call);
  const feature = `${login.service}:${need.feature}`;
  if (hasFeature(login, need.feature)) return { allowed: true, reason: '', by: need.feature };

  const twin = TEAM_PREFIX + need.feature;

  if (!need.team) return denied(`Needs ${feature}`);

  if (!hasFeature(login, twin)) {
    return denied(`Needs ${feature} or ${login.service}:${twin} for a team that owns ${targetWords(need.team)}`);
  }

  const reason = uncovered(login.teams, need.team, ownership);
  if (reason !== '') return denied(`${login.service}:${twin} ${reason}`);

  return { allowed: true, reason: '', by: twin };
}

/** needsOwnership reports whether some verdict of this login depends on who owns what. */
export function needsOwnership(login: Login): boolean {
  return !login.open && TWINNED.some(f => !hasFeature(login, f) && hasFeature(login, TEAM_PREFIX + f));
}

/** needsActionHosts: without manage the approval of an action rollout follows the action on its hosts. */
export function needsActionHosts(login: Login): boolean {
  return !login.open && !hasFeature(login, FEATURE.manage);
}
