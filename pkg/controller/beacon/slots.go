package beacon

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// Per-slot participation: shown per slot in the web UI, and summed per epoch
// into the liveness the scheduler's gate, budget and readiness work with
// (liveness.go).
//
// What is computed, per slot S and for the tracked validators (the ranges of
// all hosts):
//
//   - assigned: the tracked validators in the attestation committees of S
//     (GET /eth/v1/beacon/states/head/committees?epoch=N, once per epoch);
//   - included: the ones of them a canonical block after S carries an
//     attestation of, for slot S (GET /eth/v2/beacon/blocks/{root}, once per
//     block). A validator counts once however many aggregates name it;
//   - proposed: whether S has a canonical block.
//
// The canonical chain is followed by root: every poll starts at the head the
// node reports and walks the parent roots back to a block it already knows.
// That is one request per new block, and it makes a reorg visible: the blocks
// that are no longer on the chain are dropped with what they included and
// the slots they touched are counted again.
//
// When a slot is shown: once the wall clock is slotSettleLag slots past it,
// so the block of the next slot, where an attestation is normally included,
// has had its whole slot to arrive. From then on the slot has a row. An
// attestation may be included until the end of the epoch after its own; the
// row is rewritten whenever a later block raises the count, and is final when
// that epoch has ended (a reorg reaching further back than slotWindowEpochs
// is not followed). A slot without a block is stored as not proposed with
// what was included of it so far, which is nothing while the chain stands
// still; a node that is merely behind (it says so) gets no rows for the slots
// it has not seen.
const (
	// DefaultSlotRetention is how long slot rows are kept: the longest window
	// the web UI offers (12 h) and a margin.
	DefaultSlotRetention = 13 * time.Hour

	// slotSettleLag is how many slots the clock must be past a slot before
	// the slot is stored.
	slotSettleLag = 2
	// slotWindowEpochs is how many epochs before the current one stay in
	// memory: the blocks of the current and the previous epoch can still
	// include attestations of the previous one, and one epoch more absorbs a
	// reorg across the boundary.
	slotWindowEpochs = 2
	// slotBackfillEpochs is how many epochs before the current one are read
	// when there is nothing to connect to (the first poll, or after a gap).
	// Nodes serve the committees of the previous epoch from the head state.
	slotBackfillEpochs = 1
	// maxRootLength bounds a block root as text ("0x" and 32 bytes of hex).
	maxRootLength = 66
)

// attestation is one aggregate as a block carries it.
type attestation struct {
	// slot is the slot attested for (data.slot).
	slot uint64
	// index is the committee index (data.index); meaningful before Electra.
	index uint64
	// committeeBits selects the committees an Electra aggregate spans; nil
	// before Electra.
	committeeBits []byte
	// aggregationBits is the SSZ bitlist of who signed, with its length bit:
	// one bit per member of the committee, or since Electra of the selected
	// committees one after the other, in ascending committee order.
	aggregationBits []byte
}

// attesters calls mark for every validator the aggregate carries a vote of.
// committees are the committees of the attestation's slot by committee index.
// An aggregate that does not fit them marks nobody.
func (a *attestation) attesters(committees [][]uint64, mark func(validator uint64)) error {
	selected, err := a.selected(len(committees))
	if err != nil {
		return err
	}

	members := 0
	for _, index := range selected {
		members += len(committees[index])
	}

	length, ok := bitlistLength(a.aggregationBits)
	if !ok {
		return errors.New("aggregation bits carry no length bit")
	}

	if length != members {
		return fmt.Errorf("aggregation bits cover %d validators, the committees have %d",
			length, members)
	}

	offset := 0

	for _, index := range selected {
		for position, validator := range committees[index] {
			if bitSet(a.aggregationBits, offset+position) {
				mark(validator)
			}
		}

		offset += len(committees[index])
	}

	return nil
}

// selected returns the committee indices the aggregate spans, ascending.
func (a *attestation) selected(committees int) ([]int, error) {
	if a.committeeBits == nil {
		if a.index >= uint64(committees) {
			return nil, fmt.Errorf("committee %d of %d", a.index, committees)
		}

		return []int{int(a.index)}, nil
	}

	selected := make([]int, 0, 4)

	for index := range len(a.committeeBits) * 8 {
		if !bitSet(a.committeeBits, index) {
			continue
		}

		if index >= committees {
			return nil, fmt.Errorf("committee %d of %d", index, committees)
		}

		selected = append(selected, index)
	}

	if len(selected) == 0 {
		return nil, errors.New("no committee selected")
	}

	return selected, nil
}

