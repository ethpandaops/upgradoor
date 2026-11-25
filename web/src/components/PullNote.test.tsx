import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { Lease } from '../api/types';
import { PullNote, pullNote } from './PullNote';

const MiB = 1024 * 1024;
const now = Date.parse('2026-01-01T00:10:00Z');

function pulling(pull?: NonNullable<Lease['operation']>['pull']): NonNullable<Lease['operation']> {
  return {
    leaseId: 'ls-1',
    phase: 'pulling',
    startedAt: '2026-01-01T00:02:30Z',
    updatedAt: '2026-01-01T00:09:30Z',
    steps: [],
    disrupted: false,
    pull,
  };
}

describe('pullNote', () => {
  it('names the image, the bytes and layers so far, the attempt and the time', () => {
    const note = pullNote(
      pulling({
        image: 'ethpandaops/debian-docker:latest',
        attempt: 2,
        bytesDone: 412 * MiB,
        bytesTotal: 933 * MiB,
        layersDone: 3,
        layersTotal: 7,
        startedAt: '2026-01-01T00:03:00Z',
        lastProgressAt: '2026-01-01T00:09:40Z',
      }),
      now
    );
    expect(note).toEqual({
      text: 'pulling ethpandaops/debian-docker:latest — 412 MiB of 933 MiB, 3/7 layers, attempt 2, 7m',
    });
  });

  it('says how long the daemon reported nothing once that is a while', () => {
    const note = pullNote(
      pulling({
        image: 'geth:master',
        attempt: 1,
        startedAt: '2026-01-01T00:05:00Z',
        lastProgressAt: '2026-01-01T00:08:00Z',
      }),
      now
    );
    expect(note).toEqual({ text: 'pulling geth:master — 5m', quiet: 'no progress for 2m' });
  });

  it('gives the time since the start for an agent that does not report its pull', () => {
    expect(pullNote(pulling(), now)).toEqual({ text: 'pulling for 7m 30s' });
  });

  it('is nothing outside the pulling phase', () => {
    expect(pullNote(undefined, now)).toBeUndefined();
    expect(pullNote({ ...pulling(), phase: 'applying' }, now)).toBeUndefined();
  });
});

describe('PullNote', () => {
  it('marks a quiet pull', () => {
    render(
      <PullNote
        operation={pulling({
          image: 'geth:master',
          startedAt: new Date(Date.now() - 300_000).toISOString(),
          lastProgressAt: new Date(Date.now() - 120_000).toISOString(),
        })}
      />
    );
    expect(screen.getByText(/^pulling geth:master — 5m/)).toBeInTheDocument();
    expect(screen.getByText(/no progress for 2m/)).toHaveClass('text-warning');
  });
});
