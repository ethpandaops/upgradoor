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
	// NeedOverride brings a container in line with the host's overrides
	// file: onto an override, or back off one.
	NeedOverride NeedKind = "override"
	// NeedSpec brings a container in line with what Ansible deployed it as
	// (the image and command line in the host's managed.yaml), overrides
	// applied: the soft deploy. It is rolled in the rollout of the
	// container's team and kind.
	NeedSpec NeedKind = "spec"
)

// Verb returns the verb that serves a need of this kind.
func (k NeedKind) Verb() Verb {
	switch k {
	case NeedImage, NeedOverride, NeedSpec:
		return VerbRoll
	case NeedPackages:
		return VerbPackages
	case NeedReboot:
		return VerbReboot
	case NeedAction:
		return VerbAction
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
	// Local is set for a need that was asked for on the host itself: every
	// override need (the overrides file is the host's) and a roll requested
	// with the local command line. The controller serves them in a rollout
	// of the host's own, under the budget and the gate like any other.
	Local bool `json:"local,omitempty"`
	// PullError is why the last lease could not pull the need's image. It is
	// set until the agent lets the pull be tried again; a need with it is
	// not granted.
	PullError string `json:"pullError,omitempty"`

	Image    *ImageNeed    `json:"image,omitempty"`
	Override *OverrideNeed `json:"override,omitempty"`
	Spec     *SpecNeed     `json:"spec,omitempty"`
	Packages *PackagesNeed `json:"packages,omitempty"`
	Reboot   *RebootNeed   `json:"reboot,omitempty"`
	Action   *ActionNeed   `json:"action,omitempty"`
}

// ImageNeed asks to move one managed container to the digest its tag now
// points at. For a manual roll FromDigest may equal ToDigest: the container
// is then recreated on the same image.
type ImageNeed struct {
	Container string `json:"container"`
	// Kind and Team are the container's (Container.Kind, Container.Team):
	// the needs of one team and kind are one rollout.
	Kind ContainerKind `json:"kind,omitempty"`
	Team string        `json:"team,omitempty"`
	// Image is the container's image reference (tag form).
	Image      string `json:"image"`
	FromDigest string `json:"fromDigest,omitempty"`
	ToDigest   string `json:"toDigest"`
	// ToRevision is the revision label of the target image, if known.
	ToRevision string `json:"toRevision,omitempty"`
}

// OverrideNeed asks to recreate one managed container so that it runs what
// the host's overrides file says: another image, a changed command line, or
// the configured image and command again once the entry is gone. The need
// names the target for people; the agent takes the image and the flags from
// its own file when it serves the lease, never from the lease.
type OverrideNeed struct {
	Container string `json:"container"`
	// Image is the reference the container is recreated from.
	Image string `json:"image"`
	// FromImage is the reference it runs now, when that is another one.
	FromImage  string `json:"fromImage,omitempty"`
	FromDigest string `json:"fromDigest,omitempty"`
	ToDigest   string `json:"toDigest"`
	ToRevision string `json:"toRevision,omitempty"`
	// Flags is true when the command line changes.
	Flags bool `json:"flags,omitempty"`
	// Restore is true when the need takes an override away.
	Restore bool `json:"restore,omitempty"`
}

// Container is the container the need is about; empty for packages and
// reboots.
func (n *Need) Container() string {
	switch {
	case n.Image != nil:
		return n.Image.Container
	case n.Override != nil:
		return n.Override.Container
	case n.Spec != nil:
		return n.Spec.Container
	default:
		return ""
	}
}

// SpecNeed asks to recreate one managed container with the image and the
// command line the host's managed.yaml names now, overrides
// applied: Ansible changed them and left the restart to the agent. Like an override need it
// names the target for people; the agent takes the image and the command
// line from its own files when it serves the lease.
type SpecNeed struct {
	Container string `json:"container"`
	// Kind and Team are the container's (ImageNeed.Kind, ImageNeed.Team).
	Kind ContainerKind `json:"kind,omitempty"`
	Team string        `json:"team,omitempty"`
	// Key identifies the target across hosts: the image at its digest and
	// the arguments the command line gains and loses. Hosts whose rendered
	// command lines differ only in what stays the same share it.
	Key string `json:"key"`
	// Image is the reference the container is recreated from.
	Image string `json:"image"`
	// FromImage is the reference it runs now, when that is another one.
	FromImage  string `json:"fromImage,omitempty"`
	FromDigest string `json:"fromDigest,omitempty"`
	ToDigest   string `json:"toDigest"`
	ToRevision string `json:"toRevision,omitempty"`
	// Flags is true when the command line changes.
	Flags bool `json:"flags,omitempty"`
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

// OverrideNeedID is the ID of an override need. target identifies what the
// container is to run (image, digest and command line): the ID changes when
// the overrides file asks for something else.
func OverrideNeedID(container, target string) string {
	return "override/" + container + "/" + target
}

// SpecNeedID is the ID of a spec need.
func SpecNeedID(container, key string) string {
	return "spec/" + container + "/" + key
}

// LocalNeedID is the ID of a roll requested on the host.
func LocalNeedID(id string) string {
	return "local/" + id
}

// RebootNeedID is the ID of the observed reboot need.
const RebootNeedID = "reboot"

// ManualNeedID is the ID of a need created by a directive.
func ManualNeedID(directiveID string) string {
	return "manual/" + directiveID
}
