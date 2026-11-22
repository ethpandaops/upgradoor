import { useEffect } from 'react';

const holders = new Set<symbol>();

/** useUnsavedWork marks, while active, that a reload of the page would lose the user's input. */
export function useUnsavedWork(active: boolean): void {
  useEffect(() => {
    if (!active) return;
    const holder = Symbol('unsaved');
    holders.add(holder);
    return () => {
      holders.delete(holder);
    };
  }, [active]);
}

/** hasUnsavedWork reports whether any mounted component holds unsaved input. */
export function hasUnsavedWork(): boolean {
  return holders.size > 0;
}
