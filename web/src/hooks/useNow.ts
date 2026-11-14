import { useEffect, useState } from 'react';

/** useNow returns the current time, refreshed every intervalMs while the document is visible. */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const timer = setInterval(() => {
      if (!document.hidden) setNow(Date.now());
    }, intervalMs);
    return () => {
      clearInterval(timer);
    };
  }, [intervalMs]);

  return now;
}
