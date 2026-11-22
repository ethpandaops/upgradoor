package beacon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Every poll asks all endpoints at once for their head, whether they are in
// sync and their finality (nodeClient.probeAll). Each endpoint that answers
// and is in sync holds every chain its head is on; the chain followed is the
// one held by the most of them, on a tie the one followed so far. An endpoint
// whose head is not on it is forked, one more than behindSlots slots below
// its head is behind, the others are ok. Blocks and committees are read from
// the ok endpoints only, the ones that report the followed head first. When
// no chain is held by a strict majority, the network is split.
//
// Whether a head is on a chain is told by walking parent roots through the
// blocks the slot tracker knows. When all endpoints report the same head
// nothing is fetched; otherwise every head that is not known is walked back
// to a known block, and a block once read is kept until it leaves the window.

// behindSlots is how many slots an endpoint's head may lie below the followed
// head before the endpoint is behind.
const behindSlots = 2

// chainView is what a poll made of the endpoints' answers.
type chainView struct {
	// head is the followed head; finalized the highest finalized epoch of
	// the ok endpoints.
	head      headRef
	finalized uint64
	// usable are the ok endpoints, the ones that report head first.
	usable []int
	// split is set when no chain is held by a strict majority.
	split string
}

// judge picks the chain to follow from the probes and records the endpoint
// states. Without an endpoint that answers and is in sync it fails.
func (s *service) judge(ctx context.Context, current uint64, probes []probe) (*chainView, error) {
	states := make([]EndpointState, len(probes))
	responding := make([]int, 0, len(probes))
	reasons := make([]error, 0, len(probes))

	for i := range probes {
		label := s.client.endpoints[i].label

		switch {
		case probes[i].err != nil:
			states[i] = EndpointDown

			reasons = append(reasons, fmt.Errorf("%s: %w", label, probes[i].err))
		case probes[i].syncing:
			states[i] = EndpointSyncing

			reasons = append(reasons, fmt.Errorf("%s: syncing", label))
		default:
			responding = append(responding, i)
		}
	}

	if len(responding) == 0 {
		s.client.setStates(probes, states)

		return nil, fmt.Errorf("no beacon endpoint is usable: %w", errors.Join(reasons...))
	}

	heads := make([]headRef, 0, len(responding))
	for _, i := range responding {
		if !slices.ContainsFunc(heads, func(h headRef) bool { return h.root == probes[i].head.root }) {
			heads = append(heads, probes[i].head)
		}
	}

	candidates := heads

	if len(heads) > 1 {
		if err := s.walkHeads(ctx, current, probes, responding, heads); err != nil {
			return nil, err
		}

		// Only a head whose block is known can be followed.
		candidates = slices.DeleteFunc(slices.Clone(heads), func(h headRef) bool {
			return s.slots.known(h.root) == nil
		})

		if len(candidates) == 0 {
			return nil, errors.New("reading blocks: no endpoint serves the block of a head reported")
		}
	}

	tip, support := s.followedTip(candidates, probes, responding)
	view := &chainView{head: tip}

	for _, i := range responding {
		head := probes[i].head
		onChain, known := s.slots.descends(tip.root, head)

		switch {
		case known && !onChain:
			states[i] = EndpointForked
		case head.slot+behindSlots < tip.slot:
			states[i] = EndpointBehind
		default:
			states[i] = EndpointOK
			view.finalized = max(view.finalized, probes[i].finalized)
		}
	}

	view.usable = make([]int, 0, len(responding))

	for _, first := range []bool{true, false} {
		for _, i := range responding {
			if states[i] == EndpointOK && (probes[i].head.root == tip.root) == first {
				view.usable = append(view.usable, i)
			}
		}
	}

	if support*2 <= len(responding) {
		view.split = s.splitText(candidates, probes, responding)
	}

	s.client.setStates(probes, states)

	return view, nil
}

