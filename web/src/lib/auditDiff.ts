// Before / after diff of audit details (fleet.AuditEntry.Detail).

export interface DiffRow {
  /** key is the dotted path of the changed value. */
  key: string;
  before: unknown;
  after: unknown;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function flatten(value: unknown, prefix: string, out: Map<string, unknown>): void {
  if (isPlainObject(value) && Object.keys(value).length > 0) {
    for (const [k, v] of Object.entries(value)) {
      flatten(v, prefix ? `${prefix}.${k}` : k, out);
    }
    return;
  }
  out.set(prefix, value);
}

/**
 * diffValues lists the leaves that differ between two values, by dotted
 * path, in the order they appear (before first, then keys only in after).
 */
export function diffValues(before: unknown, after: unknown): DiffRow[] {
  const a = new Map<string, unknown>();
  const b = new Map<string, unknown>();
  flatten(before, '', a);
  flatten(after, '', b);

  const rows: DiffRow[] = [];
  for (const key of new Set([...a.keys(), ...b.keys()])) {
    const va = a.get(key);
    const vb = b.get(key);
    if (JSON.stringify(va) !== JSON.stringify(vb)) rows.push({ key, before: va, after: vb });
  }
  return rows;
}

/** changeOf returns the before / after pair of an audit detail, if it has one. */
export function changeOf(detail: Record<string, unknown> | undefined): { before: unknown; after: unknown } | null {
  if (!detail || !('before' in detail) || !('after' in detail)) return null;
  return { before: detail.before, after: detail.after };
}

/** formatValue renders a diff leaf: strings bare, everything else as JSON; absent as "–". */
export function formatValue(v: unknown): string {
  if (v === undefined) return '–';
  if (typeof v === 'string') return v;
  return JSON.stringify(v);
}
