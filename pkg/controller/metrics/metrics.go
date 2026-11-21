// Package metrics is the controller's Prometheus collector: agent API
// request counters and the fleet state read at scrape time.
package metrics

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
)

// collectTimeout bounds the store reads of one scrape.
const collectTimeout = 10 * time.Second

// metrics are the controller's Prometheus metrics. Fleet state is read from
// the store and the last plan at scrape time, so the numbers are never stale
// and nothing has to be kept in step by hand.
//
// /metrics is unauthenticated. The series are fleet-wide; label values are
// endpoint names, status codes and rollout states only: never a host name, a
// package version, a kernel or an address.
type metrics struct {
	log  logrus.FieldLogger
	opts Options

	agentRequests *prometheus.CounterVec
	agentDuration *prometheus.HistogramVec

	hosts               *prometheus.Desc
	hostsStale          *prometheus.Desc
	hostsRebootRequired *prometheus.Desc
	leasesOpen          *prometheus.Desc
	rollouts            *prometheus.Desc
	gateOK              *prometheus.Desc
	participation       *prometheus.Desc
	finalityLag         *prometheus.Desc
	budgetUnavailable   *prometheus.Desc
	budgetAllowed       *prometheus.Desc
}

// Metrics counts agent API requests and collects the fleet state.
type Metrics interface {
	prometheus.Collector
	// AgentRequest counts an agent API request by endpoint and status code.
	AgentRequest(endpoint string, status int)
	// AgentAnswered observes the time to answer a request that succeeded.
	AgentAnswered(endpoint string, took time.Duration)
}

// Store is what the collector reads of the fleet.
type Store interface {
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	ListRollouts(ctx context.Context, filter store.RolloutFilter) ([]*fleet.Rollout, error)
	ListLeases(ctx context.Context, filter store.LeaseFilter) ([]*fleet.Lease, error)
}

// Options are the collector's sources.
type Options struct {
	Store     Store
	Scheduler scheduler.Service
	// Gate is the network gate as the controller reports it.
	Gate func() scheduler.Gate
	Now  func() time.Time
}

var _ Metrics = (*metrics)(nil)

// New registers the controller's metrics with reg.
func New(log logrus.FieldLogger, reg prometheus.Registerer, opts Options) (Metrics, error) {
	m := &metrics{
		log:  log,
		opts: opts,
		agentRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "upgradoor_agent_requests_total",
			Help: "Agent API requests by endpoint and HTTP status code.",
		}, []string{"endpoint", "code"}),
		agentDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "upgradoor_agent_request_duration_seconds",
			Help: "Time to answer an agent API request that succeeded, by endpoint.",
			Buckets: []float64{
				0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
			},
		}, []string{"endpoint"}),
		hosts: prometheus.NewDesc("upgradoor_hosts",
			"Number of hosts known to the controller.", nil, nil),
		hostsStale: prometheus.NewDesc("upgradoor_hosts_stale",
			"Number of hosts whose agent has been silent for longer than hostStaleAfter.",
			nil, nil),
		hostsRebootRequired: prometheus.NewDesc("upgradoor_hosts_reboot_required",
			"Number of hosts that report a required reboot.", nil, nil),
		leasesOpen: prometheus.NewDesc("upgradoor_leases_open",
			"Number of open leases.", nil, nil),
		rollouts: prometheus.NewDesc("upgradoor_rollouts",
			"Number of open rollouts by state.", []string{"state"}, nil),
		gateOK: prometheus.NewDesc("upgradoor_network_gate_ok",
			"1 when the network gate allows new leases, 0 otherwise.", nil, nil),
		participation: prometheus.NewDesc("upgradoor_network_participation_ratio",
			"Share of tracked validators that attested in the last complete epoch.", nil, nil),
		finalityLag: prometheus.NewDesc("upgradoor_network_finality_lag_epochs",
			"Epochs between the current and the finalized epoch.", nil, nil),
		budgetUnavailable: prometheus.NewDesc("upgradoor_budget_unavailable_weight",
			"Validator weight that is unavailable or held by a lease.", nil, nil),
		budgetAllowed: prometheus.NewDesc("upgradoor_budget_allowed_weight",
			"Validator weight that may be unavailable at once.", nil, nil),
	}

	if err := reg.Register(m.agentRequests); err != nil {
		return nil, fmt.Errorf("api: register metrics: %w", err)
	}

	if err := reg.Register(m.agentDuration); err != nil {
		return nil, fmt.Errorf("api: register metrics: %w", err)
	}

	if err := reg.Register(m); err != nil {
		return nil, fmt.Errorf("api: register metrics: %w", err)
	}

	return m, nil
}

