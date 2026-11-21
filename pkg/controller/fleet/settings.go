package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// MinHostStaleAfter is the shortest HostStaleAfter the settings accept.
const MinHostStaleAfter = 30 * time.Second

// Policy says how needs of one kind are handled.
type Policy string

// Policies.
const (
	// PolicyAuto rolls out without a person.
	PolicyAuto Policy = "auto"
	// PolicyManual queues the rollout and waits for a person to approve it.
	PolicyManual Policy = "manual"
)

// Duration is a time.Duration that encodes as a Go duration string ("30m").
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30m\": %w", err)
	}

	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}

	*d = Duration(v)

	return nil
}

// Std returns the time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Settings are the operator-tunable scheduler settings. Defaults come from
// flags / helm values, are overridden by the settings table and are editable
// in the UI. Every change is audited.
type Settings struct {
	// MaxUnavailablePercent is the share of tracked validator weight that may
	// be unavailable at once (0-100).
	MaxUnavailablePercent float64 `json:"maxUnavailablePercent"`
	// MaxIndependentPercent is the share of hosts (0-100) that may hold a
	// lease for independent containers at once, at least one host. Those
	// leases have no gate and no budget and count for no other limit.
	MaxIndependentPercent float64 `json:"maxIndependentPercent"`
	// MinParticipationPercent is the network gate's participation floor.
	MinParticipationPercent float64 `json:"minParticipationPercent"`
	// MaxFinalityLagEpochs is the network gate's finality lag ceiling.
	MaxFinalityLagEpochs uint64 `json:"maxFinalityLagEpochs"`
	// ReadyEpochs is how long a host must look healthy after its operation
	// before its slot is released: the node at head with peers for this many
	// epoch durations, and its validators attesting in this many consecutive
	// complete epochs. 0 releases at the first healthy sync after the
	// operation. At most MaxReadyEpochs.
	ReadyEpochs uint64 `json:"readyEpochs"`
	// MinPeers is how many peers each of the execution and the consensus
	// client must have for the node to count as back, at most. See
	// EffectiveMinPeers.
	MinPeers int `json:"minPeers"`
	// LeaseTTL is how long an agent has to start a granted lease, and the
	// silence after which a started lease is declared failed.
	LeaseTTL Duration `json:"leaseTtl"`
	// HostStaleAfter is the sync silence after which a host counts as stale.
	HostStaleAfter Duration `json:"hostStaleAfter"`
	// ReadinessTimeout is how long the controller waits for readiness after
	// the agent reports done before it declares the slot failed.
	ReadinessTimeout Duration `json:"readinessTimeout"`

	PolicyImage    Policy `json:"policyImage"`
	PolicyPackages Policy `json:"policyPackages"`
	PolicyReboot   Policy `json:"policyReboot"`
	// PolicyClients is the image policy per client ("geth": "manual"). The
	// image rollouts of a client that is listed answer to its entry; those
	// of every other client, and of containers without a client, to
	// PolicyImage.
	PolicyClients ClientPolicies `json:"policyClients"`

	// PauseAfterCanaryImage, PauseAfterCanaryPackages and
	// PauseAfterCanaryReboot pause a rollout that serves needs of that kind
	// when it has its first success, until a person resumes it.
	PauseAfterCanaryImage    bool `json:"pauseAfterCanaryImage"`
	PauseAfterCanaryPackages bool `json:"pauseAfterCanaryPackages"`
	PauseAfterCanaryReboot   bool `json:"pauseAfterCanaryReboot"`

	// ReadyPeerShare is the share (0-1) of the fleet's median peer count that
	// each client of a host must have before its slot is released, next to
	// MinPeers. 0, the default, is off: a host whose client has a lower peer
	// limit than the others would never be ready with a share above what that
	// limit allows.
	ReadyPeerShare float64 `json:"readyPeerShare"`

	// Paused is the global pause: nothing new is granted.
	Paused bool `json:"paused"`
}

// DefaultSettings returns the built-in defaults.
func DefaultSettings() Settings {
	return Settings{
		MaxUnavailablePercent:   10,
		MaxIndependentPercent:   10,
		MinParticipationPercent: 90,
		MaxFinalityLagEpochs:    3,
		ReadyEpochs:             1,
		MinPeers:                3,
		LeaseTTL:                Duration(30 * time.Minute),
		HostStaleAfter:          Duration(2 * time.Minute),
		ReadinessTimeout:        Duration(30 * time.Minute),
		PolicyImage:             PolicyAuto,
		PolicyPackages:          PolicyAuto,
		PolicyReboot:            PolicyManual,
	}
}

// ClientPolicies is an image policy per client. In JSON it is always an
// object: none is {}, never null.
type ClientPolicies map[string]Policy

