// Package validators keeps the devnet's validator ranges inventory: which
// inventory host runs which validator indices. It fetches the inventory
// periodically and stores the last good version, which is where the store
// takes fleet.Host.Validators from.
package validators

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

const (
	defaultInterval      = 2 * time.Minute
	defaultRetryInterval = 30 * time.Second
	defaultTimeout       = 30 * time.Second
	// maxBody bounds the inventory document.
	maxBody = 8 << 20
	// RefreshFloor is the age of the last fetch below which Refresh does not
	// fetch again.
	RefreshFloor = 10 * time.Second
)

// Store is what the service keeps the inventory in.
type Store interface {
	ValidatorRanges(ctx context.Context) (*fleet.ValidatorRanges, error)
	SetValidatorRanges(ctx context.Context, ranges *fleet.ValidatorRanges) error
}

// Options configure the service.
type Options struct {
	// URL is the inventory; empty assigns no validators to any host.
	URL string
	// Store holds the last good version. Required.
	Store Store
	// Interval is the time between two fetches (2m when zero),
	// RetryInterval that after a failed one (30s when zero).
	Interval      time.Duration
	RetryInterval time.Duration
	// Client fetches the inventory; one with a 30s timeout when nil.
	Client *http.Client
	// UserAgent is sent with every fetch.
	UserAgent string
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Changed, when set, is called after a new version was stored.
	Changed func()
}

// Service keeps the inventory.
type Service interface {
	// Start drops a stored version of another URL, then fetches the
	// inventory now and every Interval until Stop.
	Start(ctx context.Context) error
	Stop() error
	// Error says why the last fetch failed; empty while it succeeded.
	Error() string
	// Status returns the state of the inventory. It never blocks on the
	// network.
	Status() Status
	// Refresh fetches the inventory now, unless no URL is configured, a
	// fetch is running or the last one is younger than RefreshFloor.
	Refresh(ctx context.Context) Refreshed
}

// Status is the state of the inventory.
type Status struct {
	// URL is the inventory URL without credentials; empty when none is
	// configured.
	URL string
	// AttemptAt is the last fetch, SuccessAt the last one that succeeded
	// (an unchanged answer included), ChangedAt the last change of the
	// ranges. Zero for none since the start; ChangedAt starts as the time
	// the stored version was fetched.
	AttemptAt time.Time
	SuccessAt time.Time
	ChangedAt time.Time
	// ETag and LastModified are those of the stored version.
	ETag         string
	LastModified string
	// Error says why the last fetch failed; empty while it succeeded.
	Error string
	// Ranges are the stored ranges, ordered by start.
	Ranges []fleet.HostRange
}

// Refreshed is what Refresh did.
type Refreshed struct {
	// Fetched says a fetch ran; Changed that it changed the ranges, Error
	// why it failed.
	Fetched bool
	Changed bool
	Error   string
	// Reason says why no fetch ran; Next is when one may run again, when
	// the floor was the reason.
	Reason string
	Next   time.Time
}

type service struct {
	log  logrus.FieldLogger
	opts Options
	// url is opts.URL without credentials.
	url string

	// fetchMu serializes the fetches; current is the stored version and
	// changes only under fetchMu and mu.
	fetchMu sync.Mutex
	current *fleet.ValidatorRanges

	mu        sync.Mutex
	problem   string
	attemptAt time.Time
	successAt time.Time
	changedAt time.Time

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var _ Service = (*service)(nil)

// New returns the service. Start runs it.
func New(log logrus.FieldLogger, opts Options) Service { //nolint:gocritic // hugeParam: copied once.
	if opts.Interval <= 0 {
		opts.Interval = defaultInterval
	}

	if opts.RetryInterval <= 0 {
		opts.RetryInterval = defaultRetryInterval
	}

	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: defaultTimeout}
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &service{log: log.WithField("package", "validators"), opts: opts, url: redactURL(opts.URL)}
}

// redactURL drops the user info of a URL.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}

	parsed.User = nil

	return parsed.String()
}

// Start implements Service.
func (s *service) Start(ctx context.Context) error {
	stored, err := s.opts.Store.ValidatorRanges(ctx)
	if err != nil {
		return fmt.Errorf("validators: %w", err)
	}

	if stored.URL != s.opts.URL {
		if len(stored.Ranges) > 0 {
			s.log.WithField("url", stored.URL).
				Info("Dropping the validator ranges of another inventory URL")
		}

		stored = &fleet.ValidatorRanges{URL: s.opts.URL}
		if err := s.opts.Store.SetValidatorRanges(ctx, stored); err != nil {
			return fmt.Errorf("validators: %w", err)
		}
	}

	s.mu.Lock()
	s.current = stored
	s.changedAt = stored.FetchedAt
	s.mu.Unlock()

	if s.opts.URL == "" {
		s.log.Info("No validator ranges URL: no host has validators")

		return nil
	}

	ctx, s.cancel = context.WithCancel(ctx)

	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		s.run(ctx)
	}()

	return nil
}

// Stop implements Service.
func (s *service) Stop() error {
	if s.cancel != nil {
		s.cancel()
	}

	s.wg.Wait()

	return nil
}

