package api

import (
	"cmp"
	"net/http"
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

// beaconErrorPublic replaces the beacon service's error text in the public
// network view: the real text can name in-cluster URLs and addresses.
const beaconErrorPublic = "beacon node unavailable"

// handleConfig serves GET /api/v1/config.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, Config{
		Network:         s.opts.Network,
		Version:         s.opts.Version,
		AuthProviderURL: s.opts.AuthProviderURL,
	})
}

// handlePublicHosts serves GET /api/v1/public/hosts.
func (s *Server) handlePublicHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.opts.Store.ListHosts(r.Context())
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, publicHosts(hosts))
}

// handlePublicImages serves GET /api/v1/public/images.
func (s *Server) handlePublicImages(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.opts.Store.ListHosts(r.Context())
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	s.writeJSON(w, http.StatusOK, publicImages(hosts))
}

// handlePublicNetwork serves GET /api/v1/public/network.
func (s *Server) handlePublicNetwork(w http.ResponseWriter, _ *http.Request) {
	// gate() hands out a copy, reasons included.
	gate := s.gate()
	scrubGate(&gate)

	s.writeJSON(w, http.StatusOK, gate)
}

// History window of GET /api/v1/public/history, in epochs.
const (
	defaultHistoryEpochs = 128
	maxHistoryEpochs     = 512
)

// handlePublicHistory serves GET /api/v1/public/history?limit=<epochs>. It
// sums the per-host liveness rows per epoch: which host attested is not
// public, how much of the network did is.
func (s *Server) handlePublicHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	limit, err := queryLimit(r, defaultHistoryEpochs, maxHistoryEpochs)
	if err != nil {
		s.writeBadRequest(w, err)

		return
	}

	rows, err := s.opts.Store.ListLiveness(ctx, limit)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		s.internalError(w, r, err)

		return
	}

	health := s.opts.Beacon.Health()

	s.writeJSON(w, http.StatusOK, PublicHistory{
		Epochs:                  participation(rows, &health),
		MinParticipationPercent: settings.MinParticipationPercent,
		EpochSeconds:            health.EpochDuration().Seconds(),
	})
}

// participation sums liveness rows per epoch, oldest epoch first. Each point
// carries the time its epoch ended, placed by the chain's slot phases; a
// point whose epoch cannot be placed carries the zero time.
func participation(rows []fleet.Liveness, health *fleet.NetworkHealth) []ParticipationPoint {
	byEpoch := make(map[uint64]*ParticipationPoint, 64)

	for i := range rows {
		row := &rows[i]

		point, ok := byEpoch[row.Epoch]
		if !ok {
			point = &ParticipationPoint{Epoch: row.Epoch}
			byEpoch[row.Epoch] = point
		}

		point.Live += row.Live
		point.Total += row.Total
	}

	out := make([]ParticipationPoint, 0, len(byEpoch))

	for _, point := range byEpoch {
		// The point stands at the end of its epoch.
		if end, ok := health.EpochStart(point.Epoch + 1); ok {
			point.Time = end.UTC()
		}

		out = append(out, *point)
	}

	slices.SortFunc(out, func(a, b ParticipationPoint) int { return cmp.Compare(a.Epoch, b.Epoch) })

	return out
}

// scrubGate removes the beacon service's error text from a gate, both from
// the network view and from every reason that quotes it.
func scrubGate(gate *scheduler.Gate) {
	secret := gate.Network.Error
	if secret == "" {
		return
	}

	gate.Network.Error = beaconErrorPublic

	reasons := make([]string, 0, len(gate.Reasons))

	for _, reason := range gate.Reasons {
		if reasonQuotes(reason, secret) {
			// Nothing of such a reason is kept: what surrounds the quote may
			// be part of the same error chain.
			reason = beaconErrorPublic
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

// gate returns the network gate of the last plan, or, before the first plan,
// a closed gate around the beacon service's current view.
func (s *Server) gate() scheduler.Gate {
	if plan := s.opts.Scheduler.Plan(); plan != nil {
		gate := plan.Gate
		gate.Reasons = append(make([]string, 0, len(plan.Gate.Reasons)), plan.Gate.Reasons...)

		return gate
	}

	return scheduler.Gate{
		OK:      false,
		Reasons: []string{"the scheduler has not planned yet"},
		Network: s.opts.Beacon.Health(),
	}
}

// publicHosts builds the public host list.
func publicHosts(hosts []*fleet.Host) PublicHosts {
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
		Custom:   c.Custom,
	}
}

// imageUse is one host running one image reference.
type imageUse struct {
	host     string
	digest   string
	revision string
	registry *protocol.RegistryCheck
}

// publicImages groups the EL and CL containers of all hosts by image
// reference.
func publicImages(hosts []*fleet.Host) PublicImages {
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
// the registry at all (custom images), the digest most hosts run.
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
