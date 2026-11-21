import { describe, expect, it } from 'vitest';

import { encodeRow } from '../mock/wireStream';
import type { HostDetail, Hosts, LiveViews, NetworkHealth } from './types';
import golden from './wire.golden.json';
import { WIRE_VERSION, WireDoc, buildHostDetail, buildViews, chainAt, decodeRow, without, type DocBody } from './wire';

// wire.golden.json is written by TestWireFiles in pkg/controller/stream: the
// views as the REST routes answer them and the events of streams that carry
// them. What the page decodes from the events must be what the routes say.

type Event = Record<string, unknown> & { v?: string; i?: number; s?: string[]; d?: Record<string, DocBody | 0> };

const NOW = golden.now * 1000;

/** plain makes two views comparable: a stream carries a time as whole seconds. */
/**
 * What a network row says about the clock, next to what chainAt() made of it,
 * and the time of the beacon node's last answer, which a stream leaves out
 * while the node answers (beaconSeen).
 */
const CLOCK_KEYS = ['clocked', 'headBehind'];

/** plain makes two views comparable: a stream carries a time as whole seconds. */
function plain(v: unknown): unknown {
  if (typeof v === 'string' && /^\d{4}-\d\d-\d\dT/.test(v)) return Math.floor(Date.parse(v) / 1000);
  if (Array.isArray(v)) return v.map(plain);
  if (v && typeof v === 'object') {
    return Object.fromEntries(
      Object.entries(v)
        .filter(([k]) => !CLOCK_KEYS.includes(k))
        .map(([k, x]) => [k, plain(k === 'network' ? without(x as { updatedAt?: string }, 'updatedAt') : x)])
    );
  }
  return v;
}

/**
 * steady drops from a host table what a stream leaves to the clock or to the
 * host's details: when a live host last synced, its uptime (bootTime says the
 * same, and the clock the rest).
 */
function steady(view: unknown): unknown {
  const hosts = structuredClone(view) as Hosts;

  for (const h of hosts.hosts ?? []) {
    const row = h as unknown as Record<string, unknown>;
    if (!h.stale) delete row.lastSeen;
    delete row.bootTime;
    delete row.uptimeSeconds;
  }

  return plain(hosts);
}

function read(event: Event, priv: boolean) {
  const { v, i, s, d } = event;
  const doc = new WireDoc(priv ? '' : 'public');
  doc.apply(without(event, 'v', 'i', 's', 'd', 't') as DocBody, true);
  return { v, i, strings: s ?? [''], doc, details: d ?? {} };
}

