package stream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// updateWire rewrites the files the page reads the encoding from:
//
//	go test ./pkg/controller/stream/ -run TestWireFiles -update-wire
var updateWire = flag.Bool("update-wire", false, "rewrite web/src/api/wire.*.json")

const (
	wireSchemaFile = "../../../web/src/api/wire.schema.json"
	wireGoldenFile = "../../../web/src/api/wire.golden.json"
)

// wireDecode is the reading side of the encoding, as the page implements it
// in web/src/api/wire.ts: the schema and nothing else says what a cell is.
func wireDecode(t *testing.T, schema string, row []any, strings []string) map[string]any {
	t.Helper()

	str := func(cell any) string {
		if n, ok := cell.(float64); ok {
			require.Less(t, int(n), len(strings))

			return strings[int(n)]
		}

		s, _ := cell.(string)

		return s
	}

	out := make(map[string]any, len(row))

	for i, f := range wire.schema.Schemas[schema] {
		var cell any = float64(0)
		if i < len(row) {
			cell = row[i]
		}

		if cell == float64(0) {
			switch {
			case f.Omit:
			case f.Type == wireBool:
				out[f.Name] = false
			case f.Type == wireNumber:
				out[f.Name] = float64(0)
			case f.Type == wireString || f.Type == wireRef:
				out[f.Name] = ""
			case f.Type == wireTime:
				out[f.Name] = float64(time.Time{}.Unix())
			default:
				out[f.Name] = nil
			}

			continue
		}

		switch f.Type {
		case wireBool:
			out[f.Name] = true
		case wireString, wireRef:
			out[f.Name] = str(cell)
		case wireJSON:
			out[f.Name] = cell.([]any)[0] //nolint:errcheck // a test: a wrong shape may panic.
		case wireList:
			list := make([]any, 0, 4)
			for _, item := range cell.([]any) { //nolint:errcheck // as above.
				list = append(list, str(item))
			}

			out[f.Name] = list
		case wireObject:
			out[f.Name] = wireDecode(t, f.Of, cell.([]any), strings) //nolint:errcheck // as above.
		case wireArray:
			list := make([]any, 0, 4)
			for _, item := range cell.([]any) { //nolint:errcheck // as above.
				list = append(list, wireDecode(t, f.Of, item.([]any), strings)) //nolint:errcheck // as above.
			}

			out[f.Name] = list
		default:
			out[f.Name] = cell
		}
	}

	return out
}

// plainJSON is v as the REST API would answer it, with every time as
// seconds since the Unix epoch: a stream does not carry more of a time.
func plainJSON(t *testing.T, v any) any {
	t.Helper()

	data, err := json.Marshal(v)
	require.NoError(t, err)

	var out any

	require.NoError(t, json.Unmarshal(data, &out))

	var walk func(v any) any

	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if at, err := time.Parse(time.RFC3339Nano, x); err == nil {
				return float64(at.Unix())
			}
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			for k := range x {
				x[k] = walk(x[k])
			}
		}

		return v
	}

	return walk(out)
}

