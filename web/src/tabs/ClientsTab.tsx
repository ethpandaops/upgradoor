import { useEffect, useMemo, useState, type ReactNode } from 'react';

import {
  list,
  type ClientHost,
  type ClientHostState,
  type ClientImage,
  type ClientLayer,
  type ClientView,
  type Container,
  type ContainerKind,
  type HostListing,
  type Policy,
  type Rollout,
} from '../api/types';
import { useApp, useAuthState, usePrivateAccess } from '../appContext';
import { useActions, type ActionIntent } from '../components/ActionDialog';
import { KIND_LABEL, clientKinds } from '../components/ClientOverrideDialog';
import { CopyButton, CopyValue, Digest } from '../components/Digest';
import { ActionIcon, ActionMenu } from '../components/icons';
import { useLive } from '../components/Live';
import { OverrideButtons, overrideState } from '../components/Overrides';
import { ActionButton, usePermissions } from '../components/Permissions';
import { RelTime } from '../components/RelTime';
import {
  Badge,
  Button,
  Empty,
  ErrorBanner,
  INPUT_CLASS,
  Loading,
  Section,
  Segmented,
  Table,
  Td,
  Th,
  cx,
} from '../components/primitives';
import { BulkAction, IntentAction, SelectionBar } from '../components/SelectionBar';
import { CLIENT_STATE_LABEL, CLIENT_STATE_MEANING, ClientStateBadge, OfflineBadge } from '../components/status';
import { useClientHosts } from '../hooks/useClientHosts';
import { useDebounced } from '../hooks/useDebounced';
import { useHashSub } from '../hooks/useHashTab';
import { setKept, useKept } from '../hooks/useKept';
import { absoluteTime, displayImage, plural, shortRevision } from '../lib/format';
import { clientPolicyIntent, holdIntent, scopeIntent } from '../lib/intents';
import { POLICIES, POLICY_MEANING } from '../lib/settingsForm';
import { RecentRollouts, RolloutCard, policyFor, useRolloutData, waitsForApproval } from './RolloutsTab';

/** The number of hosts on a page of a client's list. It is fixed, so the pages have one height. */
export const CLIENT_PAGE_SIZE = 50;

const LAYERS: readonly { id: ClientLayer; short: string; label: string }[] = [
  { id: 'consensus', short: 'CL', label: 'Consensus' },
  { id: 'execution', short: 'EL', label: 'Execution' },
];

/** What a client's page shows below its configuration. */
type PageView = 'hosts' | 'rollouts';

/** The states a list can be narrowed to, in the order of the counts in the header. */
const FILTER_STATES: readonly ClientHostState[] = ['current', 'pending', 'rolling', 'overridden', 'held', 'failed'];

/**
 * defaultClient is the client the tab opens on: the first one a team of the
 * login owns a container of, otherwise the first of the list.
 */
export function defaultClient(clients: readonly ClientView[], teams: readonly string[]): string | undefined {
  return (clients.find(c => list(c.owners).some(o => teams.includes(o))) ?? clients[0])?.name;
}

/**
 * ClientsTab shows the fleet by client software: one row of tabs, the
 * consensus clients and then the execution clients, and for the selected
 * client its images, its image policy, how many of its hosts are in which
 * state, and below either its hosts or its rollouts. The clients are rows of
 * the event stream; the hosts of one client are asked for a page at a time.
 * The selected client and view are kept in the URL hash (#clients/geth,
 * #clients/geth/rollouts).
 */
export function ClientsTab({ onOpenTab }: { onOpenTab: (tab: 'hosts') => void }) {
  const { auth } = useApp();
  const priv = usePrivateAccess();
  const teams = useAuthState(auth).teams;
  const live = useLive('clients', priv);
  const [sub, setSub] = useHashSub('clients');
  const [wanted, wantedView] = sub.split('/');
  const view: PageView = wantedView === 'rollouts' ? 'rollouts' : 'hosts';
  const go = (name: string, next: PageView) => {
    setSub(next === 'rollouts' ? `${name}/rollouts` : name);
  };

  const clients = list(live.data?.clients);
  const selected = clients.find(c => c.name === wanted)?.name ?? defaultClient(clients, teams);
  const client = clients.find(c => c.name === selected);

  if (!priv) return <Empty>Log in to see the clients.</Empty>;
  if (live.error !== undefined) return <ErrorBanner error={live.error} what="the clients" />;
  if (!live.data) return <Loading what="clients" />;

  if (!client) {
    return (
      <Empty>
        No host reports a client yet. A client appears once an agent whose configuration names the client of its
        containers has synced.
      </Empty>
    );
  }

  return (
    <div className="flex min-w-0 flex-col gap-3">
      {/* One row: the consensus clients, then the execution clients; it scrolls sideways where it does not fit. */}
      <nav aria-label="Clients" className="min-w-0 overflow-x-auto border-b border-border">
        <div className="flex w-max min-w-full items-stretch">
          {LAYERS.map((layer, i) => (
            <div key={layer.id} className={cx('flex items-stretch gap-1', i > 0 && 'ml-3 border-l border-border pl-3')}>
              <span
                title={`${layer.label} layer clients`}
                className="flex items-center pr-1 text-xs/5 font-semibold tracking-wide text-faint"
              >
                {layer.short}
              </span>
              <div role="tablist" aria-label={`${layer.label} clients`} className="flex items-stretch gap-1">
                {clients
                  .filter(c => c.layer === layer.id)
                  .map(c => (
                    <ClientTab
                      key={c.name}
                      client={c}
                      selected={c.name === client.name}
                      onSelect={() => {
                        go(c.name, view);
                      }}
                    />
                  ))}
              </div>
            </div>
          ))}
        </div>
      </nav>

      {/* A client's page keeps its own filter, page and selection: another client starts its own. */}
      <ClientPage
        key={client.name}
        client={client}
        view={view}
        onView={next => {
          go(client.name, next);
        }}
        onOpenHost={host => {
          setKept('hosts.filter', host);
          onOpenTab('hosts');
        }}
      />
    </div>
  );
}

