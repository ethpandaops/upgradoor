package views

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/beacon"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/validators"
)

// DataSources is the body of GET /api/v1/settings/sources: the beacon
// endpoints and the validator ranges inventory as the controller sees them.
// It carries no credentials.
type DataSources struct {
	Beacon []beacon.EndpointStatus `json:"beacon"`
	// Chain is what the beacon service makes of the endpoints when it fails.
	Chain           ChainErrors           `json:"chain"`
	ValidatorRanges ValidatorRangesSource `json:"validatorRanges"`
}

// ChainErrors are the error texts of the beacon service, which the other
// views carry as fixed texts (PublicGate).
type ChainErrors struct {
	// Error is why the last refresh failed, TimingError why the chain's
	// timing is invalid, LivenessError why the last count of the
	// participation failed.
	Error         string `json:"error,omitempty"`
	TimingError   string `json:"timingError,omitempty"`
	LivenessError string `json:"livenessError,omitempty"`
}

// ValidatorRangesSource is the validator ranges inventory.
type ValidatorRangesSource struct {
	// URL is the inventory URL without credentials; empty when none is
	// configured.
	URL       string    `json:"url,omitempty"`
	AttemptAt time.Time `json:"attemptAt,omitzero"`
	SuccessAt time.Time `json:"successAt,omitzero"`
	ChangedAt time.Time `json:"changedAt,omitzero"`
	ETag      string    `json:"etag,omitempty"`
	// LastModified is the Last-Modified header of the stored version.
	LastModified string `json:"lastModified,omitempty"`
	Error        string `json:"error,omitempty"`
	// Validators is the number of validators of all ranges; UnknownHosts
	// the number of inventory hosts upgradoor does not know.
	Validators   uint64 `json:"validators"`
	UnknownHosts int    `json:"unknownHosts"`
	// Ranges are ordered by start; Hosts by name.
	Ranges []ValidatorRangeRow  `json:"ranges"`
	Hosts  []ValidatorHostTotal `json:"hosts"`
}

// ValidatorRangeRow is one range of the inventory: the half-open index range
// [Start, End) of one host. Unknown marks a host upgradoor does not know.
type ValidatorRangeRow struct {
	Start   uint64 `json:"start"`
	End     uint64 `json:"end"`
	Host    string `json:"host"`
	Count   uint64 `json:"count"`
	Unknown bool   `json:"unknown,omitempty"`
}

// ValidatorHostTotal is what the inventory gives one host.
type ValidatorHostTotal struct {
	Host       string `json:"host"`
	Ranges     int    `json:"ranges"`
	Validators uint64 `json:"validators"`
	Unknown    bool   `json:"unknown,omitempty"`
}

// ValidatorRangesRefresh is the body of POST
// /api/v1/settings/sources/validator-ranges/refresh.
type ValidatorRangesRefresh struct {
	// Fetched says a fetch ran; Changed that it changed the ranges, Error
	// why it failed.
	Fetched bool   `json:"fetched"`
	Changed bool   `json:"changed,omitempty"`
	Error   string `json:"error,omitempty"`
	// Next is when a fetch may be asked for again, when one ran too
	// recently.
	Next time.Time `json:"next,omitzero"`
	// Message says what happened, for people.
	Message string      `json:"message"`
	Sources DataSources `json:"sources"`
}

// NewDataSources builds the view; known reports whether upgradoor knows a
// host.
func NewDataSources(
	endpoints []beacon.EndpointStatus, health *fleet.NetworkHealth, ranges *validators.Status,
	known func(host string) bool,
) DataSources {
	src := ValidatorRangesSource{
		URL: ranges.URL, AttemptAt: ranges.AttemptAt, SuccessAt: ranges.SuccessAt,
		ChangedAt: ranges.ChangedAt, ETag: ranges.ETag, LastModified: ranges.LastModified,
		Error:  ranges.Error,
		Ranges: make([]ValidatorRangeRow, 0, len(ranges.Ranges)),
		Hosts:  []ValidatorHostTotal{},
	}

	byHost := make(map[string]int, len(ranges.Ranges))

	for i := range ranges.Ranges {
		r := &ranges.Ranges[i]
		count := r.Count()

		idx, seen := byHost[r.Host]
		if !seen {
			idx = len(src.Hosts)
			byHost[r.Host] = idx
			src.Hosts = append(src.Hosts, ValidatorHostTotal{Host: r.Host, Unknown: !known(r.Host)})

			if src.Hosts[idx].Unknown {
				src.UnknownHosts++
			}
		}

		total := &src.Hosts[idx]
		total.Ranges++
		total.Validators += count
		src.Validators += count

		src.Ranges = append(src.Ranges, ValidatorRangeRow{
			Start: r.Start, End: r.End, Host: r.Host, Count: count, Unknown: total.Unknown,
		})
	}

	slices.SortFunc(src.Hosts, func(a, b ValidatorHostTotal) int { return cmp.Compare(a.Host, b.Host) })

	if endpoints == nil {
		endpoints = []beacon.EndpointStatus{}
	}

	chain := ChainErrors{
		Error: health.Error, TimingError: health.TimingError, LivenessError: health.LivenessError,
	}

	return DataSources{Beacon: endpoints, Chain: chain, ValidatorRanges: src}
}

// NewValidatorRangesRefresh is the answer of a refresh request.
func NewValidatorRangesRefresh(done *validators.Refreshed, sources *DataSources) ValidatorRangesRefresh {
	out := ValidatorRangesRefresh{
		Fetched: done.Fetched, Changed: done.Changed, Error: done.Error, Next: done.Next,
		Sources: *sources,
	}

	switch {
	case !done.Fetched:
		out.Message = done.Reason
	case done.Error != "":
		out.Message = "the fetch failed: " + done.Error
	case done.Changed:
		out.Message = fmt.Sprintf("fetched: %d ranges of %d hosts",
			len(sources.ValidatorRanges.Ranges), len(sources.ValidatorRanges.Hosts))
	default:
		out.Message = "fetched: unchanged"
	}

	return out
}
