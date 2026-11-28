import { list, type PackageMatch, type PackageShow, type PackageSort, type PackageVersion } from '../api/types';

/** The page sizes offered; the controller caps a page at 500 names. */
export const PAGE_SIZES = [50, 100, 250, 500] as const;
export const DEFAULT_PAGE_SIZE = 100;

export type PackagesKind = 'apt' | 'docker';

/** Where the user is on the Packages tab; it is kept in the URL hash. */
export interface PackagesPlace {
  kind: PackagesKind;
  q: string;
  size: number;
  show: PackageShow;
  sort: PackageSort;
  /** cursors[i] is the `after` of page i; null where a page was not opened (a linked page). */
  cursors: (string | null)[];
}

export const START_PLACE: PackagesPlace = {
  kind: 'apt',
  q: '',
  size: DEFAULT_PAGE_SIZE,
  show: '',
  sort: '',
  cursors: [''],
};

const SHOWS: readonly PackageShow[] = ['', 'differing', 'outdated', 'security'];
const SORTS: readonly PackageSort[] = ['', 'versions', 'hosts', 'outdated'];

/** placeFromHash reads a place from the part of the hash after the tab ("images?q=geth&size=250"). */
export function placeFromHash(path: string, params: URLSearchParams): PackagesPlace {
  const size = Number(params.get('size'));
  const show = params.get('show') ?? '';
  const sort = params.get('sort') ?? '';
  const page = Math.max(0, Math.trunc(Number(params.get('page')) || 0));
  const after = params.get('after') ?? '';
  const kind: PackagesKind = path === 'images' ? 'docker' : 'apt';
  const cursors: (string | null)[] = [''];
  if (page > 0 && after !== '') cursors.push(...Array<null>(page - 1).fill(null), after);

  return {
    kind,
    q: params.get('q') ?? '',
    size: (PAGE_SIZES as readonly number[]).includes(size) ? size : DEFAULT_PAGE_SIZE,
    show: kind === 'apt' && (SHOWS as readonly string[]).includes(show) ? (show as PackageShow) : '',
    sort: kind === 'apt' && (SORTS as readonly string[]).includes(sort) ? (sort as PackageSort) : '',
    cursors,
  };
}

/** placeToHash is the hash part of a place: the path and its parameters, defaults left out. */
export function placeToHash(place: PackagesPlace): { path: string; params: URLSearchParams } {
  const params = new URLSearchParams();
  const page = place.cursors.length - 1;
  const after = place.cursors[page] ?? '';

  if (place.q !== '') params.set('q', place.q);
  if (place.show !== '') params.set('show', place.show);
  if (place.sort !== '') params.set('sort', place.sort);
  if (place.size !== DEFAULT_PAGE_SIZE) params.set('size', String(place.size));
  if (page > 0 && after !== '') {
    params.set('page', String(page));
    params.set('after', after);
  }

  return { path: place.kind === 'docker' ? 'images' : '', params };
}

/**
 * compareDebianVersions orders two Debian versions as dpkg does: epoch, then
 * the upstream version, then the revision, each compared part by part with
 * "~" before everything, even the end.
 */
export function compareDebianVersions(a: string, b: string): number {
  const split = (v: string): [number, string, string] => {
    const colon = v.indexOf(':');
    const epoch = colon > 0 ? Number(v.slice(0, colon)) || 0 : 0;
    const rest = colon > 0 ? v.slice(colon + 1) : v;
    const dash = rest.lastIndexOf('-');
    return dash >= 0 ? [epoch, rest.slice(0, dash), rest.slice(dash + 1)] : [epoch, rest, ''];
  };

  const [ea, ua, ra] = split(a);
  const [eb, ub, rb] = split(b);

  return ea - eb || comparePart(ua, ub) || comparePart(ra, rb);
}

/** order is dpkg's weight of a character in the non-digit part of a version. */
function order(c: string | undefined): number {
  if (c === undefined) return 0;
  if (c === '~') return -1;
  if (/[A-Za-z]/.test(c)) return c.charCodeAt(0);
  return c.charCodeAt(0) + 256;
}

function comparePart(a: string, b: string): number {
  let i = 0;
  let j = 0;

  while (i < a.length || j < b.length) {
    while ((i < a.length && !/\d/.test(a[i] ?? '')) || (j < b.length && !/\d/.test(b[j] ?? ''))) {
      const ca = i < a.length && !/\d/.test(a[i] ?? '') ? a[i] : undefined;
      const cb = j < b.length && !/\d/.test(b[j] ?? '') ? b[j] : undefined;
      const diff = order(ca) - order(cb);
      if (diff !== 0) return diff;
      i++;
      j++;
    }

    const startA = i;
    const startB = j;
    while (i < a.length && /\d/.test(a[i] ?? '')) i++;
    while (j < b.length && /\d/.test(b[j] ?? '')) j++;
    const na = a.slice(startA, i);
    const nb = b.slice(startB, j);

    const diff = Number(na || 0) - Number(nb || 0);
    if (diff !== 0) return diff;
  }

  return 0;
}

/** PackageSummary is what one row of the list shows of a package. */
export interface PackageSummary {
  /** hosts is the number of hosts with the package. */
  hosts: number;
  /** top is the version on the most hosts; others are the rest, the most hosts first. */
  top: PackageVersion | undefined;
  others: PackageVersion[];
  /** outdated is the number of hosts apt offers a newer version. */
  outdated: number;
  /** newest is the newest candidate offered to a host; undefined without upgrades. */
  newest: string | undefined;
  security: boolean;
  blocked: boolean;
  /** behind maps an installed version to the number of its hosts with an upgrade. */
  behind: ReadonlyMap<string, number>;
}

/** summarize reduces a package to what its row shows. */
export function summarize(match: PackageMatch): PackageSummary {
  const versions = [...list(match.versions)].sort(
    (a, b) => b.count - a.count || compareDebianVersions(b.version, a.version)
  );
  const upgrades = list(match.upgrades);
  const behind = new Map<string, number>();
  let newest: string | undefined;

  for (const u of upgrades) {
    behind.set(u.current, (behind.get(u.current) ?? 0) + u.count);
    if (newest === undefined || compareDebianVersions(u.candidate, newest) > 0) newest = u.candidate;
  }

  return {
    hosts: versions.reduce((n, v) => n + v.count, 0),
    top: versions[0],
    others: versions.slice(1),
    outdated: upgrades.reduce((n, u) => n + u.count, 0),
    newest,
    security: upgrades.some(u => u.security),
    blocked: upgrades.some(u => u.blocked),
    behind,
  };
}
