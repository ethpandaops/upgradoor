package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
)

// NeedKind is the kind of work a host needs a slot for.
type NeedKind string

// Need kinds. Each maps to exactly one verb.
const (
	NeedImage    NeedKind = "image"
	NeedPackages NeedKind = "packages"
	NeedReboot   NeedKind = "reboot"
)

// Verb returns the verb that serves a need of this kind.
func (k NeedKind) Verb() Verb {
	switch k {
	case NeedImage:
		return VerbRoll
	case NeedPackages:
		return VerbPackages
	case NeedReboot:
		return VerbReboot
	default:
		return ""
	}
}

// Need is one open need of a host. Exactly one of Image, Packages and Reboot
// is set, matching Kind. All needs of a host are served by one lease so the
// host goes down once.
type Need struct {
	// ID is stable for as long as the need is unchanged; see ImageNeedID,
	// PackagesNeedID, RebootNeedID and ManualNeedID.
	ID   string   `json:"id"`
	Kind NeedKind `json:"kind"`
	// DirectiveID is set for a need created by a manual roll or reboot
	// directive.
	DirectiveID string `json:"directiveId,omitempty"`

	Image    *ImageNeed    `json:"image,omitempty"`
	Packages *PackagesNeed `json:"packages,omitempty"`
	Reboot   *RebootNeed   `json:"reboot,omitempty"`
}

// ImageNeed asks to move one managed container to the digest its tag now
// points at. For a manual roll FromDigest may equal ToDigest: the container
// is then recreated on the same image.
type ImageNeed struct {
	Container string `json:"container"`
	// Image is the container's image reference (tag form).
	Image      string `json:"image"`
	FromDigest string `json:"fromDigest,omitempty"`
	ToDigest   string `json:"toDigest"`
	// ToRevision is the revision label of the target image, if known.
	ToRevision string `json:"toRevision,omitempty"`
}

// PackagesNeed asks to upgrade blocked packages.
type PackagesNeed struct {
	Packages []PackageChange `json:"packages"`
}

// PackageChange is one blocked package with its installed and candidate
// version.
type PackageChange struct {
	Name string `json:"name"`
	From string `json:"from"`
	To   string `json:"to"`
}

// Reboot reasons.
const (
	RebootReasonFlag   = "flag"   // /var/run/reboot-required exists
	RebootReasonKernel = "kernel" // a newer kernel than the running one is installed
	RebootReasonManual = "manual" // requested by a person
)

// RebootNeed asks to reboot the host.
type RebootNeed struct {
	Reasons []string `json:"reasons"`
}

// ImageNeedID is the ID of an observed image need.
func ImageNeedID(container, toDigest string) string {
	return "image/" + container + "/" + toDigest
}

// PackagesNeedID is the ID of an observed packages need. It changes whenever
// the set of target versions changes.
func PackagesNeedID(pkgs []PackageChange) string {
	return "packages/" + PackagesKey(pkgs)
}

// PackagesKey identifies a set of package target versions: two hosts that
// need the same packages at the same candidate versions share the key.
func PackagesKey(pkgs []PackageChange) string {
	parts := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		parts = append(parts, p.Name+"="+p.To)
	}

	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))

	return hex.EncodeToString(sum[:8])
}

// RebootNeedID is the ID of the observed reboot need.
const RebootNeedID = "reboot"

// ManualNeedID is the ID of a need created by a directive.
func ManualNeedID(directiveID string) string {
	return "manual/" + directiveID
}