function ClientTab({ client, selected, onSelect }: { client: ClientView; selected: boolean; onSelect: () => void }) {
  return (
    <UnderlineTab selected={selected} onSelect={onSelect}>
      {client.name}
      <TabCount title={plural(client.hosts, 'host')}>{client.hosts}</TabCount>
      {client.failed > 0 && (
        <span
          role="img"
          aria-label={`${client.failed} failed`}
          title={`${plural(client.failed, 'host')} failed`}
          className="size-2 rounded-full bg-danger-solid"
        />
      )}
    </UnderlineTab>
  );
}

/** UnderlineTab is a tab of a row of tabs: the selected one is underlined in the accent colour. */
function UnderlineTab({
  selected,
  onSelect,
  children,
}: {
  selected: boolean;
  onSelect: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      role="tab"
      aria-selected={selected}
      onClick={onSelect}
      className={cx(
        '-mb-px inline-flex h-10 shrink-0 items-center gap-1.5 border-b-2 px-2.5 text-sm/6 font-medium whitespace-nowrap',
        selected
          ? 'border-accent text-foreground'
          : 'border-transparent text-muted hover:border-border-strong hover:text-foreground'
      )}
    >
      {children}
    </button>
  );
}

function TabCount({ title, tone, children }: { title?: string; tone?: 'warning'; children: ReactNode }) {
  return (
    <span
      title={title}
      className={cx(
        'min-w-6 rounded-full px-1.5 text-center text-xs/5 font-normal tabular-nums',
        tone === 'warning' ? 'bg-warning-subtle text-warning' : 'bg-surface-raised text-muted'
      )}
    >
      {children}
    </span>
  );
}

/** Where the user is in one client's list. */
interface Place {
  states: readonly ClientHostState[];
  /** override keeps the hosts with that part of an override. */
  override?: 'image' | 'flags';
  input: string;
  offset: number;
}

const START: Place = { states: [], input: '', offset: 0 };

/** The selection of one filter: rows ticked by hand, or every host the filter matches. */
interface Picked {
  filter: string;
  names: ReadonlySet<string>;
  whole: boolean;
}

