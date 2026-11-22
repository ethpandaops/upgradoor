package beacon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

const (
	// DefaultPollInterval is the poll interval until the slot duration is
	// known; after that an unset PollInterval means one slot.
	DefaultPollInterval = 12 * time.Second

	// minPollInterval keeps a derived poll interval sane on fast chains.
	minPollInterval = time.Second
)

// Store is what the beacon service needs from the controller's store.
type Store interface {
	// ListHosts returns all hosts.
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	// SaveSlotParticipation upserts slot rows and replaces their per-host
	// rows.
	SaveSlotParticipation(ctx context.Context, rows []fleet.SlotParticipation) error
	// PruneSlotParticipation deletes the rows of slots older than beforeSlot.
	PruneSlotParticipation(ctx context.Context, beforeSlot uint64) error
}

// Options configure the beacon service.
type Options struct {
	// Endpoints are beacon API base URLs. They are tried in order; the one
	// that answered last is tried first the next time.
	Endpoints []string
	// Store holds the hosts and the per-slot participation rows.
	Store Store
	// PollInterval is the time between polls. Zero means one slot (12s until
	// the slot duration is known).
	PollInterval time.Duration
	// HTTPClient is the client used for the beacon API. Nil means a client
	// with a timeout.
	HTTPClient *http.Client
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// SlotRetention is how long the per-slot participation rows are kept.
	// Zero means DefaultSlotRetention.
	SlotRetention time.Duration
}

// service implements Service.
type service struct {
	log  logrus.FieldLogger
	opts Options
	now  func() time.Time
	// slotRetention is how long slot rows are kept.
	slotRetention time.Duration

	// lifecycle guards the fields below it.
	lifecycle sync.Mutex
	started   bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	// healthMu guards health and liveness, the state shared with Health and
	// Liveness callers. liveness holds the settled epochs, ascending.
	healthMu sync.RWMutex
	health   fleet.NetworkHealth
	liveness []epochLiveness

	// Everything below belongs to the poll goroutine.
	client *nodeClient
	timing *chainTiming
	// warned remembers warnings that are logged once.
	warned map[string]struct{}
	// timingWarned is the last warning about the chain timing that was
	// logged: it is logged again when what it says changes.
	timingWarned string
	// head is the head the node reported in the last good poll; slots is the
	// per-slot participation that follows it (slots.go).
	head  headRef
	slots *slotTracker
}

var _ Service = (*service)(nil)

// New creates the beacon service. Nothing is contacted before Start.
func New(log logrus.FieldLogger, opts Options) Service {
	svc := &service{
		log:    log.WithField("package", "beacon"),
		opts:   opts,
		now:    opts.Now,
		warned: make(map[string]struct{}, 4),

		slotRetention: opts.SlotRetention,
		slots:         newSlotTracker(),
	}

	if svc.slotRetention <= 0 {
		svc.slotRetention = DefaultSlotRetention
	}

	if svc.now == nil {
		svc.now = time.Now
	}

	return svc
}

// Start validates the options and starts polling in the background. Polling
// ends when ctx is cancelled or Stop is called.
func (s *service) Start(ctx context.Context) error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()

	if s.started {
		return errors.New("beacon service already started")
	}

	if err := s.init(); err != nil {
		return err
	}

	runCtx, cancel := context.WithCancel(ctx)

	s.started = true
	s.cancel = cancel

	s.wg.Go(func() { s.run(runCtx) })

	return nil
}

// Stop ends polling and waits for the poll goroutine. It may be called more
// than once and without Start.
func (s *service) Stop() error {
	s.lifecycle.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.lifecycle.Unlock()

	if cancel != nil {
		cancel()
	}

	s.wg.Wait()

	return nil
}

// Health returns the latest view. It never blocks on the network.
func (s *service) Health() fleet.NetworkHealth {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()

	return s.health
}

