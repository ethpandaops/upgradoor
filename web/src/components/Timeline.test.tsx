import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { Outage, SlotHistory } from '../api/types';
import { Timeline, TimelineLegend, type TimelineSelection } from './Timeline';

const FIRST = Date.parse('2026-01-01T00:00:00Z');
const SLOT = 4000;
const at = (slot: number) => new Date(FIRST + slot * SLOT).toISOString();

/** Ten slots from slot 320 (the first of epoch 10) on; slot 322 has no block and three attestations miss in slot 323. */
const history: SlotHistory = {
  firstSlot: 320,
  firstTime: new Date(FIRST).toISOString(),
  slotMs: SLOT,
  slotsPerEpoch: 32,
  bucketSlots: 1,
  minParticipationPercent: 90,
  epochSeconds: 128,
  recorded: [1, 1, 1, 1, 1, 1, 1, 1, 0, 0],
  missed: [0, 0, 1, 0, 0, 0, 0, 0, 0, 0],
  assigned: [8, 8, 8, 8, 8, 8, 8, 8, 0, 0],
  included: [8, 8, 8, 5, 8, 8, 8, 8, 0, 0],
  offSlots: 3,
  outageHosts: 2,
};

/** Host b was rolled from slot 322 to slot 325; host c has been off since before the window. */
const outages: Outage[] = [
  {
    hosts: 1,
    names: ['b'],
    from: at(2),
    to: at(5),
    lease: 'l1',
    work: 'image',
    missed: 3,
    assigned: 4,
    validators: 64,
  },
  {
    hosts: 1,
    names: ['c'],
    from: at(-50),
    to: at(8),
    fromStart: true,
    open: true,
    missed: 20,
    assigned: 20,
    validators: 32,
  },
];

const NOW = FIRST + 9 * SLOT + 500;
/** The fallback width of the graph is 960: with outages a plot of 880 px from x = 72 on, ten columns of 88 px. */
const columnX = (index: number) => 72 + 88 * index + 10;
/** The bands begin under the participation panel and the strip of the blocks. */
const WORK_Y = 8 + 140 + 10 + 18 + 6 + 9;