// bitSet reports bit index of an SSZ bitfield (little-endian within a byte).
func bitSet(field []byte, index int) bool {
	return index/8 < len(field) && field[index/8]&(1<<(index%8)) != 0
}

// bitlistLength returns the number of bits of an SSZ bitlist: everything
// below its highest set bit, which marks the length.
func bitlistLength(list []byte) (int, bool) {
	if len(list) == 0 || list[len(list)-1] == 0 {
		return 0, false
	}

	return (len(list)-1)*8 + bits.Len8(list[len(list)-1]) - 1, true
}

// chainBlock is a block of the canonical chain, reduced to what is counted.
type chainBlock struct {
	slot         uint64
	root         string
	parent       string
	attestations []attestation
}

// headRef is the head the node reported in this poll.
type headRef struct {
	root string
	slot uint64
}

// slotTracker is the state of the per-slot participation. It belongs to the
// poll goroutine.
type slotTracker struct {
	// started is false until the first chain walk succeeded.
	started bool
	// base is the oldest slot that is counted: every canonical block from it
	// on is known, so nothing that includes one of its attestations is missed.
	base uint64
	// blocks is the canonical chain from base on, ascending by slot, as far
	// as it lies in the window.
	blocks []*chainBlock
	// anchor is the root of the newest canonical block below blocks: where a
	// walk ends when it has passed all of them.
	anchor string
	// fetched holds the blocks of a walk that did not finish, so a retry
	// does not ask for them again.
	fetched map[string]*chainBlock
	// committees holds the attestation committees per epoch and slot, by
	// committee index; noCommittees the epochs the node does not serve.
	committees   map[uint64]map[uint64][][]uint64
	noCommittees map[uint64]struct{}
	// hosts are the tracked ranges as last read, in hostsEpoch: they are
	// read once per epoch. hostsKey identifies the ranges the stored rows
	// were made for.
	hosts      []trackedHost
	hostsEpoch uint64
	hostsRead  bool
	hostsKey   string
	// dirty holds the slots to count again; nextSettle is the first slot
	// that has not been stored yet.
	dirty      map[uint64]struct{}
	nextSettle uint64
	// written holds what was stored per slot of the window, to store a slot
	// again only when it changed; duties the per-host counts of every slot of
	// the window that was counted with its committees (liveness.go).
	written map[uint64]fleet.SlotParticipation
	duties  map[uint64][]fleet.HostSlotParticipation
	// prunedBefore is the last prune boundary applied.
	prunedBefore uint64
	// lastError is the last failure logged as a warning.
	lastError string
}

func newSlotTracker() *slotTracker {
	return &slotTracker{
		fetched:      make(map[string]*chainBlock, 8),
		committees:   make(map[uint64]map[uint64][][]uint64, slotWindowEpochs+1),
		noCommittees: make(map[uint64]struct{}, slotWindowEpochs+1),
		dirty:        make(map[uint64]struct{}, 64),
		written:      make(map[uint64]fleet.SlotParticipation, 128),
		duties:       make(map[uint64][]fleet.HostSlotParticipation, 128),
	}
}

// setHosts takes the tracked ranges read in epoch.
func (t *slotTracker) setHosts(hosts []trackedHost, epoch uint64) {
	t.hosts = hosts
	t.hostsEpoch = epoch
	t.hostsRead = true
}

// indexOf returns the position of the block with root in blocks, or -1.
func (t *slotTracker) indexOf(root string) int {
	for i := len(t.blocks) - 1; i >= 0; i-- {
		if t.blocks[i].root == root {
			return i
		}
	}

	return -1
}

// touch marks the slots a block changes: its own and the ones it includes
// attestations of.
func (t *slotTracker) touch(block *chainBlock) {
	t.dirty[block.slot] = struct{}{}

	for i := range block.attestations {
		t.dirty[block.attestations[i].slot] = struct{}{}
	}
}

