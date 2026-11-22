// The host actions of the mock: what the hosts report, and the host-action
// endpoint. A start opens an action rollout whose leases have run at once:
// the canary succeeds, the next host fails with output, the one after it
// times out; the rollout halts on the failure.

import {
  list,
  type ActionInfo,
  type ActionResult,
  type ActionRun,
  type HostActionRequest,
  type HostDetail,
  type Lease,
  type Need,
  type Rollout,
  type RolloutHost,
} from '../api/types';
import { SECURITY_UPDATES, actionBasis, actionOf } from '../lib/hostActions';
import type { Login } from '../lib/permissions';
import type { MockData } from './fixture';

/** The mock's actions: security updates on every node, one for the lighthouse team, one for admins. */
export const SECURITY_ACTION = 'security-upgrades';
export const TEAM_ACTION = 'prune-beacon-db';
export const ADMIN_ACTION = 'docker-prune';

/** The feature that allows SECURITY_ACTION, next to admin. */
const SECURITY_FEATURE = 'upgradoor:actions';

/** MockHostActionError is a refused request: its status is the answer's. */
export class MockHostActionError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
  }
}

/** seedHostActions gives the fixture's hosts their actions and the settings a policy for one of them. */
export function seedHostActions(data: MockData): void {
  for (const host of data.hosts.values()) {
    const actions: ActionInfo[] = [];
    const nodes = list(host.containers).some(c => c.managed && c.kind === 'el');

    if (nodes) {
      actions.push({
        name: SECURITY_ACTION,
        description: 'Install the pending security updates (unattended-upgrade)',
        impact: 'node',
        feature: SECURITY_FEATURE,
        when: SECURITY_UPDATES,
        due: host.pending.security > 0,
      });
    }
    if (list(host.containers).some(c => c.managed && c.team === 'lighthouse' && c.kind === 'cl')) {
      actions.push({
        name: TEAM_ACTION,
        description: 'Prune the beacon database of lighthouse and restart it',
        impact: 'node',
        teams: ['lighthouse'],
      });
    }
    if (!nodes) {
      actions.push({ name: ADMIN_ACTION, description: 'Remove unused Docker images', impact: 'independent' });
    }

    host.actions = actions;
    if (actions.length > 0 && !list(host.verbs).includes('action')) host.verbs = [...list(host.verbs), 'action'];
  }

  data.settings = { ...data.settings, actionPolicies: { [SECURITY_ACTION]: 'manual' } };
}

/** What the endpoint needs of the mock controller. */
export interface HostActionContext {
  data: MockData;
  now: number;
  actor: string;
  newId: (prefix: string) => string;
  log: (action: string, target: string, detail: Record<string, unknown>) => void;
}

/** The outcome of each run, by the member's place in the rollout. */
const RUNS: readonly ActionRun[] = [
  {
    exitCode: 0,
    output:
      'Reading package lists...\nBuilding dependency tree...\nCalculating upgrade...\n' +
      'The following packages will be upgraded:\n  libssl3t64 openssl\n2 upgraded, 0 newly installed, 0 to remove.\n',
  },
  {
    exitCode: 100,
    output:
      'Reading package lists...\n' +
      'E: Could not get lock /var/lib/dpkg/lock-frontend. It is held by process 4122 (apt-get)\n' +
      'E: Unable to acquire the dpkg frontend lock (/var/lib/dpkg/lock-frontend), is another process using it?\n',
  },
  { exitCode: -1, output: 'Reading package lists...\nBuilding dependency tree...\n', timedOut: true },
];

/** selected resolves the request to the hosts it acts on and the named hosts it rejects. */
function selected(
  data: MockData,
  login: Login,
  req: HostActionRequest
): { hosts: HostDetail[]; rejected: Record<string, string>; refused: number } {
  const named = list(req.hosts);
  const rejected: Record<string, string> = {};
  const hosts: HostDetail[] = [];
  let refused = 0;

  const candidates =
    named.length > 0
      ? named
      : [...data.hosts.values()]
          .filter(h => req.all === true || list(h.containers).some(c => c.managed && c.team === req.client))
          .map(h => h.name);

  for (const name of candidates) {
    const host = data.hosts.get(name);
    const info = host ? actionOf(host, req.action) : undefined;
    const permitted = info ? actionBasis(login, info) !== '' : false;

    if (!host) rejected[name] = `unknown host ${name}`;
    else if (!info) {
      if (named.length > 0) rejected[name] = `the host does not report the action ${req.action}`;
    } else if (!permitted) {
      refused += 1;
      if (named.length > 0) rejected[name] = `this login may not start the action ${req.action} on this host`;
    } else if (host.hold.local || host.hold.controller || host.holdWanted) rejected[name] = 'the host is on hold';
    else hosts.push(host);
  }

  return { hosts, rejected, refused };
}

