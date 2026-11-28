import { useMemo, useState, type MouseEvent, type ReactNode } from 'react';

import { list, type AuditEntry } from '../../api/types';
import { actorKindOf, auditHash, targetKindOf, type AuditFilter } from '../../lib/auditCatalog';
import { changeOf, diffValues, formatValue } from '../../lib/auditDiff';
import { FIELDS } from '../../lib/settingsLayout';
import { Icon } from '../icons';
import { useLive } from '../Live';
import { Timestamp } from '../RelTime';
import { Badge, cx, type Tone } from '../primitives';
import {
  FALLBACK,
  auditRenderers,
  humanize,
  str,
  strings,
  type AuditContext,
  type AuditRenderer,
  type FieldSpec,
  type ValueKind,
} from './auditRenderers';

const RENDERERS = auditRenderers(new Map(FIELDS.map(f => [f.key as string, f.label])));

export function rendererOf(action: string): AuditRenderer {
  return RENDERERS[action] ?? FALLBACK;
}

/** Keys every entry of a person carries: what allowed the request. */
const AUTH_KEYS = new Set(['authorizedBy', 'teams']);
const CHANGE_KEYS = new Set(['before', 'after']);

const TONE_TEXT: Record<Tone, string> = {
  neutral: 'text-foreground',
  success: 'text-success',
  warning: 'text-warning',
  danger: 'text-danger',
  info: 'text-info',
  accent: 'text-accent',
};

// -------------------------------------------------------------- navigation

/** openHost opens a host's drawer on the Hosts tab: ?host= and #hosts, as the tab keeps them. */
export function openHost(name: string) {
  const url = new URL(window.location.href);
  url.searchParams.set('host', name);
  window.history.replaceState(window.history.state, '', url);
  window.location.hash = 'hosts';
}

export function hostHref(name: string): string {
  const url = new URL(window.location.href);
  url.searchParams.set('host', name);
  url.hash = 'hosts';
  return url.pathname + url.search + url.hash;
}

/** placeOf is where a target lives in the page, when it has a place. */
function placeOf(e: AuditEntry): { href: string; label: string; onClick?: () => void } | undefined {
  const target = e.target ?? '';
  switch (targetKindOf(e)) {
    case 'host':
      return {
        href: hostHref(target),
        label: `Open ${target}`,
        onClick: () => {
          openHost(target);
        },
      };
    case 'settings':
      return { href: '#settings', label: 'Open the settings' };
    case 'sources':
      return { href: '#sources', label: 'Open the data sources' };
    default:
      return undefined;
  }
}

/** FilterLink narrows the audit page when it is shown there, and links to it elsewhere. */
function FilterLink({
  filter,
  ctx,
  title,
  className,
  children,
}: {
  filter: Partial<AuditFilter>;
  ctx: AuditContext;
  title: string;
  className?: string;
  children: ReactNode;
}) {
  const onClick = ctx.onFilter
    ? (ev: MouseEvent) => {
        ev.preventDefault();
        ctx.onFilter?.(filter);
      }
    : undefined;
  return (
    <a href={auditHash(filter)} title={title} onClick={onClick} className={cx('hover:underline', className)}>
      {children}
    </a>
  );
}

// ----------------------------------------------------------------- pieces

/** ActorBadge says who acted: a person, the scheduler or an agent. */
export function ActorBadge({ actor, ctx }: { actor: string; ctx: AuditContext }) {
  const kind = actorKindOf(actor);
  const label = kind === 'agents' ? actor.slice('agent:'.length) : kind === 'ssh' ? actor.slice('ssh:'.length) : actor;
  const style =
    kind === 'people'
      ? 'border-transparent bg-surface-selected text-accent'
      : kind === 'agents' || kind === 'ssh'
        ? 'border-transparent bg-info-subtle text-info'
        : 'border-border bg-surface-raised text-muted';

  return (
    <FilterLink
      filter={{ actor }}
      ctx={ctx}
      title={`Only the entries of ${actor}`}
      className={cx(
        'inline-flex max-w-full min-w-0 items-center gap-1 self-start justify-self-start rounded-sm border px-1.5 text-xs/5 font-medium',
        style
      )}
    >
      {kind === 'agents' && <span className="font-normal opacity-75">agent</span>}
      {kind === 'ssh' && <span className="font-normal opacity-75">ssh</span>}
      <span className="truncate">{label}</span>
    </FilterLink>
  );
}

