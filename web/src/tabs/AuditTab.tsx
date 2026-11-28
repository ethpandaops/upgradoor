import { useCallback, useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';

import { errorText } from '../api/client';
import { list, type Audit, type AuditActors, type AuditEntry } from '../api/types';
import { useApp } from '../appContext';
import { AuditList, rendererOf } from '../components/audit/AuditList';
import { Icon } from '../components/icons';
import { useLive } from '../components/Live';
import { Button, Empty, ErrorBanner, INPUT_CLASS, Loading, Segmented, cx } from '../components/primitives';
import { useDebounced } from '../hooks/useDebounced';
import { useHashQuery } from '../hooks/useHashQuery';
import { useQuery } from '../hooks/useQuery';
import {
  AUDIT_ACTIONS,
  AUDIT_GROUPS,
  DEFAULT_FILTER,
  GROUP_LABEL,
  TARGET_KINDS,
  TARGET_KIND_LABEL,
  WHO_OPTIONS,
  apiQuery,
  filterFromParams,
  filterToParams,
  isFiltered,
  type AuditFilter,
  type AuditGroup,
  type AuditTargetKind,
} from '../lib/auditCatalog';
import { plural } from '../lib/format';

const PAGE_SIZE = 50;

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

/** The "from" of a quick range, as a datetime-local value. */
function localAgo(ms: number): string {
  const d = new Date(Date.now() - ms);
  const two = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${two(d.getMonth() + 1)}-${two(d.getDate())}T${two(d.getHours())}:${two(d.getMinutes())}`;
}

const QUICK_RANGES = [
  { label: '1 h', ms: 3600_000 },
  { label: '24 h', ms: 86400_000 },
  { label: '7 d', ms: 7 * 86400_000 },
];

/** TextFilter is a text field that commits its value once typing pauses, and follows the filter when it changes elsewhere. */
function TextFilter({
  value,
  onCommit,
  ...rest
}: {
  value: string;
  onCommit: (value: string) => void;
  label: string;
  placeholder: string;
  list?: string;
  type?: 'search' | 'text';
  className?: string;
}) {
  const [input, setInput] = useState(value);
  const debounced = useDebounced(input, 300);
  const committed = useRef(value);

  useEffect(() => {
    if (value !== committed.current) {
      committed.current = value;
      setInput(value);
    }
  }, [value]);

  useEffect(() => {
    const next = debounced.trim();
    if (next !== committed.current) {
      committed.current = next;
      onCommit(next);
    }
  }, [debounced, onCommit]);

  return (
    <input
      type={rest.type ?? 'search'}
      aria-label={rest.label}
      placeholder={rest.placeholder}
      list={rest.list}
      value={input}
      onChange={e => {
        setInput(e.target.value);
      }}
      className={cx(INPUT_CLASS, 'w-full max-sm:h-10', rest.className)}
    />
  );
}

function Field({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <label className={cx('flex min-w-0 flex-col gap-1 text-xs/5 text-muted', className)}>
      {label}
      {children}
    </label>
  );
}

const SELECT_CLASS = cx(INPUT_CLASS, 'w-full max-sm:h-10');

/** Chip is one active filter with a button that drops it. */
function Chip({ label, onClear }: { label: string; onClear: () => void }) {
  return (
    <span className="inline-flex max-w-full items-center gap-1 rounded-sm border border-border-strong bg-surface py-px pr-0.5 pl-1.5 text-xs/5">
      <span className="truncate">{label}</span>
      <button
        type="button"
        aria-label={`Remove filter ${label}`}
        onClick={onClear}
        className="rounded-xs p-0.5 text-muted hover:bg-surface-hover hover:text-foreground"
      >
        <Icon name="close" className="size-3" />
      </button>
    </span>
  );
}

/**
 * AuditTab shows the audit trail, filtered on the controller: by who acted
 * (people by default, so the scheduler's many entries stay out of the way),
 * a person, a target or kind of target, a group of actions or one action, a
 * time range and a text. The filter is kept in the URL hash, so a host, a
 * rollout or the settings can link their history. The newest page is fetched
 * again when the event stream reports a new entry; older pages are loaded on
 * demand through nextBefore.
 */
export function AuditTab() {
  const { api } = useApp();
  const ids = useId();

  const [hash, writeHash] = useHashQuery('audit');
  const filter = useMemo(() => filterFromParams(hash?.params ?? new URLSearchParams()), [hash]);
  const filterRef = useRef(filter);
  useEffect(() => {
    filterRef.current = filter;
  });
  const update = useCallback(
    (patch: Partial<AuditFilter>) => {
      writeHash('', filterToParams({ ...filterRef.current, ...patch }));
    },
    [writeHash]
  );
  // An actor or target picked from an entry shows all of its history.
  const narrow = useCallback(
    (patch: Partial<AuditFilter>) => {
      update({ ...patch, who: 'all' });
    },
    [update]
  );

  const query = useMemo(() => apiQuery(filter), [filter]);
  const key = `audit?${new URLSearchParams(query).toString()}`;

  const [loaded, setLoaded] = useState<Loaded>({ key: '', entries: new Map(), nextBefore: 0 });
  const [actors, setActors] = useState<AuditActors | undefined>(undefined);
  const [olderBusy, setOlderBusy] = useState(false);
  const [olderError, setOlderError] = useState<unknown>(undefined);

  const newest = useQuery(
    async s => {
      const page = await api.audit({ ...query, limit: PAGE_SIZE, actors: true }, s);
      setLoaded(prev => merge(prev, key, page, false));
      if (page.actors) setActors(page.actors);
      return page;
    },
    { key }
  );

  // The stream says when the log has a new entry; the newest page is then
  // asked for again. Nothing is asked for while the log stands still.
  const latestId = useLive('audit').data?.latestId;
  const seenId = useRef(latestId);
  const refreshNewest = newest.refresh;
  useEffect(() => {
    if (latestId === seenId.current) return;
    const first = seenId.current === undefined;
    seenId.current = latestId;
    // The first value arrives with the page that is being loaded anyway.
    if (!first) refreshNewest();
  }, [latestId, refreshNewest]);

  const current = loaded.key === key;
  // While another filter loads, the entries of the last one stay, dimmed.
  const entries = useMemo(() => [...loaded.entries.values()].sort((a, b) => b.id - a.id), [loaded.entries]);
  const nextBefore = current ? loaded.nextBefore : 0;

  const loadOlder = async () => {
    if (!current || nextBefore === 0 || olderBusy) return;

    setOlderBusy(true);
    setOlderError(undefined);
    try {
      const page = await api.audit({ ...query, before: nextBefore, limit: PAGE_SIZE });
      setLoaded(prev => (prev.key === key ? merge(prev, key, page, true) : prev));
    } catch (err) {
      setOlderError(err);
    } finally {
      setOlderBusy(false);
    }
  };

  // Suggestions for the target: the hosts and rollouts the page knows.
  const hosts = useLive('hosts').data;
  const rollouts = useLive('rollouts').data;
  const targets = useMemo(() => {
    const out: { value: string; label?: string }[] = [{ value: 'settings' }, { value: 'validator-ranges' }];
    for (const r of [...list(rollouts?.open), ...list(rollouts?.recent)]) out.push({ value: r.id, label: r.title });
    for (const h of list(hosts?.hosts)) out.push({ value: h.name });
    return out;
  }, [hosts, rollouts]);

  const people = list(actors?.people);
  const peopleCount = people.reduce((n, p) => n + p.entries, 0);
  const whoOptions = WHO_OPTIONS.map(o => ({
    ...o,
    label:
      o.value === 'people' && actors
        ? `${o.label} ${peopleCount.toLocaleString()}`
        : o.value === 'agents' && actors
          ? `${o.label} ${actors.agents.toLocaleString()}`
          : o.label,
  }));

  const groupActions = AUDIT_ACTIONS.filter(a => filter.group === '' || a.group === filter.group);
  const actorOptions =
    people.some(p => p.actor === filter.actor) || filter.actor === ''
      ? people
      : [{ actor: filter.actor, entries: 0 }, ...people];

  const chips: { label: string; clear: Partial<AuditFilter> }[] = [];
  if (filter.actor) chips.push({ label: `By ${filter.actor}`, clear: { actor: '' } });
  if (filter.target)
    chips.push({
      label: `About ${targets.find(t => t.value === filter.target)?.label ?? filter.target}`,
      clear: { target: '' },
    });
  if (filter.kind) chips.push({ label: `${TARGET_KIND_LABEL[filter.kind]} targets`, clear: { kind: '' } });
  if (filter.group) chips.push({ label: GROUP_LABEL[filter.group], clear: { group: '', action: '' } });
  if (filter.action)
    chips.push({
      label: rendererOf(filter.action).title({ id: 0, time: '', actor: '', action: filter.action }),
      clear: { action: '' },
    });
  if (filter.from) chips.push({ label: `From ${filter.from.replace('T', ' ')}`, clear: { from: '' } });
  if (filter.to) chips.push({ label: `Until ${filter.to.replace('T', ' ')}`, clear: { to: '' } });
  if (filter.q) chips.push({ label: `“${filter.q}”`, clear: { q: '' } });

  const narrowed = chips.length > 0 || filter.who !== 'all';

  return (
    <div className="mx-auto flex w-full max-w-6xl flex-col gap-3">
      <section
        aria-label="Filters"
        className="flex flex-col gap-3 rounded-lg border border-border bg-surface p-3 sm:p-4"
      >
        <div className="flex flex-wrap items-center gap-2">
          <div className="max-sm:w-full max-sm:[&>div]:flex max-sm:[&>div]:w-full max-sm:[&_button]:flex-1">
            <Segmented
              label="Who acted"
              options={whoOptions}
              value={filter.who}
              onChange={who => {
                update({ who, actor: who === 'people' || who === 'all' ? filter.actor : '' });
              }}
            />
          </div>
          <div className="min-w-0 flex-1 basis-60">
            <TextFilter
              label="Search"
              placeholder="Search actor, action and target…"
              value={filter.q}
              onCommit={q => {
                update({ q });
              }}
            />
          </div>
        </div>

        <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
          <Field label="Kind of action">
            <select
              className={SELECT_CLASS}
              value={filter.group}
              onChange={e => {
                const group = e.target.value as AuditGroup | '';
                const keep =
                  filter.action !== '' &&
                  (group === '' || AUDIT_ACTIONS.some(a => a.action === filter.action && a.group === group));
                update({ group, action: keep ? filter.action : '' });
              }}
            >
              <option value="">Any</option>
              {AUDIT_GROUPS.map(g => (
                <option key={g} value={g}>
                  {GROUP_LABEL[g]}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Action">
            <select
              className={SELECT_CLASS}
              value={filter.action}
              onChange={e => {
                update({ action: e.target.value });
              }}
            >
              <option value="">Any</option>
              {!groupActions.some(a => a.action === filter.action) && filter.action !== '' && (
                <option value={filter.action}>{filter.action}</option>
              )}
              {groupActions.map(a => (
                <option key={a.action} value={a.action}>
                  {rendererOf(a.action).title({ id: 0, time: '', actor: '', action: a.action })}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Kind of target">
            <select
              className={SELECT_CLASS}
              value={filter.kind}
              onChange={e => {
                update({ kind: e.target.value as AuditTargetKind | '' });
              }}
            >
              <option value="">Any</option>
              {TARGET_KINDS.map(k => (
                <option key={k} value={k}>
                  {TARGET_KIND_LABEL[k]}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Target">
            <TextFilter
              type="text"
              label="Target"
              placeholder="Host, rollout or settings"
              list={`${ids}-targets`}
              value={filter.target}
              onCommit={target => {
                if (target === '') update({ target });
                else narrow({ target });
              }}
            />
            <datalist id={`${ids}-targets`}>
              {targets.map(t => (
                <option key={t.value} value={t.value}>
                  {t.label}
                </option>
              ))}
            </datalist>
          </Field>
          <Field label="Person">
            <select
              className={SELECT_CLASS}
              value={filter.actor}
              disabled={filter.who === 'scheduler'}
              onChange={e => {
                update({ actor: e.target.value });
              }}
            >
              <option value="">Anyone</option>
              {actorOptions.map(p => (
                <option key={p.actor} value={p.actor}>
                  {p.entries > 0 ? `${p.actor} (${p.entries.toLocaleString()})` : p.actor}
                </option>
              ))}
            </select>
          </Field>
          <div className="col-span-2 flex flex-col gap-1 text-xs/5 text-muted sm:col-span-1">
            <span id={`${ids}-time`}>Time</span>
            <div role="group" aria-labelledby={`${ids}-time`} className="flex flex-wrap gap-1">
              {QUICK_RANGES.map(r => (
                <Button
                  key={r.label}
                  dense
                  className="max-sm:h-10 max-sm:flex-1"
                  onClick={() => {
                    update({ from: localAgo(r.ms), to: '' });
                  }}
                >
                  {r.label}
                </Button>
              ))}
            </div>
          </div>
          <Field label="From">
            <input
              type="datetime-local"
              className={SELECT_CLASS}
              value={filter.from}
              onChange={e => {
                update({ from: e.target.value });
              }}
            />
          </Field>
          <Field label="Until">
            <input
              type="datetime-local"
              className={SELECT_CLASS}
              value={filter.to}
              onChange={e => {
                update({ to: e.target.value });
              }}
            />
          </Field>
        </div>

        {(chips.length > 0 || isFiltered(filter)) && (
          <div className="flex flex-wrap items-center gap-1.5">
            {chips.map(c => (
              <Chip
                key={c.label}
                label={c.label}
                onClear={() => {
                  update(c.clear);
                }}
              />
            ))}
            <Button
              dense
              variant="ghost"
              onClick={() => {
                writeHash('', filterToParams(DEFAULT_FILTER));
              }}
            >
              Reset
            </Button>
          </div>
        )}
      </section>

      {newest.error !== undefined && <ErrorBanner error={newest.error} what="the audit log" onRetry={newest.refresh} />}

      {loaded.key === '' ? (
        newest.error === undefined && <Loading what="the audit log" />
      ) : current && entries.length === 0 ? (
        <Empty>
          <span className="flex flex-col items-center gap-2">
            {narrowed ? 'No audit entry matches the filters.' : 'The audit log is empty.'}
            {filter.who !== 'all' && (
              <Button
                onClick={() => {
                  update({ who: 'all' });
                }}
              >
                Show everyone's entries
              </Button>
            )}
          </span>
        </Empty>
      ) : (
        <div className={cx('flex flex-col gap-2 transition-opacity', !current && 'opacity-50')} aria-busy={!current}>
          <div className="flex items-center gap-2 text-xs/5 text-muted">
            <span>{plural(entries.length, 'entry', 'entries')} shown, newest first</span>
          </div>
          <AuditList entries={entries} label="Audit log" onFilter={narrow} />
        </div>
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
