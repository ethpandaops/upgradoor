package agent_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// An agent that pins a controller key acts on nothing of an answer that the
// controller key did not sign for this host and this request: it takes no
// lease, applies no directive, uploads no inventory and touches nothing on
// the host, however often the answer is repeated. Once the controller itself
// answers, the same lease is served.
func TestAnswersNotOfTheControllerAreNotActedOn(t *testing.T) {
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

				signer := answerSigner(t, 0)
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

				signer := answerSigner(t, 0)

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

				signer := answerSigner(t, 0)

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
				signer := answerSigner(t, 1)

				return func(answer []byte, host, proofID string) []byte {
					return sign(t, signer, host, proofID, answer)
				}
			},
			reason: "not signed by an accepted controller key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := baseScenario()
			sc.RebootRequired = true
			sc.Packages.Upgradable = dockerUpgrade()

			e := newEnv(t, sc)
			hook := logtest.NewLocal(e.log)

			e.ctrl.set(func(c *controller) {
				c.forge = tt.forge(t)
				c.lease = e.lease("lease-1", rebootNeed())
				c.directives = []protocol.Directive{
					{ID: "d-hold", Kind: protocol.DirectiveHold, Reason: "forged"},
				}
				c.wantInventory = true
			})
			e.start()

			// Many answers, all refused.
			e.ctrl.awaitSyncs(8)

			for _, req := range e.ctrl.allSyncs() {
				require.Nil(t, req.Operation, "no lease is taken")
				require.Empty(t, req.DirectiveAcks, "no directive is answered")
				require.False(t, req.Hold.Controller, "no directive is applied")
			}

			assert.Empty(t, disruptive(e.host.Calls()), "nothing on the host is touched")
			assert.Nil(t, e.stateOnDisk()["current"], "no lease is persisted")

			e.ctrl.mu.Lock()
			assert.Empty(t, e.ctrl.inventories, "no inventory is uploaded on an unverified wish")
			e.ctrl.mu.Unlock()

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
			e.ctrl.set(func(c *controller) {
				c.forge = nil
				c.directives = nil
				c.wantInventory = false
			})

			op := e.ctrl.awaitFinished("lease-1")
			require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
			assert.Contains(t, e.host.Calls(), "reboot")
		})
	}
}

// While the controller key is rotated a host lists the old and the new key
// and takes answers signed with either.
func TestAnswersOfEachListedControllerKeyAreAccepted(t *testing.T) {
	for _, generation := range []uint64{0, 1} {
		sc := baseScenario()
		sc.RebootRequired = true

		e := newEnv(t, sc)
		e.cfg.Controller.PublicKeys = []string{
			controllerPublicKey(t, 0), controllerPublicKey(t, 1),
		}
		e.ctrl.set(func(c *controller) {
			c.signer = answerSigner(t, generation)
			c.lease = e.lease("lease-1", rebootNeed())
		})
		e.start()

		op := e.ctrl.awaitFinished("lease-1")
		require.Equal(t, protocol.PhaseDone, op.Phase, op.Error)
	}
}

// The development opt-out: without controller keys the agent takes unsigned
// answers, and says at start that it does.
func TestUnsignedAnswersNeedTheOptOut(t *testing.T) {
	e := newEnv(t, baseScenario())
	hook := logtest.NewLocal(e.log)

	e.cfg.Controller.PublicKeys = nil
	e.cfg.Controller.AllowUnsignedAnswers = true

	e.ctrl.set(func(c *controller) {
		c.signer = nil
		c.directives = []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}}
	})
	e.start()

	e.ctrl.await("the hold of an unsigned answer", func(req *protocol.SyncRequest) bool {
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
	e := newEnv(t, baseScenario())

	e.cfg.Controller.PublicKeys = nil
	e.cfg.Controller.AllowUnsignedAnswers = true

	e.ctrl.set(func(c *controller) {
		c.directives = []protocol.Directive{{ID: "d-hold", Kind: protocol.DirectiveHold}}
	})
	e.start()

	e.ctrl.awaitSyncs(4)

	for _, req := range e.ctrl.allSyncs() {
		require.Empty(t, req.DirectiveAcks, "no directive is answered")
		require.False(t, req.Hold.Controller, "no directive is applied")
	}
}