describe('the stream encoding, against the controller', () => {
  it('reads the encoding the controller writes', () => {
    expect((golden.public as Event).v).toBe(WIRE_VERSION);
    expect((golden.user as Event).v).toBe(WIRE_VERSION);
  });

  it('decodes the public stream to what the public routes answer', () => {
    const { doc, strings } = read(golden.public, false);
    const views = buildViews(doc, strings, false, NOW);

    for (const name of ['public-network', 'public-hosts', 'public-images'] as const) {
      expect(plain(views[name]), name).toEqual(plain(golden.rest[name]));
    }
    expect(views.hosts).toBeUndefined();
    expect(views.plan).toBeUndefined();
  });

  it('decodes the authenticated stream to what every route answers, each fact sent once', () => {
    const { doc, strings, details } = read(golden.user as Event, true);
    const views = buildViews(doc, strings, true, NOW);

    // The public views are not on this stream: they are read off the others.
    for (const name of ['public-network', 'public-hosts', 'public-images'] as const) {
      expect(plain(views[name]), name).toEqual(plain(golden.rest[name]));
    }
    for (const name of ['plan', 'rollouts', 'settings'] as const) {
      expect(plain(views[name]), name).toEqual(plain(golden.rest[name]));
    }
    expect(steady(views.hosts)).toEqual(steady(golden.rest.hosts));

    // The uptime follows from the boot time and the clock.
    const rest = golden.rest.hosts as unknown as Hosts;
    expect(views.hosts?.hosts?.map(h => h.uptimeSeconds)).toEqual(rest.hosts?.map(h => h.uptimeSeconds));

    // A host's details are whole, clock-driven fields included.
    expect(details.nope).toBe(0);
    const detail = new WireDoc('d');
    detail.apply(details['lighthouse-geth-1'] as DocBody, true);
    expect(plain(buildHostDetail(detail))).toEqual(plain(golden.rest.host));
  });

  it('applies a patch of single cells', () => {
    const { doc, strings } = read(golden.user as Event, true);
    const { i, s } = golden.patch as Event;
    const tables = without(golden.patch as Event, 'i', 's', 'd') as DocBody;

    expect(i).toBe(((golden.user as Event).i ?? 0) + 1);
    expect(JSON.stringify(golden.patch).length).toBeLessThan(120);

    doc.apply(tables, false);
    const views = buildViews(doc, [...strings, ...(s ?? [])], true, NOW);

    expect(steady(views.hosts)).toEqual(steady(golden.hosts));
  });

  it('carries a slot as its sums, and for a login the hosts that missed in it', () => {
    const pub = read(golden.public, false);
    expect(buildViews(pub.doc, pub.strings, false, NOW).slots).toEqual({
      rows: [{ slot: 7190, proposed: true, assigned: 4, included: 3 }],
    });

    const user = read(golden.user as Event, true);
    const strings = [...user.strings, ...((golden.patch as Event).s ?? []), ...((golden.slotPatch as Event).s ?? [])];
    // A login sees which host missed attestations in the slot; the one that attested everything is not named.
    expect(buildViews(user.doc, strings, true, NOW).slots?.rows[0]?.hosts).toEqual([
      { host: 'lighthouse-geth-2', assigned: 2, included: 1 },
    ]);

    // A slot settles and a late inclusion completes the one before it: two
    // rows of four numbers, and nobody is named any more.
    const patch = without(golden.slotPatch as Event, 'i', 's', 'd') as DocBody;
    expect(Object.keys(patch)).toEqual(['sl']);
    expect(JSON.stringify(golden.slotPatch).length).toBeLessThan(60);
    user.doc.apply(patch, false);
    expect(buildViews(user.doc, strings, true, NOW).slots?.rows).toEqual([
      { slot: 7190, proposed: true, assigned: 4, included: 4 },
      { slot: 7191, proposed: false, assigned: 3, included: 3 },
    ]);
  });

  it('lets the clock say what the clock knows', () => {
    const { doc, strings } = read(golden.public, false);
    const network = buildViews(doc, strings, false, NOW)['public-network']?.network as NetworkHealth;
    const rest = (golden.rest['public-network'] as unknown as LiveViews['public-network']).network;

    // The row has no head slot and no epoch; the head is five slots behind.
    expect(network.headSlot).toBe(rest.headSlot);
    expect(network.currentEpoch).toBe(rest.currentEpoch);

    const later = chainAt(network, NOW + 2 * 32 * 12_000);
    expect(later.currentEpoch).toBe(rest.currentEpoch + 2);
    expect(later.headSlot).toBe(rest.headSlot + 64);
    expect(later.finalizedEpoch).toBe(rest.finalizedEpoch + 2);
    // The participation is of the epoch it was measured in.
    expect(later.livenessEpoch).toBe(rest.livenessEpoch);
  });
});

describe('the stream encoding, both ways', () => {
  it('writes what it reads', () => {
    const strings: string[] = [''];
    const ref = (s: string) => {
      if (!strings.includes(s)) strings.push(s);
      return strings.indexOf(s);
    };
    const host = without(golden.rest.host as unknown as HostDetail, 'inventory', 'leases', 'history');

    const row = encodeRow('wireDetail', host, ref);
    expect(plain(decodeRow('wireDetail', row, strings))).toEqual(plain(host));
    expect(JSON.stringify(row).length).toBeLessThan(JSON.stringify(host).length / 2);
  });
});
