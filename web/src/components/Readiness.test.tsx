import { act, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { Readiness, readinessNote } from './Readiness';
import { NowProvider } from './RelTime';

const waiting = 'node at head with peers, waiting for it to stay so for 5m0s';

describe('Readiness', () => {
  it('counts down to readyAt every second, then says the check is pending', () => {
    vi.useFakeTimers();
    vi.setSystemTime(Date.parse('2026-01-01T00:00:00Z'));
    render(
      <NowProvider>
        <p data-testid="note">
          <Readiness lease={{ readiness: waiting, readyAt: '2026-01-01T00:04:12Z' }} />
        </p>
      </NowProvider>
    );

    const note = screen.getByTestId('note');
    expect(note).toHaveTextContent('node at head with peers, ready in 4m 12s');

    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(note).toHaveTextContent('node at head with peers, ready in 4m 11s');

    act(() => {
      vi.advanceTimersByTime(251_000);
    });
    expect(note).toHaveTextContent('node at head with peers, ready check pending');
  });

  it('shows the note as it is without readyAt', () => {
    render(<Readiness lease={{ readiness: 'validators attesting: 1 of 2 slots since 23:17:40' }} />);
    expect(screen.getByText('validators attesting: 1 of 2 slots since 23:17:40')).toBeInTheDocument();
  });

  it('drops the short waiting tail of a host without a probe', () => {
    const now = Date.parse('2026-01-01T00:00:00Z');
    expect(
      readinessNote({ readiness: 'no node probe configured, waiting 1m0s', readyAt: '2026-01-01T00:00:30Z' }, now)
    ).toBe('no node probe configured, ready in 30s');
    expect(readinessNote({}, now)).toBeUndefined();
  });
});
