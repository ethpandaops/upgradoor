package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/issuer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/controller/controllertest"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/identity"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// streamEvent is one server-sent event; a comment line has the name ":".
type streamEvent struct {
	name string
	data string
}

// testStream reads one event stream.
type testStream struct {
	t      *testing.T
	status int
	header http.Header
	events chan streamEvent
}

// fastStreams makes the hub quick enough for a test. Call before start.
func fastStreams(h *harness) {
	h.srv.streamMinInterval = 60 * time.Millisecond
	h.srv.streamDebounce = 5 * time.Millisecond
}

// stream opens an event stream over real HTTP: a recorder cannot be read
// while the handler writes. authorization is the full header value.
func (h *harness) stream(path, authorization string) *testStream {
	h.t.Helper()

	srv := httptest.NewServer(h.handler)
	h.t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, http.NoBody)
	require.NoError(h.t, err)

	// The token is bound to the host the request names.
	req.Host = controllertest.ControllerHost

	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	resp, err := srv.Client().Do(req)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = resp.Body.Close() })

	st := &testStream{
		t: h.t, status: resp.StatusCode, header: resp.Header,
		events: make(chan streamEvent, 256),
	}

	go func() {
		defer close(st.events)

		var ev streamEvent

		lines := bufio.NewScanner(resp.Body)
		lines.Buffer(make([]byte, 0, 64<<10), 8<<20)

		for lines.Scan() {
			line := lines.Text()

			switch {
			case strings.HasPrefix(line, ":"):
				st.events <- streamEvent{name: ":", data: line}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				st.events <- ev
				ev = streamEvent{}
			}
		}
	}()

	return st
}

// next returns the next event; ok is false when the stream ended.
func (st *testStream) next() (ev streamEvent, ok bool) {
	st.t.Helper()

	select {
	case ev, ok = <-st.events:
		return ev, ok
	case <-time.After(5 * time.Second):
		require.FailNow(st.t, "no event within 5s")

		return ev, false
	}
}

// quiet asserts that nothing arrives for a while.
func (st *testStream) quiet(d time.Duration) {
	st.t.Helper()

	select {
	case ev, ok := <-st.events:
		require.Fail(st.t, "unexpected event", "%q %s (open: %v)", ev.name, ev.data, ok)
	case <-time.After(d):
	}
}

// change changes the fake store the way a write would, and says so.
func (h *harness) change(mod func(f *controllertest.Store)) {
	h.store.Lock()
	mod(h.store)
	h.store.Unlock()

	h.srv.Notify()
}

func TestEventStreamsBeforeStart(t *testing.T) {
	h := newHarness(t, nil)

	for _, path := range []string{PathPublicEvents, PathEvents} {
		rec := h.get(path)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
		assert.Equal(t, "5", rec.Header().Get("Retry-After"))
	}

	// Nobody listens: a notification is dropped.
	h.srv.Notify()
}