/** startHostAction serves POST /api/v1/actions/host-action. */
export function startHostAction(ctx: HostActionContext, login: Login, body: unknown): ActionResult {
  const req = body as HostActionRequest;
  if (typeof req.action !== 'string' || req.action === '') throw new MockHostActionError(400, 'invalid action name ""');
  const selections = [list(req.hosts).length > 0, Boolean(req.client), req.all === true].filter(Boolean).length;
  if (selections !== 1) throw new MockHostActionError(400, 'give exactly one of hosts, client and all');

  const { hosts, rejected, refused } = selected(ctx.data, login, req);
  if (hosts.length === 0) {
    if (refused > 0 || !login.open) {
      throw new MockHostActionError(
        403,
        `forbidden: this login lacks the feature ${login.service}:admin, and the feature and the teams of the action ${req.action} on every selected host`
      );
    }
    throw new MockHostActionError(409, `no host can take the action ${req.action}`);
  }

  const id = ctx.newId('ro');
  const at = (secondsAgo: number) => new Date(ctx.now - secondsAgo * 1000).toISOString();
  const reason = typeof req.reason === 'string' ? req.reason.trim() : '';
  const title = `action ${req.action} on ${hosts.length} host(s)${reason ? `: ${reason}` : ''}`;

  // The canary runs first; with pauseAfterCanary nothing runs after it.
  const ran = req.pauseAfterCanary ? 1 : RUNS.length;
  const members: RolloutHost[] = [];
  let failedHost = '';

  hosts.forEach((host, index) => {
    const run = index < ran ? RUNS[index] : undefined;
    const directive = ctx.newId('dir');
    const info = actionOf(host, req.action) as ActionInfo;
    const need: Need = {
      id: `manual/${directive}`,
      kind: 'action',
      directiveId: directive,
      action: { name: req.action, impact: info.impact },
    };

    if (!run) {
      host.needs = [...list(host.needs), need];
      members.push({
        host: host.name,
        state: 'pending',
        stage: index === 0 ? 2 : 3,
        needId: need.id,
        updatedAt: at(0),
      });
      return;
    }

    const ok = run.exitCode === 0 && !run.timedOut;
    const leaseId = ctx.newId('lease');
    const error = ok
      ? undefined
      : run.timedOut
        ? `action ${req.action} timed out`
        : `action ${req.action} exited with code ${run.exitCode}`;
    const lease: Lease = {
      id: leaseId,
      host: host.name,
      state: ok ? 'released' : 'failed',
      needs: [need],
      rollouts: { [need.id]: id },
      weight: host.validators,
      charged: host.validators,
      impact: info.impact,
      force: false,
      grantedBy: 'scheduler',
      grantedAt: at(90 - index * 20),
      expiresAt: at(90 - index * 20 - 1800),
      agentDoneAt: at(30 - index * 5),
      endedAt: at(20 - index * 5),
      operation: {
        leaseId,
        phase: ok ? 'done' : 'failed',
        startedAt: at(85 - index * 20),
        updatedAt: at(30 - index * 5),
        steps: [{ needId: need.id, status: ok ? 'done' : 'failed', error, action: run }],
        disrupted: info.impact === 'node',
        ...(error ? { error } : {}),
      },
      ...(ok ? { readiness: 'ready' } : { error }),
    };
    ctx.data.leases.push(lease);
    if (!ok && failedHost === '') failedHost = host.name;

    members.push({
      host: host.name,
      state: ok ? 'done' : 'failed',
      stage: index === 0 ? 2 : 3,
      needId: need.id,
      leaseId,
      ...(error ? { error } : {}),
      updatedAt: at(20 - index * 5),
    });
  });

  const paused = req.pauseAfterCanary === true && hosts.length > 1;
  const rollout: Rollout = {
    id,
    kind: 'action',
    key: `manual:${id}`,
    title,
    action: req.action,
    ...(req.pauseAfterCanary ? { pauseAfterCanary: true } : {}),
    state: failedHost ? 'halted' : hosts.length === 1 ? 'completed' : 'active',
    ...(failedHost ? { reason: `${failedHost} failed` } : {}),
    paused,
    ...(paused ? { pauseReason: 'paused after the canary; resume to continue' } : {}),
    approved: true,
    force: false,
    ignoreBudget: false,
    canary: hosts[0]?.name,
    createdBy: ctx.actor,
    createdAt: at(100),
    updatedAt: at(0),
    ...(hosts.length === 1 && !failedHost ? { endedAt: at(0) } : {}),
    hosts: members,
  };
  ctx.data.rollouts.push(rollout);

  ctx.log('action.host-action', id, {
    action: req.action,
    hosts: members.map(m => m.host),
    pauseAfterCanary: req.pauseAfterCanary === true,
    reason,
  });

  return {
    message: title,
    rollout: id,
    hosts: members.map(m => m.host),
    rejected: Object.keys(rejected).length > 0 ? rejected : undefined,
  };
}
