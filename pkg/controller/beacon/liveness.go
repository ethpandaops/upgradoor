package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// maxHostValidators bounds one host's validator range. A larger range is a
// broken certificate, not a host, and would only produce huge requests.
const maxHostValidators = 1 << 20

// trackedHost is a host with validators: the half-open index range from its
// certificate.
type trackedHost struct {
	name  string
	start uint64
	end   uint64
}

// livenessResponse is the answer of POST /eth/v1/validator/liveness/{epoch}.
type livenessResponse struct {
	Data []struct {
		Index beaconUint `json:"index"`
		//nolint:tagliatelle // field name of the standard Beacon API
		IsLive bool `json:"is_live"`
	} `json:"data"`
}

// refreshLiveness stores liveness for the complete epochs that are still
// missing and prunes old rows. A returned error that wraps errNotServed means
// the node does not serve the liveness endpoint.
func (s *service) refreshLiveness(ctx context.Context, now time.Time) error {
	if err := s.loadStoredEpochs(ctx); err != nil {
		return err
	}

	var collectErr error

	newest, settled := s.timing.newestSettledEpoch(now)
	if settled && (!s.unsupported || !now.Before(s.reprobeAt)) {
		collectErr = s.collect(ctx, now, newest)
	}

	if err := s.prune(ctx, now); err != nil && collectErr == nil {
		return err
	}

	return collectErr
}

// loadStoredEpochs marks the epochs a previous run already stored, so they
// are not queried again.
func (s *service) loadStoredEpochs(ctx context.Context) error {
	if s.storedLoaded {
		return nil
	}

	rows, err := s.opts.Store.ListLiveness(ctx, backfillEpochs)
	if err != nil {
		return fmt.Errorf("reading stored liveness: %w", err)
	}

	for _, row := range rows {
		s.handled[row.Epoch] = struct{}{}
	}

	s.storedLoaded = true

	return nil
}

// collect queries and stores the missing epochs up to newest, newest first.
func (s *service) collect(ctx context.Context, now time.Time, newest uint64) error {
	pending := s.pendingEpochs(newest)
	if len(pending) == 0 {
		return nil
	}

	// Hosts are read again for every epoch: new hosts appear.
	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("listing hosts: %w", err)
	}

	tracked := s.trackedHosts(hosts)

	// Rows of an earlier epoch do not describe a fleet without validators.
	s.noValidators = len(tracked) == 0

	if len(tracked) == 0 {
		// Nothing to ask for. The epoch stays pending in case a host with
		// validators registers while the node still serves it.
		return nil
	}

	indices := unionIndices(tracked)

	for _, epoch := range pending {
		err := s.collectEpoch(ctx, now, epoch, epoch == newest, tracked, indices)
		if err != nil {
			return err
		}
	}

	return nil
}

// pendingEpochs returns the unhandled epochs of the backfill window, newest
// first, and forgets handled epochs that left the window.
func (s *service) pendingEpochs(newest uint64) []uint64 {
	var oldest uint64
	if newest+1 > backfillEpochs {
		oldest = newest + 1 - backfillEpochs
	}

	for epoch := range s.handled {
		if epoch < oldest {
			delete(s.handled, epoch)
		}
	}

	pending := make([]uint64, 0, backfillEpochs)

	for epoch := newest; ; epoch-- {
		if _, done := s.handled[epoch]; !done {
			pending = append(pending, epoch)
		}

		if epoch == oldest {
			break
		}
	}

	return pending
}

