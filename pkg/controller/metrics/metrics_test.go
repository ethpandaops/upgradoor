package metrics_test

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/metrics"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
)

// The metric names are what the chart's PrometheusRule and the dashboards
// read: every one is served, and every one passes the linter.
func TestMetricNames(t *testing.T) {
	st, sched, beacon := controllertest.NewStore(), controllertest.NewScheduler(), &controllertest.Beacon{}
	controllertest.LeakFixture(t, st, sched, beacon)

	st.Leases = append(st.Leases, &fleet.Lease{ID: "l-open", Host: "lighthouse-geth-2", State: fleet.LeaseRunning})
	st.Rollouts = []*fleet.Rollout{{ID: "r1", State: fleet.RolloutActive}}

	log := logrus.New()
	log.SetOutput(io.Discard)

	reg := prometheus.NewRegistry()
	m, err := metrics.New(log, reg, metrics.Options{
		Store: st, Scheduler: sched,
		Gate: func() scheduler.Gate { return scheduler.Gate{OK: true, Network: beacon.Health()} },
		Now:  func() time.Time { return controllertest.Epoch },
	})
	require.NoError(t, err)

	// What the agent API records for a sync.
	m.AgentRequest("sync", http.StatusOK)
	m.AgentAnswered("sync", 5*time.Millisecond)

	problems, err := testutil.GatherAndLint(reg)
	require.NoError(t, err)
	assert.Empty(t, problems)

	families, err := reg.Gather()
	require.NoError(t, err)

	names := make([]string, 0, len(families))
	for _, family := range families {
		names = append(names, family.GetName())
	}

	assert.ElementsMatch(t, []string{
		"upgradoor_agent_requests_total",
		"upgradoor_agent_request_duration_seconds",
		"upgradoor_hosts",
		"upgradoor_hosts_stale",
		"upgradoor_hosts_reboot_required",
		"upgradoor_leases_open",
		"upgradoor_rollouts",
		"upgradoor_network_gate_ok",
		"upgradoor_network_participation_ratio",
		"upgradoor_network_finality_lag_epochs",
		"upgradoor_budget_unavailable_weight",
		"upgradoor_budget_allowed_weight",
	}, names)
}