// agentRequest counts one answered agent request.
func (m *metrics) AgentRequest(endpoint string, status int) {
	m.agentRequests.WithLabelValues(endpoint, strconv.Itoa(status)).Inc()
}

// agentAnswered records the time a successful agent request took.
func (m *metrics) AgentAnswered(endpoint string, took time.Duration) {
	m.agentDuration.WithLabelValues(endpoint).Observe(took.Seconds())
}

// Describe implements prometheus.Collector.
func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		m.hosts, m.hostsStale, m.hostsRebootRequired, m.leasesOpen, m.rollouts,
		m.gateOK, m.participation, m.finalityLag, m.budgetUnavailable, m.budgetAllowed,
	} {
		ch <- desc
	}
}

// Collect implements prometheus.Collector. A group whose source fails is
// left out of the scrape and logged; the other groups are still served.
func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	plan := m.opts.Scheduler.Plan()

	m.collectNetwork(ch, plan)

	if err := m.collectHosts(ctx, ch); err != nil {
		m.log.WithError(err).Warn("Cannot collect host metrics")
	}

	if err := m.collectRollouts(ctx, ch); err != nil {
		m.log.WithError(err).Warn("Cannot collect rollout metrics")
	}
}

func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}

func (m *metrics) collectNetwork(ch chan<- prometheus.Metric, plan *scheduler.Plan) {
	gate := m.opts.Gate()

	ok := 0.0
	if gate.OK {
		ok = 1
	}

	gauge(ch, m.gateOK, ok)
	gauge(ch, m.participation, gate.Network.Participation())
	gauge(ch, m.finalityLag, float64(gate.Network.FinalityLagEpochs))

	if plan != nil {
		gauge(ch, m.budgetUnavailable, float64(plan.Budget.UnavailableWeight))
		gauge(ch, m.budgetAllowed, plan.Budget.AllowedWeight)
	}
}

func (m *metrics) collectHosts(ctx context.Context, ch chan<- prometheus.Metric) error {
	hosts, err := m.opts.Store.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}

	settings, err := m.opts.Scheduler.Settings(ctx)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}

	now := m.opts.Now()
	stale, reboot := 0, 0

	for _, host := range hosts {
		if host.Stale(now, settings.HostStaleAfter.Std()) {
			stale++
		}

		if host.Status != nil && host.Status.OS.RebootRequired {
			reboot++
		}
	}

	gauge(ch, m.hosts, float64(len(hosts)))
	gauge(ch, m.hostsStale, float64(stale))
	gauge(ch, m.hostsRebootRequired, float64(reboot))

	return nil
}

func (m *metrics) collectRollouts(ctx context.Context, ch chan<- prometheus.Metric) error {
	leases, err := m.opts.Store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return fmt.Errorf("list leases: %w", err)
	}

	gauge(ch, m.leasesOpen, float64(len(leases)))

	rollouts, err := m.opts.Store.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
	if err != nil {
		return fmt.Errorf("list rollouts: %w", err)
	}

	// Closed rollouts accumulate without bound, so only open ones are
	// counted. Both open states are always present.
	counts := map[fleet.RolloutState]int{
		fleet.RolloutActive: 0,
		fleet.RolloutHalted: 0,
	}

	for _, rollout := range rollouts {
		counts[rollout.State]++
	}

	for state, n := range counts {
		gauge(ch, m.rollouts, float64(n), string(state))
	}

	return nil
}
