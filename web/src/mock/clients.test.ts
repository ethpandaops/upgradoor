import { describe, expect, it } from 'vitest';

import { CLIENT_HOST_STATES, list, type ClientHosts, type Clients } from '../api/types';
import { CLIENT_HOST_LIMIT, CLIENT_HOST_LIMIT_MAX } from './clients';
import { MockController } from './mockController';

async function get<T>(mock: MockController, path: string): Promise<T> {
  const res = await mock.fetch(`/api/v1/${path}`);
  expect(res.status).toBe(200);
  return (await res.json()) as T;
}

const status = async (mock: MockController, path: string) => (await mock.fetch(`/api/v1/${path}`)).status;

describe('the client table of the mock', () => {
  it('lists the consensus clients, then the execution clients, each with counts that add up to its hosts', async () => {
    const { clients } = await get<Clients>(new MockController(), 'clients');
    const rows = list(clients);

    expect(rows.map(c => `${c.layer} ${c.name}`)).toEqual([
      ...['grandine', 'lighthouse', 'lodestar', 'nimbus', 'prysm', 'teku'].map(n => `consensus ${n}`),
      ...['besu', 'erigon', 'geth', 'nethermind', 'reth'].map(n => `execution ${n}`),
    ]);

    for (const c of rows) {
      expect(
        CLIENT_HOST_STATES.reduce((n, s) => n + c[s], 0),
        c.name
      ).toBe(c.hosts);
      // A client's containers belong to the team of its name, on every host.
      expect(c.owners, c.name).toEqual([c.name]);
      expect(c.allOwned, c.name).toEqual([c.name]);
    }
  });

  it('gives a host one state per client, the first that applies', async () => {
    const mock = new MockController();
    const { clients } = await get<Clients>(mock, 'clients');
    const of = (name: string) => list(clients).find(c => c.name === name);
    const state = async (client: string, host: string) =>
      list((await get<ClientHosts>(mock, `clients/${client}/hosts?q=${host}`)).hosts)[0];

    // What is broken about the client is named before the failed lease of the host.
    expect((await state('nethermind', 'nimbus-nethermind-1'))?.state).toBe('failed');
    expect((await state('nethermind', 'nimbus-nethermind-1'))?.reason).toMatch(/nethermind/);
    // A local hold file is a hold; an override shows unless something above it applies.
    expect(await state('lodestar', 'lodestar-reth-3')).toMatchObject({ state: 'held' });
    expect(await state('prysm', 'prysm-geth-1')).toMatchObject({ state: 'overridden' });
    expect(await state('reth', 'lodestar-reth-2')).toMatchObject({ state: 'overridden' });
    // The deployed block names another image than the one it runs: a spec need, so behind.
    expect(await state('erigon', 'grandine-erigon-2')).toMatchObject({ state: 'pending' });
    expect(await state('lighthouse', 'lighthouse-geth-2')).toMatchObject({ state: 'rolling', reason: 'verifying' });
    expect(await state('lighthouse', 'lighthouse-geth-3')).toMatchObject({ state: 'pending' });
    expect(await state('prysm', 'prysm-geth-3')).toMatchObject({ state: 'current', stale: true });

    expect(of('prysm')?.overridden).toBe(1);
  });

  it('names the images a client follows by kind, with what the tag points at and who is behind', async () => {
    const { clients } = await get<Clients>(new MockController(), 'clients');
    const of = (name: string) => list(list(clients).find(c => c.name === name)?.images);

    // The validator client of lighthouse runs the image of the beacon node; that of prysm has its own.
    expect(of('lighthouse').map(i => [i.kind, i.image, i.hosts, i.behind])).toEqual([
      ['cl', 'sigp/lighthouse:unstable', 7, 4],
      ['vc', 'sigp/lighthouse:unstable', 5, 3],
    ]);
    expect(of('prysm').map(i => i.image)).toEqual([
      'gcr.io/offchainlabs/prysm/beacon-chain:latest',
      'gcr.io/offchainlabs/prysm/validator:latest',
    ]);
    // A custom container says nothing about the configured tag.
    expect(of('erigon')).toMatchObject([{ kind: 'el', image: 'erigontech/erigon:main', hosts: 3, behind: 0 }]);
    expect(of('geth')[0]?.digest).toMatch(/^sha256:/);
  });

  it('changes the version of a client when its list changes, and of no other', async () => {
    const mock = new MockController();
    const versions = async () =>
      Object.fromEntries(list((await get<Clients>(mock, 'clients')).clients).map(c => [c.name, c.version]));

    const before = await versions();
    expect(await versions()).toEqual(before);

    await mock.fetch('/api/v1/actions/hold', {
      method: 'POST',
      body: JSON.stringify({ hosts: ['teku-besu-1'], reason: '' }),
    });
    const after = await versions();

    expect(Object.keys(after).filter(name => after[name] !== before[name])).toEqual(['teku', 'besu']);
  });
});