// MarshalJSON implements json.Marshaler.
func (c ClientPolicies) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("{}"), nil
	}

	return json.Marshal(map[string]Policy(c))
}

// UnmarshalJSON implements json.Unmarshaler: the object replaces what was
// there, and an empty one is no policies at all (nil).
func (c *ClientPolicies) UnmarshalJSON(b []byte) error {
	var m map[string]Policy
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("policyClients must be an object of client to policy: %w", err)
	}

	if len(m) == 0 {
		m = nil
	}

	*c = m

	return nil
}

// MaxPolicyClients bounds PolicyClients.
const MaxPolicyClients = 64

// ImagePolicy is the policy the image rollouts of a client answer to: the
// client's own when one is set, PolicyImage otherwise.
func (s *Settings) ImagePolicy(client string) Policy {
	if p, ok := s.PolicyClients[client]; ok && client != "" {
		return p
	}

	return s.PolicyImage
}

// Equal reports whether two settings are the same; no client policies and an
// empty set of them are.
func (s *Settings) Equal(o *Settings) bool {
	a, b := *s, *o
	a.PolicyClients, b.PolicyClients = nil, nil

	return reflect.DeepEqual(a, b) && maps.Equal(s.PolicyClients, o.PolicyClients)
}

// PauseAfterCanaryFor reports whether a rollout that serves needs of the kind
// is paused when it has its first success.
func (s *Settings) PauseAfterCanaryFor(kind protocol.NeedKind) bool {
	switch kind {
	case protocol.NeedImage, protocol.NeedSpec:
		return s.PauseAfterCanaryImage
	case protocol.NeedPackages:
		return s.PauseAfterCanaryPackages
	case protocol.NeedReboot:
		return s.PauseAfterCanaryReboot
	default:
		return false
	}
}

// EffectiveMinPeers is the number of peers a node must have in a fleet in
// which nodes hosts run a node: MinPeers, but never more than half of the
// other nodes (rounded up, at least 1). Two or three nodes need 1 peer, four
// or five need 2, six and more need 3 with the default. A node that has
// found half of the others is back; waiting for all of them would make every
// short peer loss on a small devnet restart the check.
func (s *Settings) EffectiveMinPeers(nodes int) int {
	return max(1, min(s.MinPeers, nodes/2))
}

// MaxReadyEpochs bounds Settings.ReadyEpochs: the beacon service keeps the
// per-epoch liveness of this many epochs and a few more.
const MaxReadyEpochs = 32

// Validate checks value ranges.
func (s *Settings) Validate() error {
	if s.MaxUnavailablePercent < 0 || s.MaxUnavailablePercent > 100 {
		return errors.New("maxUnavailablePercent must be between 0 and 100")
	}

	if !(s.MaxIndependentPercent >= 0 && s.MaxIndependentPercent <= 100) {
		return errors.New("maxIndependentPercent must be between 0 and 100")
	}

	if s.MinParticipationPercent < 0 || s.MinParticipationPercent > 100 {
		return errors.New("minParticipationPercent must be between 0 and 100")
	}

	if s.MaxFinalityLagEpochs < 2 {
		return errors.New("maxFinalityLagEpochs must be at least 2 (a healthy network lags 2)")
	}

	if s.MinPeers < 1 {
		return errors.New("minPeers must be at least 1")
	}

	if s.ReadyEpochs > MaxReadyEpochs {
		return fmt.Errorf("readyEpochs must be at most %d", MaxReadyEpochs)
	}

	// Written so that NaN fails too.
	if !(s.ReadyPeerShare >= 0 && s.ReadyPeerShare <= 1) {
		return errors.New("readyPeerShare must be between 0 and 1")
	}

	if s.LeaseTTL.Std() < time.Minute {
		return errors.New("leaseTTL must be at least 1m")
	}

	if s.HostStaleAfter.Std() < MinHostStaleAfter {
		return errors.New("hostStaleAfter must be at least 30s")
	}

	if s.ReadinessTimeout.Std() < time.Minute {
		return errors.New("readinessTimeout must be at least 1m")
	}

	for _, p := range []Policy{s.PolicyImage, s.PolicyPackages, s.PolicyReboot} {
		if p != PolicyAuto && p != PolicyManual {
			return fmt.Errorf("invalid policy %q", p)
		}
	}

	if len(s.PolicyClients) > MaxPolicyClients {
		return fmt.Errorf("policyClients lists more than %d clients", MaxPolicyClients)
	}

	for client, p := range s.PolicyClients {
		if !protocol.ValidClient(client) {
			return fmt.Errorf("policyClients: %q is not a client name", client)
		}

		if p != PolicyAuto && p != PolicyManual {
			return fmt.Errorf("policyClients: invalid policy %q for %s", p, client)
		}
	}

	return nil
}
