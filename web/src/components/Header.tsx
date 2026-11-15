import type { ReactNode } from 'react';

import type { Config } from '../api/types';
import type { AuthState, AuthStore } from '../auth/authStore';
import { usePandaMenu } from '../hooks/usePandaMenu';
import type { ThemePreference } from '../hooks/useTheme';
import { Badge, Button, cx } from './primitives';

const NEXT_THEME: Record<ThemePreference, ThemePreference> = { system: 'light', light: 'dark', dark: 'system' };

// Bootstrap Icons: circle-half, sun-fill, moon-stars-fill (the icons the
// other ethpandaops tools use for their theme switch).
const THEME_ICON: Record<ThemePreference, string[]> = {
  system: ['M8 15A7 7 0 1 0 8 1v14zm0 1A8 8 0 1 1 8 0a8 8 0 0 1 0 16z'],
  light: [
    'M8 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM8 0a.5.5 0 0 1 .5.5v2a.5.5 0 0 1-1 0v-2A.5.5 0 0 1 8 0zm0 13a.5.5 0 0 1 .5.5v2a.5.5 0 0 1-1 0v-2A.5.5 0 0 1 8 13zm8-5a.5.5 0 0 1-.5.5h-2a.5.5 0 0 1 0-1h2a.5.5 0 0 1 .5.5zM3 8a.5.5 0 0 1-.5.5h-2a.5.5 0 0 1 0-1h2A.5.5 0 0 1 3 8zm10.657-5.657a.5.5 0 0 1 0 .707l-1.414 1.415a.5.5 0 1 1-.707-.708l1.414-1.414a.5.5 0 0 1 .707 0zm-9.193 9.193a.5.5 0 0 1 0 .707L3.05 13.657a.5.5 0 0 1-.707-.707l1.414-1.414a.5.5 0 0 1 .707 0zm9.193 2.121a.5.5 0 0 1-.707 0l-1.414-1.414a.5.5 0 0 1 .707-.707l1.414 1.414a.5.5 0 0 1 0 .707zM4.464 4.465a.5.5 0 0 1-.707 0L2.343 3.05a.5.5 0 1 1 .707-.707l1.414 1.414a.5.5 0 0 1 0 .708z',
  ],
  dark: [
    'M6 .278a.768.768 0 0 1 .08.858 7.208 7.208 0 0 0-.878 3.46c0 4.021 3.278 7.277 7.318 7.277.527 0 1.04-.055 1.533-.16a.787.787 0 0 1 .81.316.733.733 0 0 1-.031.893A8.349 8.349 0 0 1 8.344 16C3.734 16 0 12.286 0 7.71 0 4.266 2.114 1.312 5.124.06A.752.752 0 0 1 6 .278z',
    'M10.794 3.148a.217.217 0 0 1 .412 0l.387 1.162c.173.518.579.924 1.097 1.097l1.162.387a.217.217 0 0 1 0 .412l-1.162.387a1.734 1.734 0 0 0-1.097 1.097l-.387 1.162a.217.217 0 0 1-.412 0l-.387-1.162A1.734 1.734 0 0 0 9.31 6.593l-1.162-.387a.217.217 0 0 1 0-.412l1.162-.387a1.734 1.734 0 0 0 1.097-1.097l.387-1.162z',
  ],
};

/**
 * Header is the app bar, in the style of the other ethpandaops tools: dark in
 * both themes, pinned to the top, the brand on the left, the views as links
 * inside the bar, the session and the theme switch on the right.
 *
 * The bar is 72px high and keeps its left corner free when the panda menu is
 * on the page: the menu floats a 44px button 16px from the top-left corner,
 * which then sits centred in the bar, left of the brand.
 */
export function Header({
  config,
  auth,
  authState,
  theme,
  onTheme,
  mockLabel,
  nav,
  extra,
}: {
  config: Config;
  auth: AuthStore;
  authState: AuthState;
  theme: ThemePreference;
  onTheme: (next: ThemePreference) => void;
  mockLabel?: string;
  /** nav is the view switch, rendered inside the bar. */
  nav?: ReactNode;
  /** extra is shown before the session, e.g. the mock controls. */
  extra?: ReactNode;
}) {
  const pandaMenu = usePandaMenu();

  return (
    // "dark" scopes the dark palette to the bar, whatever the page theme is.
    <header
      className={cx(
        'dark sticky top-0 z-40 flex min-h-18 flex-wrap items-center gap-x-6 border-b border-border bg-background pr-4 text-foreground shadow-md',
        pandaMenu ? 'pl-18' : 'pl-4'
      )}
    >
      <div className="flex h-18 items-center gap-3">
        <svg aria-hidden="true" viewBox="0 0 32 32" className="size-9 shrink-0">
          <rect width="32" height="32" rx="6" className="fill-accent" />
          <path d="M16 6l8 9h-5v11h-6V15H8z" className="fill-on-accent" />
        </svg>
        <div className="flex flex-col">
          <h1 className="text-lg/6 font-semibold tracking-tight">upgradoor</h1>
          <span className="font-mono text-xs/4 text-muted" title="Network">
            {config.network || 'unknown network'}
          </span>
        </div>
      </div>

      {nav && <div className="order-last w-full self-stretch overflow-x-auto lg:order-none lg:w-auto">{nav}</div>}

      <div className="ml-auto flex items-center gap-3">
        {mockLabel && <Badge tone="warning">{mockLabel}</Badge>}
        {extra}
        {config.version && (
          <span className="hidden font-mono text-xs/5 text-faint xl:inline" title="Controller version">
            {config.version}
          </span>
        )}

        {!authState.enabled ? (
          <Badge title="No auth provider is configured: the API is open and everything is visible">open access</Badge>
        ) : authState.providerError ? (
          <Badge tone="danger" title="The auth client could not be loaded; only public data is shown">
            {authState.providerError}
          </Badge>
        ) : authState.loggedIn ? (
          <>
            <span
              className="hidden text-xs/5 text-muted sm:inline"
              title={authState.teams.length > 0 ? `Logged in as; teams: ${authState.teams.join(', ')}` : 'Logged in as'}
            >
              {authState.user ?? 'logged in'}
            </span>
            <Button
              onClick={() => {
                auth.logout();
              }}
            >
              Logout
            </Button>
          </>
        ) : (
          <Button
            variant="primary"
            onClick={() => {
              auth.login();
            }}
          >
            Login
          </Button>
        )}

        <button
          type="button"
          aria-label={`Theme: ${theme}. Switch to ${NEXT_THEME[theme]}`}
          title={`Theme: ${theme}. Click for ${NEXT_THEME[theme]}`}
          onClick={() => {
            onTheme(NEXT_THEME[theme]);
          }}
          className="flex size-9 items-center justify-center rounded-sm text-muted hover:bg-surface-hover hover:text-foreground"
        >
          <svg aria-hidden="true" viewBox="0 0 16 16" className="size-4 fill-current">
            {THEME_ICON[theme].map(d => (
              <path key={d.slice(0, 12)} d={d} />
            ))}
          </svg>
        </button>
      </div>
    </header>
  );
}
