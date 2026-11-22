package views

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The public views are built field by field into the dedicated Public* types.
// Nothing is copied wholesale from a host, so a new field on fleet.Host or
// protocol.Container cannot reach an unauthenticated visitor by accident.

// The fixed texts that stand in the public network view for the error texts
// of the beacon service: those are err.Error() of beacon calls and can name
// the endpoint and in-cluster addresses.
const (
	beaconErrorPublic   = "beacon node unavailable"
	livenessErrorPublic = "attestation data missing or stale"
	timingErrorPublic   = "invalid chain timing"
)

// PublicGate is the network gate as everybody may see it.
func (v *Views) PublicGate() scheduler.Gate {
	// Gate() hands out a copy, reasons included.
	gate := v.Gate()
	scrubGate(&gate)

	return gate
}

// scrubGate removes the beacon service's error texts from a gate: from the
// network view (Error says that there is one in a fixed text, LivenessError
// and TimingError go) and from every reason that quotes one, which becomes
// the fixed text of that error. A reason that starts with a fixed text and a
// colon (the scheduler's "attestation data missing or stale: <error>") is
// cut back to the fixed text.
func scrubGate(gate *scheduler.Gate) {
	n := &gate.Network
	secrets := []struct{ text, public string }{
		{n.LivenessError, livenessErrorPublic},
		{n.TimingError, timingErrorPublic},
		{n.Error, beaconErrorPublic},
	}

	if n.Error != "" {
		n.Error = beaconErrorPublic
	}

	n.LivenessError, n.TimingError = "", ""

	reasons := make([]string, 0, len(gate.Reasons))

	for _, reason := range gate.Reasons {
		for _, secret := range secrets {
			if strings.HasPrefix(reason, secret.public+": ") ||
				(secret.text != "" && reasonQuotes(reason, secret.text)) {
				// Nothing of such a reason is kept: what surrounds the quote
				// may be part of the same error chain.
				reason = secret.public

				break
			}
		}

		if !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
	}

	gate.Reasons = reasons
}

// reasonQuotes reports whether reason carries the error text or one of the
// errors it wraps ("a: b: c" also matches "b: c" and "c").
func reasonQuotes(reason, errText string) bool {
	for {
		if strings.Contains(reason, errText) {
			return true
		}

		_, rest, found := strings.Cut(errText, ": ")
		if !found || strings.TrimSpace(rest) == "" {
			return false
		}

		errText = rest
	}
}

// Gate returns the network gate of the last plan, or, before the first plan,
// a closed gate around the beacon service's current view.
func (v *Views) Gate() scheduler.Gate {
	if plan := v.opts.Scheduler.Plan(); plan != nil {
		gate := plan.Gate
		gate.Reasons = append(make([]string, 0, len(plan.Gate.Reasons)), plan.Gate.Reasons...)

		return gate
	}

	return scheduler.Gate{
		OK:      false,
		Reasons: []string{"the scheduler has not planned yet"},
		Network: v.opts.Beacon.Health(),
	}
}

// NewPublicHosts builds the public host list.
func NewPublicHosts(hosts []*fleet.Host) PublicHosts {
	out := PublicHosts{Hosts: make([]PublicHost, 0, len(hosts))}

	for _, host := range hosts {
		ph := PublicHost{Name: host.Name}

		if host.Status != nil {
			ph.UpdatePending = len(host.Status.Needs) > 0
			ph.EL = publicImageRef(clientContainer(host.Status.Containers, protocol.KindEL))
			ph.CL = publicImageRef(clientContainer(host.Status.Containers, protocol.KindCL))
		}

		out.Hosts = append(out.Hosts, ph)
	}

	slices.SortFunc(out.Hosts, func(a, b PublicHost) int { return cmp.Compare(a.Name, b.Name) })

	return out
}

// clientContainer returns the host's managed container of kind, or nil.
func clientContainer(containers []protocol.Container, kind protocol.ContainerKind) *protocol.Container {
	for i := range containers {
		if containers[i].Managed && containers[i].Kind == kind {
			return &containers[i]
		}
	}

	return nil
}

func publicImageRef(c *protocol.Container) *PublicImageRef {
	if c == nil {
		return nil
	}

	return &PublicImageRef{
		Image:    c.Image,
		Digest:   c.Digest,
		Revision: c.Revision,
	}
}

// imageUse is one host running one image reference.
type imageUse struct {
	host     string
	digest   string
	revision string
	registry *protocol.RegistryCheck
}

// NewPublicImages groups the EL and CL containers of all hosts by image
// reference.
func NewPublicImages(hosts []*fleet.Host) PublicImages {
	type key struct {
		kind  protocol.ContainerKind
		image string
	}

	groups := make(map[key][]imageUse, 8)

	for _, host := range hosts {
		if host.Status == nil {
			continue
		}

		for _, kind := range []protocol.ContainerKind{protocol.KindEL, protocol.KindCL} {
			c := clientContainer(host.Status.Containers, kind)
			if c == nil || c.Image == "" {
				continue
			}

			k := key{kind: kind, image: c.Image}
			groups[k] = append(groups[k], imageUse{
				host:     host.Name,
				digest:   c.Digest,
				revision: c.Revision,
				registry: c.Registry,
			})
		}
	}

	out := PublicImages{Images: make([]PublicImage, 0, len(groups))}

	for k, uses := range groups {
		digest, revision := targetDigest(uses)

		img := PublicImage{
			Image:       k.image,
			Kind:        k.kind,
			Digest:      digest,
			Revision:    revision,
			HostsOn:     make([]string, 0, len(uses)),
			HostsBehind: make([]string, 0, len(uses)),
		}

		for _, use := range uses {
			if use.digest == digest {
				img.HostsOn = append(img.HostsOn, use.host)
			} else {
				img.HostsBehind = append(img.HostsBehind, use.host)
			}
		}

		slices.Sort(img.HostsOn)
		slices.Sort(img.HostsBehind)

		out.Images = append(out.Images, img)
	}

	slices.SortFunc(out.Images, func(a, b PublicImage) int {
		// EL before CL, then by reference.
		if a.Kind != b.Kind {
			if a.Kind == protocol.KindEL {
				return -1
			}

			return 1
		}

		return cmp.Compare(a.Image, b.Image)
	})

	return out
}

// targetDigest picks the digest an image reference points at: the newest
// successful registry check of any host; failing that the newest check that
// failed but still holds its last good digest; and when no agent has checked
// the registry at all (images built locally), the digest most hosts run.
func targetDigest(uses []imageUse) (digest, revision string) {
	for _, withErrors := range []bool{false, true} {
		var newest time.Time

		for _, use := range uses {
			reg := use.registry
			if reg == nil || reg.Digest == "" || (reg.Error != "") != withErrors {
				continue
			}

			if digest == "" || reg.CheckedAt.After(newest) ||
				(reg.CheckedAt.Equal(newest) && reg.Digest < digest) {
				digest, revision, newest = reg.Digest, reg.Revision, reg.CheckedAt
			}
		}

		if digest != "" {
			break
		}
	}

	if digest == "" {
		counts := make(map[string]int, len(uses))
		for _, use := range uses {
			counts[use.digest]++
		}

		best := -1

		for d, n := range counts {
			if n > best || (n == best && d < digest) {
				digest, best = d, n
			}
		}
	}

	if revision == "" {
		// The registry check may not know the revision; a host that already
		// runs the digest does.
		for _, use := range uses {
			if use.digest == digest && use.revision != "" {
				revision = use.revision

				break
			}
		}
	}

	return digest, revision
}