// Endpoints returns what was seen of every configured endpoint; nil before
// Start. It never blocks on the network.
func (s *service) Endpoints() []EndpointStatus {
	s.lifecycle.Lock()
	client := s.client
	s.lifecycle.Unlock()

	if client == nil {
		return nil
	}

	return client.Status()
}

// init checks the options and builds the API client.
func (s *service) init() error {
	if s.opts.Store == nil {
		return errors.New("beacon service needs a store")
	}

	client, err := newNodeClient(s.log, s.opts.HTTPClient, s.now, s.opts.Endpoints)
	if err != nil {
		return err
	}

	s.client = client

	return nil
}

// run polls until ctx ends.
func (s *service) run(ctx context.Context) {
	for {
		s.poll(ctx)

		timer := time.NewTimer(s.interval())

		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}

// interval returns the time to the next poll.
func (s *service) interval() time.Duration {
	if s.opts.PollInterval > 0 {
		return s.opts.PollInterval
	}

	if s.timing == nil {
		return DefaultPollInterval
	}

	return max(s.timing.slotDuration(s.timing.epochAt(s.now())), minPollInterval)
}

// poll refreshes the chain view and, when that worked, the per-slot
// participation and the liveness summed from it.
func (s *service) poll(ctx context.Context) {
	now := s.now()

	if err := s.refreshChain(ctx, now); err != nil {
		if ctx.Err() != nil {
			return
		}

		s.log.WithError(err).Warn("Beacon node poll failed")
		s.setHealth(func(h *fleet.NetworkHealth) { h.Error = err.Error() })

		return
	}

	s.trackSlots(ctx, now)
}

// setHealth changes the published view under the lock.
func (s *service) setHealth(change func(h *fleet.NetworkHealth)) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()

	change(&s.health)
}

// beaconUint is an unsigned integer the beacon API encodes as a decimal
// string.
type beaconUint uint64

// UnmarshalJSON implements json.Unmarshaler.
func (u *beaconUint) UnmarshalJSON(raw []byte) error {
	text := string(raw)
	if len(text) >= 2 && text[0] == '"' && text[len(text)-1] == '"' {
		text = text[1 : len(text)-1]
	}

	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return fmt.Errorf("not an unsigned integer: %q", text)
	}

	*u = beaconUint(value)

	return nil
}

// Beacon API answers, reduced to the fields used.
type (
	genesisResponse struct {
		Data struct {
			//nolint:tagliatelle // field name of the standard Beacon API
			GenesisTime *beaconUint `json:"genesis_time"`
		} `json:"data"`
	}

	specResponse struct {
		Data map[string]json.RawMessage `json:"data"`
	}

	headResponse struct {
		Data struct {
			Root   string `json:"root"`
			Header struct {
				Message struct {
					Slot *beaconUint `json:"slot"`
				} `json:"message"`
			} `json:"header"`
		} `json:"data"`
	}

	finalityResponse struct {
		Data struct {
			Finalized struct {
				Epoch *beaconUint `json:"epoch"`
			} `json:"finalized"`
		} `json:"data"`
	}
)

// loadTiming fetches genesis and spec once; it is retried on every poll
// until both are available.
func (s *service) loadTiming(ctx context.Context) error {
	if s.timing != nil {
		return nil
	}

	var genesis genesisResponse
	if err := s.client.get(ctx, "/eth/v1/beacon/genesis", &genesis); err != nil {
		return fmt.Errorf("reading genesis: %w", err)
	}

	if genesis.Data.GenesisTime == nil {
		return errors.New("reading genesis: answer has no genesis_time")
	}

	var spec specResponse
	if err := s.client.get(ctx, "/eth/v1/config/spec", &spec); err != nil {
		return fmt.Errorf("reading chain spec: %w", err)
	}

	genesisTime := time.Unix(int64(*genesis.Data.GenesisTime), 0).UTC()

	timing, err := newChainTiming(genesisTime, spec.Data)
	if err != nil {
		return fmt.Errorf("reading chain spec: %w", err)
	}

	s.timing = timing

	s.log.WithFields(logrus.Fields{
		"genesis_time":    genesisTime,
		"slots_per_epoch": timing.slotsPerEpoch,
		"slot_duration":   timing.phases[0].slot.String(),
		"slot_phases":     len(timing.phases),
	}).Info("Loaded chain timing from beacon node")

	return nil
}

