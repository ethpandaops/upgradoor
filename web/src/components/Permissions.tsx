// What the login may do, for the views: one provider that knows the login
// and, for a login that acts through its teams, who owns what; a hook that
// answers for a call or for the dialog a button opens; and the button that
// is disabled, with the reason as its tooltip, when the answer is no.

import { createContext, useContext, useMemo, type ComponentProps, type ReactNode } from 'react';

import { list } from '../api/types';
import { useApp, useAuthState } from '../appContext';
import { canViewPrivate } from '../auth/authStore';
import { usePoll } from '../hooks/usePoll';
import { buildRebootRequest, buildRollRequest } from '../lib/actionRequests';
import {
  FEATURE,
  can,
  needsOwnership,
  type Login,
  type OwnedHost,
  type PermissionCall,
  type Verdict,
} from '../lib/permissions';
import type { ActionIntent } from './ActionDialog';
import { Button } from './primitives';

export interface Permissions {
  login: Login;
  /** admin reports whether the login may set force and ignoreBudget and select the whole fleet. */
  admin: boolean;
  /**
   * can decides one call. leaseHost names the host of the lease an end-lease
   * call is about, rolloutState the state of the rollout a retry is about.
   */
  can: (call: PermissionCall, leaseHost?: string, rolloutState?: string) => Verdict;
  /** canIntent decides whether the dialog of an intent can end in a call the login may make. */
  canIntent: (intent: ActionIntent) => Verdict;
  /**
   * ownContainers are the containers a roll dialog should start with: none
   * (the whole host) when the login may roll everything on the selection,
   * otherwise the choices it may roll one by one.
   */
  ownContainers: (intent: ActionIntent) => readonly string[];
}

const PermissionsContext = createContext<Permissions | null>(null);

/** The default service name, for a controller that does not report one. */
const DEFAULT_SERVICE = 'upgradoor';

const QUIET = { timing: 'now', force: false, ignoreBudget: false, reason: '' } as const;

function rollCall(intent: ActionIntent & { kind: 'roll' }, containers: readonly string[]): PermissionCall {
  return { action: 'roll', body: buildRollRequest({ hosts: intent.hosts, all: intent.all, containers, ...QUIET }) };
}

function build(login: Login, hosts: readonly OwnedHost[], ready: boolean): Permissions {
  const decide = (call: PermissionCall, leaseHost?: string, rolloutState?: string): Verdict => {
    const verdict = can(login, call, { hosts, leaseHost: () => leaseHost, rolloutState: () => rolloutState });

    // Until the hosts are loaded nobody owns anything: say so instead.
    if (!verdict.allowed && !ready) return { ...verdict, reason: 'Checking what your teams own…' };

    return verdict;
  };

  const ownContainers = (intent: ActionIntent): readonly string[] => {
    if (intent.kind !== 'roll' || intent.all || decide(rollCall(intent, [])).allowed) return [];

    return intent.containerChoices.filter(c => decide(rollCall(intent, [c])).allowed);
  };

  const canIntent = (intent: ActionIntent): Verdict => {
    switch (intent.kind) {
      case 'roll': {
        const whole = decide(rollCall(intent, []));
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
        return decide(intent.make(''), intent.leaseHost, intent.rolloutState);
    }
  };

  return {
    login,
    admin: login.open || login.hasFeature(FEATURE.admin),
    can: decide,
    canIntent,
    ownContainers,
  };
}

/**
 * PermissionsProvider answers what the login may do. A login that holds a
 * team feature without the plain one needs to know who owns what: only then
 * the host list is polled here.
 */
export function PermissionsProvider({ children }: { children: ReactNode }) {
  const { api, auth, config } = useApp();
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
  const hosts = usePoll(s => api.hosts(s), { key: 'permission-hosts', enabled: owned });

  const value = useMemo(
    () => build(login, list(hosts.data?.hosts), !owned || hosts.data !== undefined),
    [login, hosts.data, owned]
  );

  return <PermissionsContext value={value}>{children}</PermissionsContext>;
}

/** usePermissions returns what the login may do; it must be used below <PermissionsProvider>. */
export function usePermissions(): Permissions {
  const permissions = useContext(PermissionsContext);
  if (!permissions) throw new Error('usePermissions used outside PermissionsProvider');
  return permissions;
}

/** guard is the disabled state and tooltip of a control that makes a call. */
export function guard(verdict: Verdict, title?: string): { disabled: boolean; title: string | undefined } {
  return { disabled: !verdict.allowed, title: verdict.allowed ? title : verdict.reason };
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
