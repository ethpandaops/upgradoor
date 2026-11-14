package api

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

const labelHost = "host"

// metrics are the controller's Prometheus metrics. Fleet state is read from
// the store and the last plan at scrape time, so the numbers are never stale
// and nothing has to be kept in step by hand.
//
// /metrics is unauthenticated. Label values are host names, endpoint names,
// status codes and rollout states only: never a package version, a kernel or
// an address.
type metrics struct {
	log    logrus.FieldLogger
	server *Server

	agentRequests *prometheus.CounterVec

	hosts               *prometheus.Desc
	hostsStale          *prometheus.Desc
	hostsRebootRequired *prometheus.Desc
	hostPendingSecurity *prometheus.Desc
	hostLastSeen        *prometheus.Desc
	hostValidatorsLive  *prometheus.Desc
	hostValidatorsTotal *prometheus.Desc
	leasesOpen          *prometheus.Desc
	rollouts            *prometheus.Desc
	gateOK              *prometheus.Desc
	participation       *prometheus.Desc
	finalityLag         *prometheus.Desc
	budgetUnavailable   *prometheus.Desc
	budgetAllowed       *prometheus.Desc
}

var _ prometheus.Collector = (*metrics)(nil)

func newMetrics(log logrus.FieldLogger, reg prometheus.Registerer, server *Server) (*metrics, error) {
	host := []string{labelHost}

	m := &metrics{
		log:    log,
		server: server,
		agentRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "upgradoor_agent_requests_total",
			Help: "Agent API requests by endpoint and HTTP status code.",
		}, []string{"endpoint", "code"}),
		hosts: prometheus.NewDesc("upgradoor_hosts",
			"Number of hosts known to the controller.", nil, nil),
		hostsStale: prometheus.NewDesc("upgradoor_hosts_stale",
			"Number of hosts whose agent has been silent for longer than hostStaleAfter.",
			nil, nil),
		hostsRebootRequired: prometheus.NewDesc("upgradoor_hosts_reboot_required",
			"Number of hosts that report a required reboot.", nil, nil),
		hostPendingSecurity: prometheus.NewDesc("upgradoor_host_pending_security_updates",
			"Number of pending security updates of a host.", host, nil),
		hostLastSeen: prometheus.NewDesc("upgradoor_host_last_seen_timestamp_seconds",
			"Unix time of the last sync of a host.", host, nil),
		hostValidatorsLive: prometheus.NewDesc("upgradoor_host_validators_live",
			"Validators of a host that attested in the last complete epoch.", host, nil),
		// A gauge with a counter's suffix: the name is the one the project's
		// metric list fixes (chart and dashboards use it). It is the single
		// promlint exception, pinned by TestMetricNames.
		hostValidatorsTotal: prometheus.NewDesc("upgradoor_host_validators_total",
			"Validators operated by a host.", host, nil),
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

	if err := reg.Register(m); err != nil {
		return nil, fmt.Errorf("api: register metrics: %w", err)
	}

	return m, nil
}

// agentRequest counts one answered agent request.
func (m *metrics) agentRequest(endpoint string, status int) {
	m.agentRequests.WithLabelValues(endpoint, strconv.Itoa(status)).Inc()
}

// Describe implements prometheus.Collector.
func (m *metrics) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{
		m.hosts, m.hostsStale, m.hostsRebootRequired, m.hostPendingSecurity,
		m.hostLastSeen, m.hostValidatorsLive, m.hostValidatorsTotal, m.leasesOpen,
		m.rollouts, m.gateOK, m.participation, m.finalityLag, m.budgetUnavailable,
		m.budgetAllowed,
	} {
		ch <- desc
	}
}

// Collect implements prometheus.Collector. A group whose source fails is
// left out of the scrape and logged; the other groups are still served.
func (m *metrics) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()

	plan := m.server.opts.Scheduler.Plan()

	m.collectNetwork(ch, plan)

	if err := m.collectHosts(ctx, ch, plan); err != nil {
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
	gate := m.server.gate()

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

func (m *metrics) collectHosts(
	ctx context.Context, ch chan<- prometheus.Metric, plan *scheduler.Plan,
) error {
	hosts, err := m.server.opts.Store.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}

	settings, err := m.server.opts.Scheduler.Settings(ctx)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}

	now := m.server.now()
	stale, reboot := 0, 0

	for _, host := range hosts {
		if host.Stale(now, settings.HostStaleAfter.Std()) {
			stale++
		}

		gauge(ch, m.hostLastSeen, float64(host.LastSeen.UnixNano())/1e9, host.Name)
		gauge(ch, m.hostValidatorsTotal, float64(host.Weight()), host.Name)

		if host.Status != nil {
			if host.Status.OS.RebootRequired {
				reboot++
			}

			gauge(ch, m.hostPendingSecurity, float64(host.Status.Pending.Security), host.Name)
		}

		// Live validators are only known once the plan has liveness data; a
		// missing series means unknown, not zero.
		if plan != nil {
			if hp, ok := plan.Hosts[host.Name]; ok && hp.LiveKnown {
				gauge(ch, m.hostValidatorsLive, float64(hp.Live), host.Name)
			}
		}
	}

	gauge(ch, m.hosts, float64(len(hosts)))
	gauge(ch, m.hostsStale, float64(stale))
	gauge(ch, m.hostsRebootRequired, float64(reboot))

	return nil
}

func (m *metrics) collectRollouts(ctx context.Context, ch chan<- prometheus.Metric) error {
	leases, err := m.server.opts.Store.ListLeases(ctx, store.LeaseFilter{OpenOnly: true})
	if err != nil {
		return fmt.Errorf("list leases: %w", err)
	}

	gauge(ch, m.leasesOpen, float64(len(leases)))

	rollouts, err := m.server.opts.Store.ListRollouts(ctx, store.RolloutFilter{OpenOnly: true})
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