// keep drops the blocks after the first count: they are no longer canonical.
func (t *slotTracker) keep(count int) {
	for _, orphan := range t.blocks[count:] {
		t.touch(orphan)
	}

	clear(t.blocks[count:])
	t.blocks = t.blocks[:count]
}

// reset starts over at base: nothing known connects to the chain.
func (t *slotTracker) reset(base uint64, anchor string) {
	t.keep(0)

	t.started = true
	t.base = base
	t.anchor = anchor
	t.nextSettle = base
}

// extend appends new canonical blocks, ascending.
func (t *slotTracker) extend(blocks []*chainBlock) {
	for _, block := range blocks {
		t.touch(block)
	}

	t.blocks = append(t.blocks, blocks...)
}

// forget drops what lies before the window.
func (t *slotTracker) forget(windowStart, firstEpoch uint64) {
	drop := 0
	for drop < len(t.blocks) && t.blocks[drop].slot < windowStart {
		drop++
	}

	if drop > 0 {
		t.anchor = t.blocks[drop-1].root
		t.blocks = slices.Delete(t.blocks, 0, drop)
	}

	for epoch := range t.committees {
		if epoch < firstEpoch {
			delete(t.committees, epoch)
		}
	}

	for epoch := range t.noCommittees {
		if epoch < firstEpoch {
			delete(t.noCommittees, epoch)
		}
	}

	for slot := range t.written {
		if slot < windowStart {
			delete(t.written, slot)
		}
	}

	for slot := range t.duties {
		if slot < windowStart {
			delete(t.duties, slot)
		}
	}

	for slot := range t.dirty {
		if slot < windowStart || slot < t.base {
			delete(t.dirty, slot)
		}
	}
}

// proposed reports whether the slot has a canonical block.
func (t *slotTracker) proposed(slot uint64) bool {
	_, found := slices.BinarySearchFunc(t.blocks, slot, func(block *chainBlock, slot uint64) int {
		switch {
		case block.slot < slot:
			return -1
		case block.slot > slot:
			return 1
		default:
			return 0
		}
	})

	return found
}

// participation counts one slot from the blocks known: who of the tracked
// validators had to attest in it (committees are the slot's, by committee
// index) and whose attestation a later canonical block carries. The error is
// the first aggregate that did not fit the committees; it is not counted.
//
// duties are the counts of every host with a duty in the slot, in the order
// of hosts; they are summed into the per-epoch liveness (liveness.go). The
// row's Hosts are the hosts that deviated: the ones with a duty in the slot of
// which an attestation is missing. A host that attested everything is not in
// it, which on a healthy fleet is every host: what is stored and sent per slot
// does not grow with the fleet. The work is one pass over the slot's
// committees, with a binary search per member (eachOwner).
func (t *slotTracker) participation(
	slot uint64, committees [][]uint64, hosts []trackedHost,
) (row fleet.SlotParticipation, duties []fleet.HostSlotParticipation, err error) {
	row = fleet.SlotParticipation{Slot: slot, Proposed: t.proposed(slot)}

	if len(hosts) == 0 {
		return row, nil, nil
	}

	reach := hostReach(hosts)

	// The tracked validators with a duty in the slot, and whether their
	// attestation was seen. A validator sits in one committee per epoch.
	included := make(map[uint64]bool, 64)

	for _, committee := range committees {
		for _, validator := range committee {
			if tracked(hosts, reach, validator) {
				included[validator] = false
			}
		}
	}

	for _, block := range t.blocks {
		if block.slot <= slot {
			continue
		}

		for i := range block.attestations {
			vote := &block.attestations[i]
			if vote.slot != slot {
				continue
			}

			voteErr := vote.attesters(committees, func(validator uint64) {
				if _, duty := included[validator]; duty {
					included[validator] = true
				}
			})
			if voteErr != nil && err == nil {
				err = fmt.Errorf("attestation for slot %d in the block of slot %d: %w",
					slot, block.slot, voteErr)
			}
		}
	}

	row.Assigned = uint64(len(included))

	// Per host: its duties and how many of them were included.
	counts := make(map[string]*fleet.HostSlotParticipation, 8)

	for validator, got := range included {
		if got {
			row.Included++
		}

		eachOwner(hosts, reach, validator, func(host int) {
			name := hosts[host].name

			c := counts[name]
			if c == nil {
				c = &fleet.HostSlotParticipation{Host: name}
				counts[name] = c
			}

			c.Assigned++

			if got {
				c.Included++
			}
		})
	}

	duties = make([]fleet.HostSlotParticipation, 0, len(counts))

	for i := range hosts {
		c := counts[hosts[i].name]
		if c == nil {
			continue
		}

		// A host with several ranges is listed once.
		delete(counts, hosts[i].name)

		duties = append(duties, *c)

		if c.Included < c.Assigned {
			row.Hosts = append(row.Hosts, *c)
		}
	}

	return row, duties, err
}

