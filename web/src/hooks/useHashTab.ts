import { useCallback, useEffect, useState } from 'react';

/** hashParts splits the URL hash into the tab and what the tab keeps after it: "#clients/geth" is ["clients", "geth"]. */
function hashParts(): [string, string] {
  const raw = window.location.hash.replace(/^#\/?/, '');
  const slash = raw.indexOf('/');
  if (slash < 0) return [raw, ''];

  let sub = raw.slice(slash + 1);
  try {
    sub = decodeURIComponent(sub);
  } catch {
    // A hash that is not an encoded string is taken as it is.
  }

  return [raw.slice(0, slash), sub];
}

function readHash<T extends string>(tabs: readonly T[], fallback: T): T {
  const [raw] = hashParts();
  return (tabs as readonly string[]).includes(raw) ? (raw as T) : fallback;
}

/** hashOf is the hash of a tab and what it selects: ("rollouts", "leases") is "rollouts/leases". */
function hashOf(tab: string, sub = ''): string {
  return sub === '' ? tab : `${tab}/${sub.split('/').map(encodeURIComponent).join('/')}`;
}

/**
 * useHashTab keeps the active tab in the URL hash (#hosts, #rollouts, ...).
 * Selecting a tab with a sub opens it at that place (#rollouts/leases).
 */
export function useHashTab<T extends string>(tabs: readonly T[], fallback: T): [T, (tab: T, sub?: string) => void] {
  const [tab, setTab] = useState<T>(() => readHash(tabs, fallback));

  useEffect(() => {
    const onHash = () => {
      setTab(readHash(tabs, fallback));
    };
    window.addEventListener('hashchange', onHash);
    return () => {
      window.removeEventListener('hashchange', onHash);
    };
  }, [tabs, fallback]);

  const select = useCallback((next: T, sub?: string) => {
    window.location.hash = hashOf(next, sub);
    setTab(next);
  }, []);

  return [tab, select];
}

/**
 * useHashSub keeps what a tab selects in the URL hash, after the tab's name
 * (#clients/geth), so it can be linked. It is empty while the hash names
 * another tab or nothing after the tab.
 */
export function useHashSub(tab: string): [string, (sub: string) => void] {
  const read = useCallback(() => {
    const [name, sub] = hashParts();
    return name === tab ? sub : '';
  }, [tab]);
  const [sub, setSub] = useState(read);

  useEffect(() => {
    const onHash = () => {
      setSub(read());
    };
    window.addEventListener('hashchange', onHash);
    return () => {
      window.removeEventListener('hashchange', onHash);
    };
  }, [read]);

  const select = useCallback(
    (next: string) => {
      window.location.hash = hashOf(tab, next);
      setSub(next);
    },
    [tab]
  );

  return [sub, select];
}