// collectEpoch queries one epoch and stores a row per tracked host.
func (s *service) collectEpoch(
	ctx context.Context, now time.Time, epoch uint64, isNewest bool,
	tracked []trackedHost, indices []uint64,
) error {
	log := s.log.WithField("epoch", epoch)

	live, err := s.queryLiveness(ctx, epoch, indices)
	if err != nil {
		return s.queryFailed(ctx, now, epoch, isNewest, err)
	}

	rows := make([]fleet.Liveness, 0, len(tracked))

	for _, host := range tracked {
		row := fleet.Liveness{Host: host.name, Epoch: epoch, Total: host.end - host.start}

		for index := host.start; index < host.end; index++ {
			if _, isLive := live[index]; isLive {
				row.Live++
			}
		}

		rows = append(rows, row)
	}

	if err := s.opts.Store.SaveLiveness(ctx, rows); err != nil {
		return fmt.Errorf("saving liveness for epoch %d: %w", epoch, err)
	}

	s.handled[epoch] = struct{}{}

	if s.unsupported {
		s.unsupported = false
		s.unsupportedNote = ""

		log.Info("Beacon node serves validator liveness again")
	}

	log.WithFields(logrus.Fields{
		"hosts":      len(rows),
		"validators": len(indices),
		"live":       len(live),
	}).Debug("Stored validator liveness")

	return nil
}

// queryFailed decides what a failed liveness query means: an old epoch the
// node no longer serves (skipped for good), an endpoint the node does not
// serve at all (liveness unknown), or a transient failure (retried).
//
// Older epochs are best effort. Nodes refuse them with 400, but some answer
// an out-of-range epoch with another status (5xx included), so any HTTP
// answer ends the backfill of that epoch; only a node that did not answer at
// all (transport failure) is asked again.
func (s *service) queryFailed(
	ctx context.Context, now time.Time, epoch uint64, isNewest bool, err error,
) error {
	if ctx.Err() != nil {
		return err
	}

	var status *statusError

	notServed := errors.Is(err, errNotServed)
	answered := errors.As(err, &status)

	switch {
	case !isNewest && (notServed || answered):
		// Nodes serve the previous epoch only; an older one is asked for once.
		s.handled[epoch] = struct{}{}

		s.log.WithError(err).WithField("epoch", epoch).Debug(
			"Beacon node does not serve liveness for this older epoch")

		return nil
	case notServed:
		return s.markUnsupported(now, err)
	default:
		return fmt.Errorf("validator liveness for epoch %d: %w", epoch, err)
	}
}

// markUnsupported records that the node does not serve the liveness
// endpoint and schedules the next probe.
func (s *service) markUnsupported(now time.Time, cause error) error {
	err := fmt.Errorf("validator liveness unavailable: %w", cause)

	if !s.unsupported {
		s.log.WithError(cause).WithField("reprobe_in", reprobeInterval.String()).Warn(
			"Beacon node does not serve validator liveness (Teku needs " +
				"--beacon-liveness-tracking-enabled); liveness is unknown")
	}

	s.unsupported = true
	s.unsupportedNote = err.Error()
	s.reprobeAt = now.Add(reprobeInterval)

	return err
}

// queryLiveness asks the node which of the indices attested in epoch. The
// indices are sent in chunks; every chunk must succeed.
func (s *service) queryLiveness(
	ctx context.Context, epoch uint64, indices []uint64,
) (map[uint64]struct{}, error) {
	path := "/eth/v1/validator/liveness/" + strconv.FormatUint(epoch, 10)
	live := make(map[uint64]struct{}, len(indices))

	for offset := 0; offset < len(indices); offset += s.chunkSize {
		chunk := indices[offset:min(offset+s.chunkSize, len(indices))]

		request := make([]string, 0, len(chunk))
		for _, index := range chunk {
			request = append(request, strconv.FormatUint(index, 10))
		}

		body, err := json.Marshal(request)
		if err != nil {
			return nil, fmt.Errorf("encoding liveness request: %w", err)
		}

		var answer livenessResponse
		if err := s.client.post(ctx, path, body, &answer); err != nil {
			return nil, err
		}

		for _, entry := range answer.Data {
			if entry.IsLive {
				live[uint64(entry.Index)] = struct{}{}
			}
		}
	}

	return live, nil
}