// hostReach is, for hosts ordered by the start of their range, how far the
// ranges up to and including each host reach: the largest end so far. With it
// the owners of a validator are found without looking at every host.
func hostReach(hosts []trackedHost) []uint64 {
	reach := make([]uint64, len(hosts))

	var far uint64

	for i := range hosts {
		far = max(far, hosts[i].end)
		reach[i] = far
	}

	return reach
}

// eachOwner calls visit with the index of every host whose range holds the
// validator. hosts are ordered by start and reach is their hostReach: the
// search starts at the last host that begins at or before the validator and
// walks back for as long as an earlier range can still reach it. Ranges
// normally do not overlap, so that is one host.
func eachOwner(hosts []trackedHost, reach []uint64, validator uint64, visit func(host int)) {
	i := sort.Search(len(hosts), func(i int) bool { return hosts[i].start > validator }) - 1

	for ; i >= 0 && reach[i] > validator; i-- {
		if validator < hosts[i].end {
			visit(i)
		}
	}
}

// tracked reports whether a validator lies in the range of any host.
func tracked(hosts []trackedHost, reach []uint64, validator uint64) bool {
	found := false

	eachOwner(hosts, reach, validator, func(int) { found = true })

	return found
}

// sameParticipation reports whether two versions of a slot are the same row.
func sameParticipation(a, b *fleet.SlotParticipation) bool {
	return a.Slot == b.Slot && a.Proposed == b.Proposed && a.Assigned == b.Assigned &&
		a.Included == b.Included && slices.Equal(a.Hosts, b.Hosts)
}

// hostsKey identifies a set of tracked ranges.
func hostsKey(hosts []trackedHost) string {
	var key strings.Builder

	for _, host := range hosts {
		fmt.Fprintf(&key, "%s:%d-%d;", host.name, host.start, host.end)
	}

	return key.String()
}

// Beacon API answers, reduced to the fields used.
type (
	blockResponse struct {
		Data struct {
			Message struct {
				Slot *beaconUint `json:"slot"`
				//nolint:tagliatelle // field name of the standard Beacon API
				ParentRoot string `json:"parent_root"`
				Body       struct {
					Attestations []struct {
						//nolint:tagliatelle // field name of the standard Beacon API
						AggregationBits string `json:"aggregation_bits"`
						// Since Electra; absent before.
						//nolint:tagliatelle // field name of the standard Beacon API
						CommitteeBits *string `json:"committee_bits"`
						Data          struct {
							Slot  *beaconUint `json:"slot"`
							Index beaconUint  `json:"index"`
						} `json:"data"`
					} `json:"attestations"`
				} `json:"body"`
			} `json:"message"`
		} `json:"data"`
	}

	committeesResponse struct {
		Data []struct {
			Index      beaconUint   `json:"index"`
			Slot       beaconUint   `json:"slot"`
			Validators []beaconUint `json:"validators"`
		} `json:"data"`
	}

	syncingResponse struct {
		Data struct {
			//nolint:tagliatelle // field name of the standard Beacon API
			IsSyncing *bool `json:"is_syncing"`
		} `json:"data"`
	}
)

