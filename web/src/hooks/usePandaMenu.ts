import { useEffect, useState } from 'react';

// The ethpandaops panda menu (global-panda-menu) is a script injected into
// the page by the ingress. It mounts itself as #panda-menu-root and, in its
// default mode, floats a 44px button 16px from the top-left corner.
const ROOT_ID = 'panda-menu-root';

function present(): boolean {
  const w = window as { PandaMenu?: { initialized?: boolean; menuMode?: string } };
  const mode = w.PandaMenu?.menuMode;
  if (mode === 'hidden' || mode === 'sidebar') return false;
  return document.getElementById(ROOT_ID) !== null || w.PandaMenu?.initialized === true;
}

/**
 * usePandaMenu reports whether the panda menu's floating button is (or is
 * about to be) on the page, so the header can keep its corner free. The
 * script may load after the page, so the body is watched for its root.
 */
export function usePandaMenu(): boolean {
  const [shown, setShown] = useState(present);

  useEffect(() => {
    if (typeof MutationObserver === 'undefined') return;

    const observer = new MutationObserver(() => {
      setShown(present());
    });
    observer.observe(document.body, { childList: true });
    setShown(present());

    return () => {
      observer.disconnect();
    };
  }, []);

  return shown;
}
