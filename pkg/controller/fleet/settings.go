package fleet

import (
	"cmp"
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
	// MaxLeasedPercent is the share of hosts (0-100) that may hold a node or
	// slot lease at once, at least one host.
	MaxLeasedPercent float64 `json:"maxLeasedPercent"`
	// MaxIndependentPercent is the share of hosts (0-100) that may hold a
	// lease for independent containers at once, at least one host. Those
	// leases have no gate and no budget and count for no other limit.
	MaxIndependentPercent float64 `json:"maxIndependentPercent"`
	// MinParticipationPercent is the network gate's participation floor.
	MinParticipationPercent float64 `json:"minParticipationPercent"`
	// MaxFinalityLagEpochs is the network gate's finality lag ceiling.
	MaxFinalityLagEpochs uint64 `json:"maxFinalityLagEpochs"`
	// ReadyAttestations is for how many distinct slots after its operation a
	// host with validators must have attestations of them included on the
	// canonical chain before its slot is released; a host with fewer
	// validators needs one slot per validator. 1 to MaxReadyAttestations.
	ReadyAttestations uint64 `json:"readyAttestations"`
	// ReadyStableFor is how long the node of a host without validators must
	// stay at head with peers after its operation before its slot is
	// released. 0 releases at the first healthy sync. At most
	// MaxReadyStableFor.
	ReadyStableFor Duration `json:"readyStableFor"`
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
	// BaselineGrace is how long after its operation a host that was not ready
	// before its lease waits for the full readiness check before it is
	// released as no worse than before. Between ReadyStableFor and
	// ReadinessTimeout.
	BaselineGrace Duration `json:"baselineGrace"`
	// MaxConcurrentPulls bounds the leases with image work that are granted
	// or pulling at once; 0 is no bound.
	MaxConcurrentPulls int `json:"maxConcurrentPulls"`

	PolicyImage    Policy `json:"policyImage"`
	PolicyPackages Policy `json:"policyPackages"`
	PolicyReboot   Policy `json:"policyReboot"`
	// PolicyClients is the image policy per client, which is the team of
	// the containers ("geth": "manual"). The image rollouts of a team that is
	// listed answer to its entry; the others to PolicyKinds, then PolicyImage.
	PolicyClients ClientPolicies `json:"policyClients"`
	// PolicyKinds is the image policy per container kind ("cl": "manual"),
	// KindOther for unlabelled containers. It applies to the image rollouts
	// of a kind whose team has no entry in PolicyClients; a kind that is not
	// listed answers to PolicyImage.
	PolicyKinds KindPolicies `json:"policyKinds"`
	// ActionPolicies is the policy per host action name: auto opens a
	// rollout from the hosts' reports that the action is due, manual (the
	// default for an action that is not listed) waits for approval.
	ActionPolicies ActionPolicies `json:"actionPolicies"`

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

	// LookupInterval is how often the controller looks a tag up at its
	// registry. It tells every host that follows the tag what it points at.
	LookupInterval Duration `json:"lookupInterval"`

	// Paused is the global pause: nothing new is granted.
	Paused bool `json:"paused"`

	// DeployWindow is the length of the initial deployment window from the
	// controller's first start on a fresh store (DeployWindowState). 0 is no
	// window.
	DeployWindow Duration `json:"deployWindow"`
}

// DefaultSettings returns the built-in defaults.
func DefaultSettings() Settings {
	return Settings{
		MaxUnavailablePercent:   10,
		MaxLeasedPercent:        10,
		MaxIndependentPercent:   10,
		MinParticipationPercent: 90,
		MaxFinalityLagEpochs:    3,
		ReadyAttestations:       2,
		ReadyStableFor:          Duration(time.Minute),
		MinPeers:                3,
		LeaseTTL:                Duration(30 * time.Minute),
		HostStaleAfter:          Duration(2 * time.Minute),
		ReadinessTimeout:        Duration(30 * time.Minute),
		BaselineGrace:           Duration(15 * time.Minute),
		MaxConcurrentPulls:      10,
		PolicyImage:             PolicyAuto,
		PolicyPackages:          PolicyAuto,
		PolicyReboot:            PolicyManual,
		LookupInterval:          Duration(5 * time.Minute),
		DeployWindow:            Duration(24 * time.Hour),
	}
}

// MaxDeployWindow bounds Settings.DeployWindow.
const MaxDeployWindow = 30 * 24 * time.Hour

// DeployWindowState is the stored state of the initial deployment window.
// While it is open only leases of Ansible deploys are granted.
type DeployWindowState struct {
	// StartedAt is the controller's first start on a fresh store; zero when
	// the store has no window.
	StartedAt time.Time `json:"startedAt"`
	// ArmedAt is when the window was ended early, or when a store that held
	// a running fleet was upgraded to one with windows; nil while not armed.
	ArmedAt *time.Time `json:"armedAt,omitempty"`
}