/** TargetRef names the target: a rollout by its title. It narrows the page to the target; the arrow opens it. */
function TargetRef({ entry: e, ctx }: { entry: AuditEntry; ctx: AuditContext }) {
  const target = e.target ?? '';
  if (target === '') return null;

  const kind = targetKindOf(e);
  const title = kind === 'rollout' ? (ctx.rolloutTitle(target) ?? str(e.detail?.title)) : undefined;
  const place = placeOf(e);

  return (
    <span className="inline-flex max-w-full min-w-0 items-center gap-0.5">
      <FilterLink
        filter={{ target }}
        ctx={ctx}
        title={`Every entry about ${target}`}
        className={cx('min-w-0 truncate text-accent', title === undefined && 'font-mono')}
      >
        {title ?? target}
      </FilterLink>
      {place && (
        <a
          href={place.href}
          title={place.label}
          aria-label={place.label}
          onClick={
            place.onClick
              ? ev => {
                  ev.preventDefault();
                  place.onClick?.();
                }
              : undefined
          }
          className="rounded-xs p-0.5 text-faint hover:bg-surface-hover hover:text-foreground"
        >
          <Icon name="open" className="size-3.5" />
        </a>
      )}
    </span>
  );
}

function HostLinks({ hosts, ctx }: { hosts: readonly string[]; ctx: AuditContext }) {
  const [all, setAll] = useState(false);
  const shown = all ? hosts : hosts.slice(0, 12);
  return (
    <span className="flex flex-wrap items-center gap-1">
      {shown.map(h => (
        <FilterLink
          key={h}
          filter={{ target: h }}
          ctx={ctx}
          title={`Every entry about ${h}`}
          className="rounded-xs bg-surface-raised px-1 font-mono"
        >
          {h}
        </FilterLink>
      ))}
      {hosts.length > shown.length && (
        <button
          type="button"
          className="text-accent hover:underline"
          onClick={() => {
            setAll(true);
          }}
        >
          and {hosts.length - shown.length} more
        </button>
      )}
    </span>
  );
}

/** present says whether a value has something to show; fields without are left out. */
function present(kind: ValueKind, value: unknown): boolean {
  switch (kind) {
    case 'quote':
    case 'mono':
    case 'time':
      return str(value) !== undefined;
    case 'hosts':
      return str(value) !== undefined || strings(value).length > 0;
    case 'rollouts':
    case 'list':
    case 'flags':
      return strings(value).length > 0;
    case 'yes':
      return value === true;
    case 'number':
      return typeof value === 'number';
    case 'map':
      return !!value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length > 0;
    case 'text':
      return value !== undefined && value !== null && value !== '' && !(Array.isArray(value) && value.length === 0);
  }
}

