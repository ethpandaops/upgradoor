import { describe, expect, it } from 'vitest';

import { validateImageRef } from './imageRef';

describe('an image reference for an override', () => {
  it('names a tag', () => {
    for (const ok of ['geth:v1', 'ethpandaops/geth:master', 'localhost:5000/geth:pr-1', 'ghcr.io/a/b:x']) {
      expect(validateImageRef(ok), ok).toBeNull();
    }
    for (const bad of [
      '',
      'geth',
      'ethpandaops/geth',
      'localhost:5000/geth',
      'geth:',
      'a/b@sha256:00',
      'a b:c',
      ':tag',
    ]) {
      expect(validateImageRef(bad), bad).not.toBeNull();
    }
  });
});
