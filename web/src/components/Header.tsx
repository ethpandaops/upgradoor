import type { Config } from '../api/types';
import type { AuthState, AuthStore } from '../auth/authStore';
import type { ThemePreference } from '../hooks/useTheme';
import { Badge, Button } from './primitives';

const NEXT_THEME: Record<ThemePreference, ThemePreference> = { system: 'light', light: 'dark', dark: 'system' };

/** Header shows the network, the session (Login / user + Logout) and the theme switch. */
export function Header({
  config,
  auth,
  authState,
  theme,
  onTheme,
  mockLabel,
}: {
  config: Config;
  auth: AuthStore;
  authState: AuthState;
  theme: ThemePreference;
  onTheme: (next: ThemePreference) => void;
  mockLabel?: string;
}) {
  return (
    <header className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-border bg-surface px-3 py-1.5">
      <h1 className="text-sm/6 font-semibold">upgradoor</h1>
      <span className="font-mono text-xs/5 text-muted" title="Network">
        {config.network || 'unknown network'}
      </span>
      {config.version && (
        <span className="font-mono text-xs/5 text-faint" title="Controller version">
          {config.version}
        </span>
      )}
      {mockLabel && <Badge tone="warning">{mockLabel}</Badge>}

      <div className="ml-auto flex flex-wrap items-center gap-2">
        {!authState.enabled ? (
          <Badge title="No auth provider is configured: the API is open and everything is visible">open access</Badge>
        ) : authState.providerError ? (
          <Badge tone="danger" title="The auth client could not be loaded; only public data is shown">
            {authState.providerError}
          </Badge>
        ) : authState.loggedIn ? (
          <>
            <span className="text-xs/5" title="Logged in as">
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

        <Button
          variant="ghost"
          aria-label={`Theme: ${theme}. Switch to ${NEXT_THEME[theme]}`}
          title="Switch between system, light and dark"
          onClick={() => {
            onTheme(NEXT_THEME[theme]);
          }}
        >
          Theme: {theme}
        </Button>
      </div>
    </header>
  );
}