describe('the hosts of a client in the mock', () => {
  it('serves a page of the list, ordered by name, with the containers of the client whole', async () => {
    const mock = new MockController({ fleet: 'large' });
    const view = list((await get<Clients>(mock, 'clients')).clients).find(c => c.name === 'lighthouse');
    const first = await get<ClientHosts>(mock, 'clients/lighthouse/hosts');

    expect(first).toMatchObject({
      client: 'lighthouse',
      version: view?.version,
      total: view?.hosts,
      matched: view?.hosts,
      offset: 0,
      limit: CLIENT_HOST_LIMIT,
    });
    const names = list(first.hosts).map(h => h.host);
    expect(names).toHaveLength(CLIENT_HOST_LIMIT);
    expect([...names].sort()).toEqual(names);

    for (const row of list(first.hosts)) {
      expect(list(row.containers).length).toBeGreaterThan(0);
      for (const c of list(row.containers)) {
        expect(c).toMatchObject({ client: 'lighthouse', managed: true, owners: ['lighthouse'] });
        expect(c.command?.length).toBeGreaterThan(0);
      }
    }

    const second = await get<ClientHosts>(mock, `clients/lighthouse/hosts?offset=${CLIENT_HOST_LIMIT}&limit=20`);
    expect(list(second.hosts)).toHaveLength(20);
    expect(list(second.hosts).filter(h => names.includes(h.host))).toEqual([]);

    const most = await get<ClientHosts>(mock, 'clients/lighthouse/hosts?limit=5000');
    expect(most.limit).toBe(CLIENT_HOST_LIMIT_MAX);
    expect(list(most.hosts)).toHaveLength(CLIENT_HOST_LIMIT_MAX);

    // The large fleet runs every client, ethrex included.
    expect((await get<ClientHosts>(mock, 'clients/ethrex/hosts')).total).toBeGreaterThan(0);
  });

  it('serves a fleet of twelve hundred hosts a page at a time', async () => {
    const mock = new MockController({ fleet: 'huge' });
    const { clients } = await get<Clients>(mock, 'clients');

    expect(mock.data.hosts.size).toBe(1200);
    expect(list(clients).find(c => c.name === 'geth')?.hosts).toBeGreaterThan(400);
    const page = await get<ClientHosts>(mock, 'clients/geth/hosts?offset=350');
    expect(list(page.hosts)).toHaveLength(CLIENT_HOST_LIMIT);
    // A page is its hosts, not the fleet.
    expect(JSON.stringify(page).length).toBeLessThan(150_000);
  });

  it('filters by state and by a part of the host name', async () => {
    const mock = new MockController();
    const all = await get<ClientHosts>(mock, 'clients/lighthouse/hosts');
    const behind = await get<ClientHosts>(mock, 'clients/lighthouse/hosts?state=pending,rolling');

    expect(behind.total).toBe(all.total);
    expect(behind.matched).toBe(list(all.hosts).filter(h => h.state === 'pending' || h.state === 'rolling').length);
    expect(list(behind.hosts).every(h => h.state === 'pending' || h.state === 'rolling')).toBe(true);

    const named = await get<ClientHosts>(mock, 'clients/lighthouse/hosts?q=GETH-&state=current');
    expect(list(named.hosts).map(h => h.host)).toEqual(['lighthouse-geth-1']);

    expect((await get<ClientHosts>(mock, 'clients/lighthouse/hosts?q=nope')).hosts).toEqual([]);
  });

  it('answers 400 for a bad filter or page and 404 for an unknown client', async () => {
    const mock = new MockController();

    expect(await status(mock, 'clients/lighthouse/hosts?state=broken')).toBe(400);
    expect(await status(mock, 'clients/lighthouse/hosts?offset=-1')).toBe(400);
    expect(await status(mock, 'clients/lighthouse/hosts?limit=0')).toBe(400);
    expect(await status(mock, `clients/lighthouse/hosts?q=${'x'.repeat(129)}`)).toBe(400);
    expect(await status(mock, 'clients/dora/hosts')).toBe(404);
  });

  it('shows the overrides the hosts report: applied with an end, being put back, and one in error', async () => {
    const now = Date.now();
    const mock = new MockController({ now: () => now });
    const override = async (client: string, host: string) =>
      list(list((await get<ClientHosts>(mock, `clients/${client}/hosts?q=${host}`)).hosts)[0]?.containers).find(
        c => c.override
      )?.override;

    const applied = await override('prysm', 'prysm-geth-1');
    expect(applied).toMatchObject({ applied: true, by: 'alice@example.org', addFlags: ['--verbosity=debug'] });
    expect(Date.parse(applied?.until ?? '')).toBeGreaterThan(now + 3600_000);
    expect(await override('reth', 'lodestar-reth-2')).toMatchObject({ applied: true, restoring: true });
    expect((await override('besu', 'teku-besu-2'))?.error).toMatch(/not allowed/);
  });
});

describe('the overrides of a client in the mock', () => {
  it('sends an override per host, lists the hosts that cannot take it, and filters and names the hosts with one', async () => {
    const mock = new MockController({});
    const all = await get<ClientHosts>(mock, 'clients/teku/hosts?names=true');
    expect(list(all.names)).toHaveLength(all.matched);
    expect(all.hosts).toEqual([]);

    const res = await mock.fetch('/api/v1/actions/client-override', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        client: 'teku',
        kind: 'cl',
        hosts: [...list(all.names), 'ghost-1'],
        part: 'flags',
        addFlags: ['--logging=debug'],
        reason: 'debug',
      }),
    });
    expect(res.status).toBe(200);
    const result = (await res.json()) as { hosts: string[]; rejected?: Record<string, string> };
    expect(result.rejected?.['ghost-1']).toBe('unknown host');
    expect(result.hosts.length).toBeGreaterThan(0);

    const view = list((await get<Clients>(mock, 'clients')).clients).find(c => c.name === 'teku');
    expect(view?.flagOverrides).toBe(result.hosts.length);
    const flagged = await get<ClientHosts>(mock, 'clients/teku/hosts?override=flags&names=true');
    expect([...list(flagged.names)].sort()).toEqual([...result.hosts].sort());
    expect(await status(mock, 'clients/teku/hosts?override=both')).toBe(400);
  });
});
