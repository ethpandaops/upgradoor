import { useEffect, useRef, useState, type RefObject } from 'react';

/** useWidth measures an element's content width and follows its resizes. */
export function useWidth<T extends HTMLElement>(fallback: number): [RefObject<T | null>, number] {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(fallback);

  useEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === 'undefined') return;

    const observer = new ResizeObserver(entries => {
      const w = entries[0]?.contentRect.width;
      if (w && w > 0) setWidth(Math.round(w));
    });
    observer.observe(el);

    return () => {
      observer.disconnect();
    };
  }, []);

  return [ref, width];
}
