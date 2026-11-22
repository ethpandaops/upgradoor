import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';

import { errorText, type ApiClient } from './api/client';
import type { Config } from './api/types';
import { AppContext, useAuthState, type AppServices } from './appContext';
import { canViewPrivate, type AuthStore } from './auth/authStore';
import { Header } from './components/Header';
import { LiveBanner, LiveIndicator, LiveProvider } from './components/Live';
import { DEFAULT_SERVICE, PermissionsProvider } from './components/Permissions';
import { NowProvider } from './components/RelTime';
import { Button, Loading, cx } from './components/primitives';
import { useHashTab } from './hooks/useHashTab';
import { useTheme } from './hooks/useTheme';
import { FEATURE } from './lib/permissions';
import { AuditTab } from './tabs/AuditTab';
import { ClientsTab } from './tabs/ClientsTab';
import { HostsTab } from './tabs/HostsTab';
import { OverviewTab } from './tabs/OverviewTab';
import { PackagesTab } from './tabs/PackagesTab';
import { RolloutsTab } from './tabs/RolloutsTab';
import { DataSourcesTab } from './tabs/DataSourcesTab';
import { SettingsTab } from './tabs/SettingsTab';

export const TABS = ['overview', 'hosts', 'clients', 'rollouts', 'packages', 'settings', 'sources', 'audit'] as const;
type TabId = (typeof TABS)[number];

const TAB_LABEL: Record<TabId, string> = {
  overview: 'Overview',
  hosts: 'Hosts',
  clients: 'Clients',
  rollouts: 'Rollouts',
  packages: 'Packages',
  settings: 'Settings',
  sources: 'Data sources',
  audit: 'Audit',
};

/** Settings and data sources need the feature admin. */
const ADMIN_TABS: ReadonlySet<TabId> = new Set(['settings', 'sources']);

/** Packages needs the feature details. */
const DETAIL_TABS: ReadonlySet<TabId> = new Set(['packages']);

/** Overview and Hosts have a public view; the rest needs a login. */
const PRIVATE_TABS: ReadonlySet<TabId> = new Set(['clients', 'rollouts', 'packages', 'settings', 'sources', 'audit']);

/**
 * visibleTabs are the tabs the login can use; the others are not in the menu.
 * The rules are the controller's: without a login only the public views
 * exist, and the settings can only be changed with the feature admin, so for
 * every other login the Settings tab would be a page of controls it cannot
 * use; the data sources are served to admin only. Without an auth provider
 * (open mode) everything is shown. This only decides what is offered: the
 * controller decides what each request may do.
 */
export function visibleTabs(access: { priv: boolean; admin: boolean; hostDetails: boolean }): TabId[] {
  return TABS.filter(id =>
    ADMIN_TABS.has(id)
      ? access.admin
      : DETAIL_TABS.has(id)
        ? access.priv && access.hostDetails
        : !PRIVATE_TABS.has(id) || access.priv
  );
}

/** MockControls lets the mock mode flip the network gate and, with a fake auth client, the login from the UI. */
export interface MockControls {
  label: string;
  gateFailing: () => boolean;
  setGateFailing: (failing: boolean) => void;
  /** login switches who the fake auth client is logged in as (auth mode only). */
  login?: {
    options: readonly string[];
    current: () => string;
    set: (profile: string) => void;
  };
}

type Boot = { phase: 'loading' } | { phase: 'failed'; error: unknown } | { phase: 'ready'; config: Config };

/**
 * App loads the controller config, starts the auth store with it and then
 * renders the one page with its tabs.
 */
export function App({ api, auth, mock }: { api: ApiClient; auth: AuthStore; mock?: MockControls }) {
  const [boot, setBoot] = useState<Boot>({ phase: 'loading' });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let cancelled = false;

    api
      .config()
      .then(async config => {
        await auth.start(config.authProviderUrl);
        if (!cancelled) setBoot({ phase: 'ready', config });
      })
      .catch((error: unknown) => {
        if (!cancelled) setBoot({ phase: 'failed', error });
      });

    return () => {
      cancelled = true;
    };
  }, [api, auth, attempt]);

  if (boot.phase === 'loading') {
    return (
      <div className="flex min-h-dvh items-center justify-center">
        <Loading what="upgradoor" />
      </div>
    );
  }

  if (boot.phase === 'failed') {
    return (
      <div className="flex min-h-dvh flex-col items-center justify-center gap-3 p-4 text-sm/6">
        <p role="alert" className="text-danger">
          Could not reach the controller: {errorText(boot.error)}
        </p>
        <Button
          variant="primary"
          onClick={() => {
            setBoot({ phase: 'loading' });
            setAttempt(n => n + 1);
          }}
        >
          Retry
        </Button>
      </div>
    );
  }

  return <Shell api={api} auth={auth} config={boot.config} mock={mock} />;
}

