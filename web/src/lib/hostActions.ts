// Who may start a host action, decided the way the controller decides it
// (authz.ActionBasis, userapi/hostactions.go): open mode, admin, the action's
// feature (manage when it names none), or a team the action names. The rule
// is per host: each host reports its actions with their feature and teams.

import { list, type ActionInfo, type HostActionRequest, type Impact, type Policy, type Rollout } from '../api/types';
import type { HostActionIntent } from '../components/HostActionDialog';
import { FEATURE, hasFeature, type Login, type Verdict } from './permissions';

/** SECURITY_UPDATES is the condition of an action that installs pending security updates. */
export const SECURITY_UPDATES = 'securityUpdates';

/** ActionHost is the part of a host the rule reads; a HostListing is one. */
export interface ActionHost {
  name: string;
  actions?: readonly ActionInfo[] | null;
  verbs?: readonly string[] | null;
}

/**
 * actionBasis says what lets the login start the action: open, admin, feature or team; empty when nothing does.
 * The action's feature is a plain one of the controller, manage when it names none; an unknown one allows nobody.
 */
export function actionBasis(login: Login, info: ActionInfo): string {
  if (login.open) return 'open';
  if (login.hasFeature(FEATURE.admin)) return FEATURE.admin;

  const feature = info.feature || FEATURE.manage;
  if ((Object.values(FEATURE) as string[]).includes(feature) && hasFeature(login, feature)) return 'feature';

  if (list(info.teams).some(t => login.teams.includes(t))) return 'team';
  return '';
}

/** actionOf is the action of that name as the host reports it. */
export function actionOf(host: ActionHost, name: string): ActionInfo | undefined {
  return list(host.actions).find(a => a.name === name);
}

/** runnableActions are the host's actions the login may start, by name. */
export function runnableActions(login: Login, host: ActionHost): ActionInfo[] {
  return list(host.actions)
    .filter(a => actionBasis(login, a) !== '')
    .sort((a, b) => a.name.localeCompare(b.name));
}

/** securityAction is the host's action that installs security updates, if the login may start it. */
export function securityAction(login: Login, host: ActionHost): ActionInfo | undefined {
  return runnableActions(login, host).find(a => a.when === SECURITY_UPDATES);
}

/** verbBlocked says why the agent does not take an action now; empty when it does. */
export function verbBlocked(host: ActionHost): string {
  return list(host.verbs).includes('action') ? '' : 'The agent does not allow the verb action';
}

/** SelectionAction is one action that a selection of hosts offers. */
export interface SelectionAction {
  name: string;
  description?: string;
  impact: Impact;
  /** hosts are the selected hosts that report the action and on which the login may start it. */
  hosts: readonly string[];
}

/** selectionActions lists every action the login may start on at least one of the hosts, by name. */
export function selectionActions(login: Login, hosts: readonly ActionHost[]): SelectionAction[] {
  const byName = new Map<string, SelectionAction & { hosts: string[] }>();

  for (const h of hosts) {
    for (const a of runnableActions(login, h)) {
      const entry = byName.get(a.name) ?? { name: a.name, description: a.description, impact: a.impact, hosts: [] };
      entry.hosts.push(h.name);
      byName.set(a.name, entry);
    }
  }

  return [...byName.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/**
 * hostActionVerdict judges a host-action request against the hosts the page
 * knows: it is allowed when the login may start the action on one of the
 * hosts it selects. The controller rejects the others host by host.
 */
export function hostActionVerdict(login: Login, body: HostActionRequest, hosts: readonly ActionHost[]): Verdict {
  if (login.open) return { allowed: true, reason: '', by: 'open' };
  if (login.hasFeature(FEATURE.admin)) return { allowed: true, reason: '', by: FEATURE.admin };

  const named = list(body.hosts);
  const selected = named.length > 0 ? hosts.filter(h => named.includes(h.name)) : body.all ? hosts : [];
  const basis = selected
    .map(h => actionOf(h, body.action))
    .map(a => (a ? actionBasis(login, a) : ''))
    .find(b => b !== '');

  if (basis) return { allowed: true, reason: '', by: basis };
  return {
    allowed: false,
    reason: `Needs ${login.service}:${FEATURE.admin}, or the feature or a team of the action ${body.action} on a selected host`,
    by: '',
  };
}

/** RolloutAction is the host action an action rollout runs and its member hosts. */
export interface RolloutAction {
  action: string;
  hosts: readonly string[];
}

/** rolloutActionOf is the action and members of an action rollout; undefined for any other kind. */
export function rolloutActionOf(r: Pick<Rollout, 'kind' | 'action' | 'hosts'>): RolloutAction | undefined {
  if (r.kind !== 'action' || !r.action) return undefined;
  return { action: r.action, hosts: list(r.hosts).map(h => h.host) };
}

/**
 * approvalBasis says what lets the login resume (approve) an action rollout
 * without manage (userapi allowActionApproval): it may start the action on
 * every member host. Empty when a member does not report the action or does
 * not permit it.
 */
export function approvalBasis(login: Login, ra: RolloutAction, hosts: readonly ActionHost[]): string {
  if (ra.hosts.length === 0) return '';

  const byName = new Map(hosts.map(h => [h.name, h]));
  let by = '';

  for (const name of ra.hosts) {
    const host = byName.get(name);
    const info = host && actionOf(host, ra.action);
    by = info ? actionBasis(login, info) : '';
    if (by === '') return '';
  }

  return by;
}

/** actionPolicyNames lists the actions a policy can be set for: those the hosts report and those that have one. */
export function actionPolicyNames(hosts: readonly ActionHost[], policies: Record<string, Policy>): string[] {
  const names = new Set(Object.keys(policies));
  for (const h of hosts) for (const a of list(h.actions)) names.add(a.name);
  return [...names].sort((a, b) => a.localeCompare(b));
}

/** hostActionIntent is the dialog that starts a host action on the hosts; action picks the choice it starts with. */
export function hostActionIntent(
  login: Login,
  hosts: readonly ActionHost[],
  action?: string,
  hiddenHosts: readonly string[] = []
): HostActionIntent {
  return {
    kind: 'host-action',
    hosts: hosts.map(h => h.name),
    choices: selectionActions(login, hosts),
    action,
    hiddenHosts,
  };
}