// End is when the window closes by time; zero when there is no window.
func (w *DeployWindowState) End(length time.Duration) time.Time {
	if w.StartedAt.IsZero() || length <= 0 {
		return time.Time{}
	}

	return w.StartedAt.Add(length)
}

// Open reports whether the window is open at now.
func (w *DeployWindowState) Open(now time.Time, length time.Duration) bool {
	end := w.End(length)

	return w.ArmedAt == nil && !end.IsZero() && now.Before(end)
}

// MinLookupInterval and MaxLookupInterval bound Settings.LookupInterval.
const (
	MinLookupInterval = 30 * time.Second
	MaxLookupInterval = 24 * time.Hour
)

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

// KindPolicies is an image policy per container kind. In JSON it is always
// an object: none is {}, never null.
type KindPolicies map[string]Policy

// MarshalJSON implements json.Marshaler.
func (k KindPolicies) MarshalJSON() ([]byte, error) {
	if k == nil {
		return []byte("{}"), nil
	}

	return json.Marshal(map[string]Policy(k))
}

// UnmarshalJSON implements json.Unmarshaler: the object replaces what was
// there, and an empty one is no policies at all (nil).
func (k *KindPolicies) UnmarshalJSON(b []byte) error {
	var m map[string]Policy
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("policyKinds must be an object of kind to policy: %w", err)
	}

	if len(m) == 0 {
		m = nil
	}

	*k = m

	return nil
}

// KindOther is the PolicyKinds key of unlabelled containers.
const KindOther = "other"

// MaxPolicyKinds bounds PolicyKinds.
const MaxPolicyKinds = 64

// PolicySource says what decided the policy of an image rollout.
type PolicySource string

// Policy sources.
const (
	// PolicySourceClient: the team's entry in PolicyClients.
	PolicySourceClient PolicySource = "client"
	// PolicySourceKind: the kind's entry in PolicyKinds.
	PolicySourceKind PolicySource = "kind"
	// PolicySourceDefault: PolicyImage.
	PolicySourceDefault PolicySource = "default"
	// PolicySourceDeploy: every target is an Ansible deploy, always auto.
	PolicySourceDeploy PolicySource = "deploy"
)

// ActionPolicies is a policy per host action. In JSON it is always an
// object: none is {}, never null.
type ActionPolicies map[string]Policy

// MarshalJSON implements json.Marshaler.
func (a ActionPolicies) MarshalJSON() ([]byte, error) {
	if a == nil {
		return []byte("{}"), nil
	}

	return json.Marshal(map[string]Policy(a))
}

// UnmarshalJSON implements json.Unmarshaler: the object replaces what was
// there, and an empty one is no policies at all (nil).
func (a *ActionPolicies) UnmarshalJSON(b []byte) error {
	var m map[string]Policy
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("actionPolicies must be an object of action to policy: %w", err)
	}

	if len(m) == 0 {
		m = nil
	}

	*a = m

	return nil
}

// MaxActionPolicies bounds ActionPolicies.
const MaxActionPolicies = 64

// ActionPolicy is the policy of the rollouts a host action's condition
// opens: its own when one is set, manual otherwise.
func (s *Settings) ActionPolicy(name string) Policy {
	if p, ok := s.ActionPolicies[name]; ok {
		return p
	}

	return PolicyManual
}

// ImagePolicy is the policy the image rollouts of a team's containers of a
// kind answer to (empty kind: unlabelled containers), and the setting it
// comes from: the team's entry in PolicyClients, else the kind's in
// PolicyKinds, else PolicyImage.
func (s *Settings) ImagePolicy(team string, kind protocol.ContainerKind) (Policy, PolicySource) {
	if p, ok := s.PolicyClients[team]; ok && team != "" {
		return p, PolicySourceClient
	}

	return s.KindPolicy(kind)
}

// KindPolicy is the image policy of a kind without a client policy: the
// kind's entry in PolicyKinds (KindOther for the empty kind), else
// PolicyImage.
func (s *Settings) KindPolicy(kind protocol.ContainerKind) (Policy, PolicySource) {
	key := cmp.Or(string(kind), KindOther)
	if p, ok := s.PolicyKinds[key]; ok {
		return p, PolicySourceKind
	}

	return s.PolicyImage, PolicySourceDefault
}

// GroupPolicy is the policy of a group rollout and what decided it: auto
// when every target is an Ansible deploy (Rollout.Deployed), else
// ImagePolicy of its team and container kind.
func (s *Settings) GroupPolicy(r *Rollout) (Policy, PolicySource) {
	if r.Deployed() {
		return PolicyAuto, PolicySourceDeploy
	}

	return s.ImagePolicy(r.Team, r.ContainerKind)
}

