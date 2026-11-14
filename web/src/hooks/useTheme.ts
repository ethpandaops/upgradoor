import { useCallback, useEffect, useState } from 'react';

/** "system" follows the OS; "light" / "dark" are the persisted override. */
export type ThemePreference = 'system' | 'light' | 'dark';

const STORAGE_KEY = 'upgradoor.theme';
const DARK_QUERY = '(prefers-color-scheme: dark)';

function readPreference(): ThemePreference {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    return v === 'light' || v === 'dark' ? v : 'system';
  } catch {
    return 'system';
  }
}

function systemDark(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia(DARK_QUERY).matches;
}

/** applyTheme sets the .dark class on <html> for a preference. */
export function applyTheme(pref: ThemePreference = readPreference()): void {
  const dark = pref === 'dark' || (pref === 'system' && systemDark());
  document.documentElement.classList.toggle('dark', dark);
}

/** useTheme exposes the theme preference; changes are persisted and applied. */
export function useTheme(): { preference: ThemePreference; setPreference: (p: ThemePreference) => void } {
  const [preference, setPref] = useState<ThemePreference>(readPreference);

  useEffect(() => {
    applyTheme(preference);
    if (preference !== 'system' || typeof window.matchMedia !== 'function') return;

    const mq = window.matchMedia(DARK_QUERY);
    const onChange = () => {
      applyTheme('system');
    };
    mq.addEventListener('change', onChange);
    return () => {
      mq.removeEventListener('change', onChange);
    };
  }, [preference]);

  const setPreference = useCallback((p: ThemePreference) => {
    try {
      if (p === 'system') localStorage.removeItem(STORAGE_KEY);
      else localStorage.setItem(STORAGE_KEY, p);
    } catch {
      // Storage unavailable: the choice lasts for this page only.
    }
    setPref(p);
  }, []);

  return { preference, setPreference };
}
