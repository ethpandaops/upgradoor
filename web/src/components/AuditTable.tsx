import { Fragment, useState } from 'react';

import type { AuditEntry } from '../api/types';
import { changeOf, diffValues, formatValue } from '../lib/auditDiff';
import { RelTime } from './RelTime';
import { Dash, Table, Td, Th } from './primitives';

/** AuditDetail renders an entry's detail: a before / after diff when it is a change, JSON otherwise. */
function AuditDetail({ detail }: { detail: Record<string, unknown> }) {
  const change = changeOf(detail);
  const rest = Object.fromEntries(Object.entries(detail).filter(([k]) => !change || (k !== 'before' && k !== 'after')));
  const rows = change ? diffValues(change.before, change.after) : [];

  return (
    <div className="flex flex-col gap-2">
      {change && (
        <table aria-label="Changes" className="w-fit border-collapse text-xs/5">
          <thead>
            <tr>
              <th scope="col" className="pr-4 text-left font-medium text-muted">
                Field
              </th>
              <th scope="col" className="pr-4 text-left font-medium text-muted">
                Before
              </th>
              <th scope="col" className="text-left font-medium text-muted">
                After
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && (
              <tr>
                <td colSpan={3} className="text-muted">
                  no difference
                </td>
              </tr>
            )}
            {rows.map(r => (
              <tr key={r.key}>
                <td className="pr-4 font-mono">{r.key}</td>
                <td className="pr-4 font-mono whitespace-pre-wrap text-danger">{formatValue(r.before)}</td>
                <td className="font-mono whitespace-pre-wrap text-success">{formatValue(r.after)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {Object.keys(rest).length > 0 && (
        <pre className="overflow-x-auto font-mono text-xs/5 whitespace-pre-wrap">{JSON.stringify(rest, null, 2)}</pre>
      )}
    </div>
  );
}

function detailPreview(detail: Record<string, unknown> | undefined): string {
  if (!detail) return '';
  const change = changeOf(detail);
  if (change) {
    const rows = diffValues(change.before, change.after);
    return rows.map(r => `${r.key}: ${formatValue(r.before)} → ${formatValue(r.after)}`).join(', ');
  }
  if (typeof detail.reason === 'string' && detail.reason !== '') return detail.reason;
  if (typeof detail.error === 'string' && detail.error !== '') return detail.error;
  return JSON.stringify(detail);
}

/** AuditTable lists audit entries: when, who, what, target and the detail (expandable). */
export function AuditTable({
  entries,
  label,
  showTarget = true,
}: {
  entries: readonly AuditEntry[];
  label: string;
  showTarget?: boolean;
}) {
  const [open, setOpen] = useState<ReadonlySet<number>>(new Set());

  const toggle = (id: number) => {
    setOpen(prev => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  return (
    <Table label={label}>
      <thead>
        <tr>
          <Th className="w-6">
            <span className="sr-only">Expand</span>
          </Th>
          <Th>When</Th>
          <Th>Who</Th>
          <Th>What</Th>
          {showTarget && <Th>Target</Th>}
          <Th className="w-full">Detail</Th>
        </tr>
      </thead>
      <tbody>
        {entries.map(e => {
          const isOpen = open.has(e.id);
          const hasDetail = e.detail !== undefined && Object.keys(e.detail).length > 0;
          return (
            <Fragment key={e.id}>
              <tr className="hover:bg-surface-hover">
                <Td>
                  {hasDetail && (
                    <button
                      type="button"
                      aria-expanded={isOpen}
                      aria-label={`${isOpen ? 'Collapse' : 'Expand'} entry ${e.id}`}
                      onClick={() => {
                        toggle(e.id);
                      }}
                      className="rounded-xs px-1 text-muted hover:bg-surface-hover hover:text-foreground"
                    >
                      {isOpen ? '▾' : '▸'}
                    </button>
                  )}
                </Td>
                <Td>
                  <RelTime fixed value={e.time} />
                </Td>
                <Td>{e.actor}</Td>
                <Td className="font-mono">{e.action}</Td>
                {showTarget && <Td className="font-mono">{e.target || <Dash />}</Td>}
                <Td className="max-w-md truncate text-muted" title={hasDetail ? detailPreview(e.detail) : undefined}>
                  {hasDetail ? detailPreview(e.detail) : <Dash />}
                </Td>
              </tr>
              {isOpen && e.detail && (
                <tr>
                  <td colSpan={showTarget ? 6 : 5} className="border-b border-border bg-background px-8 py-2">
                    <AuditDetail detail={e.detail} />
                  </td>
                </tr>
              )}
            </Fragment>
          );
        })}
      </tbody>
    </Table>
  );
}