// followedTip returns the head to follow among candidates and how many of
// the responding endpoints hold its chain. Ties go to the chain followed so
// far, then to the higher slot.
func (s *service) followedTip(
	candidates []headRef, probes []probe, responding []int,
) (tip headRef, support int) {
	type ranked struct {
		head     headRef
		support  int
		followed bool
	}

	best := ranked{support: -1}

	for _, head := range candidates {
		c := ranked{head: head, support: s.support(head, probes, responding)}

		if s.head.root != "" {
			ahead, _ := s.slots.descends(head.root, s.head)
			back, _ := s.slots.descends(s.head.root, head)
			c.followed = ahead || back
		}

		if cmp.Or(
			cmp.Compare(c.support, best.support),
			compareBool(c.followed, best.followed),
			cmp.Compare(c.head.slot, best.head.slot),
			strings.Compare(best.head.root, c.head.root),
		) > 0 {
			best = c
		}
	}

	return best.head, best.support
}

// support counts the responding endpoints that hold the chain of tip: their
// head is on it, or where it lies cannot be told.
func (s *service) support(tip headRef, probes []probe, responding []int) int {
	count := 0

	for _, i := range responding {
		if onChain, known := s.slots.descends(tip.root, probes[i].head); onChain || !known {
			count++
		}
	}

	return count
}

// splitText says how the responding endpoints divide over the chains: the
// candidates no other candidate builds on.
func (s *service) splitText(candidates []headRef, probes []probe, responding []int) string {
	type chain struct {
		slot    uint64
		support int
	}

	chains := make([]chain, 0, len(candidates))

	for _, tip := range candidates {
		extended := slices.ContainsFunc(candidates, func(other headRef) bool {
			onChain, _ := s.slots.descends(other.root, tip)

			return other.root != tip.root && onChain
		})

		if !extended {
			chains = append(chains, chain{slot: tip.slot, support: s.support(tip, probes, responding)})
		}
	}

	slices.SortFunc(chains, func(a, b chain) int {
		return cmp.Or(cmp.Compare(b.support, a.support), cmp.Compare(b.slot, a.slot))
	})

	parts := make([]string, 0, len(chains))
	for _, c := range chains {
		parts = append(parts, fmt.Sprintf("%d/%d", c.support, len(responding)))
	}

	text := parts[len(parts)-1]
	if len(parts) > 1 {
		text = strings.Join(parts[:len(parts)-1], ", ") + " and " + text
	}

	return fmt.Sprintf("%d chains, %s endpoints", len(chains), text)
}

// walkHeads walks every head that is not known back to a known block, so
// that descends can place it. A head that cannot be walked stays unplaced.
func (s *service) walkHeads(
	ctx context.Context, current uint64, probes []probe, responding []int, heads []headRef,
) error {
	floor := (current - min(current, slotBackfillEpochs)) * s.timing.slotsPerEpoch
	if s.slots.started {
		floor = (current - min(current, slotWindowEpochs)) * s.timing.slotsPerEpoch
	}

	for _, head := range heads {
		// The endpoints that report the head are asked for its blocks first.
		order := make([]int, 0, len(responding))

		for _, first := range []bool{true, false} {
			for _, i := range responding {
				if (probes[i].head.root == head.root) == first {
					order = append(order, i)
				}
			}
		}

		err := s.walk(ctx, head.root, floor, order)
		if err == nil {
			continue
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		s.log.WithError(err).WithField("head", head.root).Debug(
			"Cannot tell which chain a beacon endpoint's head is on")
	}

	return nil
}

// walk follows parent roots from root until a canonical block, the block
// below floor or the first block of the chain, reading the blocks on the way
// that are not known yet.
func (s *service) walk(ctx context.Context, root string, floor uint64, order []int) error {
	tracker := s.slots
	below := uint64(math.MaxUint64)

	for (!tracker.started || root != tracker.anchor) && tracker.indexOf(root) < 0 {
		block, err := s.fetchBlock(ctx, root, order)
		if err != nil {
			return err
		}

		if block.slot >= below {
			return fmt.Errorf("block %s of slot %d is the parent of a block of slot %d",
				root, block.slot, below)
		}

		below = block.slot

		if block.slot < floor || block.first() {
			return nil
		}

		root = block.parent
	}

	return nil
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}
