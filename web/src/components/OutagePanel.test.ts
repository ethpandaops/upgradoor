import { describe, expect, it } from 'vitest';

import type { Outage, SlotRow } from '../api/types';
import { hostMisses, outageCauses, outagesIn } from './OutagePanel';

const at = (s: number) => new Date(Date.parse('2026-01-01T00:00:00Z') + s * 1000).toISOString();
const ms = (s: number) => Date.parse(at(s));

describe('hostMisses', () => {
  const rows: SlotRow[] = [
    { slot: 10, proposed: true, assigned: 8, included: 8 },
    {
      slot: 11,
      proposed: true,
      assigned: 8,
      included: 5,
      hosts: [
        { host: 'a', assigned: 2, included: 1 },
        { host: 'b', assigned: 2, included: 0 },
      ],
    },
    { slot: 12, proposed: false, assigned: 8, included: 6, hosts: [{ host: 'b', assigned: 2, included: 0 }] },
    { slot: 13, proposed: true, assigned: 8, included: 7, hosts: [{ host: 'c', assigned: 1, included: 0 }] },
  ];

  it('sums the slots of the span per host, the host that missed most first', () => {
    expect(hostMisses(rows, 11, 2)).toEqual({
      hosts: [
        { host: 'b', slots: 2, assigned: 4, included: 0 },
        { host: 'a', slots: 1, assigned: 2, included: 1 },
      ],
      covered: 2,
    });
  });

  it('says how many slots of the span the stream still holds', () => {
    expect(hostMisses(rows, 8, 4)).toEqual({
      hosts: [
        { host: 'a', slots: 1, assigned: 2, included: 1 },
        { host: 'b', slots: 1, assigned: 2, included: 0 },
      ].reverse(),
      covered: 2,
    });
    expect(hostMisses(rows, 0, 4)).toEqual({ hosts: [], covered: 0 });
  });
});

describe('outagesIn', () => {
  const span = (name: string, from: number, to: number, more: Partial<Outage> = {}): Outage => ({
    hosts: 1,
    names: [name],
    from: at(from),
    to: at(to),
    missed: 0,
    assigned: 0,
    validators: 0,
    ...more,
  });

  it('keeps the spans that overlap the time, work by upgradoor first; an open span reaches to now', () => {
    const outages = [
      span('off', 0, 50),
      span('before', 0, 10, { work: 'image' }),
      span('rolled', 15, 25, { work: 'image' }),
      span('open', 5, 6, { open: true }),
      span('after', 31, 40, { work: 'reboot' }),
    ];

    expect(outagesIn(outages, ms(20), ms(30), ms(60)).map(o => o.names?.[0])).toEqual(['rolled', 'off', 'open']);
  });

  it('lists a span of the block once after the window replaced its object, as the window has it now', () => {
    const clicked = [span('rolled', 15, 25, { work: 'image', lease: 'l1', open: true })];
    const refreshed = [span('off', 0, 50), span('rolled', 15, 25, { work: 'image', lease: 'l1', missed: 3 })];

    const causes = outageCauses(clicked, refreshed, ms(20), ms(30), ms(60));

    expect(causes.map(o => o.names?.[0])).toEqual(['rolled', 'off']);
    expect(causes[0]).toBe(refreshed[1]);
  });
});
