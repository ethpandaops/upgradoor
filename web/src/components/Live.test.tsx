import { act, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { ApiError } from '../api/client';
import { OutdatedError, type LiveStatus } from '../api/live';
import { Dialog } from './Dialog';
import { LiveNotice, STALE_AFTER_MS } from './Live';

const T0 = new Date(2026, 10, 23, 14, 5, 9).getTime();

function reconnecting(at: number): LiveStatus {
  return {
    state: 'reconnecting',
    error: new ApiError(0, 'controller unreachable'),
    lostAt: T0,
    waitingSince: at,
  };
}

describe('LiveNotice', () => {
  it('warns once the stream has been reconnecting for a while, and not after a snapshot', () => {
    vi.useFakeTimers({ now: T0 });
    const reload = vi.fn();
    const { rerender } = render(<LiveNotice status={reconnecting(T0)} reload={reload} />);

    act(() => {
      vi.advanceTimersByTime(STALE_AFTER_MS - 1);
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(1);
    });
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('Not live since 14:05:09 — showing the last known state.');
    expect(banner).toHaveTextContent('controller unreachable');

    rerender(<LiveNotice status={{ state: 'live', error: undefined }} reload={reload} />);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(reload).not.toHaveBeenCalled();
  });

  it('waits again after a pause instead of warning at once', () => {
    vi.useFakeTimers({ now: T0 });
    const paused: LiveStatus = { state: 'paused', error: undefined, lostAt: T0 };
    const { rerender } = render(<LiveNotice status={paused} reload={vi.fn()} />);

    act(() => {
      vi.advanceTimersByTime(5 * 60_000);
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    rerender(<LiveNotice status={reconnecting(Date.now())} reload={vi.fn()} />);
    act(() => {
      vi.advanceTimersByTime(STALE_AFTER_MS - 1);
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('reloads an outdated page when nothing is unsaved', () => {
    const reload = vi.fn();
    render(<LiveNotice status={{ state: 'outdated', error: new OutdatedError() }} reload={reload} />);

    expect(reload).toHaveBeenCalledOnce();
  });

  it('asks for the reload while a dialog is open', () => {
    const reload = vi.fn();
    render(
      <>
        <Dialog title="Confirm" onClose={vi.fn()} footer={null}>
          sure?
        </Dialog>
        <LiveNotice status={{ state: 'outdated', error: new OutdatedError() }} reload={reload} />
      </>
    );

    expect(reload).not.toHaveBeenCalled();
    expect(screen.getByRole('alert')).toHaveTextContent('upgradoor was updated. Reload to see current data.');

    screen.getByRole('button', { name: 'Reload' }).click();
    expect(reload).toHaveBeenCalledOnce();
  });
});