function ClientPage({
  client,
  view,
  onView,
  onOpenHost,
}: {
  client: ClientView;
  view: PageView;
  onView: (view: PageView) => void;
  onOpenHost: (host: string) => void;
}) {
  const { api } = useApp();
  const actions = useActions();
  const hostTable = useLive('hosts');
  const rollouts = useRolloutData();
  const open = list(rollouts.rollouts.data?.open).filter(r => r.client === client.name);
  const recent = list(rollouts.rollouts.data?.recent).filter(r => r.client === client.name);
  const approvals = open.filter(r => waitsForApproval(r, policyFor(rollouts.settings, r)));

  // Kept while another tab is shown, per client.
  const [place, setPlace] = useKept<Place>(`clients/${client.name}`, START);
  const term = useDebounced(place.input.trim(), 300);

  // A page belongs to the filter it was opened under: while the typed text
  // settles, the first page of the new filter is what will be asked for.
  const typing = place.input.trim() !== term;
  const offset = typing ? 0 : place.offset;
  const filter = { states: place.states, q: term, override: place.override };

  const result = useClientHosts(client.name, { ...filter, offset, limit: CLIENT_PAGE_SIZE }, client.version);
  const page = result.page?.client === client.name ? result.page : undefined;
  const rows = list(page?.hosts);
  const waiting = typing || result.loading;

  // A list that got shorter than where the user was: the last page it has.
  const matched = page?.matched;
  useEffect(() => {
    if (matched === undefined || waiting) return;
    const last = Math.max(0, Math.ceil(matched / CLIENT_PAGE_SIZE) - 1) * CLIENT_PAGE_SIZE;
    if (place.offset > last) setPlace(prev => ({ ...prev, offset: last }));
  }, [matched, waiting, place.offset, setPlace]);

  // The selection belongs to the filter it was made under; it spans pages.
  const filterKey = `${place.states.join(',')}/${term}/${place.override ?? ''}`;
  const [picked, setPicked] = useState<Picked>({ filter: filterKey, names: new Set(), whole: false });
  const current = picked.filter === filterKey ? picked : { filter: filterKey, names: new Set<string>(), whole: false };
  const count = current.whole ? (matched ?? 0) : current.names.size;
  const isPicked = (host: string) => current.whole || current.names.has(host);
  const pick = (names: ReadonlySet<string>) => {
    setPicked({ filter: filterKey, names, whole: false });
  };
  const pagePicked = rows.length > 0 && rows.every(r => isPicked(r.host));

  const summaries = useMemo(() => new Map(list(hostTable.data?.hosts).map(h => [h.name, h])), [hostTable.data]);

  // Names of a whole-filter selection come from the controller when an action is chosen.
  const [resolving, setResolving] = useState(false);
  const [resolveError, setResolveError] = useState<unknown>(undefined);
  const resolve = async (wholeClient: boolean): Promise<string[] | undefined> => {
    if (!wholeClient && !current.whole) return [...current.names].sort();
    setResolving(true);
    setResolveError(undefined);
    try {
      return await api.clientHostNames(client.name, wholeClient ? {} : filter);
    } catch (err) {
      setResolveError(err);
      return undefined;
    } finally {
      setResolving(false);
    }
  };
  const runOn = (wholeClient: boolean, make: (hosts: string[]) => ActionIntent) => {
    void resolve(wholeClient).then(hosts => {
      if (hosts && hosts.length > 0) actions.request(make(hosts));
    });
  };

  const setFilter = (next: Partial<Place>) => {
    setPlace(prev => ({ ...prev, offset: 0, ...next }));
  };
  const toggleState = (state: ClientHostState) => {
    setFilter({
      states: place.states.includes(state) ? place.states.filter(s => s !== state) : [...place.states, state],
    });
  };

  // The permission of a bulk action is judged on the ticked rows of this page, or on the page for a whole filter.
  const sample = current.whole ? rows : rows.filter(r => current.names.has(r.host));
  const sampleNames = sample.map(r => r.host);
  const nothing = count === 0 ? 'Select hosts first' : resolving ? 'Fetching the selected hosts…' : '';
  const containers = clientContainers(sample);

  return (
    <div className="flex min-w-0 flex-col gap-3">
      <ClientConfig
        client={client}
        place={place}
        busy={resolving}
        onFilter={next => {
          setFilter(next);
          onView('hosts');
        }}
        onAll={part => {
          runOn(true, hosts => ({ kind: 'client-override', part, client, hosts }));
        }}
        onAction={actions.request}
      />

      {actions.notice}
      {resolveError !== undefined && <ErrorBanner error={resolveError} what="the selected hosts" />}

      {approvals.length > 0 && (
        <ApprovalCallout
          client={client}
          rollouts={approvals}
          onShow={
            view === 'rollouts'
              ? undefined
              : () => {
                  onView('rollouts');
                }
          }
          onAction={actions.request}
        />
      )}

      <div role="tablist" aria-label={`View of ${client.name}`} className="flex border-b border-border">
        <UnderlineTab
          selected={view === 'hosts'}
          onSelect={() => {
            onView('hosts');
          }}
        >
          Hosts
          <TabCount>{client.hosts}</TabCount>
        </UnderlineTab>
        <UnderlineTab
          selected={view === 'rollouts'}
          onSelect={() => {
            onView('rollouts');
          }}
        >
          Rollouts
          <TabCount
            tone={approvals.length > 0 ? 'warning' : undefined}
            title={approvals.length > 0 ? `${approvals.length} waiting for approval` : 'Open rollouts'}
          >
            {open.length}
          </TabCount>
        </UnderlineTab>
      </div>

      {view === 'rollouts' ? (
        <ClientRollouts client={client} open={open} recent={recent} data={rollouts} onAction={actions.request} />
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
            <div role="group" aria-label="Filter by state" className="flex flex-wrap items-center gap-1">
              <StateChip
                label="all"
                count={client.hosts}
                pressed={place.states.length === 0 && !place.override}
                onClick={() => {
                  setFilter({ states: [], override: undefined });
                }}
              />
              {FILTER_STATES.map(state => (
                <StateChip
                  key={state}
                  label={CLIENT_STATE_LABEL[state]}
                  title={CLIENT_STATE_MEANING[state]}
                  count={client[state]}
                  pressed={place.states.includes(state)}
                  onClick={() => {
                    toggleState(state);
                  }}
                />
              ))}
              {place.override && (
                <StateChip
                  label={`${place.override} override`}
                  title="Remove this filter"
                  count={place.override === 'image' ? client.imageOverrides : client.flagOverrides}
                  pressed
                  onClick={() => {
                    setFilter({ override: undefined });
                  }}
                />
              )}
            </div>

            <input
              type="search"
              aria-label="Filter hosts by name"
              placeholder="Host name…"
              value={place.input}
              onChange={e => {
                setPlace(prev => ({ ...prev, input: e.target.value, offset: 0 }));
              }}
              className={cx(INPUT_CLASS, 'w-48')}
            />

            {/* The pager keeps its place and its width, whatever page is shown. */}
            <div role="group" aria-label="Pages" className="ml-auto flex items-center gap-2 text-xs/5 text-muted">
              <span className="min-w-36 text-right tabular-nums" aria-live="polite">
                {page === undefined
                  ? ''
                  : page.matched === 0
                    ? 'no host'
                    : `${page.offset + 1}–${page.offset + rows.length} of ${plural(page.matched, 'host')}`}
              </span>
              <Button
                dense
                aria-label="Previous page"
                disabled={offset === 0 || waiting}
                onClick={() => {
                  setPlace(prev => ({ ...prev, offset: Math.max(0, offset - CLIENT_PAGE_SIZE) }));
                }}
              >
                ‹ Previous
              </Button>
              <Button
                dense
                aria-label="Next page"
                disabled={waiting || page === undefined || page.offset + rows.length >= page.matched}
                onClick={() => {
                  setPlace(prev => ({ ...prev, offset: offset + CLIENT_PAGE_SIZE }));
                }}
              >
                Next ›
              </Button>
            </div>
          </div>

          <SelectionBar
            count={count}
            matched={matched}
            wholeFilter={current.whole}
            onSelectWholeFilter={() => {
              setPicked({ filter: filterKey, names: new Set(), whole: true });
            }}
            onClear={() => {
              pick(new Set());
            }}
          >
            <BulkAction
              icon="roll"
              label="Roll"
              blocked={
                nothing || (containers.length === 0 && sample.length > 0 ? 'No container of the client to roll' : '')
              }
              check={rollIntent(sample, summaries)}
              run={() => {
                runOn(false, hosts => ({ ...rollIntent(sample, summaries), hosts }));
              }}
            />
            <BulkAction
              icon="hold"
              label="Hold"
              blocked={nothing}
              check={holdIntent('hold', sampleNames)}
              run={() => {
                runOn(false, hosts => holdIntent('hold', hosts));
              }}
            />
            <BulkAction
              icon="unhold"
              label="Unhold"
              blocked={nothing}
              check={holdIntent('unhold', sampleNames)}
              run={() => {
                runOn(false, hosts => holdIntent('unhold', hosts));
              }}
            />
            <span aria-hidden="true" className="mx-1 hidden h-4 w-px bg-border-strong sm:inline" />
            {(['image', 'flags', 'clear'] as const).map(part => (
              <BulkAction
                key={part}
                icon={part}
                label={part === 'image' ? 'Change image' : part === 'flags' ? 'Change flags' : 'Clear override'}
                blocked={nothing}
                check={{ kind: 'client-override', part, client, hosts: sampleNames }}
                run={() => {
                  runOn(false, hosts => ({ kind: 'client-override', part, client, hosts }));
                }}
              />
            ))}
          </SelectionBar>

          {result.error !== undefined && (
            <ErrorBanner error={result.error} what={`the hosts of ${client.name}`} onRetry={result.refresh} />
          )}

          {page === undefined ? (
            result.error === undefined && <Loading what={`the hosts of ${client.name}`} />
          ) : (
            // The page that is shown stays, dimmed, while the next one loads.
            <div className={cx('min-w-0', waiting && 'opacity-60')}>
              <Table label={`Hosts of ${client.name}`} fixed>
                <thead>
                  <tr>
                    <Th tight className="w-8 pl-3 text-center">
                      <input
                        type="checkbox"
                        aria-label="Select the hosts of this page"
                        checked={pagePicked}
                        disabled={rows.length === 0 || current.whole}
                        onChange={() => {
                          const next = new Set(current.names);
                          for (const r of rows) {
                            if (pagePicked) next.delete(r.host);
                            else next.add(r.host);
                          }
                          pick(next);
                        }}
                        className="accent-accent"
                      />
                    </Th>
                    <Th className="w-52">Host</Th>
                    <Th className="w-80">State</Th>
                    <Th>
                      <span className="flex items-center gap-2">
                        <span className={KIND_WIDTH}>Kind</span>
                        <span className={IMAGE_WIDTH}>Running image</span>
                        <span className={DIGEST_WIDTH}>Digest</span>
                        <span>Revision</span>
                      </span>
                    </Th>
                    <Th className="relative w-44">
                      <span className="sr-only">Actions</span>
                    </Th>
                  </tr>
                </thead>
                <tbody>
                  {rows.length === 0 && (
                    <tr>
                      <Td colSpan={5} className="py-4 text-center text-muted">
                        No host of {client.name} matches the filter.
                      </Td>
                    </tr>
                  )}
                  {rows.map(row => (
                    <ClientHostRow
                      key={row.host}
                      client={client}
                      row={row}
                      summary={summaries.get(row.host)}
                      selected={isPicked(row.host)}
                      onSelect={() => {
                        // Unticking one host of a whole-filter selection keeps the others of this page.
                        const next = new Set(current.whole ? rows.map(r => r.host) : current.names);
                        if (isPicked(row.host)) next.delete(row.host);
                        else next.add(row.host);
                        pick(next);
                      }}
                      onOpenHost={onOpenHost}
                      onAction={actions.request}
                    />
                  ))}
                </tbody>
              </Table>
            </div>
          )}
        </>
      )}

      {actions.dialog}
    </div>
  );
}

