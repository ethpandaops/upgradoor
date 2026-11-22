import '@testing-library/jest-dom/vitest';

import { cleanup } from '@testing-library/react';
import { afterEach, vi } from 'vitest';

import { resetKept } from '../hooks/useKept';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  resetKept();
  window.location.hash = '';
  delete window.ethpandaops;
  localStorage.clear();
  sessionStorage.clear();
});
