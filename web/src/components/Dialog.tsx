import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode } from 'react';

import { useUnsavedWork } from '../hooks/useUnsavedWork';

const FOCUSABLE =
  'button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), a[href]';

/**
 * Dialog is a modal: it takes the focus, keeps Tab inside, closes on Escape
 * and on a click on the backdrop, and gives the focus back when it closes.
 */
export function Dialog({
  title,
  onClose,
  children,
  footer,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer: ReactNode;
}) {
  const titleId = useId();
  const panelRef = useRef<HTMLDivElement>(null);
  useUnsavedWork(true);

  useEffect(() => {
    const previous = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const panel = panelRef.current;
    const first = panel?.querySelector<HTMLElement>('[data-autofocus]') ?? panel?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? panel)?.focus();

    return () => {
      previous?.focus();
    };
  }, []);

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      onClose();
      return;
    }

    if (e.key !== 'Tab' || !panelRef.current) return;

    // Sorted explicitly: not every DOM implementation returns the matches of
    // a selector list in document order.
    const items = Array.from(panelRef.current.querySelectorAll<HTMLElement>(FOCUSABLE)).sort((a, b) =>
      a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING ? -1 : 1
    );
    const first = items[0];
    const last = items[items.length - 1];
    if (!first || !last) return;

    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-scrim p-4 pt-16"
      onMouseDown={e => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        onKeyDown={onKeyDown}
        className="flex w-full max-w-xl flex-col rounded-md border border-border-strong bg-surface shadow-lg"
      >
        <h2 id={titleId} className="border-b border-border px-4 py-2 text-sm/6 font-semibold">
          {title}
        </h2>
        <div className="flex flex-col gap-3 px-4 py-3 text-sm/6">{children}</div>
        <div className="flex flex-wrap justify-end gap-2 border-t border-border px-4 py-2">{footer}</div>
      </div>
    </div>
  );
}