function Shell({ api, auth, config, mock }: { api: ApiClient; auth: AuthStore; config: Config; mock?: MockControls }) {
  const services = useMemo<AppServices>(() => ({ api, auth, config }), [api, auth, config]);
  const authState = useAuthState(auth);
  const { preference, setPreference } = useTheme();
  // A link to a host (?host=<name>) without a tab opens the Hosts tab.
  const [linkedHost] = useState(() => new URLSearchParams(window.location.search).has('host'));
  const [wanted, setTab] = useHashTab<TabId>(TABS, linkedHost ? 'hosts' : 'overview');
  const [gateFailing, setGateFailing] = useState(() => mock?.gateFailing() ?? false);
  const [mockLogin, setMockLogin] = useState(() => mock?.login?.current() ?? '');
  const tabRefs = useRef(new Map<TabId, HTMLButtonElement>());

  useEffect(() => {
    document.title = config.network ? `upgradoor · ${config.network}` : 'upgradoor';
  }, [config.network]);

  const priv = canViewPrivate(authState);
  const service = config.authService || DEFAULT_SERVICE;
  // Open mode has no login and no features: everything may be done.
  const admin = !authState.enabled || (priv && auth.hasFeature(service, FEATURE.admin));
  const hostDetails = !authState.enabled || (priv && auth.hasFeature(service, FEATURE.details));
  const tabs = useMemo(() => visibleTabs({ priv, admin, hostDetails }), [priv, admin, hostDetails]);

  // A tab that is not in the menu is not shown either: its URL, and the tab a
  // login was on when it ended, lead to the overview. The panel is the
  // overview's from the first render, so nothing of the hidden tab flashes.
  const tab: TabId = tabs.includes(wanted) ? wanted : 'overview';
  useEffect(() => {
    if (wanted !== tab) setTab(tab);
  }, [wanted, tab, setTab]);

  const onTabKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const index = tabs.indexOf(tab);
    let next: TabId | undefined;

    if (e.key === 'ArrowRight') next = tabs[(index + 1) % tabs.length];
    else if (e.key === 'ArrowLeft') next = tabs[(index - 1 + tabs.length) % tabs.length];
    else if (e.key === 'Home') next = tabs[0];
    else if (e.key === 'End') next = tabs[tabs.length - 1];

    if (next) {
      e.preventDefault();
      setTab(next);
      tabRefs.current.get(next)?.focus();
    }
  };

  let panel: ReactNode;
  switch (tab) {
    case 'overview':
      panel = <OverviewTab onOpenTab={setTab} />;
      break;
    case 'hosts':
      panel = <HostsTab />;
      break;
    case 'clients':
      panel = <ClientsTab onOpenTab={setTab} />;
      break;
    case 'rollouts':
      panel = <RolloutsTab />;
      break;
    case 'packages':
      panel = <PackagesTab />;
      break;
    case 'settings':
      panel = <SettingsTab />;
      break;
    case 'sources':
      panel = <DataSourcesTab />;
      break;
    case 'audit':
      panel = <AuditTab />;
      break;
  }

  return (
    <AppContext value={services}>
      <LiveProvider>
        <NowProvider>
          <div className="flex min-h-dvh flex-col bg-background text-foreground">
            <Header
              config={config}
              auth={auth}
              authState={authState}
              theme={preference}
              onTheme={setPreference}
              mockLabel={mock?.label}
              extra={
                <>
                  <LiveIndicator />
                  {mock && (
                    <>
                      <label className="hidden items-center gap-1.5 text-xs/5 text-muted md:flex">
                        <input
                          type="checkbox"
                          checked={gateFailing}
                          onChange={e => {
                            mock.setGateFailing(e.target.checked);
                            setGateFailing(e.target.checked);
                          }}
                          className="accent-accent"
                        />
                        failing gate
                      </label>
                      {mock.login && (
                        <label className="hidden items-center gap-1.5 text-xs/5 text-muted md:flex">
                          login as
                          <select
                            value={mockLogin}
                            onChange={e => {
                              mock.login?.set(e.target.value);
                              setMockLogin(e.target.value);
                            }}
                            className="rounded-sm border border-border-strong bg-surface px-1 text-foreground"
                          >
                            {mock.login.options.map(option => (
                              <option key={option} value={option}>
                                {option}
                              </option>
                            ))}
                          </select>
                        </label>
                      )}
                    </>
                  )}
                </>
              }
              nav={
                <div
                  role="tablist"
                  aria-label="Views"
                  onKeyDown={onTabKey}
                  className="flex h-full min-h-10 items-stretch"
                >
                  {tabs.map(id => {
                    return (
                      <button
                        key={id}
                        ref={el => {
                          if (el) tabRefs.current.set(id, el);
                          else tabRefs.current.delete(id);
                        }}
                        type="button"
                        role="tab"
                        id={`tab-${id}`}
                        aria-selected={tab === id}
                        aria-controls="tab-panel"
                        tabIndex={tab === id ? 0 : -1}
                        onClick={() => {
                          setTab(id);
                        }}
                        className={cx(
                          'border-y-2 border-t-transparent px-3 text-sm/6 font-medium whitespace-nowrap',
                          tab === id
                            ? 'border-b-accent text-foreground'
                            : 'border-b-transparent text-muted hover:text-foreground'
                        )}
                      >
                        {TAB_LABEL[id]}
                      </button>
                    );
                  })}
                </div>
              }
            />
            <LiveBanner />

            <main id="tab-panel" role="tabpanel" aria-labelledby={`tab-${tab}`} className="grow p-4">
              <PermissionsProvider>{panel}</PermissionsProvider>
            </main>
          </div>
        </NowProvider>
      </LiveProvider>
    </AppContext>
  );
}