/**
 * ApprovalCallout names the rollouts of the client that wait for a person to
 * approve them, with the approval: a client on the manual policy rolls
 * nothing until then.
 */
function ApprovalCallout({
  client,
  rollouts,
  onShow,
  onAction,
}: {
  client: ClientView;
  rollouts: readonly Rollout[];
  onShow: (() => void) | undefined;
  onAction: (intent: ActionIntent) => void;
}) {
  return (
    <section
      aria-label={`Rollouts of ${client.name} waiting for approval`}
      className="flex flex-col gap-2 rounded-md border border-warning bg-warning-subtle px-4 py-3"
    >
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h3 className="text-sm/6 font-semibold text-warning">
          {rollouts.length === 1 ? 'A rollout waits' : `${rollouts.length} rollouts wait`} for approval
        </h3>
        <span className="text-xs/5 text-muted">
          {client.name} rolls out on the manual policy: nothing is rolled until a person approves.
        </span>
        {onShow && (
          <button type="button" onClick={onShow} className="ml-auto text-xs/5 font-medium text-accent hover:underline">
            Show the rollouts
          </button>
        )}
      </div>
      <ul className="flex flex-col gap-1.5">
        {rollouts.map(r => (
          <li key={r.id} className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span className="min-w-0 font-mono text-sm/6 wrap-anywhere">{r.title}</span>
            <span className="text-xs/5 text-muted">
              {plural(list(r.hosts).length, 'host')} · opened <RelTime fixed value={r.createdAt} />
            </span>
            <span className="ml-auto">
              <IntentAction
                icon={r.paused ? 'resume' : 'check'}
                label={r.paused ? 'Resume and approve…' : 'Approve…'}
                onAction={onAction}
                intent={scopeIntent('resume', `Approve rollout: ${r.title}`, r.id)}
              />
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}

/** ClientRollouts are the client's rollouts: the open ones as cards, as on the Rollouts tab, and the finished ones. */
function ClientRollouts({
  client,
  open,
  recent,
  data,
  onAction,
}: {
  client: ClientView;
  open: readonly Rollout[];
  recent: readonly Rollout[];
  data: ReturnType<typeof useRolloutData>;
  onAction: (intent: ActionIntent) => void;
}) {
  if (data.rollouts.error !== undefined) return <ErrorBanner error={data.rollouts.error} what="the rollouts" />;
  if (!data.rollouts.data) return <Loading what="rollouts" />;

  return (
    <div className="flex min-w-0 flex-col gap-6">
      <Section title={`Open rollouts (${open.length})`}>
        {open.length === 0 ? (
          <Empty>
            No rollout of {client.name} is open. One opens when a host reports a newer image or agent.yaml changes a
            container of the client.
          </Empty>
        ) : (
          <div className="flex flex-col gap-3">
            {open.map(r => (
              <RolloutCard
                key={r.id}
                rollout={r}
                policy={policyFor(data.settings, r)}
                queue={data.queue}
                leases={data.leases}
                onAction={onAction}
              />
            ))}
          </div>
        )}
      </Section>

      <Section title={`Finished rollouts (${recent.length})`}>
        {recent.length === 0 ? (
          <Empty>No rollout of {client.name} has finished recently.</Empty>
        ) : (
          <RecentRollouts rollouts={recent} leases={data.leases} label={`Finished rollouts of ${client.name}`} />
        )}
      </Section>
    </div>
  );
}

function StateChip({
  label,
  title,
  count,
  pressed,
  onClick,
}: {
  label: string;
  title?: string;
  count: number;
  pressed: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      title={title}
      aria-pressed={pressed}
      onClick={onClick}
      className={cx(
        'inline-flex h-7 items-center gap-1.5 rounded-full border px-2.5 text-xs/5 font-medium whitespace-nowrap',
        pressed
          ? 'border-accent bg-surface-selected text-foreground'
          : 'border-border-strong bg-surface text-muted hover:bg-surface-hover hover:text-foreground'
      )}
    >
      {label}
      {/* A fixed width: a count that changes does not move the chips after it. */}
      <span className="min-w-6 text-right font-normal tabular-nums">{count}</span>
    </button>
  );
}

// ------------------------------------------------------------------ header

/**
 * ClientConfig is the client's configuration, first on its page: per kind
 * of container the image it follows (with what the tag points at) and the
 * flags its hosts report as their base; how many hosts deviate, as chips that
 * filter the list; the changes for every host of the client; the policy.
 */
function ClientConfig({
  client,
  place,
  busy,
  onFilter,
  onAll,
  onAction,
}: {
  client: ClientView;
  place: Place;
  busy: boolean;
  onFilter: (next: Partial<Place>) => void;
  onAll: (part: 'image' | 'flags' | 'clear') => void;
  onAction: (intent: ActionIntent) => void;
}) {
  const permissions = usePermissions();
  const owners = list(client.owners);
  const kinds = clientKinds(client);
  const overrides = client.imageOverrides + client.flagOverrides;

  const deviations: {
    label: string;
    title: string;
    count: number;
    tone: 'warning' | 'danger' | 'info';
    pressed: boolean;
    next: Partial<Place>;
  }[] = [
    {
      label: 'image override',
      title: 'Hosts on which a container of the client runs an image override',
      count: client.imageOverrides,
      tone: 'info',
      pressed: place.override === 'image',
      next: { override: place.override === 'image' ? undefined : 'image', states: [] },
    },
    {
      label: 'flag override',
      title: 'Hosts on which a container of the client runs with a flag override',
      count: client.flagOverrides,
      tone: 'info',
      pressed: place.override === 'flags',
      next: { override: place.override === 'flags' ? undefined : 'flags', states: [] },
    },
    ...(['pending', 'held', 'failed'] as const).map(state => {
      const only = place.states.length === 1 && place.states[0] === state && !place.override;
      return {
        label: CLIENT_STATE_LABEL[state],
        title: CLIENT_STATE_MEANING[state],
        count: client[state],
        tone: state === 'failed' ? ('danger' as const) : ('warning' as const),
        pressed: only,
        next: { states: only ? [] : [state], override: undefined },
      };
    }),
  ];

  return (
    <section
      aria-label={`Client ${client.name}`}
      className="flex flex-col gap-4 rounded-md border border-border bg-surface px-4 py-3"
    >
      <div className="flex flex-col gap-2">
        <div className="flex min-w-0 flex-wrap items-baseline gap-x-2">
          <h2 className="text-lg/7 font-semibold">{client.name}</h2>
          <span className="text-xs/5 text-muted">
            {client.layer} client · {plural(client.hosts, 'host')}
            {owners.length > 0 && (
              <span title="The teams that own a container of this client"> · owned by {owners.join(', ')}</span>
            )}
          </span>
        </div>
        <PolicySetting client={client} onAction={onAction} />
      </div>

      <div className="grid gap-3 lg:grid-cols-2">
        {kinds.map(kind => (
          <KindConfig key={kind} kind={kind} images={list(client.images).filter(i => i.kind === kind)} />
        ))}
      </div>

      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-border pt-3">
        <div role="group" aria-label="Hosts that deviate" className="flex flex-wrap items-center gap-1.5">
          <span className="mr-1 text-xs/5 text-muted">Deviating:</span>
          {deviations.map(d => (
            <DeviationChip
              key={d.label}
              {...d}
              onClick={() => {
                onFilter(d.next);
              }}
            />
          ))}
        </div>

        <div role="group" aria-label={`Change every host of ${client.name}`} className="ml-auto flex flex-wrap gap-1">
          {(['image', 'flags', 'clear'] as const).map(part => {
            const verdict = permissions.canIntent({ kind: 'client-override', part, client, hosts: [] });
            const blocked = busy
              ? 'Fetching the hosts…'
              : !verdict.allowed
                ? verdict.reason
                : part === 'clear' && overrides === 0
                  ? `No host of ${client.name} has an override`
                  : '';
            return (
              <BulkAction
                key={part}
                icon={part}
                label={part === 'image' ? 'Change image…' : part === 'flags' ? 'Change flags…' : 'Clear overrides…'}
                check={undefined}
                blocked={blocked}
                run={() => {
                  onAll(part);
                }}
              />
            );
          })}
        </div>
      </div>
    </section>
  );
}

/** KindConfig is the configuration of one kind of container of the client: image, tag target, flags. */
function KindConfig({ kind, images }: { kind: ContainerKind; images: readonly ClientImage[] }) {
  // The reference most hosts follow carries the flags; other references (a host configured apart) are listed under it.
  const main = [...images].sort((a, b) => b.hosts - a.hosts)[0];
  if (!main) return null;
  const others = images.filter(i => i !== main);
  const flags = list(main.command);
  const differ = main.hosts - main.commandHosts;

  return (
    <div className="flex min-w-0 flex-col gap-2 rounded-sm bg-surface-raised px-3 py-2.5">
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs/5 font-semibold tracking-wide text-muted uppercase">{KIND_LABEL[kind] ?? kind}</span>
        <span className="text-xs/5 text-muted tabular-nums">
          {plural(main.hosts, 'host')}
          {main.behind > 0 && <span className="text-warning"> · {main.behind} behind</span>}
        </span>
      </div>

      <div className="flex flex-col gap-1">
        <span className="font-mono text-sm/6 font-medium break-all">{main.image}</span>
        <span className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs/5 text-muted">
          {main.digest ? (
            <span className="inline-flex items-center gap-1">
              points at <Digest value={main.digest} />
            </span>
          ) : (
            <span>no registry check yet</span>
          )}
          {main.revision && (
            <span className="inline-flex items-center gap-1">
              revision <CopyValue value={main.revision} short={shortRevision(main.revision)} label="revision" />
            </span>
          )}
        </span>
        {others.map(o => (
          <span key={o.image} className="text-xs/5 text-muted">
            also <span className="font-mono break-all">{o.image}</span> on {plural(o.hosts, 'host')}
          </span>
        ))}
      </div>

      <div className="flex flex-col gap-1">
        <span className="flex items-center justify-between gap-2 text-xs/5 text-muted">
          <span>
            {flags.length === 0 ? 'No flags reported' : `Flags (${flags.length})`}
            {differ > 0 && flags.length > 0 && (
              <span className="text-warning"> · {plural(differ, 'host runs', 'hosts run')} other flags</span>
            )}
          </span>
          {flags.length > 0 && (
            <CopyButton value={flags.join(' ')} label={`Copy the flags of the ${KIND_LABEL[kind] ?? kind}`} />
          )}
        </span>
        {flags.length > 0 && (
          // A compact code block: the flags flow together, wrapping by whole flag (a flag longer than the
          // line breaks anywhere); past its height the block scrolls instead of growing.
          <ul
            aria-label={`Flags of the ${KIND_LABEL[kind] ?? kind}`}
            className="flex max-h-28 min-w-0 flex-wrap content-start gap-x-2.5 overflow-y-auto rounded-sm border border-border bg-surface px-2.5 py-1.5 font-mono text-xs/5"
          >
            {flags.map((flag, i) => (
              <li key={`${i} ${flag}`} className="min-w-0 wrap-anywhere">
                {flag}
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

const DEVIATION_TONE: Record<'warning' | 'danger' | 'info', string> = {
  warning: 'text-warning',
  danger: 'text-danger',
  info: 'text-accent',
};

function DeviationChip({
  label,
  title,
  count,
  tone,
  pressed,
  onClick,
}: {
  label: string;
  title: string;
  count: number;
  tone: 'warning' | 'danger' | 'info';
  pressed: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      title={`${title}: show them`}
      aria-pressed={pressed}
      disabled={count === 0 && !pressed}
      onClick={onClick}
      className={cx(
        'inline-flex h-6 items-center gap-1.5 rounded-full border px-2 text-xs/5 whitespace-nowrap',
        'disabled:cursor-default disabled:opacity-50',
        pressed ? 'border-accent bg-surface-selected' : 'border-border enabled:hover:bg-surface-hover'
      )}
    >
      <span className={cx('font-semibold tabular-nums', count > 0 ? DEVIATION_TONE[tone] : 'text-muted')}>{count}</span>
      <span className="text-muted">{label}</span>
    </button>
  );
}

const POLICY_LABEL: Record<Policy, string> = { auto: 'Automatic', manual: 'Manual' };

/**
 * PolicySetting is the image policy of the client, a setting of its header:
 * automatic or manual, whether that is the client's own or the default, and
 * the way back to the default. It is disabled, and says why, for a login
 * that may not set it.
 */
function PolicySetting({ client, onAction }: { client: ClientView; onAction: (intent: ActionIntent) => void }) {
  const permissions = usePermissions();
  const settings = useLive('settings');
  const fallback = settings.data?.settings.policyImage;

  const verdict = permissions.canIntent(clientPolicyIntent(client.name, client.policy));

  return (
    <div role="group" aria-label="Roll policy" className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs/5">
      <span className="text-sm/6 font-medium" title={POLICIES.map(p => `${p}: ${POLICY_MEANING[p]}`).join('\n')}>
        Roll policy
      </span>
      <Segmented<Policy>
        label={`Roll policy of ${client.name}`}
        value={client.policy}
        disabled={!verdict.allowed}
        options={POLICIES.map(p => ({
          value: p,
          label: POLICY_LABEL[p],
          title: verdict.allowed ? `${POLICY_LABEL[p]}: ${POLICY_MEANING[p]}` : verdict.reason,
        }))}
        onChange={policy => {
          if (policy !== client.policy) onAction(clientPolicyIntent(client.name, policy));
        }}
      />
      <span className="text-muted">
        {client.policySet
          ? 'set for this client'
          : `the default for images${fallback ? ` (${POLICY_LABEL[fallback].toLowerCase()})` : ''}`}
        {' · '}
        {POLICY_MEANING[client.policy]}
      </span>
      {client.policySet && (
        <ActionButton dense variant="ghost" onAction={onAction} intent={clientPolicyIntent(client.name, '')}>
          Use the default{fallback ? ` (${POLICY_LABEL[fallback].toLowerCase()})` : ''}…
        </ActionButton>
      )}
      {!verdict.allowed && <span className="text-muted">Read only: {verdict.reason}.</span>}
    </div>
  );
}

// -------------------------------------------------------------------- rows

const KIND_WIDTH = 'w-6 shrink-0';
const IMAGE_WIDTH = 'w-72 shrink-0';
const DIGEST_WIDTH = 'w-24 shrink-0';

/** clientContainers are the containers of the rows that a roll of the client recreates. */
function clientContainers(rows: readonly ClientHost[]): string[] {
  return [...new Set(rows.flatMap(r => list(r.containers).map(c => c.name)))].sort();
}

/**
 * rollIntent rolls the client's containers on the hosts of the rows. The
 * dialog offers every container of those hosts and starts with the client's.
 */
function rollIntent(
  rows: readonly ClientHost[],
  summaries: ReadonlyMap<string, HostListing>
): ActionIntent & { kind: 'roll' } {
  const own = clientContainers(rows);
  const others = rows.flatMap(r => list(summaries.get(r.host)?.containers).flatMap(c => (c.managed ? [c.name] : [])));

  return {
    kind: 'roll',
    hosts: rows.map(r => r.host),
    all: false,
    containerChoices: [...new Set([...own, ...others])].sort(),
    containers: own,
  };
}

function ClientHostRow({
  client,
  row,
  summary,
  selected,
  onSelect,
  onOpenHost,
  onAction,
}: {
  client: ClientView;
  row: ClientHost;
  /** summary is the host's row of the host table: its hold, and what the agent allows. */
  summary: HostListing | undefined;
  selected: boolean;
  onSelect: () => void;
  onOpenHost: (host: string) => void;
  onAction: (intent: ActionIntent) => void;
}) {
  const containers = list(row.containers);
  const held = summary ? summary.holdWanted || summary.hold.controller : row.state === 'held';
  const rollable = clientContainers([row]).length > 0;
  const mayRoll = summary === undefined || list(summary.verbs).includes('roll');
  const overridden = containers.some(c => c.override);

  return (
    // One height for every row: two lines, whatever the state says and however many containers there are.
    <tr className={cx('h-14 hover:bg-surface-hover', selected && 'bg-surface-selected')}>
      <Td tight className="pl-3 text-center">
        <input
          type="checkbox"
          aria-label={`Select ${row.host}`}
          checked={selected}
          onChange={onSelect}
          className="accent-accent"
        />
      </Td>
      <Td>
        <button
          type="button"
          title={`Open ${row.host} on the Hosts tab`}
          onClick={() => {
            onOpenHost(row.host);
          }}
          className="block max-w-full truncate rounded-xs text-sm/6 font-medium hover:text-accent hover:underline"
        >
          {row.host}
        </button>
      </Td>
      <Td>
        <span className="flex items-center gap-2">
          <HostStateCell row={row} />
          <OfflineBadge mark={summary?.offline} />
        </span>
      </Td>
      <Td>
        <span className="flex flex-col">
          {containers.map(c => (
            <ContainerLine key={c.name} container={c} />
          ))}
        </span>
      </Td>
      <Td>
        <span role="group" aria-label={`Actions for ${row.host}`} className="flex items-center justify-end gap-0.5">
          <ActionIcon
            icon="roll"
            label={`Roll ${client.name} on ${row.host}`}
            blocked={
              !mayRoll
                ? 'The agent does not allow the verb roll'
                : rollable
                  ? ''
                  : 'No container of the client can be rolled'
            }
            onAction={onAction}
            intent={rollIntent([row], summary ? new Map([[row.host, summary]]) : new Map())}
          />
          <ActionIcon
            icon={held ? 'unhold' : 'hold'}
            label={held ? `Unhold ${row.host}` : `Hold ${row.host}`}
            onAction={onAction}
            intent={holdIntent(held ? 'unhold' : 'hold', [row.host])}
          />
          <OverrideButtons host={row.host} containers={containers} stale={row.stale === true} onAction={onAction} />
          <ActionMenu
            label={`More actions for ${row.host}`}
            onAction={onAction}
            items={[
              {
                icon: 'open',
                label: 'Open on the Hosts tab',
                onSelect: () => {
                  onOpenHost(row.host);
                },
              },
              {
                icon: 'reboot',
                label: 'Reboot the host…',
                intent: { kind: 'reboot', hosts: [row.host], all: false },
                blocked:
                  summary && !list(summary.verbs).includes('reboot') ? 'The agent does not allow the verb reboot' : '',
              },
              {
                icon: 'clear',
                label: 'Clear the override…',
                intent: { kind: 'client-override', part: 'clear', client, hosts: [row.host] },
                blocked: overridden ? '' : 'No container of the client has an override here',
              },
            ]}
          />
        </span>
      </Td>
    </tr>
  );
}

/** HostStateCell is two lines of a fixed width: the state with its marks, and what there is to say about it. */
function HostStateCell({ row }: { row: ClientHost }) {
  const override = list(row.containers).find(c => c.override)?.override;
  const state = override ? overrideState(override) : undefined;
  // An override that is in order says until when and by whom; one that is not says what it waits for, or what is wrong.
  const shown = override && row.state === 'overridden' ? override : undefined;
  const who = shown?.by ? `by ${shown.by}` : '';

  let about: string;
  if (!shown) about = row.reason ?? '';
  else if (shown.error) about = shown.error;
  else if (shown.restoring) about = 'waits for a lease that puts the configured back';
  else if (!shown.applied) about = [who, shown.reason ?? ''].filter(Boolean).join(' · ');
  else about = shown.until ? who : ['no end', who].filter(Boolean).join(' · ');

  const title = [about, shown?.reason ? `“${shown.reason}”` : ''].filter(Boolean).join(' · ');

  return (
    <span className="flex w-72 flex-col">
      <span className="flex h-5 items-center gap-1.5">
        <ClientStateBadge state={row.state} />
        {state && state.label !== 'applied' && (
          <Badge tone={state.tone} title={state.title}>
            {state.label}
          </Badge>
        )}
        {row.stale && (
          <Badge tone="warning" title="The agent is silent; this is its last report">
            silent
          </Badge>
        )}
      </span>
      <span className={cx('flex h-5 items-center gap-1.5', shown?.error ? 'text-warning' : 'text-muted')}>
        {shown?.applied && !shown.restoring && !shown.error && shown.until && (
          <span className="shrink-0" title={`The override ends ${absoluteTime(shown.until)}`}>
            until <RelTime value={shown.until} />
          </span>
        )}
        <span className="min-w-0 truncate" title={title || undefined}>
          {about}
        </span>
      </span>
    </span>
  );
}

/** ContainerLine is one container of the client: kind, running image, digest and revision, in columns of fixed widths. */
function ContainerLine({ container: c }: { container: Container }) {
  const configured = c.configuredImage || c.image;
  const differs = c.image !== configured;
  const behind = !differs && c.registry?.digest !== undefined && c.registry.digest !== (c.digest ?? '');

  return (
    <span className="flex h-5 items-center gap-2">
      <span
        className={cx(KIND_WIDTH, 'font-semibold text-muted uppercase')}
        title={`container ${c.name}${c.group ? `, group ${c.group}` : ''}`}
      >
        {c.kind ?? '–'}
      </span>
      <span className={cx(IMAGE_WIDTH, 'flex items-center gap-1.5')}>
        <span className="min-w-0 truncate font-mono" title={c.image}>
          {displayImage(c.image)}
        </span>
        {differs && (
          <Badge
            tone={c.override ? 'warning' : 'info'}
            title={
              c.override
                ? `Overridden; configured: ${configured}`
                : `agent.yaml names ${configured}: the container is rolled onto it`
            }
          >
            {c.override ? 'override' : 'rolling to spec'}
          </Badge>
        )}
        {c.state !== 'running' && <Badge tone="danger">{c.state}</Badge>}
      </span>
      <span className={cx(DIGEST_WIDTH, 'flex items-center gap-1')}>
        <Digest value={c.digest} />
        {behind && (
          <span className="text-warning" title={`The tag points at ${c.registry?.digest ?? ''}`}>
            ↑
          </span>
        )}
      </span>
      {c.revision ? (
        <CopyValue value={c.revision} short={shortRevision(c.revision)} label="revision" />
      ) : (
        <span className="text-faint">–</span>
      )}
      {c.group && (
        <span className="truncate text-muted" title="The roll group: its containers are rolled together, by one lease">
          group {c.group}
        </span>
      )}
    </span>
  );
}
