package scheduler

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/discovery"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// fakeLookups is the tracker of the followed tags, set by hand.
type fakeLookups struct {
	mu    sync.Mutex
	tags  map[string]discovery.Lookup
	asked []string
}

func (f *fakeLookups) RefreshTag(ref string, _ time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked = append(f.asked, ref)
	_, ok := f.tags[ref]

	return ok
}

func (f *fakeLookups) LookedUp(ref string) (discovery.Lookup, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	lk, ok := f.tags[ref]

	return lk, ok
}

func (f *fakeLookups) set(ref string, lk discovery.Lookup) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.tags[ref] = lk
}

func (f *fakeLookups) refreshes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.asked)
}

// tagMoveHarness is a fleet of hosts whose geth tag was looked up at
// digestA: an intermediate push, the tag points at digestB by now.
func tagMoveHarness(t *testing.T, hosts ...string) (*harness, *fakeLookups) {
	t.Helper()

	h := newHarness(t)
	lookups := &fakeLookups{tags: map[string]discovery.Lookup{
		imgGeth: {Digest: digestA, Started: h.now.Add(-time.Minute)},
	}}
	h.svc.opts.Lookups = lookups

	for _, name := range hosts {
		h.addHost(name, 0)
		h.wantsGeth(name, digestA)
	}

	return h, lookups
}

// wantsGeth makes the host run geth at digestOld and ask for digest.
func (h *harness) wantsGeth(name, digest string) {
	h.sync(name, func(st *protocol.SyncRequest) {
		st.Containers = []protocol.Container{container("geth", imgGeth, digestOld, digest, h.now)}
		st.Needs = []protocol.Need{imageNeed("geth", imgGeth, digest)}
	})
}

// supersede makes the host's agent report its lease done with every step
// superseded and nothing disrupted: the tag moved on before the pull.
func (h *harness) supersede(name string) {
	h.t.Helper()

	lease := h.assignment(name).Lease
	require.NotNil(h.t, lease, "host %s holds no lease", name)

	h.sync(name, func(st *protocol.SyncRequest) {
		op := &protocol.Operation{LeaseID: lease.ID, Phase: protocol.PhaseDone}
		for _, n := range lease.Needs {
			op.Steps = append(op.Steps, protocol.Step{
				NeedID: n.ID, Status: protocol.StepSuperseded, Error: "tag now points at " + digestB,
			})
		}

		st.Operation = op
	})
}

// queued returns the waiting reason of a host, empty when it does not wait.
func (h *harness) queued(name string) string {
	for _, q := range h.svc.Plan().Queue {
		if q.Host == name {
			return q.Reason
		}
	}

	return ""
}

func TestASupersededImageNeedWaitsForALookupOfItsTag(t *testing.T) {
	h, lookups := tagMoveHarness(t, "a", "b")
	h.tick()

	stale := h.openRollout(imageKey("", imgGeth, digestA))
	canary := h.leased()
	require.Len(t, canary, 1)

	host := canary[0]
	h.supersede(host)
	h.tick()

	// The lease ended without disruption, and the need is not granted again.
	assert.Empty(t, h.leased())
	assert.Equal(t, []string{imgGeth}, lookups.refreshes(), "one lookup asked for")
	assert.Equal(t, "waiting for a new lookup of "+imgGeth+": the host saw it move on", h.queued(host))

	for range 5 {
		h.advance(2 * time.Second)
		h.tick()
	}

	assert.Empty(t, h.leased())
	assert.Len(t, lookups.refreshes(), 1, "asked once")

	// The lookup finds the newer digest: the hosts take it up with their
	// next sync.
	lookups.set(imgGeth, discovery.Lookup{Digest: digestB, Started: h.now})
	h.advance(time.Second)
	h.tick()

	assert.Empty(t, h.leased())
	assert.Equal(t, "waiting for the host to take up the new digest of "+imgGeth, h.queued(host))

	h.wantsGeth("a", digestB)
	h.wantsGeth("b", digestB)
	h.tick()

	assert.Equal(t, fleet.RolloutSuperseded, h.rollout(stale.ID).State)

	fresh := h.openRollout(imageKey("", imgGeth, digestB))
	leased := h.leased()
	require.Len(t, leased, 1)

	lease := h.assignment(leased[0]).Lease
	require.NotNil(t, lease)
	assert.Equal(t, []string{protocol.ImageNeedID("geth", digestB)}, needIDs(lease.Needs))
	assert.Equal(t, fleet.HostLeased, memberStates(fresh)[leased[0]])
	assert.Len(t, lookups.refreshes(), 1)
}