// trackSlots brings the per-slot participation up to date and publishes the
// epochs it settled. A failure is published with them: the liveness of the
// epochs it holds up stays missing, and the scheduler's gate says why.
func (s *service) trackSlots(ctx context.Context, now time.Time) {
	settled, err := s.refreshSlots(ctx, now)
	if err == nil {
		s.slots.lastError = ""
		s.publishLiveness(settled, "")

		return
	}

	if ctx.Err() != nil {
		return
	}

	s.publishLiveness(settled, err.Error())

	// A node that keeps failing the same way is reported once.
	level := logrus.WarnLevel
	if err.Error() == s.slots.lastError {
		level = logrus.DebugLevel
	}

	s.slots.lastError = err.Error()

	s.log.WithError(err).Log(level, "Per-slot participation refresh failed")
}

// refreshSlots follows the chain to the head, counts the slots that changed
// and stores them, and prunes old rows. It returns the liveness of the
// settled epochs of the window that were counted completely.
func (s *service) refreshSlots(ctx context.Context, now time.Time) ([]epochLiveness, error) {
	if now.Before(s.timing.genesis) {
		return nil, nil
	}

	var (
		tracker = s.slots
		perSlot = s.timing.slotsPerEpoch
		current = s.timing.epochAt(now)
	)

	firstEpoch := current - min(current, slotWindowEpochs)
	tracker.forget(firstEpoch*perSlot, firstEpoch)

	if err := s.followChain(ctx, current); err != nil {
		return nil, err
	}

	// A host that registers shows up with the next epoch at the latest; what
	// is in memory of the window is then counted again with it.
	if !tracker.hostsRead || tracker.hostsEpoch != current {
		hosts, err := s.opts.Store.ListHosts(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing hosts: %w", err)
		}

		tracker.setHosts(s.trackedHosts(hosts), current)
	}

	tracked := tracker.hosts
	first := max(tracker.base, firstEpoch*perSlot)

	horizon, settled := s.settledSlot(ctx, now)
	if !settled || horizon < first {
		return nil, nil
	}

	// A fleet that changed is counted again over the whole window.
	if key := hostsKey(tracked); key != tracker.hostsKey {
		tracker.hostsKey = key
		tracker.nextSettle = first
	}

	for slot := max(tracker.nextSettle, first); slot <= horizon; slot++ {
		tracker.dirty[slot] = struct{}{}
	}

	tracker.nextSettle = horizon + 1

	countErr := s.storeSlots(ctx, current, first, horizon, tracked)
	liveness := tracker.settledLiveness(firstEpoch, current, perSlot, horizon)

	if err := s.pruneSlots(ctx, now); err != nil && countErr == nil {
		return liveness, err
	}

	return liveness, countErr
}

// followChain walks from the head the node reported back to a block that is
// known and makes the blocks on the way the end of the canonical chain.
// Blocks that are no longer on it are dropped. With nothing to connect to it
// starts over slotBackfillEpochs before the current epoch.
func (s *service) followChain(ctx context.Context, current uint64) error {
	tracker := s.slots
	floor := (current - min(current, slotBackfillEpochs)) * s.timing.slotsPerEpoch

	if s.head.root == "" {
		return errors.New("reading head: answer has no root")
	}

	var (
		fresh     = make([]*chainBlock, 0, 2)
		root      = s.head.root
		connected = -1
		below     string
	)

	for {
		if tracker.started && root == tracker.anchor {
			connected = 0

			break
		}

		if index := tracker.indexOf(root); index >= 0 {
			connected = index + 1

			break
		}

		block, err := s.fetchBlock(ctx, root)
		if err != nil {
			return err
		}

		if block.slot < floor {
			below = root

			break
		}

		if len(fresh) > 0 && block.slot >= fresh[len(fresh)-1].slot {
			return fmt.Errorf("block %s of slot %d is the parent of a block of slot %d",
				root, block.slot, fresh[len(fresh)-1].slot)
		}

		fresh = append(fresh, block)

		// The first block of a chain has no parent to read: its parent root
		// is all zeros.
		if block.slot == 0 || strings.Trim(block.parent, "0x") == "" {
			break
		}

		root = block.parent
	}

	slices.Reverse(fresh)

	if connected >= 0 {
		tracker.keep(connected)
	} else {
		tracker.reset(floor, below)
	}

	tracker.extend(fresh)
	clear(tracker.fetched)

	return nil
}

