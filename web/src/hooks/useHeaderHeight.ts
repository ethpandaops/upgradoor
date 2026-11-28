import { useEffect, useState } from 'react';

/**
 * useHeaderHeight is the height of the page's sticky header, followed as it
 * wraps, so a sticky element below it can sit right under it.
 */
export function useHeaderHeight(): number {
  const [height, setHeight] = useState(0);

  useEffect(() => {
    const header = document.querySelector('header');
    if (!header) return;

    const measure = () => {
      setHeight(header.getBoundingClientRect().height);
    };
    measure();

    if (typeof ResizeObserver !== 'function') return;
    const observer = new ResizeObserver(measure);
    observer.observe(header);
    return () => {
      observer.disconnect();
    };
  }, []);

  return height;
}
