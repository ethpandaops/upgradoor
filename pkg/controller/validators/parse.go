package validators

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/identity"
)

const (
	// maxIndex bounds the validator indices of a range.
	maxIndex = 1 << 40
	// maxRanges bounds the ranges of one mapping.
	maxRanges = 1 << 16
)

// document is the validator ranges inventory: "ranges" maps inclusive
// "<start>-<end>" validator index ranges to inventory host names; "history"
// holds the earlier mappings.
type document struct {
	Ranges  map[string]string `json:"ranges"`
	History []struct {
		Ranges map[string]string `json:"ranges"`
	} `json:"history"`
}

// Parse decodes and checks a validator ranges inventory and returns its
// current mapping as half-open ranges ordered by start. The history is
// checked like the current mapping and otherwise ignored.
func Parse(raw []byte) ([]fleet.HostRange, error) {
	var doc document

	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	if doc.Ranges == nil {
		return nil, errors.New("no ranges")
	}

	for i := range doc.History {
		if _, err := parseRanges(doc.History[i].Ranges); err != nil {
			return nil, fmt.Errorf("history entry %d: %w", i, err)
		}
	}

	return parseRanges(doc.Ranges)
}

// parseRanges turns one mapping into disjoint half-open ranges ordered by
// start.
func parseRanges(in map[string]string) ([]fleet.HostRange, error) {
	if len(in) > maxRanges {
		return nil, fmt.Errorf("%d ranges, at most %d", len(in), maxRanges)
	}

	out := make([]fleet.HostRange, 0, len(in))

	for key, host := range in {
		start, end, err := parseKey(key)
		if err != nil {
			return nil, err
		}

		if err := identity.ValidateName(host); err != nil {
			return nil, fmt.Errorf("range %s: host: %w", key, err)
		}

		out = append(out, fleet.HostRange{
			Host: host, ValidatorRange: identity.ValidatorRange{Start: start, End: end + 1},
		})
	}

	slices.SortFunc(out, func(a, b fleet.HostRange) int { return cmp.Compare(a.Start, b.Start) })

	for i := 1; i < len(out); i++ {
		if prev := &out[i-1]; out[i].Start < prev.End {
			return nil, fmt.Errorf("range %d-%d of %s overlaps range %d-%d of %s",
				out[i].Start, out[i].End-1, out[i].Host, prev.Start, prev.End-1, prev.Host)
		}
	}

	return out, nil
}

// parseKey parses an inclusive range "<start>-<end>".
func parseKey(key string) (start, end uint64, err error) {
	first, second, ok := strings.Cut(key, "-")
	if !ok {
		return 0, 0, fmt.Errorf("range %q is not \"<start>-<end>\"", key)
	}

	if start, err = parseIndex(first); err != nil {
		return 0, 0, fmt.Errorf("range %q: start: %w", key, err)
	}

	if end, err = parseIndex(second); err != nil {
		return 0, 0, fmt.Errorf("range %q: end: %w", key, err)
	}

	if start > end {
		return 0, 0, fmt.Errorf("range %q ends before it starts", key)
	}

	return start, end, nil
}

// parseIndex parses a validator index: decimal digits below maxIndex.
func parseIndex(s string) (uint64, error) {
	if s == "" || len(s) > 20 || strings.Trim(s, "0123456789") != "" {
		return 0, errors.New("not a number")
	}

	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n >= maxIndex {
		return 0, fmt.Errorf("index beyond %d", uint64(maxIndex))
	}

	return n, nil
}
