import { useMemo, useState } from 'react';

import { errorText } from '../api/client';
import { list, type Audit, type AuditEntry } from '../api/types';
import { useApp } from '../appContext';
import { AuditTable } from '../components/AuditTable';
import { Button, Empty, ErrorBanner, INPUT_CLASS, Loading, cx } from '../components/primitives';
import { useDebounced } from '../hooks/useDebounced';
import { usePoll } from '../hooks/usePoll';
import { plural } from '../lib/format';

const PAGE_SIZE = 50;

/** Action prefixes offered in the filter (fleet.AuditEntry.Action). */
const ACTION_PREFIXES = ['action.', 'settings.', 'rollout.', 'lease.', 'host.'];

/** Everything loaded for one filter: the entries seen so far and the paging cursor. */
interface Loaded {
  key: string;
  entries: ReadonlyMap<number, AuditEntry>;
  /** nextBefore pages backwards from the oldest loaded page; 0 at the end. */
  nextBefore: number;
}

function merge(prev: Loaded, key: string, page: Audit, older: boolean): Loaded {
  const fresh = prev.key !== key;
  const entries = new Map(fresh ? [] : prev.entries);
  for (const e of list(page.entries)) entries.set(e.id, e);

  // Only the first page of a filter and explicit "older" loads move the cursor;
  // a refresh of the newest page must not skip what lies between.
  const nextBefore = fresh || older ? page.nextBefore : prev.nextBefore;

  return { key, entries, nextBefore };
}

/**
 * AuditTab shows the audit trail: who did what, when, to which target, with
 * the before / after of changes. The newest page refreshes by itself; older
 * pages are loaded on demand through nextBefore.
 */
export function AuditTab() {
  const { api } = useApp();

  const [targetInput, setTargetInput] = useState('');
  const [actionInput, setActionInput] = useState('');
  const target = useDebounced(targetInput.trim(), 300);
  const action = useDebounced(actionInput.trim(), 300);
  const key = `audit/${target}/${action}`;

  const [loaded, setLoaded] = useState<Loaded>({ key: '', entries: new Map(), nextBefore: 0 });
  const [olderBusy, setOlderBusy] = useState(false);
  const [olderError, setOlderError] = useState<unknown>(undefined);

  const newest = usePoll(
    async s => {
      const page = await api.audit({ target, action, limit: PAGE_SIZE }, s);
      setLoaded(prev => merge(prev, key, page, false));
      return page;
    },
    { key }
  );

  const current = loaded.key === key;
  const entries = useMemo(
    () => (current ? [...loaded.entries.values()].sort((a, b) => b.id - a.id) : []),
    [current, loaded.entries]
  );
  const nextBefore = current ? loaded.nextBefore : 0;

  const loadOlder = async () => {
    if (!current || nextBefore === 0 || olderBusy) return;

    setOlderBusy(true);
    setOlderError(undefined);
    try {
      const page = await api.audit({ target, action, before: nextBefore, limit: PAGE_SIZE });
      setLoaded(prev => (prev.key === key ? merge(prev, key, page, true) : prev));
    } catch (err) {
      setOlderError(err);
    } finally {
      setOlderBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <input
          type="search"
          aria-label="Filter by target"
          placeholder="Target: host, rollout id or setting"
          value={targetInput}
          onChange={e => {
            setTargetInput(e.target.value);
          }}
          className={cx(INPUT_CLASS, 'w-64')}
        />
        <input
          type="search"
          aria-label="Filter by action"
          placeholder="Action prefix, e.g. action. or lease.failed"
          list="audit-action-prefixes"
          value={actionInput}
          onChange={e => {
            setActionInput(e.target.value);
          }}
          className={cx(INPUT_CLASS, 'w-64')}
        />
        <datalist id="audit-action-prefixes">
          {ACTION_PREFIXES.map(p => (
            <option key={p} value={p} />
          ))}
        </datalist>
        {(targetInput !== '' || actionInput !== '') && (
          <Button
            dense
            variant="ghost"
            onClick={() => {
              setTargetInput('');
              setActionInput('');
            }}
          >
            Clear filters
          </Button>
        )}
        {current && <span className="text-xs/5 text-muted">{plural(entries.length, 'entry', 'entries')} loaded</span>}
      </div>

      {newest.error !== undefined && <ErrorBanner error={newest.error} what="the audit log" onRetry={newest.refresh} />}

      {!current ? (
        newest.error === undefined && <Loading what="the audit log" />
      ) : entries.length === 0 ? (
        <Empty>
          {target !== '' || action !== '' ? 'No audit entry matches the filters.' : 'The audit log is empty.'}
        </Empty>
      ) : (
        <AuditTable entries={entries} label="Audit log" />
      )}

      {olderError !== undefined && (
        <p role="alert" className="text-xs/5 text-danger">
          Could not load older entries: {errorText(olderError)}
        </p>
      )}

      {current && entries.length > 0 && (
        <div className="flex items-center gap-2">
          {nextBefore !== 0 ? (
            <Button onClick={() => void loadOlder()} disabled={olderBusy}>
              {olderBusy ? 'Loading…' : 'Load older entries'}
            </Button>
          ) : (
            <span className="text-xs/5 text-muted">End of the audit log.</span>
          )}
        </div>
      )}
    </div>
  );
}