// fetchBlock reads one block by root.
func (s *service) fetchBlock(ctx context.Context, root string) (*chainBlock, error) {
	if block, ok := s.slots.fetched[root]; ok {
		return block, nil
	}

	if !plausibleRoot(root) {
		return nil, errors.New("reading block: the node named a root that is not one")
	}

	var answer blockResponse
	if err := s.client.get(ctx, "/eth/v2/beacon/blocks/"+root, &answer); err != nil {
		return nil, fmt.Errorf("reading block %s: %w", root, err)
	}

	message := &answer.Data.Message
	if message.Slot == nil {
		return nil, fmt.Errorf("reading block %s: answer has no slot", root)
	}

	block := &chainBlock{
		slot:         uint64(*message.Slot),
		root:         root,
		parent:       message.ParentRoot,
		attestations: make([]attestation, 0, len(message.Body.Attestations)),
	}

	for i := range message.Body.Attestations {
		raw := &message.Body.Attestations[i]

		vote := attestation{index: uint64(raw.Data.Index)}

		var err error
		if vote.aggregationBits, err = decodeHex(raw.AggregationBits); err != nil || raw.Data.Slot == nil {
			s.warnOnce("attestation-shape", logrus.Fields{"block_slot": block.slot},
				"A block carries an attestation that cannot be read; it is not counted")

			continue
		}

		vote.slot = uint64(*raw.Data.Slot)

		if raw.CommitteeBits != nil {
			if vote.committeeBits, err = decodeHex(*raw.CommitteeBits); err != nil {
				s.warnOnce("attestation-shape", logrus.Fields{"block_slot": block.slot},
					"A block carries an attestation that cannot be read; it is not counted")

				continue
			}
		}

		block.attestations = append(block.attestations, vote)
	}

	s.slots.fetched[root] = block

	return block, nil
}

// plausibleRoot reports whether root is 0x-prefixed hex of at most 32 bytes:
// it becomes part of a request path.
func plausibleRoot(root string) bool {
	if len(root) < 3 || len(root) > maxRootLength || !strings.HasPrefix(root, "0x") {
		return false
	}

	for _, char := range root[2:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}

	return true
}

// decodeHex reads a 0x-prefixed hex string. The result is never nil.
func decodeHex(text string) ([]byte, error) {
	raw, ok := strings.CutPrefix(text, "0x")
	if !ok {
		return nil, errors.New("not 0x-prefixed")
	}

	out := make([]byte, hex.DecodedLen(len(raw)))
	if _, err := hex.Decode(out, []byte(raw)); err != nil {
		return nil, err
	}

	return out, nil
}

// settledSlot returns the newest slot that is stored in this poll: the one
// slotSettleLag behind the clock. When the node has no block that recent,
// either proposals were missed or the node is behind; a node that does not
// say it is in sync is believed up to the slot before its head only.
func (s *service) settledSlot(ctx context.Context, now time.Time) (uint64, bool) {
	clock := s.timing.slotAt(now)
	if clock < slotSettleLag {
		return 0, false
	}

	horizon := clock - slotSettleLag
	if s.head.slot >= horizon {
		return horizon, true
	}

	var answer syncingResponse

	err := s.client.get(ctx, "/eth/v1/node/syncing", &answer)
	if err == nil && answer.Data.IsSyncing != nil && !*answer.Data.IsSyncing {
		return horizon, true
	}

	if err != nil {
		s.log.WithError(err).Debug("Beacon node does not say whether it is in sync")
	}

	if s.head.slot == 0 {
		return 0, false
	}

	return s.head.slot - 1, true
}

