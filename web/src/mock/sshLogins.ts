// The SSH login history of the mock: the login groups the hosts reported,
// and GET /api/v1/ssh-logins over them as the controller answers it.

import type { SSHLoginCluster, SSHLoginGroup, SSHLogins } from '../api/types';

/** The window in which two groups of one identity and login user are one cluster (fleet.SSHClusterGap). */
const CLUSTER_GAP_MS = 10 * 60_000;
const MAX_CLUSTER_FROM = 10;
const MAX_CLUSTER_HOSTS = 5;
const DEFAULT_LIMIT = 50;
const MAX_LIMIT = 200;

const PK910_IP = '10.16.10.176';
const UNKNOWN_FINGERPRINT = 'SHA256:q3J8Ue0vH0pWm1yQqz6vDk9W2c1f9m2k0c8YbQk3x7E';

/**
 * buildSSHLogins writes the groups of the fixture, with every time relative
 * to now: Ansible runs of two people across the fleet, interactive sessions,
 * a key nobody's key map knows, a session logged in now and one cut off by a
 * reboot.
 */
export function buildSSHLogins(hosts: readonly string[], now: number): SSHLoginGroup[] {
  const groups: SSHLoginGroup[] = [];
  const iso = (ms: number) => new Date(ms).toISOString();
  const add = (g: Omit<SSHLoginGroup, 'id' | 'closed' | 'lastLogout'> & { end: number; closed?: boolean }) => {
    const { end, closed, ...rest } = g;
    groups.push({
      ...rest,
      id: groups.length + 1,
      lastLogout: iso(end),
      // A group closes ten minutes after its last session ended.
      closed: closed ?? (rest.open === 0 && now - end >= CLUSTER_GAP_MS),
    });
  };

  // An Ansible run: one group per host, many short sessions each.
  const ansible = (identity: string, from: string, startAgoMin: number, members: readonly string[], seed: number) => {
    const start = now - startAgoMin * 60_000;
    members.forEach((host, i) => {
      const sessions = 22 + ((i * 7 + seed) % 17);
      const first = start + Math.floor((i * 900_000) / Math.max(members.length, 1));
      const totalMs = sessions * (2_400 + ((i + seed) % 5) * 300);
      const longestMs = 9_000 + ((i * 13 + seed) % 11) * 1_000;
      add({
        host,
        identity,
        source: 'github',
        fingerprint: `SHA256:${identity === 'github:pk910' ? 'pk910' : 'barnabas'}Ed25519KeyFingerprintAbCdEf0123456789`,
        keyType: 'ED25519',
        user: 'devops',
        from,
        firstLogin: iso(first),
        sessions,
        open: 0,
        longestMs,
        totalMs,
        end: first + 4 * 60_000 + ((i * 17) % 180) * 1000,
      });
    });
  };

  ansible('github:pk910', PK910_IP, 50 * 60 + 2, hosts, 3);
  ansible('github:pk910', PK910_IP, 26 * 60 + 14, hosts, 5);
  ansible(
    'github:barnabasbusa',
    '10.16.10.180',
    20 * 60 + 40,
    hosts.filter((_, i) => i % 2 === 0),
    7
  );
  // The newest run was limited to a few hosts.
  ansible('github:pk910', PK910_IP, 2 * 60 + 5, hosts.slice(0, 6), 11);

  const pick = (i: number) => hosts[i % Math.max(hosts.length, 1)] ?? 'node-1';
  const interactive = (
    host: string,
    g: Partial<SSHLoginGroup> & { startAgoMin: number; durMin: number; sessions: number; longestMin: number }
  ) => {
    const first = now - g.startAgoMin * 60_000;
    add({
      host,
      identity: g.identity,
      source: g.source ?? 'github',
      fingerprint: g.fingerprint,
      keyType: g.keyType ?? 'ED25519',
      user: g.user ?? 'devops',
      from: g.from ?? PK910_IP,
      firstLogin: iso(first),
      sessions: g.sessions,
      open: g.open ?? 0,
      longestMs: g.longestMin * 60_000,
      totalMs: g.durMin * 60_000,
      endUnknown: g.endUnknown,
      end: g.open ? first + (g.durMin - g.longestMin) * 60_000 : first + g.durMin * 60_000,
      closed: g.open ? false : undefined,
    });
  };

  interactive(pick(1), {
    identity: 'github:barnabasbusa',
    fingerprint: 'SHA256:barnabasEd25519KeyFingerprintAbCdEf0123456789',
    from: '192.0.2.44',
    startAgoMin: 4 * 60 + 12,
    durMin: 52,
    longestMin: 41,
    sessions: 3,
  });
  interactive(pick(4), {
    identity: 'ops@laptop',
    source: 'comment',
    fingerprint: 'SHA256:opsLaptopRsaKeyFingerprint0123456789AbCdEfGh',
    keyType: 'RSA',
    user: 'root',
    from: '198.51.100.7',
    startAgoMin: 30 * 60 + 3,
    durMin: 7,
    longestMin: 7,
    sessions: 1,
  });
  interactive(pick(6), {
    source: 'unknown',
    fingerprint: UNKNOWN_FINGERPRINT,
    from: '203.0.113.9',
    startAgoMin: 9 * 60 + 31,
    durMin: 3,
    longestMin: 2,
    sessions: 2,
  });
  interactive(pick(2), {
    identity: 'github:pk910',
    fingerprint: 'SHA256:pk910Ed25519KeyFingerprintAbCdEf0123456789',
    startAgoMin: 25,
    durMin: 14,
    longestMin: 12,
    sessions: 2,
    open: 1,
  });
  interactive(pick(3), {
    identity: 'github:pk910',
    fingerprint: 'SHA256:pk910Ed25519KeyFingerprintAbCdEf0123456789',
    startAgoMin: 3 * 24 * 60 + 40,
    durMin: 95,
    longestMin: 95,
    sessions: 1,
    endUnknown: true,
  });

  return groups;
}

