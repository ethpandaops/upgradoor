import { useCallback, useEffect, useState } from 'react';

interface HashQuery {
  /** path is what follows the tab up to "?": "images" in #packages/images?q=geth. */
  path: string;
  params: URLSearchParams;
}

function readHashQuery(tab: string): HashQuery | undefined {
  const raw = window.location.hash.replace(/^#\/?/, '');
  if (raw !== tab && !raw.startsWith(`${tab}/`)) return undefined;

  const rest = raw.slice(tab.length + 1);
  const mark = rest.indexOf('?');
  const path = mark < 0 ? rest : rest.slice(0, mark);

  let decoded = path;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    // A path that is not an encoded string is taken as it is.
  }

  return { path: decoded, params: new URLSearchParams(mark < 0 ? '' : rest.slice(mark + 1)) };
}

/**
 * useHashQuery keeps a tab's view state in the URL hash, after the tab's name
 * (#packages/images?q=geth&size=250), so it can be linked. A change replaces
 * the history entry. The first value is undefined while the hash names
 * another tab.
 */
export function useHashQuery(tab: string): [HashQuery | undefined, (path: string, params: URLSearchParams) => void] {
  const [value, setValue] = useState(() => readHashQuery(tab));

  useEffect(() => {
    const onHash = () => {
      setValue(readHashQuery(tab));
    };
    window.addEventListener('hashchange', onHash);
    return () => {
      window.removeEventListener('hashchange', onHash);
    };
  }, [tab]);

  const write = useCallback(
    (path: string, params: URLSearchParams) => {
      const query = params.toString();
      const hash =
        path === '' && query === '' ? tab : `${tab}/${encodeURIComponent(path)}${query === '' ? '' : `?${query}`}`;
      const url = new URL(window.location.href);
      url.hash = hash;
      if (url.href !== window.location.href) window.history.replaceState(window.history.state, '', url);
      setValue({ path, params });
    },
    [tab]
  );

  return [value, write];
}