// Whatever a view holds comes out of the encoding as it went in.
func TestWireRoundTrip(t *testing.T) {
	h := newHarness(t, nil)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	ctx := context.Background()

	detail, err := h.views.HostDetail(ctx, "lighthouse-geth-1")
	require.NoError(t, err)

	rollouts, err := h.views.Rollouts(ctx, 50)
	require.NoError(t, err)

	plan := h.views.Plan()
	started := controllertest.Epoch.Add(-time.Minute)
	lease := *h.store.Leases[0]
	lease.Operation = &protocol.Operation{
		LeaseID: lease.ID, Phase: protocol.PhaseApplying, StartedAt: started,
		Steps: []protocol.Step{{NeedID: "reboot", Status: protocol.StepRunning}},
	}
	lease.Rollouts = map[string]string{"reboot": "rollout-zz"}
	lease.EndedAt = &started

	views := []any{
		detail.HostSummary,
		wireDetail{HostSummary: detail.HostSummary, CertHash: detail.CertHash, FirstSeen: detail.FirstSeen},
		*detail.Inventory,
		detail.History[0],
		*rollouts.Open[0],
		lease,
		plan.Queue[0],
		plan.Budget,
		wireGate{OK: true, Reasons: []string{}, Network: wireNetwork{NetworkHealth: plan.Gate.Network}},
		views.NewPublicHosts([]*fleet.Host{h.store.Hosts["lighthouse-geth-1"]}).Hosts[0],
		views.HostSummary{Name: "empty", Roles: []string{}},
	}

	for _, strings := range []*stringTable{nil, newStringTable()} {
		for _, view := range views {
			b := newDocBuilder(strings, "")
			schema := wire.set.schemaOf(reflect.TypeOf(view))

			var row []any

			data, err := json.Marshal(b.row(reflect.ValueOf(view), schema))
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &row))

			var table []string
			if strings != nil {
				table = strings.list
			}

			assert.Equal(t, plainJSON(t, view), wireDecode(t, schema, row, table), "%s: %s", schema, data)
		}
	}
}

func TestWireRowsAreCompact(t *testing.T) {
	strings := newStringTable()
	b := newDocBuilder(strings, "public.")

	ref := &views.PublicImageRef{Image: "sigp/lighthouse:latest", Digest: "sha256:c1c1"}
	b.add(tableHosts, views.PublicHost{Name: "a", CL: ref})
	b.add(tableHosts, views.PublicHost{Name: "b", CL: ref, UpdatePending: true})
	require.NoError(t, b.err)

	data, err := json.Marshal(b.doc[tableHosts].full())
	require.NoError(t, err)
	assert.JSONEq(t, `{"r":[["a",0,[1,2]],["b",0,[1,2],1]]}`, string(data),
		"no keys, no trailing defaults, a repeated string once")
	assert.Equal(t, []string{"", "sigp/lighthouse:latest", "sha256:c1c1"}, strings.list)

	b.add(tableHosts, views.HostSummary{})
	require.Error(t, b.err, "a table holds rows of its schema only")
}

// What only a client's page and a host's details show of a container (its
// command line and override) is not in the host table: a host's row is byte
// for byte what it is without them.
func TestWireHostRowLeavesOutWhatTheClientPageServes(t *testing.T) {
	row := func(change func(c *protocol.Container)) string {
		c := protocol.Container{
			Name: "geth", Image: "ethpandaops/geth:master", Managed: true, Kind: protocol.KindEL,
			Team: "geth",
		}
		change(&c)

		sum := views.HostSummary{Name: "a", Roles: []string{}, Containers: []protocol.Container{c}}
		b := newDocBuilder(newStringTable(), "")
		b.add(tableHosts, newWireHost(&sum, true, map[string]time.Time{}))
		require.NoError(t, b.err)

		data, err := json.Marshal(b.doc[tableHosts].full())
		require.NoError(t, err)

		return string(data)
	}

	until := controllertest.Epoch.Add(time.Hour)

	assert.Equal(t, row(func(*protocol.Container) {}), row(func(c *protocol.Container) {
		c.Command = []string{"--http", "--syncmode=snap"}
		c.Override = &protocol.ContainerOverride{Image: "me/geth:fix", By: "sam", Until: &until}
		c.Overridable = true
	}))
}

