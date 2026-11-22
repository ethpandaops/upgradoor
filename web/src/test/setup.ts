import '@testing-library/jest-dom/vitest';

import { cleanup } from '@testing-library/react';
import { afterEach, vi } from 'vitest';

import { resetKept } from '../hooks/useKept';

// jsdom has no layout: scrolling is a no-op.
Element.prototype.scrollIntoView = () => undefined;

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  resetKept();
  window.location.hash = '';
  window.history.replaceState(null, '', '/');
  delete window.ethpandaops;
  localStorage.clear();
  sessionStorage.clear();
});