// Error implements Service.
func (s *service) Error() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.problem
}

// Status implements Service.
func (s *service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := Status{
		URL:       s.url,
		AttemptAt: s.attemptAt,
		SuccessAt: s.successAt,
		ChangedAt: s.changedAt,
		Error:     s.problem,
	}

	if s.current != nil {
		out.ETag = s.current.ETag
		out.LastModified = s.current.LastModified
		out.Ranges = s.current.Ranges
	}

	return out
}

// Refresh implements Service.
func (s *service) Refresh(ctx context.Context) Refreshed {
	if s.opts.URL == "" {
		return Refreshed{Reason: "no validator ranges URL is configured"}
	}

	if !s.fetchMu.TryLock() {
		return Refreshed{Reason: "a fetch is running"}
	}
	defer s.fetchMu.Unlock()

	if s.current == nil {
		return Refreshed{Reason: "the validator ranges service is not running"}
	}

	s.mu.Lock()
	next := s.attemptAt.Add(RefreshFloor)
	s.mu.Unlock()

	if s.opts.Now().Before(next) {
		return Refreshed{
			Reason: fmt.Sprintf("fetched within the last %s", RefreshFloor), Next: next.UTC(),
		}
	}

	changed, err := s.fetchLocked(ctx)
	if err != nil {
		return Refreshed{Fetched: true, Error: err.Error()}
	}

	return Refreshed{Fetched: true, Changed: changed}
}

// run fetches until ctx ends.
func (s *service) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		s.fetchMu.Lock()
		_, err := s.fetchLocked(ctx)
		s.fetchMu.Unlock()

		if ctx.Err() != nil {
			return
		}

		wait := s.opts.Interval
		if err != nil {
			wait = s.opts.RetryInterval
		}

		timer.Reset(wait)
	}
}

// fetchLocked fetches once and records the outcome; fetchMu is held. A fetch
// cut short by ctx is not recorded as a failure.
func (s *service) fetchLocked(ctx context.Context) (bool, error) {
	s.mu.Lock()
	s.attemptAt = s.opts.Now().UTC()
	s.mu.Unlock()

	changed, err := s.refresh(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.fail(err)
		}

		return false, err
	}

	s.succeed()

	return changed, nil
}

// fail records a failed fetch and warns when the reason is new.
func (s *service) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.log.WithError(err).WithField("url", s.opts.URL)
	if err.Error() == s.problem {
		entry.Debug("Fetching the validator ranges failed again; keeping the last good version")

		return
	}

	entry.Warn("Fetching the validator ranges failed; keeping the last good version")

	s.problem = err.Error()
}

// succeed clears the failure of an earlier fetch.
func (s *service) succeed() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.problem != "" {
		s.log.Info("Fetching the validator ranges works again")
	}

	s.problem = ""
	s.successAt = s.opts.Now().UTC()
}

// refresh fetches the inventory and stores it when it changed; it reports
// whether the ranges changed.
func (s *service) refresh(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.URL, http.NoBody)
	if err != nil {
		return false, err
	}

	req.Header.Set("Accept", "application/json")

	if s.opts.UserAgent != "" {
		req.Header.Set("User-Agent", s.opts.UserAgent)
	}

	if s.current.ETag != "" {
		req.Header.Set("If-None-Match", s.current.ETag)
	}

	if s.current.LastModified != "" {
		req.Header.Set("If-Modified-Since", s.current.LastModified)
	}

	resp, err := s.opts.Client.Do(req)
	if err != nil {
		// The client's error quotes the URL, which may carry credentials.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return false, urlErr.Err
		}

		return false, err
	}

	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		return false, nil
	case http.StatusOK:
	default:
		return false, fmt.Errorf("HTTP %s", resp.Status)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return false, fmt.Errorf("read: %w", err)
	}

	if len(raw) > maxBody {
		return false, fmt.Errorf("larger than %d bytes", maxBody)
	}

	ranges, err := Parse(raw)
	if err != nil {
		return false, err
	}

	next := &fleet.ValidatorRanges{
		URL:          s.opts.URL,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FetchedAt:    s.opts.Now().UTC(),
		Ranges:       ranges,
	}

	changed := !slices.Equal(ranges, s.current.Ranges)
	if !changed && next.ETag == s.current.ETag && next.LastModified == s.current.LastModified {
		return false, nil
	}

	if err := s.opts.Store.SetValidatorRanges(ctx, next); err != nil {
		return false, fmt.Errorf("store the validator ranges: %w", err)
	}

	s.mu.Lock()
	s.current = next

	if changed {
		s.changedAt = next.FetchedAt
	}
	s.mu.Unlock()

	if changed {
		s.log.WithFields(logrus.Fields{"ranges": len(ranges), "hosts": countHosts(ranges)}).
			Info("Validator ranges updated")

		if s.opts.Changed != nil {
			s.opts.Changed()
		}
	}

	return changed, nil
}

// countHosts counts the distinct hosts of ranges.
func countHosts(ranges []fleet.HostRange) int {
	hosts := make(map[string]struct{}, len(ranges))
	for i := range ranges {
		hosts[ranges[i].Host] = struct{}{}
	}

	return len(hosts)
}
