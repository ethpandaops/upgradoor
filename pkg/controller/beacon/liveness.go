package beacon

import (
	"cmp"
	"slices"
	"sort"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// maxHostValidators bounds one host's validator range. A larger range is a
// broken certificate, not a host.
const maxHostValidators = 1 << 20

// trackedHost is a host with validators: the half-open index range from its
// certificate.
type trackedHost struct {
	name  string
	start uint64
	end   uint64
}

// The per-epoch liveness the scheduler works with comes from the per-slot
// counts of slots.go: for every host and epoch, "total" is the host's
// validators with an attestation duty in the epoch and "live" the ones of them
// whose attestation a canonical block carries.
//
// An epoch is settled, and published, once the slot tracker has stored every
// one of its slots: the clock is slotSettleLag slots past its last slot, so
// the block of the first slot of the next epoch, where the last votes are
// normally included, has had its whole slot to arrive. Until it leaves the
// tracker's window its counts are revised whenever a later block includes
// more of it. An epoch the tracker could not count completely (committees not
// served, slots before the tracker's start, a count that failed) is not
// published: the scheduler then finds the liveness missing or stale and keeps
// the gate closed, and no host becomes ready by its attestations.

// livenessEpochs is how many settled epochs are kept in memory: enough for
// the longest readiness run (fleet.MaxReadyEpochs) and the gate.
const livenessEpochs = fleet.MaxReadyEpochs + 8

// epochLiveness is the liveness of one settled epoch, by host.
type epochLiveness struct {
	epoch uint64
	rows  []fleet.Liveness
}

// settledLiveness sums the per-slot duties of every epoch of [firstEpoch,
// current] that is settled and was counted completely.
func (t *slotTracker) settledLiveness(
	firstEpoch, current, perSlot, horizon uint64,
) []epochLiveness {
	out := make([]epochLiveness, 0, current-firstEpoch+1)

	for epoch := firstEpoch; epoch <= current; epoch++ {
		first, last := epoch*perSlot, (epoch+1)*perSlot-1
		if last > horizon {
			break
		}

		if first < t.base {
			continue
		}

		counts := make(map[string]*fleet.Liveness, len(t.hosts))
		complete := true

		for slot := first; slot <= last && complete; slot++ {
			duties, counted := t.duties[slot]
			if _, pending := t.dirty[slot]; pending || !counted {
				complete = false

				break
			}

			for _, duty := range duties {
				row := counts[duty.Host]
				if row == nil {
					row = &fleet.Liveness{Host: duty.Host, Epoch: epoch}
					counts[duty.Host] = row
				}

				row.Total += duty.Assigned
				row.Live += duty.Included
			}
		}

		if !complete {
			continue
		}

		// Every tracked host has a row: one whose validators had no duty
		// (not active yet, or exited) has nothing live.
		rows := make([]fleet.Liveness, 0, len(t.hosts))
		for _, host := range t.hosts {
			row := fleet.Liveness{Host: host.name, Epoch: epoch}
			if counted := counts[host.name]; counted != nil {
				row = *counted
			}

			rows = append(rows, row)
		}

		out = append(out, epochLiveness{epoch: epoch, rows: rows})
	}

	return out
}

// publishLiveness merges the settled epochs into the kept ones and sums the
// newest into the published view. problem is why this poll's count failed,
// or "".
func (s *service) publishLiveness(settled []epochLiveness, problem string) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()

	for _, epoch := range settled {
		i, found := slices.BinarySearchFunc(s.liveness, epoch.epoch,
			func(e epochLiveness, target uint64) int { return cmp.Compare(e.epoch, target) })
		if found {
			s.liveness[i] = epoch
		} else {
			s.liveness = slices.Insert(s.liveness, i, epoch)
		}
	}

	if drop := len(s.liveness) - livenessEpochs; drop > 0 {
		s.liveness = slices.Delete(s.liveness, 0, drop)
	}

	h := &s.health
	h.LivenessError = problem

	if len(s.liveness) == 0 {
		return
	}

	newest := &s.liveness[len(s.liveness)-1]

	h.LivenessKnown = true
	h.LivenessEpoch = newest.epoch
	h.Tracked, h.Live = 0, 0

	for _, row := range newest.rows {
		h.Tracked += row.Total
		h.Live += row.Live
	}
}

// Liveness returns the rows of the newest settled epochs, at most epochs of
// them, newest epoch first.
func (s *service) Liveness(epochs int) []fleet.Liveness {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()

	keep := s.liveness[len(s.liveness)-min(epochs, len(s.liveness)):]

	size := 0
	for _, epoch := range keep {
		size += len(epoch.rows)
	}

	out := make([]fleet.Liveness, 0, size)
	for i := len(keep) - 1; i >= 0; i-- {
		out = append(out, keep[i].rows...)
	}

	return out
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
