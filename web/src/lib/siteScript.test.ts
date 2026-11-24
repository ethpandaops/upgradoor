import { afterEach, describe, expect, it } from 'vitest';

import { includeSiteScript } from './siteScript';

const SRC = 'https://menu.example.org/menu-1.0.js';
const INTEGRITY = 'sha384-abc';

describe('includeSiteScript', () => {
  afterEach(() => {
    document.head.querySelectorAll('script').forEach(s => {
      s.remove();
    });
  });

  it('adds the script with its integrity once', () => {
    includeSiteScript({ uiScript: SRC, uiScriptIntegrity: INTEGRITY });
    includeSiteScript({ uiScript: SRC, uiScriptIntegrity: INTEGRITY });

    const scripts = document.head.querySelectorAll('script');
    expect(scripts).toHaveLength(1);
    expect(scripts[0]?.getAttribute('src')).toBe(SRC);
    expect(scripts[0]?.getAttribute('integrity')).toBe(INTEGRITY);
    expect(scripts[0]?.getAttribute('crossorigin')).toBe('anonymous');
  });

  it('adds nothing without a script', () => {
    includeSiteScript({});
    expect(document.head.querySelectorAll('script')).toHaveLength(0);
  });
});
