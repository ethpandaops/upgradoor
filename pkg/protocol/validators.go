package protocol

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Where a host's validator range was read from.
const (
	// ValidatorsFromMarker: the marker file the validator_keys Ansible role
	// writes next to the validator client's keys.
	ValidatorsFromMarker = "marker"
	// ValidatorsFromConfig: the static range of the agent configuration.
	ValidatorsFromConfig = "config"
)

// What a host knows about its validator range.
const (
	// ValidatorsKnown: Start and End are the host's range; an empty range
	// (Start == End) says that the host has no validators.
	ValidatorsKnown = "known"
	// ValidatorsMissing: the marker file is configured and does not exist.
	// That is the state of a host whose keys are being reshuffled; the host
	// does not know its range and says so instead of "none".
	ValidatorsMissing = "missing"
	// ValidatorsInvalid: the range cannot be used. The agent sets it for a
	// marker file it cannot read or parse, the controller for a reported
	// range that is out of bounds. Error says why.
	ValidatorsInvalid = "invalid"
)

const (
	// MaxValidatorIndex bounds the end of a validator range.
	MaxValidatorIndex = 1 << 40
	// MaxHostValidators bounds the number of validators of one host.
	MaxHostValidators = 1 << 20
	// MaxKeyRangeFile bounds what is read of a marker file.
	MaxKeyRangeFile = 256
)

// Validators is what a host says about the beacon validators it operates:
// the half-open range [Start, End) of validator indices. The host reads it
// locally and reports it with every sync; nothing in its certificate backs
// it. It is absent for a host that has neither a marker file nor a static
// range configured: such a host has no validators.
type Validators struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
	// Source is ValidatorsFromMarker or ValidatorsFromConfig.
	Source string `json:"source"`
	// State is ValidatorsKnown, ValidatorsMissing or ValidatorsInvalid. Only
	// with ValidatorsKnown do Start and End mean anything.
	State string `json:"state"`
	// Error is the reason for ValidatorsInvalid.
	Error string `json:"error,omitempty"`
}

// Known reports whether the host stated its range: v.State is
// ValidatorsKnown. A known range may be empty.
func (v *Validators) Known() bool { return v != nil && v.State == ValidatorsKnown }

// Count is the number of validators of a known range, else 0.
func (v *Validators) Count() uint64 {
	if !v.Known() || v.End <= v.Start {
		return 0
	}

	return v.End - v.Start
}

// Validate checks a reported range: a known state, and for a known range
// Start <= End within the bounds.
func (v *Validators) Validate() error {
	switch v.State {
	case ValidatorsMissing, ValidatorsInvalid:
		return nil
	case ValidatorsKnown:
		return CheckValidatorRange(v.Start, v.End)
	default:
		return fmt.Errorf("unknown validator range state %q", v.State)
	}
}

// CheckValidatorRange checks the half-open range [start, end): not reversed
// and within MaxValidatorIndex and MaxHostValidators. An empty range is
// valid and means no validators.
func CheckValidatorRange(start, end uint64) error {
	switch {
	case start > end:
		return fmt.Errorf("validator range %d-%d ends before it starts", start, end)
	case end > MaxValidatorIndex:
		return fmt.Errorf("validator range %d-%d ends beyond index %d",
			start, end, uint64(MaxValidatorIndex))
	case end-start > MaxHostValidators:
		return fmt.Errorf("validator range %d-%d holds more than %d validators",
			start, end, uint64(MaxHostValidators))
	}

	return nil
}

// ParseKeyRange parses the content of a validator key range marker file:
// "<start>-<end>" on one line, half-open like the inventory's
// validator_start and validator_end, so "0-64" is the validators 0 to 63 and
// "0-0" is none. Surrounding white space is ignored.
func ParseKeyRange(content string) (start, end uint64, err error) {
	text := strings.TrimSpace(content)
	if text == "" {
		return 0, 0, errors.New("the key range marker is empty")
	}

	if len(text) > 64 || strings.ContainsAny(text, "\r\n") {
		return 0, 0, errors.New("the key range marker is not one line \"<start>-<end>\"")
	}

	first, second, ok := strings.Cut(text, "-")
	if !ok {
		return 0, 0, fmt.Errorf("key range %q is not \"<start>-<end>\"", text)
	}

	start, err = parseIndex(first)
	if err != nil {
		return 0, 0, fmt.Errorf("key range %q: start: %w", text, err)
	}

	end, err = parseIndex(second)
	if err != nil {
		return 0, 0, fmt.Errorf("key range %q: end: %w", text, err)
	}

	if rangeErr := CheckValidatorRange(start, end); rangeErr != nil {
		return 0, 0, rangeErr
	}

	return start, end, nil
}

// parseIndex parses a validator index: decimal digits and nothing else.
func parseIndex(s string) (uint64, error) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, errors.New("not a number")
	}

	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, errors.New("number out of range")
	}

	return n, nil
}