function Value({ kind, value, ctx }: { kind: ValueKind; value: unknown; ctx: AuditContext }): ReactNode {
  switch (kind) {
    case 'quote':
      return <span className="whitespace-pre-wrap">“{str(value)}”</span>;
    case 'mono':
      return <span className="font-mono break-all">{str(value)}</span>;
    case 'hosts':
      return <HostLinks hosts={typeof value === 'string' ? [value] : strings(value)} ctx={ctx} />;
    case 'rollouts':
      return (
        <span className="flex flex-wrap gap-x-2 gap-y-1">
          {strings(value).map(id => (
            <FilterLink key={id} filter={{ target: id }} ctx={ctx} title={id} className="text-accent">
              {ctx.rolloutTitle(id) ?? <span className="font-mono">{id}</span>}
            </FilterLink>
          ))}
        </span>
      );
    case 'list':
      return (
        <span className="flex flex-wrap gap-1">
          {strings(value).map(i => (
            <Badge key={i}>{i}</Badge>
          ))}
        </span>
      );
    case 'flags':
      return <span className="font-mono break-all">{strings(value).join(' ')}</span>;
    case 'time':
      return <Timestamp value={str(value)} />;
    case 'yes':
      return 'yes';
    case 'number':
      return <span className="tabular-nums">{value as number}</span>;
    case 'map':
      return (
        <span className="flex flex-col">
          {Object.entries(value as Record<string, unknown>).map(([k, v]) => (
            <span key={k}>
              <span className="font-mono">{k}</span>
              <span className="text-muted">: {formatValue(v)}</span>
            </span>
          ))}
        </span>
      );
    case 'text':
      if (typeof value === 'boolean') return value ? 'yes' : 'no';
      if (Array.isArray(value)) return value.map(v => formatValue(v)).join(', ');
      return typeof value === 'object' ? (
        <span className="font-mono break-all">{JSON.stringify(value)}</span>
      ) : (
        <span className="break-words">{formatValue(value)}</span>
      );
  }
}

