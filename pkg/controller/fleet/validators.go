package fleet

import (
	"time"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// ValidatorRanges is the devnet's validator ranges inventory as last fetched:
// which host runs which validator indices.
type ValidatorRanges struct {
	// URL is where it was fetched from.
	URL string
	// ETag and LastModified are those of the answer it came in, for the next
	// conditional fetch.
	ETag         string
	LastModified string
	// FetchedAt is when it was fetched; zero for none.
	FetchedAt time.Time
	// Ranges are disjoint and ordered by start.
	Ranges []HostRange
}

// HostRange is a validator index range of one host.
type HostRange struct {
	Host string
	identity.ValidatorRange
}
