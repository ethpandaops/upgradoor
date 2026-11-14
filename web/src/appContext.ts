import { createContext, useContext, useSyncExternalStore } from 'react';

import type { ApiClient } from './api/client';
import type { Config } from './api/types';
import { canViewPrivate, type AuthState, type AuthStore } from './auth/authStore';

/** Everything a view needs: the API client, the auth store and the controller config. */
export interface AppServices {
  api: ApiClient;
  auth: AuthStore;
  config: Config;
}

export const AppContext = createContext<AppServices | null>(null);

/** useApp returns the app services; it must be used below <AppContext>. */
export function useApp(): AppServices {
  const services = useContext(AppContext);
  if (!services) throw new Error('useApp used outside AppContext');
  return services;
}

/** useAuthState subscribes to the auth store. */
export function useAuthState(auth: AuthStore): AuthState {
  return useSyncExternalStore(auth.subscribe, auth.getState);
}

/** usePrivateAccess reports whether the authenticated views may be requested. */
export function usePrivateAccess(): boolean {
  const { auth } = useApp();
  return canViewPrivate(useAuthState(auth));
}