// trackedHosts returns the hosts with validators ordered by range start and
// warns once about ranges that overlap or are implausibly large.
func (s *service) trackedHosts(hosts []*fleet.Host) []trackedHost {
	tracked := make([]trackedHost, 0, len(hosts))

	for _, host := range hosts {
		if host == nil || host.Validators.Count() == 0 {
			continue
		}

		if host.Validators.Count() > maxHostValidators {
			s.warnOnce("range:"+host.Name, logrus.Fields{
				"host":       host.Name,
				"validators": host.Validators.Count(),
			}, "Host validator range is implausibly large; its liveness is not tracked")

			continue
		}

		tracked = append(tracked, trackedHost{
			name:  host.Name,
			start: host.Validators.Start,
			end:   host.Validators.End,
		})
	}

	sort.Slice(tracked, func(i, j int) bool {
		if tracked[i].start != tracked[j].start {
			return tracked[i].start < tracked[j].start
		}

		return tracked[i].name < tracked[j].name
	})

	// The host reaching furthest so far is the one a later range overlaps.
	for i, furthest := 1, 0; i < len(tracked); i++ {
		if tracked[i].start < tracked[furthest].end {
			s.warnOnce("overlap:"+tracked[furthest].name+"|"+tracked[i].name, logrus.Fields{
				"host":       tracked[furthest].name,
				"other_host": tracked[i].name,
			}, "Validator ranges of two hosts overlap; shared validators count for both")
		}

		if tracked[i].end > tracked[furthest].end {
			furthest = i
		}
	}

	return tracked
}

// warnOnce logs a warning the first time key is seen.
func (s *service) warnOnce(key string, fields logrus.Fields, msg string) {
	if _, seen := s.warned[key]; seen {
		return
	}

	s.warned[key] = struct{}{}

	s.log.WithFields(fields).Warn(msg)
}

// unionIndices returns every validator index of the hosts once, ascending.
// The hosts must be ordered by range start.
func unionIndices(tracked []trackedHost) []uint64 {
	var total uint64
	for _, host := range tracked {
		total += host.end - host.start
	}

	indices := make([]uint64, 0, total)

	var next uint64

	for _, host := range tracked {
		for index := max(host.start, next); index < host.end; index++ {
			indices = append(indices, index)
		}

		next = max(next, host.end)
	}

	return indices
}

// prune deletes liveness rows older than the retention, once per epoch.
func (s *service) prune(ctx context.Context, now time.Time) error {
	current := s.timing.epochAt(now)
	if current <= s.retention {
		return nil
	}

	before := current - s.retention
	if before <= s.prunedBefore {
		return nil
	}

	if err := s.opts.Store.PruneLiveness(ctx, before); err != nil {
		return fmt.Errorf("pruning liveness: %w", err)
	}

	s.prunedBefore = before

	return nil
}

// publishLiveness sums the newest stored epoch into the published view. note
// is the problem of this poll, if any; it wins over a store read failure.
// While no host has validators nothing is tracked and the view says so,
// whatever older rows are still stored.
func (s *service) publishLiveness(ctx context.Context, note string) error {
	var (
		rows []fleet.Liveness
		err  error
	)

	if !s.noValidators {
		rows, err = s.opts.Store.ListLiveness(ctx, 1)
		if err != nil && note == "" {
			note = "reading stored liveness: " + err.Error()
		}
	}

	var epoch, tracked, live uint64

	for i, row := range rows {
		if i == 0 {
			epoch = row.Epoch
		}

		if row.Epoch != epoch {
			continue
		}

		tracked += row.Total
		live += row.Live
	}

	unsupported := s.unsupported

	s.setHealth(func(h *fleet.NetworkHealth) {
		if note != "" {
			h.Error = note
		}

		if unsupported {
			h.LivenessKnown = false
		}

		if err != nil {
			return
		}

		h.LivenessKnown = len(rows) > 0 && !unsupported
		h.LivenessEpoch = epoch
		h.Tracked = tracked
		h.Live = live
	})

	if err != nil {
		return fmt.Errorf("reading stored liveness: %w", err)
	}

	return nil
}