// storeSlots counts the dirty slots of [first, horizon] and stores the ones
// that changed. A slot whose committees the node did not answer for stays
// dirty and is tried again.
func (s *service) storeSlots(
	ctx context.Context, current, first, horizon uint64, tracked []trackedHost,
) error {
	tracker := s.slots

	pending := make([]uint64, 0, len(tracker.dirty))

	for slot := range tracker.dirty {
		if slot >= first && slot <= horizon {
			pending = append(pending, slot)
		}
	}

	slices.Sort(pending)

	var (
		rows     = make([]fleet.SlotParticipation, 0, len(pending))
		done     = make([]uint64, 0, len(pending))
		countErr error
	)

	for _, slot := range pending {
		var committees [][]uint64

		if len(tracked) > 0 {
			epoch, served, err := s.epochCommittees(ctx, slot/s.timing.slotsPerEpoch, current)
			if err != nil {
				countErr = err

				break
			}

			if !served {
				// Nobody can say who had to attest: the slot gets no row, and
				// its epoch no liveness.
				delete(tracker.duties, slot)

				done = append(done, slot)

				continue
			}

			committees = epoch[slot]
		}

		row, duties, err := tracker.participation(slot, committees, tracked)
		tracker.duties[slot] = duties

		if err != nil {
			s.warnOnce("attestation-fit", logrus.Fields{"slot": slot, "reason": err.Error()},
				"An attestation does not fit the committees of its slot; it is not counted")
		}

		done = append(done, slot)

		if before, stored := tracker.written[slot]; !stored || !sameParticipation(&before, &row) {
			rows = append(rows, row)
		}
	}

	if err := s.opts.Store.SaveSlotParticipation(ctx, rows); err != nil {
		return fmt.Errorf("saving slot participation: %w", err)
	}

	for i := range rows {
		tracker.written[rows[i].Slot] = rows[i]
	}

	for _, slot := range done {
		delete(tracker.dirty, slot)
	}

	if len(rows) > 0 {
		s.log.WithFields(logrus.Fields{
			"slots":  len(rows),
			"newest": rows[len(rows)-1].Slot,
		}).Debug("Stored slot participation")
	}

	return countErr
}

// epochCommittees returns the attestation committees of an epoch per slot,
// by committee index. served is false when the node does not answer for the
// epoch. An epoch before the current one is asked for once: nodes serve the
// previous epoch from the head state and refuse older ones; the current
// epoch is asked for until it is answered.
func (s *service) epochCommittees(
	ctx context.Context, epoch, current uint64,
) (committees map[uint64][][]uint64, served bool, err error) {
	tracker := s.slots

	if known, ok := tracker.committees[epoch]; ok {
		return known, true, nil
	}

	if _, refused := tracker.noCommittees[epoch]; refused {
		return nil, false, nil
	}

	var answer committeesResponse

	path := "/eth/v1/beacon/states/head/committees?epoch=" + strconv.FormatUint(epoch, 10)
	if err := s.client.get(ctx, path, &answer); err != nil {
		var status *statusError

		if ctx.Err() == nil && epoch < current &&
			(errors.As(err, &status) || errors.Is(err, errNotServed)) {
			tracker.noCommittees[epoch] = struct{}{}

			s.log.WithError(err).WithField("epoch", epoch).Debug(
				"Beacon node does not serve the committees of this older epoch")

			return nil, false, nil
		}

		return nil, false, fmt.Errorf("reading committees of epoch %d: %w", epoch, err)
	}

	committees = make(map[uint64][][]uint64, s.timing.slotsPerEpoch)

	for i := range answer.Data {
		entry := &answer.Data[i]

		slot, index := uint64(entry.Slot), uint64(entry.Index)
		if slot/s.timing.slotsPerEpoch != epoch || index >= maxCommitteesPerSlot {
			return nil, false, fmt.Errorf(
				"reading committees of epoch %d: committee %d of slot %d is out of range",
				epoch, index, slot)
		}

		members := make([]uint64, 0, len(entry.Validators))
		for _, validator := range entry.Validators {
			members = append(members, uint64(validator))
		}

		for uint64(len(committees[slot])) <= index {
			committees[slot] = append(committees[slot], nil)
		}

		committees[slot][index] = members
	}

	tracker.committees[epoch] = committees

	return committees, true, nil
}

// maxCommitteesPerSlot bounds a committee index in an answer; the mainnet
// preset has 64 committees per slot.
const maxCommitteesPerSlot = 1 << 12

// pruneSlots deletes the slot rows older than the retention, once per epoch.
func (s *service) pruneSlots(ctx context.Context, now time.Time) error {
	before := s.timing.slotAt(now.Add(-s.slotRetention))
	if before == 0 || before < s.slots.prunedBefore+s.timing.slotsPerEpoch {
		return nil
	}

	if err := s.opts.Store.PruneSlotParticipation(ctx, before); err != nil {
		return fmt.Errorf("pruning slot participation: %w", err)
	}

	s.slots.prunedBefore = before

	return nil
}
