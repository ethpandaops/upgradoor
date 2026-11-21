import { useCallback, useState } from 'react';

// State that outlives its component: a tab is unmounted when another one is
// shown, and what was typed and where the user was should still be there
// when it is shown again. It lasts as long as the page.
const kept = new Map<string, unknown>();

/** resetKept forgets everything (tests). */
export function resetKept(): void {
  kept.clear();
}

/** setKept puts a value where the component that keeps it will find it when it is shown next. */
export function setKept(key: string, value: unknown): void {
  kept.set(key, value);
}

/** useKept is useState for a value that survives the component under its key. */
export function useKept<T>(key: string, initial: T): [T, (next: T | ((prev: T) => T)) => void] {
  const [value, setValue] = useState<T>(() => (kept.has(key) ? (kept.get(key) as T) : initial));

  const set = useCallback(
    (next: T | ((prev: T) => T)) => {
      setValue(prev => {
        const resolved = typeof next === 'function' ? (next as (prev: T) => T)(prev) : next;
        kept.set(key, resolved);
        return resolved;
      });
    },
    [key]
  );

  return [value, set];
}