// A patch names the cells that changed, the rows that are new and the rows
// that are gone, and nothing else.
func TestWireTableDiff(t *testing.T) {
	build := func(hosts ...views.PublicHost) *wireTable {
		b := newDocBuilder(nil, "public.")
		for _, host := range hosts {
			b.add(tableHosts, host)
		}

		require.NoError(t, b.err)

		return b.table(tableHosts)
	}

	ref := &views.PublicImageRef{Image: "geth:stable", Digest: "sha256:e1e1"}
	before := build(views.PublicHost{Name: "a", EL: ref, UpdatePending: true}, views.PublicHost{Name: "b"}, views.PublicHost{Name: "c"})

	_, changed := before.diff(before)
	assert.False(t, changed)

	after := build(views.PublicHost{Name: "a", EL: ref}, views.PublicHost{Name: "c", UpdatePending: true}, views.PublicHost{Name: "d"})

	patch, changed := after.diff(before)
	require.True(t, changed)

	data, err := json.Marshal(patch)
	require.NoError(t, err)
	assert.JSONEq(t, `{"r":[["d"]],"u":[["a",3,0],["c",3,1]],"x":["b"]}`, string(data))

	// A table of one row has no key: 0 stands for it.
	one := func(live uint64) wireDoc {
		b := newDocBuilder(nil, "")
		b.one(tableAudit, views.AuditHead{LatestID: int64(live)})
		b.order(tableLeases, []string{"x"})
		require.NoError(t, b.err)

		return b.doc
	}

	data, err = json.Marshal(one(8).diff(one(7)))
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":{"u":[[0,0,8]]}}`, string(data))

	data, err = json.Marshal(wireDoc{}.diff(one(7)))
	require.NoError(t, err)
	assert.JSONEq(t, `{"a":{"x":[0]},"le":{"o":0}}`, string(data))
}

// What the clock says is not in the network's row: slot after slot of a
// healthy chain encodes the same.
func TestWireNetworkFollowsTheClock(t *testing.T) {
	genesis := controllertest.Epoch.Add(-24 * time.Hour)
	at := func(slot uint64, head uint64) fleet.NetworkHealth {
		epoch := slot / 32

		return fleet.NetworkHealth{
			UpdatedAt: genesis.Add(time.Duration(slot)*12*time.Second + time.Second), GenesisTime: genesis,
			SecondsPerSlot: 12, SlotsPerEpoch: 32, Phases: fleet.SinglePhase(genesis, 12*time.Second),
			HeadSlot: head, CurrentEpoch: epoch, FinalizedEpoch: epoch - 2, FinalityLagEpochs: 2,
			LivenessKnown: true, LivenessEpoch: epoch - 1, Tracked: 200, Live: 200,
		}
	}
	// The row as built some time after the poll.
	row := func(n fleet.NetworkHealth, after time.Duration) wireNetwork {
		return newWireNetwork(n, n.UpdatedAt.Add(after))
	}

	first := row(at(7200, 7200), time.Second)
	assert.True(t, first.Clocked)
	assert.Zero(t, first.HeadSlot+first.CurrentEpoch+first.FinalizedEpoch)
	assert.True(t, first.UpdatedAt.IsZero())

	// The next slot, and a poll that still sees the slot before.
	assert.Equal(t, first, row(at(7201, 7201), time.Second))
	assert.Equal(t, first, row(at(7202, 7201), 5*time.Second))

	// The next epoch brings its participation, and nothing else.
	next := row(at(7232, 7232), time.Second)
	assert.Equal(t, first.LivenessEpoch+1, next.LivenessEpoch)

	next.LivenessEpoch = first.LivenessEpoch
	assert.Equal(t, first, next)

	// A head that stays behind is said, as how far.
	assert.Equal(t, uint64(5), row(at(7210, 7205), time.Second).HeadBehind)

	// An error that leaves the polls working moves
	// nothing either. A beacon node that stopped answering did so at a time
	// that stands still, and is said.
	failed := at(7210, 7210)
	failed.Error = "beacon node unreachable"
	assert.True(t, row(failed, time.Second).UpdatedAt.IsZero())

	silent := row(failed, 2*time.Minute)
	assert.Equal(t, failed.UpdatedAt, silent.UpdatedAt)
	assert.Equal(t, silent, row(failed, 3*time.Minute))

	// Numbers the clock does not explain travel as they are.
	odd := at(7210, 7210)
	odd.CurrentEpoch = 3
	assert.Equal(t, wireNetwork{NetworkHealth: odd}, row(odd, time.Second))
	assert.False(t, row(fleet.NetworkHealth{HeadSlot: 5}, time.Second).Clocked, "no timing yet")
}

