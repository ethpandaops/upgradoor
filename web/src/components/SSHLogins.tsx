import type { ReactNode } from 'react';

import type { SSHLoginCluster, SSHLoginGroup } from '../api/types';
import { absoluteTime, plural, timestamp } from '../lib/format';
import { durationText, githubName } from '../lib/sshLogins';
import { hostHref, openHost } from './audit/AuditList';
import { Badge, Table, Td, Th } from './primitives';

/**
 * At is a local timestamp that stands still, the UTC time on hover. Next to
 * the start of a range the end leaves out what it shares with it: the day,
 * or the year.
 */
function At({ value, after }: { value: string; after?: string }) {
  const text = timestamp(value);
  const start = after === undefined ? '' : timestamp(after);
  const shown =
    start.slice(0, 10) === text.slice(0, 10)
      ? text.slice(11)
      : start.slice(0, 4) === text.slice(0, 4)
        ? text.slice(5)
        : text;
  return (
    <time dateTime={value} title={absoluteTime(value)} className="whitespace-nowrap tabular-nums">
      {shown}
    </time>
  );
}

/** TimeRange is first login to last logout; a group with a session logged in now says so instead of an end. */
export function TimeRange({ first, last, open }: { first: string; last: string; open: number }) {
  return (
    <span className="inline-flex flex-wrap items-center gap-x-1 sm:flex-nowrap">
      <At value={first} />
      <span className="text-faint">–</span>
      {open > 0 ? (
        <Badge tone="success" title={`${plural(open, 'session')} logged in now`}>
          logged in now
        </Badge>
      ) : (
        <At value={last} after={first} />
      )}
    </span>
  );
}

/** Identity says whose key logged in: a GitHub user with a link, a key's comment, or an unknown key by its fingerprint. */
export function Identity({
  identity,
  fingerprint,
  keyType,
}: Pick<SSHLoginGroup, 'identity' | 'fingerprint' | 'keyType'>) {
  const github = githubName(identity);
  if (github) {
    return (
      <a
        href={`https://github.com/${github}`}
        target="_blank"
        rel="noreferrer"
        className="font-medium text-accent hover:underline"
      >
        {identity}
      </a>
    );
  }
  if (identity) return <span className="font-medium">{identity}</span>;
  if (fingerprint) {
    return (
      <span className="inline-flex max-w-full min-w-0 flex-wrap items-center gap-x-1.5">
        <span className="max-w-48 truncate font-mono" title={fingerprint}>
          {fingerprint}
        </span>
        <Badge tone="warning" title="The host's key map does not know this key">
          unknown key
        </Badge>
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1.5">
      {keyType && <span className="font-mono">{keyType}</span>}
      <Badge tone="warning">no key</Badge>
    </span>
  );
}

/** HostLink opens a host's drawer on the Hosts tab. */
export function HostLink({ name }: { name: string }) {
  return (
    <a
      href={hostHref(name)}
      onClick={e => {
        e.preventDefault();
        openHost(name);
      }}
      className="font-medium hover:underline"
    >
      {name}
    </a>
  );
}

/** Duration is total / longest, with the mark of a session whose end is not known. */
export function Duration({ item }: { item: SSHLoginGroup | SSHLoginCluster }) {
  return (
    <span className="inline-flex items-center gap-1.5 tabular-nums">
      <span title="total / longest session">{durationText(item)}</span>
      {item.endUnknown && (
        <Badge
          tone="neutral"
          title="A session ended without a logout (a reboot or an agent restart): it counts until it was last seen"
        >
          end unknown
        </Badge>
      )}
    </span>
  );
}

/** GroupsTable lists login groups, each of one host. */
export function GroupsTable({
  groups,
  label,
  showHost = true,
  footer,
}: {
  groups: readonly SSHLoginGroup[];
  label: string;
  showHost?: boolean;
  footer?: ReactNode;
}) {
  return (
    <Table label={label} stack>
      <thead>
        <tr>
          <Th>Time</Th>
          {showHost && <Th>Host</Th>}
          <Th>Identity</Th>
          <Th>User</Th>
          <Th>From</Th>
          <Th className="text-right">Sessions</Th>
          <Th>Duration</Th>
        </tr>
      </thead>
      <tbody>
        {groups.map(g => (
          <tr key={g.id}>
            <Td data-label="Time">
              <TimeRange first={g.firstLogin} last={g.lastLogout} open={g.open} />
            </Td>
            {showHost && (
              <Td data-label="Host">
                <HostLink name={g.host} />
              </Td>
            )}
            <Td data-label="Identity">
              <Identity identity={g.identity} fingerprint={g.fingerprint} keyType={g.keyType} />
            </Td>
            <Td data-label="User" className="font-mono">
              {g.user}
            </Td>
            <Td data-label="From" className="font-mono">
              {g.from}
            </Td>
            <Td data-label="Sessions" className="tabular-nums sm:text-right">
              {g.sessions.toLocaleString()}
            </Td>
            <Td data-label="Duration">
              <Duration item={g} />
            </Td>
          </tr>
        ))}
        {footer}
      </tbody>
    </Table>
  );
}

/** GroupsList lists one host's login groups in two lines each, for narrow places such as the host drawer. */
export function GroupsList({ groups, label }: { groups: readonly SSHLoginGroup[]; label: string }) {
  return (
    <ul aria-label={label} className="divide-y divide-border rounded-lg border border-border bg-surface text-xs/5">
      {groups.map(g => (
        <li key={g.id} className="flex flex-col gap-0.5 px-3 py-2">
          <div className="flex flex-wrap items-center justify-between gap-x-3">
            <TimeRange first={g.firstLogin} last={g.lastLogout} open={g.open} />
            <span className="inline-flex items-center gap-1.5 text-muted">
              <span className="tabular-nums">{plural(g.sessions, 'session')}</span>
              <Duration item={g} />
            </span>
          </div>
          <div className="flex min-w-0 flex-wrap items-center gap-x-1.5">
            <Identity identity={g.identity} fingerprint={g.fingerprint} keyType={g.keyType} />
            <span className="text-muted">as</span>
            <span className="font-mono">{g.user}</span>
            <span className="text-muted">from</span>
            <span className="font-mono">{g.from}</span>
          </div>
        </li>
      ))}
    </ul>
  );
}
