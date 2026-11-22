import { useCallback, useEffect, useState, type CSSProperties, type PointerEvent, type ReactNode } from 'react';

import { IconButton } from './icons';

/** Keys typed into these belong to the field, not the drawer. */
const TYPING =
  'input:not([type="checkbox"]):not([type="radio"]), textarea, select, [contenteditable="true"], [role="menu"]';

const WIDTH_KEY = 'upgradoor.drawerWidth';
const DEFAULT_WIDTH = 520;
const MIN_WIDTH = 360;
/** Page width the drawer always leaves uncovered. */
const MIN_PAGE = 240;
const KEY_STEP = 32;

function clampWidth(w: number): number {
  return Math.round(Math.max(MIN_WIDTH, Math.min(w, window.innerWidth - MIN_PAGE)));
}

function readWidth(): number {
  try {
    const v = Number(localStorage.getItem(WIDTH_KEY));
    return Number.isFinite(v) && v > 0 ? v : DEFAULT_WIDTH;
  } catch {
    return DEFAULT_WIDTH;
  }
}

function storeWidth(w: number): void {
  try {
    if (w === DEFAULT_WIDTH) localStorage.removeItem(WIDTH_KEY);
    else localStorage.setItem(WIDTH_KEY, String(w));
  } catch {
    // Storage unavailable: the width lasts for this page only.
  }
}

/** useDrawerWidth is the drawer width on wide screens, persisted per browser. */
function useDrawerWidth(): [number, (w: number, persist: boolean) => void] {
  const [width, setWidth] = useState(readWidth);

  const set = useCallback((w: number, persist: boolean) => {
    const next = clampWidth(w);
    setWidth(next);
    if (persist) storeWidth(next);
  }, []);

  return [width, set];
}

/**
 * Drawer is a panel at the right edge over the page, which stays usable
 * behind it. Escape closes it; ArrowUp / ArrowDown step to the previous or
 * next item of the list behind it. Keys are left alone while a modal dialog
 * is open or a field has the focus. On wide screens its left edge resizes
 * it; the width is kept in localStorage.
 */
export function Drawer({
  label,
  header,
  onClose,
  onPrevious,
  onNext,
  children,
}: {
  label: string;
  header: ReactNode;
  onClose: () => void;
  onPrevious?: () => void;
  onNext?: () => void;
  children: ReactNode;
}) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.altKey || e.ctrlKey || e.metaKey) return;
      if (document.querySelector('[aria-modal="true"]')) return;
      if (e.target instanceof Element && e.target.closest(TYPING)) return;

      const run =
        e.key === 'Escape' ? onClose : e.key === 'ArrowUp' ? onPrevious : e.key === 'ArrowDown' ? onNext : undefined;
      if (!run) return;
      e.preventDefault();
      run();
    };

    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('keydown', onKey);
    };
  }, [onClose, onPrevious, onNext]);

  const [width, setWidth] = useDrawerWidth();
  const [dragging, setDragging] = useState(false);

  const fromPointer = (e: PointerEvent<HTMLDivElement>) => window.innerWidth - e.clientX;

  return (
    <aside
      aria-label={label}
      style={{ '--drawer-w': `${clampWidth(width)}px` } as CSSProperties}
      className={`fixed inset-y-0 right-0 z-40 flex w-full flex-col border-l border-border-strong bg-surface shadow-lg sm:w-(--drawer-w) ${dragging ? 'select-none' : ''}`}
    >
      <div
        role="separator"
        aria-orientation="vertical"
        aria-label="Resize (← →, double-click resets)"
        aria-valuenow={clampWidth(width)}
        aria-valuemin={MIN_WIDTH}
        tabIndex={0}
        title="Drag to resize, double-click to reset"
        className={`absolute inset-y-0 left-0 hidden w-2 -translate-x-1/2 cursor-col-resize hover:bg-accent/40 focus-visible:bg-accent/40 focus-visible:outline-hidden sm:block ${dragging ? 'bg-accent/40' : ''}`}
        onPointerDown={e => {
          if (e.button !== 0) return;
          e.preventDefault();
          e.currentTarget.setPointerCapture(e.pointerId);
          setDragging(true);
        }}
        onPointerMove={e => {
          if (dragging) setWidth(fromPointer(e), false);
        }}
        onPointerUp={e => {
          if (!dragging) return;
          setDragging(false);
          setWidth(fromPointer(e), true);
        }}
        onPointerCancel={() => {
          setDragging(false);
          setWidth(width, true);
        }}
        onDoubleClick={() => {
          setWidth(DEFAULT_WIDTH, true);
        }}
        onKeyDown={e => {
          if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
          e.preventDefault();
          setWidth(width + (e.key === 'ArrowLeft' ? KEY_STEP : -KEY_STEP), true);
        }}
      />
      <div className="flex items-start gap-2 border-b border-border px-4 pt-3 pb-2">
        <div className="flex min-w-0 grow flex-col gap-1.5">{header}</div>
        <span className="flex shrink-0 items-center">
          <IconButton
            icon="chevron"
            label="Previous (↑)"
            disabled={!onPrevious}
            onClick={() => onPrevious?.()}
            className="[&_svg]:rotate-180"
          />
          <IconButton icon="chevron" label="Next (↓)" disabled={!onNext} onClick={() => onNext?.()} />
          <IconButton icon="close" label="Close (Esc)" onClick={onClose} />
        </span>
      </div>
      <div className="flex min-h-0 grow flex-col">{children}</div>
    </aside>
  );
}