describe('Timeline', () => {
  it('draws what was off as blocks, not as a row per host', () => {
    render(<Timeline history={history} outages={outages} now={NOW} rangeMs={10 * SLOT} />);

    const chart = screen.getByRole('img', {
      name: 'Attestations: 8 of 8 included in slot 327 (100%); 2 outage blocks',
    });
    expect(within(chart).getByText('Updates')).toBeInTheDocument();
    expect(within(chart).getByText('Offline')).toBeInTheDocument();
    expect(within(chart).getByText('b · image roll')).toBeInTheDocument();
    expect(within(chart).getByText('c · offline')).toBeInTheDocument();
    expect(within(chart).getByRole('img', { name: 'b: image roll' })).toBeInTheDocument();
    expect(screen.getByText(/One column is one slot \(4s\)/)).toHaveTextContent(
      'missed attestations in 3 or more slots within an epoch'
    );
    expect(screen.getByText(/2 hosts were off in this window/)).toBeInTheDocument();
  });

  it('has the same height for four hosts and for a thousand', () => {
    const small = render(<Timeline history={history} outages={outages} now={NOW} rangeMs={10 * SLOT} />);
    const height = small.container.querySelector('svg')?.getAttribute('height');
    small.unmount();

    // A thousand hosts rolled a hundred at a time, as the controller answers it: ten spans.
    const waves: Outage[] = Array.from({ length: 10 }, (_, i) => ({
      hosts: 100,
      names: Array.from({ length: 8 }, (_, n) => `node-${i * 100 + n}`),
      from: at(i),
      to: at(i + 3),
      work: 'image',
      rollout: 'r1',
      missed: 300,
      assigned: 300,
      validators: 3200,
    }));
    const large = render(<Timeline history={history} outages={waves} now={NOW} rangeMs={10 * SLOT} />);

    expect(large.container.querySelector('svg')?.getAttribute('height')).toBe(height);
    // Three lanes for updates: waves that overlap more than that are one block that counts its hosts.
    const blocks = screen.getAllByRole('img', { name: /hosts: image roll/ });
    expect(blocks.length).toBeLessThan(10);
    const hosts = blocks.map(b => Number(/^(\d+) hosts/.exec(b.getAttribute('aria-label') ?? '')?.[1]));
    expect(hosts.reduce((a, b) => a + b, 0)).toBe(1000);
  });

  it('says that nothing is off as good news', () => {
    render(<Timeline history={{ ...history, outageHosts: 0 }} outages={[]} now={NOW} rangeMs={10 * SLOT} />);

    expect(screen.getByRole('img', { name: /nothing is off$/ })).toBeInTheDocument();
    expect(screen.getByText('✓ Nothing is off: no host was updated in this window')).toBeInTheDocument();
    expect(screen.getByText('✓ Nothing is off: no host was offline in this window')).toBeInTheDocument();
  });

  it('gives the exact numbers of the slot under the pointer, and of a block', () => {
    const onOpenHost = vi.fn();
    render(<Timeline history={history} outages={outages} now={NOW} rangeMs={10 * SLOT} onOpenHost={onOpenHost} />);
    const chart = screen.getAllByRole('img')[0] as HTMLElement;

    // Slot 323 in the participation panel.
    fireEvent.pointerMove(chart, { clientX: columnX(3), clientY: 40 });
    let tip = screen.getByRole('tooltip');
    expect(tip).toHaveTextContent('Slot 323');
    expect(tip).toHaveTextContent(/epoch 10 · \d\d:\d\d:\d\d/);
    expect(tip).toHaveTextContent('5 of 8 attested (62.5%)');
    expect(tip).toHaveTextContent('Block proposed');

    fireEvent.pointerMove(chart, { clientX: columnX(2), clientY: 40 });
    expect(screen.getByRole('tooltip')).toHaveTextContent('Block missed');

    // The block of host b: who, what, when, and what it took out.
    fireEvent.pointerMove(chart, { clientX: columnX(3), clientY: WORK_Y });
    tip = screen.getByRole('tooltip');
    expect(tip).toHaveTextContent('Image roll · b');
    expect(tip).toHaveTextContent('Finished.');
    expect(tip).toHaveTextContent('(12s)');
    expect(tip).toHaveTextContent('64 validators out · 3 of 4 attestations missed');

    // The slot the clock is in has not settled.
    fireEvent.pointerMove(chart, { clientX: columnX(9), clientY: 40 });
    expect(screen.getByRole('tooltip')).toHaveTextContent('Not settled yet');

    fireEvent.pointerLeave(chart);
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();

    // Focus says the same as the pointer; a block of one host leads to the host.
    const block = screen.getByRole('button', { name: 'c: offline' });
    fireEvent.focus(block);
    expect(screen.getByRole('tooltip')).toHaveTextContent('Offline · c');
    expect(screen.getByRole('tooltip')).toHaveTextContent(/before .* – now/);
    fireEvent.click(block);
    expect(onOpenHost).toHaveBeenCalledWith('c');
  });

  it('hands a clicked column, and a clicked block with the columns under it, to onSelect', () => {
    const onSelect = vi.fn<(selection: TimelineSelection) => void>();
    const onOpenHost = vi.fn();
    render(
      <Timeline
        history={history}
        outages={outages}
        now={NOW}
        rangeMs={10 * SLOT}
        onOpenHost={onOpenHost}
        onSelect={onSelect}
      />
    );
    const chart = screen.getAllByRole('img')[0] as HTMLElement;

    // Slot 323, where three attestations miss.
    fireEvent.pointerMove(chart, { clientX: columnX(3), clientY: 40 });
    expect(screen.getByRole('tooltip')).toHaveTextContent('Click to see which hosts missed and why.');
    fireEvent.click(chart, { clientX: columnX(3), clientY: 40 });
    expect(onSelect).toHaveBeenLastCalledWith({
      fromMs: FIRST + 3 * SLOT,
      toMs: FIRST + 4 * SLOT,
      slot: 323,
      slots: 1,
      slotMs: SLOT,
      slotsPerEpoch: 32,
      recorded: 1,
      missed: 0,
      assigned: 8,
      included: 5,
    });

    // The block of host b lies over slots 322 to 324: the click asks about those, not about the host.
    const block = screen.getByRole('button', { name: 'b: image roll' });
    fireEvent.focus(block);
    expect(screen.getByRole('tooltip')).toHaveTextContent('Click to see what was being rolled.');
    fireEvent.keyDown(block, { key: 'Enter' });
    expect(onSelect).toHaveBeenLastCalledWith(
      expect.objectContaining({ slot: 322, slots: 3, missed: 1, assigned: 24, included: 21 })
    );
    expect(onSelect).toHaveBeenLastCalledWith(
      expect.objectContaining({ block: expect.objectContaining({ outages: [outages[0]] }) as unknown })
    );

    fireEvent.click(block);
    expect(onSelect).toHaveBeenCalledTimes(3);
    expect(onOpenHost).not.toHaveBeenCalled();
  });

  it('draws a host action in a colour of its own and keys it in the legend', () => {
    const action: Outage[] = [{ ...(outages[0] as Outage), work: 'action' }];
    const { container } = render(
      <>
        <Timeline history={history} outages={action} now={NOW} rangeMs={10 * SLOT} />
        <TimelineLegend lanes threshold={undefined} />
      </>
    );

    expect(screen.getByRole('img', { name: 'b: host action' })).toBeInTheDocument();
    expect(container.querySelector('.fill-series-action')).not.toBeNull();
    expect(container.querySelector('.fill-series-packages')).toBeNull();
    expect(screen.getByRole('list', { name: 'Legend' })).toHaveTextContent('Host action');
  });

  it('edges an image roll deployed on the host and keys it in the legend', () => {
    const deployed: Outage[] = [{ ...(outages[0] as Outage), work: 'image', deployed: true }];
    const { container } = render(
      <>
        <Timeline history={history} outages={deployed} now={NOW} rangeMs={10 * SLOT} />
        <TimelineLegend lanes threshold={undefined} />
      </>
    );

    expect(screen.getByRole('img', { name: 'b: image roll' })).toBeInTheDocument();
    expect(container.querySelector('rect.fill-series-image.stroke-accent')).not.toBeNull();
    expect(screen.getByRole('list', { name: 'Legend' })).toHaveTextContent('Image roll deployed on the host');
  });

  it('has no bands without a login, and says so when there is nothing to draw', () => {
    render(
      <Timeline
        history={{ ...history, assigned: [0, 0, 0, 0, 0, 0, 0, 0, 0, 0] }}
        outages={undefined}
        now={NOW}
        rangeMs={10 * SLOT}
        emptyNote="No validators to watch on this fleet"
      />
    );

    const chart = screen.getByRole('img', { name: 'No participation data yet' });
    expect(within(chart).getByText('No validators to watch on this fleet')).toBeInTheDocument();
    expect(within(chart).queryByText('Offline')).not.toBeInTheDocument();
    expect(screen.queryByText(/Nothing is off/)).not.toBeInTheDocument();
  });
});
