import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';

import { errorText, type ApiClient } from './api/client';
import type { Config } from './api/types';
import { AppContext, useAuthState, type AppServices } from './appContext';
import { canViewPrivate, type AuthState, type AuthStore } from './auth/authStore';
import { Header } from './components/Header';
import { NowProvider } from './components/RelTime';
import { Badge, Button, Loading, cx } from './components/primitives';
import { useHashTab } from './hooks/useHashTab';
import { useTheme } from './hooks/useTheme';
import { AuditTab } from './tabs/AuditTab';
import { HostsTab } from './tabs/HostsTab';
import { PackagesTab } from './tabs/PackagesTab';
import { RolloutsTab } from './tabs/RolloutsTab';
import { SettingsTab } from './tabs/SettingsTab';

const TABS = ['hosts', 'rollouts', 'packages', 'settings', 'audit'] as const;
type TabId = (typeof TABS)[number];

const TAB_LABEL: Record<TabId, string> = {
  hosts: 'Hosts',
  rollouts: 'Rollouts',
  packages: 'Packages',
  settings: 'Settings',
  audit: 'Audit',
};

/** Only the Hosts tab has a public view. */
const PRIVATE_TABS: ReadonlySet<TabId> = new Set(['rollouts', 'packages', 'settings', 'audit']);

/** MockControls lets the mock mode flip the network gate from the UI. */
export interface MockControls {
  label: string;
  gateFailing: () => boolean;
  setGateFailing: (failing: boolean) => void;
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
  const [tab, setTab] = useHashTab<TabId>(TABS, 'hosts');
  const [gateFailing, setGateFailing] = useState(() => mock?.gateFailing() ?? false);
  const tabRefs = useRef(new Map<TabId, HTMLButtonElement>());

  useEffect(() => {
    document.title = config.network ? `upgradoor · ${config.network}` : 'upgradoor';
  }, [config.network]);

  const priv = canViewPrivate(authState);

  const onTabKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const index = TABS.indexOf(tab);
    let next: TabId | undefined;

    if (e.key === 'ArrowRight') next = TABS[(index + 1) % TABS.length];
    else if (e.key === 'ArrowLeft') next = TABS[(index - 1 + TABS.length) % TABS.length];
    else if (e.key === 'Home') next = TABS[0];
    else if (e.key === 'End') next = TABS[TABS.length - 1];

    if (next) {
      e.preventDefault();
      setTab(next);
      tabRefs.current.get(next)?.focus();
    }
  };

  let panel: ReactNode;
  if (PRIVATE_TABS.has(tab) && !priv) {
    panel = <LoginPrompt tab={TAB_LABEL[tab]} auth={auth} authState={authState} />;
  } else {
    switch (tab) {
      case 'hosts':
        panel = <HostsTab />;
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
      case 'audit':
        panel = <AuditTab />;
        break;
    }
  }

  return (
    <AppContext value={services}>
      <NowProvider>
        <div className="flex min-h-dvh flex-col bg-background text-foreground">
          <Header
            config={config}
            auth={auth}
            authState={authState}
            theme={preference}
            onTheme={setPreference}
            mockLabel={mock?.label}
          />

          <div className="flex flex-wrap items-end gap-x-3 border-b border-border bg-surface px-3">
            <div role="tablist" aria-label="Views" onKeyDown={onTabKey} className="flex gap-1">
              {TABS.map(id => {
                const locked = PRIVATE_TABS.has(id) && !priv;
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
                    title={locked ? 'Needs a login' : undefined}
                    onClick={() => {
                      setTab(id);
                    }}
                    className={cx(
                      '-mb-px border-b-2 px-3 py-1.5 text-sm/6 font-medium',
                      tab === id
                        ? 'border-accent text-foreground'
                        : 'border-transparent text-muted hover:border-border-strong hover:text-foreground',
                      locked && tab !== id && 'text-faint'
                    )}
                  >
                    {TAB_LABEL[id]}
                    {locked && (
                      <span aria-hidden="true" className="text-faint">
                        {' '}
                        (login)
                      </span>
                    )}
                  </button>
                );
              })}
            </div>

            {mock && (
              <label className="ml-auto flex items-center gap-1.5 py-1.5 text-xs/5 text-muted">
                <input
                  type="checkbox"
                  checked={gateFailing}
                  onChange={e => {
                    mock.setGateFailing(e.target.checked);
                    setGateFailing(e.target.checked);
                  }}
                  className="accent-accent"
                />
                mock: failing network gate
              </label>
            )}
          </div>

          <main id="tab-panel" role="tabpanel" aria-labelledby={`tab-${tab}`} className="grow p-2">
            {panel}
          </main>
        </div>
      </NowProvider>
    </AppContext>
  );
}

function LoginPrompt({ tab, auth, authState }: { tab: string; auth: AuthStore; authState: AuthState }) {
  return (
    <div className="mx-auto flex max-w-md flex-col items-center gap-3 rounded-sm border border-border bg-surface p-6 text-center text-sm/6">
      <Badge>{tab}</Badge>
      {authState.providerError ? (
        <p role="alert" className="text-danger">
          The auth provider is unreachable, so nobody can log in right now. Only the public host list is available.
        </p>
      ) : (
        <>
          <p>This view shows host details that are only served to logged-in users.</p>
          <Button
            variant="primary"
            onClick={() => {
              auth.login();
            }}
          >
            Login
          </Button>
        </>
      )}
    </div>
  );
}