// timingWarning is logged when the timing read from the node is invalid.
const timingWarning = "The chain timing of the beacon node is invalid: the network gate is closed " +
	"and no host is found ready"

// checkTiming validates the timing as it is published, on every poll. It
// returns why the timing is invalid (fleet.ValidateTiming), or "" when it is
// good. An invalid timing is logged once, and again when the reason changes;
// the scheduler closes the network gate with the reason.
func (s *service) checkTiming(current uint64, phases []fleet.SlotPhase) string {
	err := fleet.ValidateTiming(s.timing.slotsPerEpoch, phases)
	if err == nil {
		s.timingWarned = ""

		return ""
	}

	reason := strings.TrimPrefix(err.Error(), fleet.ErrInvalidTiming.Error()+": ")

	if reason != s.timingWarned {
		s.timingWarned = reason

		s.log.WithFields(logrus.Fields{
			"reason":        reason,
			"current_epoch": current,
		}).Warn(timingWarning)
	}

	return reason
}

// refreshChain reads head and finality and publishes the chain part of the
// view.
func (s *service) refreshChain(ctx context.Context, now time.Time) error {
	if err := s.loadTiming(ctx); err != nil {
		return err
	}

	current := s.timing.epochAt(now)
	slot := s.timing.slotDuration(current)
	slotSeconds := wholeSeconds(slot)

	if slot%time.Second != 0 {
		// NetworkHealth carries whole seconds only; the service itself keeps
		// the exact duration for epochs and slots.
		s.warnOnce("slot:"+slot.String(), logrus.Fields{
			"slot_duration":    slot.String(),
			"seconds_per_slot": slotSeconds,
		}, "Slot duration is not a whole number of seconds; the published "+
			"seconds per slot (and the epoch duration derived from it) is rounded")
	}

	phases := s.timing.published()
	timingError := s.checkTiming(current, phases)

	// Timing is known from here on, even when the node stops answering.
	s.setHealth(func(h *fleet.NetworkHealth) {
		h.GenesisTime = s.timing.genesis
		h.SecondsPerSlot = slotSeconds
		h.SlotsPerEpoch = s.timing.slotsPerEpoch
		h.Phases = phases
		h.TimingError = timingError
	})

	var head headResponse
	if err := s.client.get(ctx, "/eth/v1/beacon/headers/head", &head); err != nil {
		return fmt.Errorf("reading head: %w", err)
	}

	if head.Data.Header.Message.Slot == nil {
		return errors.New("reading head: answer has no slot")
	}

	var finality finalityResponse

	err := s.client.get(ctx, "/eth/v1/beacon/states/head/finality_checkpoints", &finality)
	if err != nil {
		return fmt.Errorf("reading finality: %w", err)
	}

	if finality.Data.Finalized.Epoch == nil {
		return errors.New("reading finality: answer has no finalized epoch")
	}

	headSlot := uint64(*head.Data.Header.Message.Slot)
	finalized := uint64(*finality.Data.Finalized.Epoch)

	s.client.noteHead(headSlot)

	s.head = headRef{root: head.Data.Root, slot: headSlot}

	var lag uint64
	if current > finalized {
		lag = current - finalized
	}

	s.setHealth(func(h *fleet.NetworkHealth) {
		h.UpdatedAt = now
		h.Error = ""
		h.HeadSlot = headSlot
		h.CurrentEpoch = current
		h.FinalizedEpoch = finalized
		h.FinalityLagEpochs = lag
	})

	return nil
}