// event decodes the data of the next event, which must have the given name.
func (st *testStream) event(name string) map[string]json.RawMessage {
	st.t.Helper()

	for {
		ev, ok := st.next()
		require.True(st.t, ok, "the stream ended before %q", name)

		if ev.name == ":" {
			continue
		}

		require.Equal(st.t, name, ev.name, ev.data)

		var out map[string]json.RawMessage

		require.NoError(st.t, json.Unmarshal([]byte(ev.data), &out))

		return out
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	slices.Sort(out)

	return out
}

// A server that shuts down closes its streams first: an open stream would
// keep it waiting.
func TestCloseStreams(t *testing.T) {
	h := newHarness(t, nil)
	h.start()

	st := h.stream(PathPublicEvents, "")
	st.event(eventSnapshot)

	require.NoError(t, h.srv.Stop())

	_, open := st.next()
	assert.False(t, open, "the stream ended")
	assert.Equal(t, http.StatusServiceUnavailable, h.get(PathPublicEvents).Code)

	require.NoError(t, h.srv.Stop())
}

// The public stream carries the public tables and nothing else: not at the
// start, not after a change, and not for a client that asks for more.
func TestPublicEventStream(t *testing.T) {
	h := newHarness(t, nil)
	fastStreams(h)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	// What a host says about its validator range is not public either.
	h.store.Hosts["lighthouse-geth-1"].Status.Validators = &protocol.Validators{
		Start: 4242, End: 4343, Source: protocol.ValidatorsFromMarker,
		State: protocol.ValidatorsInvalid, Error: "zz range file unreadable",
	}
	h.start()

	st := h.stream(PathPublicEvents+"?host=lighthouse-geth-1", "Bearer whatever")
	require.Equal(t, http.StatusOK, st.status)
	assert.Equal(t, "text/event-stream", st.header.Get("Content-Type"))
	assert.Equal(t, "no", st.header.Get("X-Accel-Buffering"))
	// Cache-Control: the server's TestAPIHeaders.

	noLeak := func(event map[string]json.RawMessage) {
		for key, data := range event {
			for _, secret := range append([]string{"4242", "4343"}, controllertest.LeakSecrets...) {
				assert.NotContains(t, string(data), secret, key)
			}
		}
	}

	snapshot := st.event(eventSnapshot)
	assert.Equal(t, []string{
		tableHosts, wireKeyVersion, tableImages, tableNetwork, wireKeyStrings, tableSlots, wireKeyTime,
		wireKeySchema,
	}, keysOf(snapshot))
	noLeak(snapshot)

	// A slot is its sums: who of the hosts attested in it is not public.
	assert.JSONEq(t, `{"r":[[7190,1,4,3]]}`, string(snapshot[tableSlots]))

	// A change that the public views do not show is no event.
	h.change(func(f *controllertest.Store) { f.Hosts["lighthouse-geth-1"].RemoteAddr = "203.0.113.99" })
	st.quiet(300 * time.Millisecond)

	// One that they show is, and it is that one cell: the host caught up, so
	// no update is pending any more.
	h.change(func(f *controllertest.Store) {
		status := *f.Hosts["lighthouse-geth-1"].Status
		status.Needs = nil
		f.Hosts["lighthouse-geth-1"].Status = &status
	})

	patch := st.event(eventPatch)
	noLeak(patch)
	assert.Equal(t, []string{tableHosts, wireKeyVersion}, keysOf(patch))
	assert.JSONEq(t, `{"u":[["lighthouse-geth-1",3,0]]}`, string(patch[tableHosts]))
	assert.JSONEq(t, "2", string(patch[wireKeyVersion]), "one more than the snapshot's")
	st.quiet(300 * time.Millisecond)
}

// The authenticated stream carries every table and the details of the hosts
// it names. A sync that says what the last one said is no event; a change is
// the cells that changed.
func TestUserEventStream(t *testing.T) {
	c := controllertest.NewClock(controllertest.Epoch)
	provider := controllertest.NewAuthProvider(t, c, true)

	h := newHarness(t, func(o *harnessOptions) {
		o.AuthProviderURL = provider.Server.URL
		o.Now = c.Now
	})
	h.clock = c

	fastStreams(h)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)
	h.start()

	// Basic access is all a read needs, on a stream as on a route.
	authorization := provider.Grant(t, "+upgradoor")

	require.Equal(t, http.StatusUnauthorized, h.stream(PathEvents, "").status)

	tooMany := PathEvents + "?"
	for i := range maxStreamHosts + 1 {
		tooMany += fmt.Sprintf("host=h%d&", i)
	}

	require.Equal(t, http.StatusBadRequest, h.stream(tooMany, authorization).status)

	st := h.stream(PathEvents+"?host=lighthouse-geth-1&host=nope", authorization)
	require.Equal(t, http.StatusOK, st.status)

	snapshot := st.event(eventSnapshot)
	assert.Equal(t, []string{
		tableAudit, wireKeyDetails, tableHosts, wireKeyVersion, tableImages, tableLeases, tableMembers,
		tableNetwork, tablePlan, tableQueue, tableRollouts, wireKeyStrings, tableSettings, tableSlots,
		wireKeyTime, wireKeySchema,
	}, keysOf(snapshot))

	// A login sees which hosts missed attestations in a slot; a host that
	// attested everything is not named.
	assert.Regexp(t, `^\{"r":\[\[7190,1,4,3,\[\[\d+,2,1\]\]\]\]\}$`, string(snapshot[tableSlots]))

	var details map[string]json.RawMessage

	require.NoError(t, json.Unmarshal(snapshot[wireKeyDetails], &details))
	assert.JSONEq(t, "0", string(details["nope"]), "an unknown host")
	assert.Contains(t, string(details["lighthouse-geth-1"]), controllertest.SecretCertHash)

	// The next sync of a host says what the last one said, later.
	h.change(func(f *controllertest.Store) {
		host := f.Hosts["lighthouse-geth-2"]
		status := *host.Status
		status.UptimeSeconds += 30
		host.Status = &status
		host.LastSeen = host.LastSeen.Add(30 * time.Second)
	})
	st.quiet(300 * time.Millisecond)

	// A hold is lifted: two cells of the host's row, and of its details.
	h.change(func(f *controllertest.Store) { f.Hosts["lighthouse-geth-1"].HoldWanted = false })

	patch := st.event(eventPatch)
	assert.Equal(t, []string{wireKeyDetails, tableHosts, wireKeyVersion}, keysOf(patch))

	var table wireTableJSON

	require.NoError(t, json.Unmarshal(patch[tableHosts], &table))
	require.Len(t, table.Updates, 1)
	assert.Empty(t, table.Rows)
	assert.JSONEq(t, `"lighthouse-geth-1"`, string(table.Updates[0][0]))
	assert.Len(t, table.Updates[0], 1+2*2, "holdWanted and holdBy: %s", patch[tableHosts])

	// A host that is forgotten leaves the table, and its details say so.
	h.change(func(f *controllertest.Store) { delete(f.Hosts, "lighthouse-geth-1") })

	patch = st.event(eventPatch)
	require.NoError(t, json.Unmarshal(patch[tableHosts], &table))
	assert.Equal(t, []json.RawMessage{json.RawMessage(`"lighthouse-geth-1"`)}, table.Removed)
	assert.JSONEq(t, `{"lighthouse-geth-1":0}`, string(patch[wireKeyDetails]))
	assert.JSONEq(t, "3", string(patch[wireKeyVersion]))
}

