package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Policy says how needs of one kind are handled.
type Policy string

// Policies.
const (
	// PolicyAuto rolls out without a person.
	PolicyAuto Policy = "auto"
	// PolicyManual queues the rollout and waits for a person to approve it.
	PolicyManual Policy = "manual"
	// PolicyPaused queues and grants nothing.
	PolicyPaused Policy = "paused"
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
	// MinParticipationPercent is the network gate's participation floor.
	MinParticipationPercent float64 `json:"minParticipationPercent"`
	// MaxFinalityLagEpochs is the network gate's finality lag ceiling.
	MaxFinalityLagEpochs uint64 `json:"maxFinalityLagEpochs"`
	// SoakEpochs is how many consecutive complete epochs a host's validators
	// must be live after its slot before the slot is released.
	SoakEpochs uint64 `json:"soakEpochs"`
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

	// Paused is the global pause: nothing new is granted.
	Paused bool `json:"paused"`
}

// DefaultSettings returns the built-in defaults.
func DefaultSettings() Settings {
	return Settings{
		MaxUnavailablePercent:   10,
		MinParticipationPercent: 90,
		MaxFinalityLagEpochs:    3,
		SoakEpochs:              1,
		LeaseTTL:                Duration(30 * time.Minute),
		HostStaleAfter:          Duration(2 * time.Minute),
		ReadinessTimeout:        Duration(30 * time.Minute),
		PolicyImage:             PolicyAuto,
		PolicyPackages:          PolicyAuto,
		PolicyReboot:            PolicyManual,
	}
}

// PolicyFor returns the policy for a need kind.
func (s *Settings) PolicyFor(kind protocol.NeedKind) Policy {
	switch kind {
	case protocol.NeedImage:
		return s.PolicyImage
	case protocol.NeedPackages:
		return s.PolicyPackages
	case protocol.NeedReboot:
		return s.PolicyReboot
	default:
		return PolicyPaused
	}
}

// Validate checks value ranges.
func (s *Settings) Validate() error {
	if s.MaxUnavailablePercent < 0 || s.MaxUnavailablePercent > 100 {
		return errors.New("maxUnavailablePercent must be between 0 and 100")
	}

	if s.MinParticipationPercent < 0 || s.MinParticipationPercent > 100 {
		return errors.New("minParticipationPercent must be between 0 and 100")
	}

	if s.MaxFinalityLagEpochs < 2 {
		return errors.New("maxFinalityLagEpochs must be at least 2 (a healthy network lags 2)")
	}

	if s.LeaseTTL.Std() < time.Minute {
		return errors.New("leaseTTL must be at least 1m")
	}

	if s.HostStaleAfter.Std() < 30*time.Second {
		return errors.New("hostStaleAfter must be at least 30s")
	}

	if s.ReadinessTimeout.Std() < time.Minute {
		return errors.New("readinessTimeout must be at least 1m")
	}

	for _, p := range []Policy{s.PolicyImage, s.PolicyPackages, s.PolicyReboot} {
		if p != PolicyAuto && p != PolicyManual && p != PolicyPaused {
			return fmt.Errorf("invalid policy %q", p)
		}
	}

	return nil
}