/** sshAuditDetail is the detail of the ssh.session entry of a closed group. */
export function sshAuditDetail(g: SSHLoginGroup): Record<string, unknown> {
  return {
    user: g.user,
    from: g.from,
    identity: g.identity ?? '',
    source: g.source,
    fingerprint: g.fingerprint ?? '',
    keyType: g.keyType ?? '',
    firstLogin: g.firstLogin,
    lastLogout: g.lastLogout,
    sessions: g.sessions,
    longestMs: g.longestMs,
    totalMs: g.totalMs,
    endUnknown: g.endUnknown ?? false,
  };
}

/** sshActor is fleet.SSHLoginGroup.Actor. */
export function sshActor(g: SSHLoginGroup): string {
  return `ssh:${g.identity || g.fingerprint || g.keyType || 'unknown'}`;
}

/** keyOf is what a cluster gathers: one identity (or key) and login user. */
function keyOf(g: SSHLoginGroup): string {
  return `${g.identity || g.fingerprint || g.keyType || ''}\u0000${g.user}`;
}

const byNewest = (a: SSHLoginGroup, b: SSHLoginGroup) =>
  Date.parse(b.firstLogin) - Date.parse(a.firstLogin) || b.id - a.id;

/** matching applies the filters of GET /api/v1/ssh-logins. */
function matching(groups: readonly SSHLoginGroup[], q: URLSearchParams): SSHLoginGroup[] {
  const from = q.get('from') ? Date.parse(q.get('from') ?? '') : NaN;
  const to = q.get('to') ? Date.parse(q.get('to') ?? '') : NaN;
  const eq = (key: string, value: string | undefined) => !q.get(key) || q.get(key) === (value ?? '');

  return groups
    .filter(g => {
      const start = Date.parse(g.firstLogin);
      return (
        eq('host', g.host) &&
        eq('identity', g.identity) &&
        eq('fingerprint', g.fingerprint) &&
        eq('user', g.user) &&
        (Number.isNaN(from) || start >= from) &&
        (Number.isNaN(to) || start < to) &&
        (q.get('unknown') !== '1' || !g.identity) &&
        (q.get('open') !== '1' || g.open > 0)
      );
    })
    .sort(byNewest);
}

/** clusters gathers the groups, newest first, into the fleet view's rows. */
function clusters(groups: readonly SSHLoginGroup[], now: number): SSHLoginCluster[] {
  const out: (SSHLoginCluster & { hostSet: Set<string> })[] = [];
  const current = new Map<string, SSHLoginCluster & { hostSet: Set<string> }>();

  for (const g of groups) {
    const key = keyOf(g);
    const start = Date.parse(g.firstLogin);
    const end = g.open > 0 ? now : Date.parse(g.lastLogout);
    let c = current.get(key);

    if (!c || end < Date.parse(c.firstLogin) - CLUSTER_GAP_MS) {
      c = {
        identity: g.identity,
        source: g.source,
        fingerprint: g.fingerprint,
        keyType: g.keyType,
        user: g.user,
        from: [],
        firstLogin: g.firstLogin,
        lastStart: g.firstLogin,
        lastLogout: g.lastLogout,
        hosts: 0,
        hostNames: [],
        groups: 0,
        sessions: 0,
        open: 0,
        longestMs: 0,
        totalMs: 0,
        hostSet: new Set(),
      };
      current.set(key, c);
      out.push(c);
    }

    if (start < Date.parse(c.firstLogin)) c.firstLogin = g.firstLogin;
    if (Date.parse(g.lastLogout) > Date.parse(c.lastLogout)) c.lastLogout = g.lastLogout;
    const fromList = c.from as string[];
    if (!fromList.includes(g.from) && fromList.length < MAX_CLUSTER_FROM) fromList.push(g.from);
    if (!c.hostSet.has(g.host)) {
      c.hostSet.add(g.host);
      c.hosts += 1;
      const names = c.hostNames as string[];
      if (names.length < MAX_CLUSTER_HOSTS) names.push(g.host);
    }
    c.groups += 1;
    c.sessions += g.sessions;
    c.open += g.open;
    c.totalMs += g.totalMs;
    c.longestMs = Math.max(c.longestMs, g.longestMs);
    if (g.endUnknown) c.endUnknown = true;
  }

  return out.map(({ hostSet: _, ...c }) => c);
}

/** sshLoginsPage answers GET /api/v1/ssh-logins, both lists always there; the cursor is an offset. */
export function sshLoginsPage(groups: readonly SSHLoginGroup[], q: URLSearchParams, now: number): SSHLogins {
  const limit = Math.min(Number(q.get('limit')) || DEFAULT_LIMIT, MAX_LIMIT);
  const offset = Number(q.get('cursor')) || 0;
  const selected = matching(groups, q);

  if (q.get('view') === 'groups') {
    const page = selected.slice(offset, offset + limit);
    return { groups: page, clusters: [], next: offset + limit < selected.length ? String(offset + limit) : '' };
  }

  const all = clusters(selected, now);
  const page = all.slice(offset, offset + limit);
  return { groups: [], clusters: page, next: offset + limit < all.length ? String(offset + limit) : '' };
}