// A stream is opened by a token and ends with it.
func TestEventStreamEndsWithTheToken(t *testing.T) {
	h, provider := authHarness(t, true)

	short := "Bearer " + provider.Token(t, "alice", controllertest.UserEmail, func(c *issuer.Config) {
		c.TTL = time.Second
	})

	st := h.stream(PathEvents, short)
	require.Equal(t, http.StatusOK, st.status)
	st.event(eventSnapshot)

	started := time.Now()
	st.event(eventExpired)

	_, open := st.next()
	assert.False(t, open, "the server closed the stream")
	assert.Less(t, time.Since(started), 3*time.Second)

	// The expired token does not open another one.
	h.clock.Advance(2 * time.Second)
	assert.Equal(t, http.StatusUnauthorized, h.stream(PathEvents, short).status)
}

// Notifications are coalesced, a plan that says the same is not sent again,
// and without a change nothing is sent but the heartbeat.
func TestEventStreamSendsWhatChangedOnce(t *testing.T) {
	h := newHarness(t, nil)
	fastStreams(h)
	h.srv.streamHeartbeat = 40 * time.Millisecond
	h.store.Hosts["lighthouse-geth-1"] = &fleet.Host{Name: "lighthouse-geth-1", LastSeen: controllertest.Epoch}
	h.sched.Set(func(f *controllertest.Scheduler) {
		f.CurrentPlan = &scheduler.Plan{ComputedAt: controllertest.Epoch, Budget: scheduler.Budget{TotalWeight: 64}}
	})
	h.start()

	st := h.stream(PathEvents, "")
	require.Equal(t, http.StatusOK, st.status)
	st.event(eventSnapshot)

	ev, ok := st.next()
	require.True(t, ok)
	assert.Equal(t, streamEvent{name: ":", data: ": ping"}, ev, "an idle stream gets a heartbeat")

	// The scheduler computed the same plan again, later.
	h.sched.Set(func(f *controllertest.Scheduler) {
		f.CurrentPlan = &scheduler.Plan{ComputedAt: controllertest.Epoch.Add(time.Minute), Budget: f.CurrentPlan.Budget}
	})
	h.srv.Notify()

	// A burst of writes: every one is announced, the last one is what counts.
	for i := 1; i <= 20; i++ {
		h.change(func(f *controllertest.Store) {
			f.Hosts["lighthouse-geth-1"].HoldWanted = true
			f.Hosts["lighthouse-geth-1"].HoldReason = fmt.Sprintf("reason %d", i)
		})
	}

	var patches []string

	deadline := time.After(400 * time.Millisecond)

collect:
	for {
		select {
		case ev := <-st.events:
			if ev.name == ":" {
				continue
			}

			require.Equal(t, eventPatch, ev.name, ev.data)
			assert.NotContains(t, ev.data, `"`+tablePlan+`"`)

			patches = append(patches, ev.data)
		case <-deadline:
			break collect
		}
	}

	require.NotEmpty(t, patches)
	assert.LessOrEqual(t, len(patches), 3, "twenty writes in a burst are not twenty events")
	assert.Contains(t, patches[len(patches)-1], "reason 20")
}

