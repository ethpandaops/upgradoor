import { useCallback, useEffect, useState } from 'react';

function readHash<T extends string>(tabs: readonly T[], fallback: T): T {
  const raw = window.location.hash.replace(/^#\/?/, '');
  return (tabs as readonly string[]).includes(raw) ? (raw as T) : fallback;
}

/** useHashTab keeps the active tab in the URL hash (#hosts, #rollouts, ...). */
export function useHashTab<T extends string>(tabs: readonly T[], fallback: T): [T, (tab: T) => void] {
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

  const select = useCallback((next: T) => {
    window.location.hash = next;
    setTab(next);
  }, []);

  return [tab, select];
}
