package agent_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
)

func TestNewValidatesItsInput(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())

	_, err := agent.New(e.Log, e.Cfg, e.Creds, agent.Deps{Runtime: e.Host, Registry: e.Host})
	require.Error(t, err, "a system is required")

	_, err = agent.New(e.Log, nil, e.Creds, agent.Deps{})
	require.Error(t, err)

	_, err = agent.New(e.Log, e.Cfg, nil, agent.Deps{Runtime: e.Host, Registry: e.Host, System: e.Host})
	require.Error(t, err)

	raw := *e.Cfg
	raw.Verify.Window = 0
	_, err = agent.New(e.Log, &raw, e.Creds, agent.Deps{Runtime: e.Host, Registry: e.Host, System: e.Host})
	require.Error(t, err, "a configuration without defaults is refused")

	svc, err := agent.New(e.Log, e.Cfg, e.Creds, agent.Deps{Runtime: e.Host, Registry: e.Host, System: e.Host})
	require.NoError(t, err)
	require.NoError(t, svc.Stop(), "stopping an agent that never started is fine")
	require.NoError(t, svc.Start(t.Context()))
	require.Error(t, svc.Start(t.Context()), "an agent starts once")
	require.NoError(t, svc.Stop())
	require.NoError(t, svc.Stop())
}
