// What the login may do, for the views: one provider that knows the login
// and, for a login that acts through its teams, who owns what; a hook that
// answers for a call or for the dialog a button opens; and the button that
// is disabled, with the reason as its tooltip, when the answer is no.

import { createContext, useContext, useMemo, type ComponentProps, type ReactNode } from 'react';

import { list } from '../api/types';
import { useApp, useAuthState } from '../appContext';
import { canViewPrivate } from '../auth/authStore';
import { buildRebootRequest, buildRollRequest } from '../lib/actionRequests';
import {
  FEATURE,
  can,
  hasFeature,
  needsActionHosts,
  needsOwnership,
  type Login,
  type CallContext,
  type OwnedClient,
  type OwnedHost,
  type PermissionCall,
  type Verdict,
} from '../lib/permissions';
import type { ActionIntent } from './ActionDialog';
import { intentVerdict } from './HostActionDialog';
import { useLive } from './Live';
import { Button } from './primitives';

export interface Permissions {
  login: Login;
  /** refreshImages reports whether the login may ask for a registry lookup of every tag now (the feature roll). */
  refreshImages: boolean;
  /** hostDetails: the login reads every host in full (the feature details); the stream is reduced otherwise. */
  hostDetails: boolean;
  /** can decides one call; context says what the page knows of the lease or rollout it is about. */
  can: (call: PermissionCall, context?: CallContext) => Verdict;
  /** offers reports whether the page shows an intent at all: a login without reboot is not offered one. */
  offers: (intent: ActionIntent) => boolean;
  /** canIntent decides whether the dialog of an intent can end in a call the login may make. */
  canIntent: (intent: ActionIntent) => Verdict;
  /**
   * ownContainers are the containers a roll dialog should start with: none
   * (the whole host) when the login may roll everything on the selection,
   * otherwise the choices it may roll one by one.
   */
  ownContainers: (intent: ActionIntent) => readonly string[];
  /**
   * rollChoices are the containers a roll dialog offers, each with whether the login may roll it and its owners;
   * whole says whether the login may roll every managed container of the selection (nothing ticked).
   */
  rollChoices: (intent: ActionIntent & { kind: 'roll' }) => { whole: boolean; choices: readonly RollChoice[] };
}

/** RollChoice is one container of a roll dialog. */
export interface RollChoice {
  name: string;
  allowed: boolean;
  /** owners are the teams of the container on the selected hosts. */
  owners: readonly string[];
}

const PermissionsContext = createContext<Permissions | null>(null);

/** The default service name, for a controller that does not report one. */
export const DEFAULT_SERVICE = 'upgradoor';

const QUIET = { timing: 'now', force: false, ignoreBudget: false, reason: '' } as const;

const NONE: readonly string[] = [];

function rollCall(intent: ActionIntent & { kind: 'roll' }, containers: readonly string[]): PermissionCall {
  return { action: 'roll', body: buildRollRequest({ hosts: intent.hosts, all: intent.all, containers, ...QUIET }) };
}

