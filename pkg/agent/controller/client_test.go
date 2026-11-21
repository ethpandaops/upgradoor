package controller_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/agenttest"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// An agent that pins a controller key acts on nothing of an answer that the
// controller key did not sign for this host and this request: it takes no
// lease, applies no directive, uploads no inventory and touches nothing on
// the host, however often the answer is repeated. Once the controller itself
// answers, the same lease is served.
func TestAnswersNotOfTheControllerAreNotActedOn(t *testing.T) {
	t.Parallel()

	sign := func(t *testing.T, s *identity.AnswerSigner, host, id string, body []byte) []byte {
		t.Helper()

		out, err := s.Sign(host, id, body)
		require.NoError(t, err)

		return out
	}

	tests := []struct {
		name string
		// forge returns what is sent instead of the controller's answer.
		forge func(t *testing.T) func(answer []byte, host, proofID string) []byte
		// reason is what the agent logs.
		reason string
	}{
		{
			name: "unsigned answer",
			forge: func(*testing.T) func([]byte, string, string) []byte {
				return func(answer []byte, _, _ string) []byte { return answer }
			},
			reason: "it is not signed",
		},
		{
			name: "answer to an earlier request, replayed",
			forge: func(t *testing.T) func([]byte, string, string) []byte {
				t.Helper()

				signer := agenttest.AnswerSigner(t, 0)
				earlier := "the-request-before-the-first"

				return func(answer []byte, host, proofID string) []byte {
					// Signed by the controller key, for this host, with this
					// content: but as the answer to the request before.
					out := sign(t, signer, host, earlier, answer)
					earlier = proofID

					return out
				}
			},
			reason: "it answers another request",
		},
		{
			name: "answer for another host",
			forge: func(t *testing.T) func([]byte, string, string) []byte {
				t.Helper()

				signer := agenttest.AnswerSigner(t, 0)

				return func(answer []byte, _, proofID string) []byte {
					return sign(t, signer, "prysm-besu-1", proofID, answer)
				}
			},
			reason: "it is for another host",
		},
		{
			name: "body altered in transit",
			forge: func(t *testing.T) func([]byte, string, string) []byte {
				t.Helper()

				signer := agenttest.AnswerSigner(t, 0)

				return func(answer []byte, host, proofID string) []byte {
					// The controller signed an answer without a lease and
					// without directives; the lease is put in on the way.
					harmless, err := json.Marshal(protocol.SyncResponse{})
					require.NoError(t, err)

					var signed identity.SignedAnswer
					require.NoError(t, json.Unmarshal(sign(t, signer, host, proofID, harmless), &signed))

					signed.Payload = base64.RawURLEncoding.EncodeToString(answer)

					out, err := json.Marshal(signed)
					require.NoError(t, err)

					return out
				}
			},
			reason: "the body does not match its signature",
		},
		{
			name: "signed with a controller key the host does not accept",
			forge: func(t *testing.T) func([]byte, string, string) []byte {
				t.Helper()

				// The next generation: a rotated key this host was not given.
				signer := agenttest.AnswerSigner(t, 1)

				return func(answer []byte, host, proofID string) []byte {
					return sign(t, signer, host, proofID, answer)
				}
			},
			reason: "not signed by an accepted controller key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sc := agenttest.BaseScenario()
			sc.RebootRequired = true
			sc.Packages.Upgradable = agenttest.DockerUpgrade()

			e := agenttest.NewEnv(t, sc)
			hook := logtest.NewLocal(e.Log)

			e.Ctrl.Set(func(c *agenttest.Controller) {
				c.Forge = tt.forge(t)
				c.Lease = e.Lease("lease-1", agenttest.RebootNeed())
				c.Directives = []protocol.Directive{
					{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "forged"},
				}
				c.WantInventory = true
			})
			e.Start()

			// Many answers, all refused.
			e.Ctrl.AwaitSyncs(8)

			for _, req := range e.Ctrl.AllSyncs() {
				require.Nil(t, req.Operation, "no lease is taken")
				require.Empty(t, req.DirectiveAcks, "no directive is answered")
				require.False(t, req.Hold.Controller, "no directive is applied")
			}

			assert.Empty(t, agenttest.Disruptive(e.Host.Calls()), "nothing on the host is touched")
			assert.Nil(t, e.StateOnDisk()["current"], "no lease is persisted")

			assert.Empty(t, e.Ctrl.Inventories(), "no inventory is uploaded on an unverified wish")

			// Said once for the whole episode, with the reason.
			refusals := make([]string, 0, 1)

			for _, entry := range hook.AllEntries() {
				if entry.Level == logrus.ErrorLevel &&
					strings.Contains(entry.Message, "refused the controller's answer") {
					err, ok := entry.Data[logrus.ErrorKey].(error)
					require.True(t, ok)

					refusals = append(refusals, err.Error())
				}
			}

			require.Len(t, refusals, 1)
			assert.Contains(t, refusals[0], tt.reason)

			// The controller itself answers again: the lease is served.
			e.Ctrl.Set(func(c *agenttest.Controller) {
				c.Forge = nil
				c.Directives = nil
				c.WantInventory = false
			})

			op := e.Ctrl.AwaitFinished("lease-1")
			require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
			assert.Contains(t, e.Host.Calls(), "reboot")
		})
	}
}

