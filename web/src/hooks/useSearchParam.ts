import { useCallback, useEffect, useState } from 'react';

function readParam(name: string): string {
  return new URLSearchParams(window.location.search).get(name) ?? '';
}

/**
 * useSearchParam keeps one value in the URL query (?host=...), so it can be
 * linked. A change replaces the history entry; empty removes the parameter.
 */
export function useSearchParam(name: string): [string, (value: string) => void] {
  const [value, setValue] = useState(() => readParam(name));

  useEffect(() => {
    const onPop = () => {
      setValue(readParam(name));
    };
    window.addEventListener('popstate', onPop);
    return () => {
      window.removeEventListener('popstate', onPop);
    };
  }, [name]);

  const select = useCallback(
    (next: string) => {
      const url = new URL(window.location.href);
      if (next === '') url.searchParams.delete(name);
      else url.searchParams.set(name, next);
      window.history.replaceState(window.history.state, '', url);
      setValue(next);
    },
    [name]
  );

  return [value, select];
}