function build(
  login: Login,
  hosts: readonly OwnedHost[],
  clients: readonly OwnedClient[],
  ready: boolean,
  hostsReady: boolean
): Permissions {
  const decide = (call: PermissionCall, context: CallContext = {}): Verdict => {
    const verdict = can(login, call, {
      hosts,
      clients,
      leaseWork: () => context.lease,
      rolloutKind: () => context.rolloutKind,
      rolloutClient: () => context.rolloutClient,
      rolloutAction: () => context.rolloutAction,
    });

    // Until the hosts are loaded nobody owns anything: say so instead.
    if (!verdict.allowed && (!ready || (context.rolloutAction !== undefined && !hostsReady))) {
      return { ...verdict, reason: 'Checking what your teams own…' };
    }

    return verdict;
  };

  const ownContainers = (intent: ActionIntent): readonly string[] => {
    if (intent.kind !== 'roll' || intent.all) return [];

    // A roll that is about some containers starts with those, as far as the login may roll them.
    const wanted = intent.containers ?? NONE;
    if (decide(rollCall(intent, wanted)).allowed) return wanted;

    return (wanted.length > 0 ? wanted : intent.containerChoices).filter(c => decide(rollCall(intent, [c])).allowed);
  };

  const rollChoices = (intent: ActionIntent & { kind: 'roll' }) => {
    const selected = intent.all ? hosts : hosts.filter(h => intent.hosts.includes(h.name));
    return {
      whole: decide(rollCall(intent, NONE)).allowed,
      choices: intent.containerChoices.map(name => ({
        name,
        allowed: decide(rollCall(intent, [name])).allowed,
        owners: [
          ...new Set(
            selected.flatMap(h =>
              list(h.containers).flatMap(c => (c.managed && c.name === name && c.team ? [c.team] : []))
            )
          ),
        ].sort(),
      })),
    };
  };

  const canIntent = (intent: ActionIntent): Verdict => {
    switch (intent.kind) {
      case 'roll': {
        const whole = decide(rollCall(intent, intent.containers ?? NONE));
        if (whole.allowed || ownContainers(intent).length === 0) return whole;

        // The dialog offers the containers; some of them are the login's.
        return decide(rollCall(intent, ownContainers(intent)));
      }
      case 'reboot':
        return decide({
          action: 'reboot',
          body: buildRebootRequest({ hosts: intent.hosts, all: intent.all, ...QUIET }),
        });
      case 'call':
        return decide(intent.make(''), intent);
      case 'client-override': {
        // One part the login may override is enough to open the dialog; the controller judges every host.
        const parts = intent.part === 'clear' ? (['image', 'flags'] as const) : ([intent.part] as const);
        const verdicts = parts.map(part =>
          decide({
            action: 'client-override',
            body: { client: intent.client.name, kind: 'cl', hosts: [...intent.hosts], part, reason: '' },
          })
        );
        return verdicts.find(v => v.allowed) ?? (verdicts[0] as Verdict);
      }
      case 'image-override':
      case 'flag-override': {
        // The dialog lets the user pick the container: one the login may override is enough to open it.
        const verdicts = intent.containers.map(c =>
          decide(
            intent.kind === 'image-override'
              ? { action: intent.kind, body: { host: intent.host, container: c.name, image: '', reason: '' } }
              : {
                  action: intent.kind,
                  body: { host: intent.host, container: c.name, addFlags: [], removeFlags: [], reason: '' },
                }
          )
        );
        return (
          verdicts.find(v => v.allowed) ??
          verdicts[0] ?? { allowed: false, reason: 'This host has no such container', by: '' }
        );
      }
      case 'host-action':
        // The choices were judged host by host when the intent was built.
        return intentVerdict(intent);
    }
  };

  return {
    login,
    refreshImages: login.open || hasFeature(login, FEATURE.manage),
    hostDetails: login.open || hasFeature(login, FEATURE.details),
    can: decide,
    offers: intent => intent.kind !== 'reboot' || login.open || hasFeature(login, FEATURE.manage),
    canIntent,
    ownContainers,
    rollChoices,
  };
}

/**
 * PermissionsProvider answers what the login may do. A login that holds a
 * team feature without the plain one needs to know who owns what: that is read
 * from the host table of the event stream. A login without manage reads the
 * host table too: the approval of an action rollout follows the action on its hosts.
 */
export function PermissionsProvider({ children }: { children: ReactNode }) {
  const { auth, config } = useApp();
  const state = useAuthState(auth);
  const service = config.authService || DEFAULT_SERVICE;

  const login = useMemo<Login>(
    () => ({
      open: state.ready && !state.enabled,
      service,
      hasFeature: feature => auth.hasFeature(service, feature),
      teams: state.teams,
    }),
    // A new state is a new login: the features are read from the auth client.
    [auth, service, state]
  );

  const owned = canViewPrivate(state) && needsOwnership(login);
  const actionHosts = canViewPrivate(state) && needsActionHosts(login);
  const hosts = useLive('hosts', owned || actionHosts);
  const clients = useLive('clients', owned);

  const value = useMemo(
    () =>
      build(
        login,
        list(hosts.data?.hosts),
        list(clients.data?.clients),
        !owned || hosts.data !== undefined,
        !actionHosts || hosts.data !== undefined
      ),
    [login, hosts.data, clients.data, owned, actionHosts]
  );

  return <PermissionsContext value={value}>{children}</PermissionsContext>;
}

/** usePermissions returns what the login may do; it must be used below <PermissionsProvider>. */
export function usePermissions(): Permissions {
  const permissions = useContext(PermissionsContext);
  if (!permissions) throw new Error('usePermissions used outside PermissionsProvider');
  return permissions;
}

/**
 * ActionButton opens the dialog of an intent. It is disabled, and says why in
 * its tooltip, when the login may not make the call the dialog would send.
 */
export function ActionButton({
  intent,
  onAction,
  disabled,
  title,
  ...rest
}: Omit<ComponentProps<typeof Button>, 'onClick'> & {
  intent: ActionIntent;
  onAction: (intent: ActionIntent) => void;
}) {
  const verdict = usePermissions().canIntent(intent);

  return (
    <Button
      {...rest}
      disabled={disabled === true || !verdict.allowed}
      title={verdict.allowed ? title : verdict.reason}
      onClick={() => {
        onAction(intent);
      }}
    />
  );
}
