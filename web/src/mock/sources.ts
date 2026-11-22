// The data sources of the mock controller: two beacon endpoints, one of them
// failing, and a validator ranges inventory built from the fleet's hosts plus
// hosts upgradoor does not know.

import type {
  DataSources,
  ValidatorHostTotal,
  ValidatorRangeRow,
  ValidatorRangesRefresh,
  ValidatorRangesSource,
} from '../api/types';

/** REFRESH_FLOOR_MS is the age of the last fetch below which a refresh does not fetch (validators.RefreshFloor). */
export const REFRESH_FLOOR_MS = 10_000;

/** UNKNOWN_HOSTS are inventory hosts that no agent reports. */
const UNKNOWN_HOSTS = ['lodestar-nethermind-9', 'spare-keys-1'];

/** MockSources is the state of the mock's validator ranges fetches. */
export class MockSources {
  private attemptAt: number;
  private changedAt: number;

  constructor(
    private readonly now: () => number,
    private readonly hosts: () => readonly { name: string; validators: number }[]
  ) {
    this.attemptAt = now() - 90_000;
    this.changedAt = now() - 3 * 3600_000;
  }

  view(): DataSources {
    const now = this.now();
    const iso = (ms: number) => new Date(ms).toISOString();

    return {
      beacon: [
        { label: 'https://bn-1.devnet.example', active: true, successAt: iso(now - 4_000), headSlot: 181_042 },
        {
          label: 'https://bn-2.devnet.example',
          active: false,
          successAt: iso(now - 2 * 3600_000),
          headSlot: 180_441,
          error: 'GET /eth/v1/beacon/headers/head: HTTP 503: beacon node is syncing',
          errorAt: iso(now - 2 * 3600_000 + 12_000),
        },
      ],
      validatorRanges: this.ranges(iso),
    };
  }

  refresh(): ValidatorRangesRefresh {
    const now = this.now();
    const next = this.attemptAt + REFRESH_FLOOR_MS;
    if (now < next) {
      return {
        fetched: false,
        next: new Date(next).toISOString(),
        message: 'fetched within the last 10s',
        sources: this.view(),
      };
    }

    this.attemptAt = now;
    return { fetched: true, message: 'fetched: unchanged', sources: this.view() };
  }

  private ranges(iso: (ms: number) => string): ValidatorRangesSource {
    const owners = [
      ...this.hosts()
        .filter(h => h.validators > 0)
        .map(h => ({ host: h.name, count: h.validators, unknown: false })),
      ...UNKNOWN_HOSTS.map(host => ({ host, count: 64, unknown: true })),
    ];

    const ranges: ValidatorRangeRow[] = [];
    const hosts: ValidatorHostTotal[] = [];
    let start = 0;
    for (const o of owners) {
      ranges.push({ start, end: start + o.count, host: o.host, count: o.count, ...(o.unknown && { unknown: true }) });
      hosts.push({ host: o.host, ranges: 1, validators: o.count, ...(o.unknown && { unknown: true }) });
      start += o.count;
    }
    hosts.sort((a, b) => a.host.localeCompare(b.host));

    return {
      url: 'https://config.devnet.example/api/v1/nodes/validator-ranges',
      attemptAt: iso(this.attemptAt),
      successAt: iso(this.attemptAt),
      changedAt: iso(this.changedAt),
      etag: '"5d41402abc4b2a76"',
      lastModified: new Date(this.changedAt).toUTCString(),
      validators: start,
      unknownHosts: UNKNOWN_HOSTS.length,
      ranges,
      hosts,
    };
  }
}