// A sync moves the time a host was last seen and its uptime, and the probes'
// heads: none of that is in the host's row.
func TestWireHostRowIgnoresTheSync(t *testing.T) {
	boots := make(map[string]time.Time, 1)
	row := func(seen time.Time, uptime int64, head uint64, stale bool) wireHost {
		return newWireHost(&views.HostSummary{
			Name: "a", LastSeen: seen, UptimeSeconds: uptime, Stale: stale,
			Node: protocol.NodeHealth{
				EL: &protocol.ELHealth{Reachable: true, Peers: int(head % 7), Head: head},
				CL: &protocol.CLHealth{Reachable: true, Peers: 9, HeadSlot: head},
			},
			Containers: []protocol.Container{{Name: "geth", Registry: &protocol.RegistryCheck{
				Digest: "sha256:e1e1", CheckedAt: seen,
			}}},
		}, true, boots)
	}

	first := row(controllertest.Epoch, 3600, 100, false)
	assert.True(t, first.LastSeen.IsZero())
	assert.Equal(t, controllertest.Epoch.Add(-time.Hour), first.BootTime)

	// Thirty seconds on; the uptime is a second off, as whole seconds are.
	assert.Equal(t, first, row(controllertest.Epoch.Add(30*time.Second), 3631, 103, false))

	// A reboot is a new boot time; a stale host says when it was last seen.
	rebooted := row(controllertest.Epoch.Add(time.Minute), 20, 105, true)
	assert.Equal(t, controllertest.Epoch.Add(40*time.Second), rebooted.BootTime)
	assert.Equal(t, controllertest.Epoch.Add(time.Minute), rebooted.LastSeen)
}

// wireGolden is what the page's decoder is tested against: the views as the
// REST routes answer them, and the events of a stream that carries them.
type wireGolden struct {
	// Now is the page's clock when it reads the events, in seconds.
	Now  int64                      `json:"now"`
	REST map[string]json.RawMessage `json:"rest"`
	// Public and User are the snapshots of the two streams, Patch what
	// follows on the user stream after lighthouse-geth-2 was put on hold.
	Public json.RawMessage `json:"public"`
	User   json.RawMessage `json:"user"`
	Patch  json.RawMessage `json:"patch"`
	// SlotPatch follows it: slot 7191 settled without a block, and slot 7190
	// got the attestation it lacked.
	SlotPatch json.RawMessage `json:"slotPatch"`
	// views.Hosts is GET /api/v1/hosts after the patch.
	Hosts json.RawMessage `json:"hosts"`
}