// RolloutPolicy is the policy that governs a rollout: that of its kind, and
// for an image rollout GroupPolicy. A rollout a person or a host asked for
// answers to no policy: auto.
func (s *Settings) RolloutPolicy(r *Rollout) Policy {
	switch r.Kind {
	case RolloutGroup:
		p, _ := s.GroupPolicy(r)

		return p
	case RolloutPackages:
		return s.PolicyPackages
	case RolloutReboot:
		return s.PolicyReboot
	case RolloutAction:
		if r.Directed() {
			return PolicyAuto
		}

		return s.ActionPolicy(r.Action)
	default:
		return PolicyAuto
	}
}

// Equal reports whether two settings are the same; no client, kind or
// action policies and an empty set of them are.
func (s *Settings) Equal(o *Settings) bool {
	a, b := *s, *o
	a.PolicyClients, b.PolicyClients = nil, nil
	a.PolicyKinds, b.PolicyKinds = nil, nil
	a.ActionPolicies, b.ActionPolicies = nil, nil

	return reflect.DeepEqual(a, b) && maps.Equal(s.PolicyClients, o.PolicyClients) &&
		maps.Equal(s.PolicyKinds, o.PolicyKinds) && maps.Equal(s.ActionPolicies, o.ActionPolicies)
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

// MaxReadyAttestations bounds Settings.ReadyAttestations: the beacon service
// keeps this many attested slots per host. MaxReadyStableFor bounds
// Settings.ReadyStableFor.
const (
	MaxReadyAttestations = 32
	MaxReadyStableFor    = 30 * time.Minute
	// MaxConcurrentPulls bounds Settings.MaxConcurrentPulls.
	MaxConcurrentPulls = 10000
)

// Validate checks value ranges.
func (s *Settings) Validate() error {
	if s.MaxUnavailablePercent < 0 || s.MaxUnavailablePercent > 100 {
		return errors.New("maxUnavailablePercent must be between 0 and 100")
	}

	if !(s.MaxLeasedPercent > 0 && s.MaxLeasedPercent <= 100) {
		return errors.New("maxLeasedPercent must be above 0 and at most 100")
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

	if s.ReadyAttestations < 1 || s.ReadyAttestations > MaxReadyAttestations {
		return fmt.Errorf("readyAttestations must be between 1 and %d", MaxReadyAttestations)
	}

	if s.ReadyStableFor < 0 || s.ReadyStableFor.Std() > MaxReadyStableFor {
		return errors.New("readyStableFor must be between 0 and 30m")
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

	if s.BaselineGrace < s.ReadyStableFor || s.BaselineGrace > s.ReadinessTimeout {
		return errors.New("baselineGrace must be between readyStableFor and readinessTimeout")
	}

	if s.MaxConcurrentPulls < 0 || s.MaxConcurrentPulls > MaxConcurrentPulls {
		return fmt.Errorf("maxConcurrentPulls must be between 0 and %d", MaxConcurrentPulls)
	}

	if s.LookupInterval.Std() < MinLookupInterval || s.LookupInterval.Std() > MaxLookupInterval {
		return errors.New("lookupInterval must be between 30s and 24h")
	}

	if s.DeployWindow < 0 || s.DeployWindow.Std() > MaxDeployWindow {
		return errors.New("deployWindow must be between 0 and 720h")
	}

	return s.validatePolicies()
}

// validatePolicies checks the policies: the need kinds', the clients', the
// container kinds' and the actions'.
func (s *Settings) validatePolicies() error {
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

	if len(s.PolicyKinds) > MaxPolicyKinds {
		return fmt.Errorf("policyKinds lists more than %d kinds", MaxPolicyKinds)
	}

	for kind, p := range s.PolicyKinds {
		if !protocol.ValidKind(protocol.ContainerKind(kind)) || kind == string(protocol.KindNone) {
			return fmt.Errorf("policyKinds: %q is not a kind name", kind)
		}

		if p != PolicyAuto && p != PolicyManual {
			return fmt.Errorf("policyKinds: invalid policy %q for %s", p, kind)
		}
	}

	if len(s.ActionPolicies) > MaxActionPolicies {
		return fmt.Errorf("actionPolicies lists more than %d actions", MaxActionPolicies)
	}

	for name, p := range s.ActionPolicies {
		if !protocol.ValidActionName(name) {
			return fmt.Errorf("actionPolicies: %q is not an action name", name)
		}

		if p != PolicyAuto && p != PolicyManual {
			return fmt.Errorf("actionPolicies: invalid policy %q for %s", p, name)
		}
	}

	return nil
}
