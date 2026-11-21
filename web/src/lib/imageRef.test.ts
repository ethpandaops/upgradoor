import { describe, expect, it } from 'vitest';

import { imageAllowed, validateImageRef } from './imageRef';

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

  it('is allowed when its repository matches a pattern of the host', () => {
    expect(imageAllowed('ethpandaops/geth:pr-1', ['ethpandaops/geth'])).toBe(true);
    expect(imageAllowed('docker.io/ethpandaops/geth:pr-1', ['ethpandaops/geth'])).toBe(true);
    expect(imageAllowed('ethpandaops/geth:pr-1', ['docker.io/ethpandaops/*'])).toBe(true);
    expect(imageAllowed('nginx:1', ['docker.io/library/nginx'])).toBe(true);
    expect(imageAllowed('ghcr.io/ethereum/go-ethereum:x', ['ghcr.io/ethereum/*'])).toBe(true);

    // The star does not cross a "/"; another registry is another repository.
    expect(imageAllowed('ghcr.io/ethereum/a/b:x', ['ghcr.io/ethereum/*'])).toBe(false);
    expect(imageAllowed('ghcr.io/ethpandaops/geth:x', ['ethpandaops/geth'])).toBe(false);
    expect(imageAllowed('ethpandaops/geth-fork:x', ['ethpandaops/geth'])).toBe(false);
    expect(imageAllowed('ethpandaops/geth:x', [])).toBe(false);
  });
});