func TestATagMoveEnds(t *testing.T) {
	tests := []struct {
		name string
		// after runs once the lease ended; it returns the time to advance.
		after func(h *harness, lookups *fakeLookups) time.Duration
	}{
		{
			name: "a lookup after the end finds the need's digest",
			after: func(h *harness, lookups *fakeLookups) time.Duration {
				lookups.set(imgGeth, discovery.Lookup{Digest: digestA, Started: h.now.Add(time.Second)})

				return 2 * time.Second
			},
		},
		{
			name: "no lookup comes within the wait",
			after: func(*harness, *fakeLookups) time.Duration {
				return 5 * time.Minute
			},
		},
		{
			name: "the lookups fail for the length of the wait",
			after: func(h *harness, lookups *fakeLookups) time.Duration {
				lookups.set(imgGeth, discovery.Lookup{Digest: digestA, Started: h.now.Add(time.Second), Error: "503"})

				return 5 * time.Minute
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, lookups := tagMoveHarness(t, "a")
			h.tick()
			h.supersede("a")
			h.tick()
			require.Empty(t, h.leased())

			wait := tt.after(h, lookups)
			h.advance(wait - time.Second)
			h.tick()

			if wait > time.Minute {
				assert.Empty(t, h.leased(), "within the wait")
			}

			h.advance(time.Second)
			h.tick()
			assert.Equal(t, []string{"a"}, h.leased())
		})
	}
}

func TestATagMoveThatRepeatsWaitsWhateverALookupSays(t *testing.T) {
	h, lookups := tagMoveHarness(t, "a")
	h.tick()
	h.supersede("a")
	h.tick()

	// The registry says the need's digest: the need goes again, and the
	// agent supersedes it again.
	lookups.set(imgGeth, discovery.Lookup{Digest: digestA, Started: h.now.Add(time.Second)})
	h.advance(2 * time.Second)
	h.tick()
	require.Equal(t, []string{"a"}, h.leased())

	h.supersede("a")
	h.tick()

	lookups.set(imgGeth, discovery.Lookup{Digest: digestA, Started: h.now.Add(time.Second)})
	h.advance(2 * time.Second)
	h.tick()

	assert.Empty(t, h.leased())
	assert.Contains(t, h.queued("a"), "move on again")
	assert.Len(t, lookups.refreshes(), 1, "no lookup for a repeat")

	h.advance(5 * time.Minute)
	h.tick()
	assert.Equal(t, []string{"a"}, h.leased())
}

func TestATagMoveOfAHostThatTookUpAnotherNeedEnds(t *testing.T) {
	h, _ := tagMoveHarness(t, "a")
	h.tick()
	h.supersede("a")
	h.tick()
	require.Len(t, h.svc.moves, 1)

	h.sync("a", func(st *protocol.SyncRequest) { st.Needs = nil })
	h.tick()
	assert.Empty(t, h.svc.moves)
}

func TestWithoutLookupsASupersededNeedIsGrantedAgain(t *testing.T) {
	h, _ := tagMoveHarness(t, "a")
	h.svc.opts.Lookups = nil

	h.tick()
	h.supersede("a")
	h.tick()
	assert.Equal(t, []string{"a"}, h.leased())
	assert.Empty(t, h.svc.moves)
}

func needIDs(needs []protocol.Need) []string {
	out := make([]string, 0, len(needs))
	for _, n := range needs {
		out = append(out, n.ID)
	}

	return out
}