// The page interprets the schema from a file and tests its decoder against
// real events: both files are written here, and stale ones fail the test.
func TestWireFiles(t *testing.T) {
	h := newHarness(t, nil)
	fastStreams(h)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	// A chain the clock explains, with the head a few slots behind.
	h.beacon.Network.Error = ""
	h.beacon.Network.HeadSlot = 7195
	until := controllertest.Epoch.Add(12 * time.Hour)
	h.sched.Set(func(f *controllertest.Scheduler) {
		f.CurrentPlan.Gate.Network = h.beacon.Network
		f.CurrentPlan.DeployWindow = scheduler.DeployWindow{Open: true, Until: &until}
	})
	h.start()

	golden := wireGolden{Now: controllertest.Epoch.Unix(), REST: make(map[string]json.RawMessage, 12)}

	for name, route := range map[string]string{
		"public-network": "/api/v1/public/network", "public-hosts": "/api/v1/public/hosts",
		"public-images": "/api/v1/public/images",
		"hosts":         "/api/v1/hosts", "plan": "/api/v1/plan", "rollouts": "/api/v1/rollouts?limit=50",
		"settings": "/api/v1/settings", "host": "/api/v1/hosts/lighthouse-geth-1",
	} {
		rec := h.get(route)
		require.Equal(t, http.StatusOK, rec.Code, route)

		golden.REST[name] = json.RawMessage(rec.Body.Bytes())
	}

	raw := func(event map[string]json.RawMessage) json.RawMessage {
		data, err := json.Marshal(event)
		require.NoError(t, err)

		return data
	}

	golden.Public = raw(h.stream(PathPublicEvents, "").event(eventSnapshot))

	user := h.stream(PathEvents+"?host=lighthouse-geth-1&host=nope", "")
	golden.User = raw(user.event(eventSnapshot))

	h.change(func(f *controllertest.Store) {
		host := f.Hosts["lighthouse-geth-2"]
		status := *host.Status
		status.Hold = protocol.Hold{Holds: []protocol.HostHold{
			{Holder: fleet.HolderStaff, By: "alice", Reason: "golden", At: controllertest.Epoch},
			{Holder: "geth", By: "gina", At: controllertest.Epoch},
		}}
		host.Status = &status
	})

	golden.Patch = raw(user.event(eventPatch))

	// A slot settles, and a late inclusion completes the one before it.
	h.change(func(f *controllertest.Store) {
		f.Slots[0].Included = 4
		f.Slots[0].Hosts[1].Included = 2
		f.Slots = append(f.Slots, fleet.SlotParticipation{
			Slot: 7191, Assigned: 3, Included: 3,
			Hosts: []fleet.HostSlotParticipation{{Host: "lighthouse-geth-2", Assigned: 3, Included: 3}},
		})
	})

	golden.SlotPatch = raw(user.event(eventPatch))
	golden.Hosts = json.RawMessage(h.get("/api/v1/hosts").Body.Bytes())

	body, err := json.MarshalIndent(golden, "", " ")
	require.NoError(t, err)

	for file, want := range map[string][]byte{wireSchemaFile: wire.json, wireGoldenFile: append(body, '\n')} {
		if *updateWire {
			require.NoError(t, os.WriteFile(file, want, 0o600))

			continue
		}

		have, err := os.ReadFile(filepath.Clean(file))
		require.NoError(t, err)
		assert.Equal(t, string(want), string(have),
			"%s is out of date: go test ./pkg/controller/stream/ -run TestWireFiles -update-wire", file)
	}
}

// fleetHash is a hex digest that stands in for container and image IDs.
func fleetHash(parts ...any) string {
	sum := sha256.Sum256([]byte(fmt.Sprint(parts...)))

	return hex.EncodeToString(sum[:])
}