function ChangeTable({ before, after, label }: { before: unknown; after: unknown; label: (key: string) => string }) {
  const rows = diffValues(before, after);
  return (
    <div className="overflow-x-auto">
      <table aria-label="Changes" className="w-full border-collapse text-xs/5">
        <thead>
          <tr className="text-left text-muted">
            <th scope="col" className="py-0.5 pr-4 font-medium">
              Setting
            </th>
            <th scope="col" className="py-0.5 pr-4 font-medium">
              Before
            </th>
            <th scope="col" className="py-0.5 font-medium">
              After
            </th>
          </tr>
        </thead>
        <tbody className="align-top">
          {rows.length === 0 && (
            <tr>
              <td colSpan={3} className="text-muted">
                no difference
              </td>
            </tr>
          )}
          {rows.map(r => (
            <tr key={r.key} className="border-t border-border">
              <td className="py-1 pr-4" title={r.key}>
                {label(r.key)}
              </td>
              <td className="py-1 pr-4 font-mono break-all whitespace-pre-wrap text-danger">
                <span aria-hidden="true">− </span>
                {formatValue(r.before)}
              </td>
              <td className="py-1 font-mono break-all whitespace-pre-wrap text-success">
                <span aria-hidden="true">+ </span>
                {formatValue(r.after)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** AuditDetail shows an entry's detail as its renderer reads it, with the raw JSON a click away. */
export function AuditDetail({ entry: e, ctx }: { entry: AuditEntry; ctx: AuditContext }) {
  const [raw, setRaw] = useState(false);
  const detail = e.detail ?? {};
  const renderer = rendererOf(e.action);
  const change = changeOf(detail);

  const specs: FieldSpec[] = [...(renderer.fields ?? [])];
  const known = new Set(specs.map(s => s.key));
  for (const key of Object.keys(detail)) {
    if (!known.has(key) && !AUTH_KEYS.has(key) && !(change && CHANGE_KEYS.has(key))) {
      specs.push({ key, label: humanize(key), kind: 'text' });
    }
  }

  const rows = specs.filter(s => present(s.kind, detail[s.key]));

  const by = str(detail.authorizedBy);
  const teams = strings(detail.teams);

  return (
    <div className="flex flex-col gap-3 rounded-md border border-border bg-background p-3 text-xs/5">
      {rows.length > 0 && (
        <dl className="grid grid-cols-1 gap-x-4 gap-y-1 @2xl:grid-cols-[minmax(0,10rem)_minmax(0,1fr)]">
          {rows.map(s => (
            <div key={s.key} className="contents">
              <dt className="text-muted">{s.label}</dt>
              <dd className="mb-1 min-w-0 @2xl:mb-0">
                <Value kind={s.kind} value={detail[s.key]} ctx={ctx} />
              </dd>
            </div>
          ))}
        </dl>
      )}
      {change && <ChangeTable before={change.before} after={change.after} label={renderer.changeLabel ?? humanize} />}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-muted">
        {by && (
          <span>
            Allowed through <span className="font-mono">{by}</span>
            {teams.length > 0 && <> · teams {teams.join(', ')}</>}
          </span>
        )}
        <span className="font-mono text-faint">#{e.id}</span>
        <button
          type="button"
          aria-expanded={raw}
          onClick={() => {
            setRaw(v => !v);
          }}
          className="ml-auto text-accent hover:underline"
        >
          {raw ? 'Hide raw' : 'Raw'}
        </button>
      </div>
      {raw && (
        <pre className="overflow-x-auto rounded-sm bg-surface-raised p-2 font-mono whitespace-pre-wrap">
          {JSON.stringify(e, null, 2)}
        </pre>
      )}
    </div>
  );
}

function AuditRow({ entry: e, ctx, showTarget }: { entry: AuditEntry; ctx: AuditContext; showTarget: boolean }) {
  const [open, setOpen] = useState(false);
  const renderer = rendererOf(e.action);
  const tone = renderer.tone?.(e);
  const summary = renderer.summary?.(e);
  const hasDetail = e.detail !== undefined && Object.keys(e.detail).length > 0;

  return (
    <li className="flex flex-col gap-1 px-3 py-2 @2xl:grid @2xl:grid-cols-[9.5rem_minmax(0,12rem)_minmax(0,1fr)] @2xl:items-start @2xl:gap-x-3">
      <div className="flex min-w-0 items-center gap-2 @2xl:contents">
        <Timestamp value={e.time} className="text-muted @2xl:pt-px" />
        <ActorBadge actor={e.actor} ctx={ctx} />
      </div>
      <div className="flex min-w-0 flex-col gap-2">
        <div className="flex min-w-0 items-start gap-2">
          <div className="flex min-w-0 flex-1 flex-wrap items-baseline gap-x-2 gap-y-0.5">
            <span className={cx('font-medium', TONE_TEXT[tone ?? 'neutral'])} title={e.action}>
              {renderer.title(e)}
            </span>
            {showTarget && <TargetRef entry={e} ctx={ctx} />}
            {summary && <span className="min-w-0 break-words text-muted">{summary}</span>}
          </div>
          {hasDetail && (
            <button
              type="button"
              aria-expanded={open}
              aria-label={`${open ? 'Hide' : 'Show'} the detail of entry ${e.id}`}
              onClick={() => {
                setOpen(v => !v);
              }}
              className="-my-0.5 inline-flex shrink-0 items-center gap-0.5 rounded-xs px-1 py-0.5 text-muted hover:bg-surface-hover hover:text-foreground @max-2xl:py-2"
            >
              <span className="@max-2xl:sr-only">Detail</span>
              <Icon name="chevron" className={cx('size-3.5 transition-transform', open && 'rotate-180')} />
            </button>
          )}
        </div>
        {open && <AuditDetail entry={e} ctx={ctx} />}
      </div>
    </li>
  );
}

/**
 * AuditList is the audit trail as a list, newest first: when, who, what
 * happened to which target, and the detail on demand. On the audit page a
 * click on an actor or target narrows the page; elsewhere it links there.
 */
export function AuditList({
  entries,
  label,
  showTarget = true,
  onFilter,
}: {
  entries: readonly AuditEntry[];
  label: string;
  showTarget?: boolean;
  onFilter?: (f: Partial<AuditFilter>) => void;
}) {
  const rollouts = useLive('rollouts').data;
  const titles = useMemo(
    () => new Map([...list(rollouts?.open), ...list(rollouts?.recent)].map(r => [r.id, r.title])),
    [rollouts]
  );
  const ctx = useMemo<AuditContext>(() => ({ rolloutTitle: id => titles.get(id), onFilter }), [titles, onFilter]);

  return (
    <ol
      aria-label={label}
      className="@container divide-y divide-border rounded-lg border border-border bg-surface text-xs/5"
    >
      {entries.map(e => (
        <AuditRow key={e.id} entry={e} ctx={ctx} showTarget={showTarget} />
      ))}
    </ol>
  );
}