// While the controller key is rotated a host lists the old and the new key
// and takes answers signed with either.
func TestAnswersOfEachListedControllerKeyAreAccepted(t *testing.T) {
	t.Parallel()

	for _, generation := range []uint64{0, 1} {
		sc := agenttest.BaseScenario()
		sc.RebootRequired = true

		e := agenttest.NewEnv(t, sc)
		e.Cfg.Controller.PublicKeys = []string{
			agenttest.ControllerPublicKey(t, 0), agenttest.ControllerPublicKey(t, 1),
		}
		e.Ctrl.Set(func(c *agenttest.Controller) {
			c.Signer = agenttest.AnswerSigner(t, generation)
			c.Lease = e.Lease("lease-1", agenttest.RebootNeed())
		})
		e.Start()

		op := e.Ctrl.AwaitFinished("lease-1")
		require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	}
}

// The development opt-out: without controller keys the agent takes unsigned
// answers, and says at start that it does.
func TestUnsignedAnswersNeedTheOptOut(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())
	hook := logtest.NewLocal(e.Log)

	e.Cfg.Controller.PublicKeys = nil
	e.Cfg.Controller.AllowUnsignedAnswers = true

	e.Ctrl.Set(func(c *agenttest.Controller) {
		c.Signer = nil
		c.Directives = []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}}
	})
	e.Start()

	e.Ctrl.Await("the hold of an unsigned answer", func(req *protocol.SyncRequest) bool {
		return req.Hold.Controller
	})

	warned := false

	for _, entry := range hook.AllEntries() {
		if entry.Level == logrus.WarnLevel && strings.HasPrefix(entry.Message, "INSECURE") {
			warned = true
		}
	}

	assert.True(t, warned, "running without verification is logged loudly")
}

// An agent without controller keys cannot check a signed answer: it acts on
// nothing of it instead of reading it as an empty one.
func TestSignedAnswersAreRefusedWithoutControllerKeys(t *testing.T) {
	t.Parallel()

	e := agenttest.NewEnv(t, agenttest.BaseScenario())

	e.Cfg.Controller.PublicKeys = nil
	e.Cfg.Controller.AllowUnsignedAnswers = true

	e.Ctrl.Set(func(c *agenttest.Controller) {
		c.Directives = []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}}
	})
	e.Start()

	e.Ctrl.AwaitSyncs(4)

	for _, req := range e.Ctrl.AllSyncs() {
		require.Empty(t, req.DirectiveAcks, "no directive is answered")
		require.False(t, req.Hold.Controller, "no directive is applied")
	}
}