// largeFleet is n host summaries as a devnet reports them: an EL, a CL, a VC
// and an exporter per host, three client pairs, a few hosts with a need.
func largeFleet(n int) []views.HostSummary {
	pairs := [][2]string{
		{"ethpandaops/geth:master", "sigp/lighthouse:latest"},
		{"ethpandaops/nethermind:master", "consensys/teku:latest"},
		{"ethpandaops/besu:main", "ethpandaops/prysm-beacon-chain:develop"},
	}
	out := make([]views.HostSummary, 0, n)

	for i := range n {
		pair := pairs[i%len(pairs)]
		name := fmt.Sprintf("%s-%s-%d", pair[1][strings.Index(pair[1], "/")+1:strings.Index(pair[1], ":")],
			pair[0][strings.Index(pair[0], "/")+1:strings.Index(pair[0], ":")], i)
		container := func(cname, image string, kind protocol.ContainerKind) protocol.Container {
			return protocol.Container{
				Name: cname, ID: fleetHash(name, cname), Image: image, ImageID: "sha256:" + fleetHash(image),
				Digest: "sha256:" + fleetHash("digest", image), Revision: fleetHash("rev", image)[:12],
				Version: "v1.2.3", State: protocol.StateRunning, RestartCount: i % 3,
				StartedAt: controllertest.Epoch.Add(-time.Duration(i) * time.Minute), Managed: kind != "",
				Kind: kind, ConfiguredImage: image, Team: "devops",
				Registry: &protocol.RegistryCheck{
					Digest: "sha256:" + fleetHash("digest", image), CheckedAt: controllertest.Epoch, Pulled: true,
				},
			}
		}
		sum := views.HostSummary{
			Name: name, Roles: []string{"validator"}, Validators: 256,
			LastSeen: controllertest.Epoch, AgentVersion: "v0.1.0-abcdef",
			Containers: []protocol.Container{
				container("execution", pair[0], protocol.KindEL),
				container("beacon", pair[1], protocol.KindCL),
				container("validator", pair[1], protocol.KindVC),
				container("node-exporter", "prom/node-exporter:latest", ""),
			},
			Node: protocol.NodeHealth{
				EL: &protocol.ELHealth{Reachable: true, Peers: 50, Head: 123456},
				CL: &protocol.CLHealth{Reachable: true, Peers: 80, HeadSlot: 654321},
			},
			OS: protocol.OSSummary{
				ID: "debian", VersionID: "13", PrettyName: "Debian GNU/Linux 13 (trixie)", Arch: "amd64",
				RunningKernel: "6.12.57+deb13-amd64", NewestKernel: "6.12.57+deb13-amd64",
			},
			Pending:       protocol.Pending{Upgradable: i % 7, Security: i % 2},
			UptimeSeconds: int64(86400 + i),
			Verbs:         []protocol.Verb{protocol.VerbRoll, protocol.VerbPackages, protocol.VerbReboot},
			Needs:         []protocol.Need{},
			Plan:          scheduler.HostPlan{Slot: scheduler.SlotIdle, Weight: 256, Live: 256, LiveKnown: true},
		}

		if i%10 == 0 {
			sum.Needs = []protocol.Need{{
				ID: fleetHash("need", name)[:16], Kind: protocol.NeedImage,
				Image: &protocol.ImageNeed{Container: "execution", Image: pair[0], ToDigest: "sha256:" + fleetHash(i)},
			}}
		}

		out = append(out, sum)
	}

	return out
}

// The host table of a thousand hosts: what a page load carries, and what a
// build costs when nothing changed. A row whose status hash and plan did not
// change is not encoded again; one that changed is.
func TestWireHostTableOfALargeFleet(t *testing.T) {
	const n = 1000

	sums := largeFleet(n)
	hashes := make([]string, n)

	for i := range hashes {
		hashes[i] = fleetHash("status", i)
	}

	st := newEventState()
	planned := func(string) bool { return true }

	build := func() wireDoc {
		b := newDocBuilder(st.private.strings, "")
		st.hostRows = addHosts(b, sums, hashes, planned, st.boots, st.hostRows)
		require.NoError(t, b.err)

		return b.doc
	}

	start := time.Now()
	doc := build()
	first := time.Since(start)

	full, err := json.Marshal(map[string]any{tableHosts: doc[tableHosts].full(), wireKeyStrings: st.private.strings.list})
	require.NoError(t, err)

	start = time.Now()
	again := build()
	cached := time.Since(start)

	assert.Empty(t, again.diff(doc), "nothing changed")
	assert.Less(t, len(full)/n, 300, "bytes per host")
	t.Logf("host table of %d hosts: %d bytes, %d per host; first build %s, a build without changes %s",
		n, len(full), len(full)/n, first, cached)

	// A changed plan is a changed row, whatever the hash says; so is a new hash.
	sums[3].Plan.Slot = scheduler.SlotLeased
	sums[3].Plan.LeaseID = "l-1"
	sums[4].Pending.Security = 9
	hashes[4] = "changed"
	sums[5].Pending.Security = 9 // without a new hash: the cached row stays

	patch, err := json.Marshal(build().diff(again))
	require.NoError(t, err)
	assert.Contains(t, string(patch), sums[3].Name)
	assert.Contains(t, string(patch), sums[4].Name)
	assert.NotContains(t, string(patch), sums[5].Name)
}
