package config

import (
	"errors"
	"fmt"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Validators says where the agent learns which beacon validators the
// host operates. The range is not part of the host certificate: keys are
// moved between hosts without new certificates, so the host reads its range
// locally and reports it.
type Validators struct {
	// MarkerFile is the key range marker the validator_keys Ansible role
	// writes into the validator client's data directory
	// (.validator_keyrange): "<start>-<end>" on one line, half-open, "0-0"
	// for none. The role removes it while it reshuffles the keys. When set,
	// the marker alone decides and Start and End are not used.
	MarkerFile string `yaml:"markerFile"`
	// Start and End are a static half-open range [Start, End), used only
	// when no MarkerFile is configured. Both zero: none.
	Start uint64 `yaml:"start"`
	End   uint64 `yaml:"end"`
}

// validate checks the static range.
func (c *Validators) validate() error {
	if err := protocol.CheckValidatorRange(c.Start, c.End); err != nil {
		return fmt.Errorf("validators: %w", err)
	}

	if c.MarkerFile == "" && c.Start == c.End && c.End != 0 {
		return errors.New("validators: start and end are equal: leave both out for no validators")
	}

	return nil
}
