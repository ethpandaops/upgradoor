import '@testing-library/jest-dom/vitest';

import { cleanup } from '@testing-library/react';
import { afterEach } from 'vitest';

import { resetKept } from '../hooks/useKept';

afterEach(() => {
  cleanup();
  resetKept();
  window.location.hash = '';
  delete window.ethpandaops;
  localStorage.clear();
  sessionStorage.clear();
});