// A host goes stale because time passes: no write announces it.
func TestEventStreamHostGoesStale(t *testing.T) {
	h := newHarness(t, nil)
	fastStreams(h)

	staleAfter := fleet.DefaultSettings().HostStaleAfter.Std()
	h.store.Hosts["lighthouse-geth-1"] = &fleet.Host{
		Name: "lighthouse-geth-1", LastSeen: controllertest.Epoch.Add(100*time.Millisecond - staleAfter),
	}
	h.start()

	st := h.stream(PathEvents, "")
	st.event(eventSnapshot)

	// The hub wakes up when the host is due; by then the clock says so too.
	h.clock.Advance(time.Second)

	// The row now says when the host was last seen, and that it is stale.
	patch := st.event(eventPatch)
	cell := func(field string) int {
		return slices.IndexFunc(wire.schema.Schemas["wireHost"], func(f wireField) bool { return f.Name == field })
	}

	assert.JSONEq(t, fmt.Sprintf(`{"u":[["lighthouse-geth-1",%d,%d,%d,1]]}`, cell("lastSeen"),
		controllertest.Epoch.Add(100*time.Millisecond-staleAfter).Unix(), cell("stale")), string(patch[tableHosts]))
}

// A settled slot costs the stream a few tens of bytes, on a fleet of a
// thousand hosts as on one of four: a slot's row names only the hosts that
// missed attestations in it.
func TestEventStreamSlotOfALargeFleet(t *testing.T) {
	h := newHarness(t, nil)
	fastStreams(h)
	controllertest.LeakFixture(t, h.store, h.sched, h.beacon)

	for i := range 1000 {
		name := fmt.Sprintf("node-%04d", i)
		h.store.Hosts[name] = &fleet.Host{
			Name: name, LastSeen: controllertest.Epoch,
			Validators: &identity.ValidatorRange{Start: uint64(1000 + i*32), End: uint64(1032 + i*32)},
		}
	}

	h.store.Slots = []fleet.SlotParticipation{{Slot: 7190, Proposed: true, Assigned: 1000, Included: 1000}}
	h.start()

	st := h.stream(PathEvents, "")
	snapshot := st.event(eventSnapshot)
	assert.JSONEq(t, `{"r":[[7190,1,1000,1000]]}`, string(snapshot[tableSlots]))

	// Everybody attests.
	h.change(func(f *controllertest.Store) {
		f.Slots = append(f.Slots, fleet.SlotParticipation{Slot: 7191, Proposed: true, Assigned: 1000, Included: 1000})
	})

	ev, ok := st.next()
	require.True(t, ok)
	require.Equal(t, eventPatch, ev.name)
	assert.JSONEq(t, `{"i":2,"sl":{"r":[[7191,1,1000,1000]]}}`, ev.data)
	assert.Less(t, len(ev.data), 48)
	t.Logf("a settled slot of 1000 healthy hosts on the stream: %d bytes: %s", len(ev.data), ev.data)

	// Three of the thousand miss: the row names those three.
	h.change(func(f *controllertest.Store) {
		f.Slots = append(f.Slots, fleet.SlotParticipation{
			Slot: 7192, Proposed: true, Assigned: 1000, Included: 997,
			Hosts: []fleet.HostSlotParticipation{
				{Host: "node-0001", Assigned: 1}, {Host: "node-0002", Assigned: 1}, {Host: "node-0003", Assigned: 1},
				// What attested everything is not said, whoever hands it in.
				{Host: "node-0004", Assigned: 1, Included: 1},
			},
		})
	})

	ev, ok = st.next()
	require.True(t, ok)
	assert.Less(t, len(ev.data), 160)
	assert.NotContains(t, ev.data, "node-0004")
	t.Logf("a slot in which 3 of 1000 hosts missed: %d bytes: %s", len(ev.data), ev.data)
}
