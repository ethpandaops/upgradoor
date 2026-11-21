package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/authz"
	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/controller/userapi"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
)

// The event streams push the views of the REST API to the web UI as
// server-sent events, so the page does not poll.
//
//	GET /api/v1/public/events          the public views, for everybody
//	GET /api/v1/events[?host=<name>]   every view, behind the user
//	                                   authentication like the REST routes
//	GET /api/v1/events/schema          the description of the encoding
//
// A stream starts with one "snap" event: every table of its audience in the
// wire encoding (wire.go). After that it carries "patch" events, one per
// build that found a difference, each with nothing but the rows and cells
// that changed. What follows from the clock (the head slot, the current
// epoch, how long ago a host synced) is not in the tables at all, so a quiet
// fleet sends heartbeats and nothing else.
//
// Nothing is replayed: a client that reconnects gets a new snapshot. A
// patch says which version of the tables it makes ("i", one more than the
// one before); a client that sees a gap reconnects.
//
// Nothing is built on a timer. Whoever changes state (the scheduler after a
// commit or a plan that differs, the agent API after a sync or an inventory,
// the user API after an action) calls Handler.Notify; the hub then builds the
// tables once, compares them with what the subscribers have, and sends what
// differs, at most once per streamMinInterval. Without subscribers nothing
// is built.

const (
	PathPublicEvents = "/api/v1/public/events"
	PathEvents       = "/api/v1/events"

	eventSnapshot = "snap"    // every table of the stream
	eventPatch    = "patch"   // what changed since the event before
	eventExpired  = "expired" // the token expired; the stream ends

	// streamRolloutLimit is the window of the rollout view on a stream.
	streamRolloutLimit = 50
	// streamSlotEpochs is how many epochs before the current one a stream
	// carries the slots of: see streamSlots.
	streamSlotEpochs = 2
	// maxStreamHosts bounds the host details one stream may ask for.
	maxStreamHosts = 16
	// maxStreamHostName bounds one name in the host parameter.
	maxStreamHostName = 253
	// maxStreams bounds the open streams: the public one needs no login.
	maxStreams = 256
	// maxStreamStrings bounds a string table. One that grows past it is
	// started again, with every stream of its audience.
	maxStreamStrings = 20000

	// streamMinInterval is the least time between two builds for the
	// subscribers; streamDebounce lets the writes of one change settle.
	streamMinInterval = time.Second
	streamDebounce    = 50 * time.Millisecond
	// streamHeartbeat is the time between comment lines on an idle stream.
	// It must stay below the idle timeout of every proxy in front.
	streamHeartbeat = 15 * time.Second
	// streamWriteTimeout bounds one write to a client.
	streamWriteTimeout = 30 * time.Second
	// streamBacklog is how many builds a client may fall behind before its
	// stream is closed.
	streamBacklog = 16
	// streamBuildTimeout bounds the store reads of one build.
	streamBuildTimeout = 20 * time.Second
)

var (
	// errStreamsClosed is returned while the server is not started.
	errStreamsClosed = errors.New("the event stream is not available yet")
	// errStreamsFull is returned while maxStreams streams are open.
	errStreamsFull = errors.New("too many open event streams")

	heartbeatFrame = []byte(": ping\n\n")
)

// frame builds one server-sent event. data is JSON on one line.
func frame(event string, data []byte) []byte {
	out := make([]byte, 0, len(event)+len(data)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, "\ndata: "...)
	out = append(out, data...)

	return append(out, "\n\n"...)
}

// Notify says that state the API serves may have changed. It never blocks,
// so it can be called under any lock.
func (s *Handler) Notify() {
	if hub := s.events.Load(); hub != nil {
		hub.notify()
	}
}

// Stop ends every open event stream and refuses new ones. An open stream
// never becomes idle, so an http.Server that serves the handler cannot shut
// down gracefully before this was called.
func (s *Handler) Stop() error {
	s.events.stop()

	return nil
}

// HandlePublic serves GET /api/v1/public/events: the public views.
func (s *Handler) HandlePublic(w http.ResponseWriter, r *http.Request) {
	s.serveEvents(w, r, nil, nil)
}

// Handle serves GET /api/v1/events for u, whom the user authentication let
// in: every view, and the details of the hosts the "host" parameter names.
// The stream ends when the token that opened it expires.
func (s *Handler) Handle(w http.ResponseWriter, r *http.Request, u *authz.User) {
	// A login whose end is not known cannot be held to it.
	if !u.Open && u.Expires.IsZero() {
		userapi.Unauthorized(s.log, w)

		return
	}

	hosts, err := streamHosts(r)
	if err != nil {
		httpjson.BadRequest(s.log, w, err)

		return
	}

	s.serveEvents(w, r, u, hosts)
}

// streamHosts reads the hosts whose details a stream asks for.
func streamHosts(r *http.Request) ([]string, error) {
	names := r.URL.Query()["host"]
	out := make([]string, 0, len(names))

	for _, name := range names {
		if name == "" || len(name) > maxStreamHostName {
			return nil, httpjson.BadRequestf("host must be a host name")
		}

		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}

	if len(out) > maxStreamHosts {
		return nil, httpjson.BadRequestf("at most %d hosts per stream", maxStreamHosts)
	}

	return out, nil
}

// serveEvents runs one stream; u is nil for the public one.
func (s *Handler) serveEvents(w http.ResponseWriter, r *http.Request, u *authz.User, hosts []string) {
	ctx := r.Context()

	hub := s.events.Load()
	if hub == nil {
		w.Header().Set("Retry-After", "5")
		httpjson.Error(s.log, w, http.StatusServiceUnavailable, errStreamsClosed.Error())

		return
	}

	sub, err := hub.subscribe(ctx, u != nil, hosts)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		httpjson.Error(s.log, w, http.StatusServiceUnavailable, err.Error())

		return
	}

	defer hub.unsubscribe(sub)

	// The token is checked once, here; the stream must not outlive it.
	var expired <-chan time.Time

	if u != nil && !u.Open {
		timer := time.NewTimer(u.Expires.Sub(s.now()))
		defer timer.Stop()

		expired = timer.C
	}

	heartbeat := time.NewTicker(s.streamHeartbeat)
	defer heartbeat.Stop()

	out := newStreamWriter(w)
	out.start()

	for {
		var writeErr error

		select {
		case <-ctx.Done():
			return
		case frames, open := <-sub.out:
			if !open {
				// Too far behind, or the server stops: the client reconnects.
				return
			}

			writeErr = out.write(frames...)
		case <-heartbeat.C:
			writeErr = out.write(heartbeatFrame)
		case <-expired:
			_ = out.write(frame(eventExpired, []byte("{}")))

			return
		}

		if writeErr != nil {
			s.log.WithError(writeErr).Debug("Event stream ended")

			return
		}
	}
}

// streamWriter writes events to one client and flushes after each batch.
type streamWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newStreamWriter(w http.ResponseWriter) *streamWriter {
	return &streamWriter{w: w, rc: http.NewResponseController(w)}
}

// start sends the headers, so the client knows the stream is open before
// the first event.
func (sw *streamWriter) start() {
	header := sw.w.Header()
	header.Set(httpjson.HeaderContentType, "text/event-stream")
	// nginx buffers a proxied answer unless it is told not to.
	header.Set("X-Accel-Buffering", "no")
	sw.w.WriteHeader(http.StatusOK)

	_ = sw.rc.Flush()
}

func (sw *streamWriter) write(frames ...[]byte) error {
	// The server's write timeout is that of a request; a stream gets a new
	// deadline with every write. A writer that cannot set one has none.
	if err := sw.rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("set write deadline: %w", err)
	}

	for _, f := range frames {
		if _, err := sw.w.Write(f); err != nil {
			return fmt.Errorf("write event: %w", err)
		}
	}

	if err := sw.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("flush events: %w", err)
	}

	return nil
}

// subscriber is one open stream.
type subscriber struct {
	// private is false for the public stream, which gets the public tables
	// and nothing else.
	private bool
	// hosts are the hosts whose details the stream asked for.
	hosts []string
	// out carries the events, one batch per build. The hub closes it.
	out chan [][]byte
}

// eventHub builds the tables when something changed and hands the
// subscribers what differs. One goroutine owns all of its state.
type eventHub struct {
	log      logrus.FieldLogger
	srv      *Handler
	interval time.Duration
	debounce time.Duration

	dirty   chan struct{}
	join    chan *subscriber
	leave   chan *subscriber
	streams atomic.Int32

	cancel  context.CancelFunc
	stopped chan struct{}

	// Owned by run.
	subs  map[*subscriber]struct{}
	state eventState
}

// audience is what the streams of one kind (public, authenticated) have been
// sent: the tables, the strings and the version the next patch makes.
type audience struct {
	doc     wireDoc
	strings *stringTable
	// sent is how many strings the subscribers have.
	sent    int
	version uint64
}

func newAudience() *audience {
	return &audience{doc: wireDoc{}, strings: newStringTable(), sent: 1}
}

// eventState is what every subscriber has been sent.
type eventState struct {
	// built is false until the first build; stale is set by a notification
	// and cleared by the build that follows it.
	built bool
	stale bool
	at    time.Time

	public  *audience
	private *audience
	// details holds the tables of every host somebody asked for; nil for a
	// host that does not exist.
	details map[string]wireDoc
	// boots remembers the boot time sent for each host (newWireHost).
	boots map[string]time.Time
	// hostRows are the encoded host rows of the last build (addHosts).
	hostRows map[string]cachedHostRow
}

func newEventState() eventState {
	return eventState{
		public:  newAudience(),
		private: newAudience(),
		details: make(map[string]wireDoc, 2),
		boots:   make(map[string]time.Time, 16),
	}
}

func newEventHub(log logrus.FieldLogger, srv *Handler) *eventHub {
	return &eventHub{
		log:      log,
		srv:      srv,
		interval: srv.streamMinInterval,
		debounce: srv.streamDebounce,
		dirty:    make(chan struct{}, 1),
		join:     make(chan *subscriber),
		leave:    make(chan *subscriber),
		stopped:  make(chan struct{}),
		subs:     make(map[*subscriber]struct{}, 8),
		state:    newEventState(),
	}
}

// start runs the hub until stop.
func (h *eventHub) start(ctx context.Context) {
	ctx, h.cancel = context.WithCancel(ctx)

	go h.run(ctx)
}

// stop ends the hub, which closes every stream, and waits for it.
func (h *eventHub) stop() {
	h.cancel()
	<-h.stopped
}

func (h *eventHub) notify() {
	select {
	case h.dirty <- struct{}{}:
	default:
	}
}

// subscribe registers a stream. The snapshot is the first batch on its
// channel.
func (h *eventHub) subscribe(ctx context.Context, private bool, hosts []string) (*subscriber, error) {
	if h.streams.Add(1) > maxStreams {
		h.streams.Add(-1)

		return nil, errStreamsFull
	}

	sub := &subscriber{private: private, hosts: hosts, out: make(chan [][]byte, streamBacklog)}

	select {
	case h.join <- sub:
		return sub, nil
	case <-h.stopped:
		h.streams.Add(-1)

		return nil, errStreamsClosed
	case <-ctx.Done():
		h.streams.Add(-1)

		return nil, ctx.Err()
	}
}

func (h *eventHub) unsubscribe(sub *subscriber) {
	select {
	case h.leave <- sub:
	case <-h.stopped:
	}

	h.streams.Add(-1)
}

func (h *eventHub) run(ctx context.Context) {
	defer close(h.stopped)

	// build fires when the subscribers are due a build; staleAt when the
	// next host goes stale, which no write announces.
	build := time.NewTimer(time.Hour)
	build.Stop()

	staleAt := time.NewTimer(time.Hour)
	staleAt.Stop()

	defer build.Stop()
	defer staleAt.Stop()

	armed := false
	arm := func() {
		if armed || len(h.subs) == 0 {
			return
		}

		armed = true

		build.Reset(max(h.debounce, h.interval-time.Since(h.state.at)))
	}

	for {
		select {
		case <-ctx.Done():
			for sub := range h.subs {
				close(sub.out)
			}

			return
		case sub := <-h.join:
			h.admit(ctx, staleAt, sub)

			if h.state.stale {
				arm()
			}
		case sub := <-h.leave:
			h.drop(sub)
		case <-h.dirty:
			h.state.stale = true

			arm()
		case <-staleAt.C:
			h.state.stale = true

			arm()
		case <-build.C:
			armed = false

			if h.state.stale && len(h.subs) > 0 {
				h.rebuild(ctx, staleAt, nil)

				// A build that failed is tried again.
				if h.state.stale {
					arm()
				}
			}
		}
	}
}

// admit takes a subscriber in and sends it its snapshot.
func (h *eventHub) admit(ctx context.Context, staleAt *time.Timer, sub *subscriber) {
	// Nobody holds an index of the string tables: start them again, so
	// they do not grow for as long as the controller runs.
	if len(h.subs) == 0 {
		h.state = newEventState()
	}

	// A build that is recent enough is good for a snapshot; what it lacks
	// follows with the next build.
	if !h.state.built || (h.state.stale && time.Since(h.state.at) >= h.interval) {
		h.rebuild(ctx, staleAt, sub.hosts)
	} else {
		h.addDetails(ctx, sub.hosts)
	}

	h.subs[sub] = struct{}{}

	if frame, ok := h.snapshot(sub); ok {
		h.send(sub, [][]byte{frame})
	}
}

// drop closes a subscriber's stream, once.
func (h *eventHub) drop(sub *subscriber) {
	if _, ok := h.subs[sub]; ok {
		delete(h.subs, sub)
		close(sub.out)
	}
}

// send hands a subscriber its events. One that does not keep up is dropped:
// it reconnects and starts from a snapshot.
func (h *eventHub) send(sub *subscriber, frames [][]byte) {
	if len(frames) == 0 {
		return
	}

	select {
	case sub.out <- frames:
	default:
		h.log.Debug("Event stream fell behind, closing it")
		h.drop(sub)
	}
}

// wanted lists the hosts whose details the subscribers, and extra, ask for.
func (h *eventHub) wanted(extra []string) []string {
	out := slices.Clone(extra)

	for sub := range h.subs {
		for _, name := range sub.hosts {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}

	return out
}

// Keys of an event besides its tables.
const (
	wireKeyVersion = "i" // the version of the tables after this event
	wireKeySchema  = "v" // snapshot: the version of the encoding
	wireKeyTime    = "t" // snapshot: the controller's clock, in seconds
	wireKeyStrings = "s" // the string table (snapshot) or what is new in it
	wireKeyDetails = "d" // host details by host name; 0 for an unknown host
)

// rebuild builds the tables and sends every subscriber what changed. extra
// names host details a joining subscriber asks for.
func (h *eventHub) rebuild(ctx context.Context, staleAt *time.Timer, extra []string) {
	ctx, cancel := context.WithTimeout(ctx, streamBuildTimeout)
	defer cancel()

	// What is notified from here on is not in this build.
	select {
	case <-h.dirty:
	default:
	}

	st := &h.state
	details := h.wanted(extra)

	// A string table that outgrew its bound starts again; so does everybody
	// who holds indices into it.
	if len(st.public.strings.list) > maxStreamStrings || len(st.private.strings.list) > maxStreamStrings {
		for sub := range h.subs {
			h.drop(sub)
		}

		h.state = newEventState()
		st = &h.state
	}

	built, err := h.srv.streamDocs(ctx, st, details)

	st.at = time.Now()

	if err != nil {
		st.stale = true

		if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.log.WithError(err).Warn("Cannot build the event stream tables")
		}

		return
	}

	st.stale = false
	st.built = true

	if built.nextStale > 0 {
		staleAt.Reset(built.nextStale)
	}

	public := st.public.advance(built.public)
	private := st.private.advance(built.private)

	changed := make(map[string]any, len(details))

	for name, doc := range built.details {
		before, known := st.details[name]

		switch {
		case doc == nil && (!known || before != nil):
			changed[name] = 0
		case doc != nil && before == nil:
			changed[name] = doc.full()
		case doc != nil:
			if patch := doc.diff(before); len(patch) > 0 {
				changed[name] = patch
			}
		}

		st.details[name] = doc
	}

	for name := range st.details {
		if !slices.Contains(details, name) {
			delete(st.details, name)
		}
	}

	for sub := range h.subs {
		body := public
		if sub.private {
			body = private
		}

		if frame, ok := h.patch(sub, body, changed); ok {
			h.send(sub, [][]byte{frame})
		}
	}
}

// advance takes a build into the audience and returns the body of the patch
// that says what changed, or nil.
func (a *audience) advance(doc wireDoc) map[string]any {
	body := doc.diff(a.doc)
	a.doc = doc

	if len(body) == 0 {
		// Strings of rows that were built and not sent stay in the table;
		// they travel with the first patch that has something to say.
		return nil
	}

	a.version++
	body[wireKeyVersion] = a.version

	if len(a.strings.list) > a.sent {
		body[wireKeyStrings] = a.strings.list[a.sent:]
		a.sent = len(a.strings.list)
	}

	return body
}

// patch builds the patch event of one subscriber: the changes of its
// audience and of the host details it asked for.
func (h *eventHub) patch(sub *subscriber, body, details map[string]any) ([]byte, bool) {
	var mine map[string]any

	if sub.private {
		for _, name := range sub.hosts {
			if d, ok := details[name]; ok {
				if mine == nil {
					mine = make(map[string]any, len(sub.hosts))
				}

				mine[name] = d
			}
		}
	}

	if body == nil && mine == nil {
		return nil, false
	}

	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}

	if mine != nil {
		out[wireKeyDetails] = mine
	}

	return h.event(eventPatch, out)
}

// snapshot is everything a new subscriber may see, from the state.
func (h *eventHub) snapshot(sub *subscriber) ([]byte, bool) {
	a := h.state.public
	if sub.private {
		a = h.state.private
	}

	out := a.doc.full()
	out[wireKeySchema] = wire.schema.Version
	out[wireKeyVersion] = a.version
	out[wireKeyTime] = h.srv.now().Unix()
	out[wireKeyStrings] = a.strings.list[:a.sent]

	if sub.private && len(sub.hosts) > 0 {
		details := make(map[string]any, len(sub.hosts))

		for _, name := range sub.hosts {
			doc, known := h.state.details[name]

			switch {
			case !known:
				// It could not be built; the next build sends it.
			case doc == nil:
				details[name] = 0
			default:
				details[name] = doc.full()
			}
		}

		out[wireKeyDetails] = details
	}

	return h.event(eventSnapshot, out)
}

func (h *eventHub) event(name string, body map[string]any) ([]byte, bool) {
	data, err := json.Marshal(body)
	if err != nil {
		h.log.WithError(err).Error("Cannot encode an event")

		return nil, false
	}

	return frame(name, data), true
}

// addDetails builds the host details a joining subscriber asks for and the
// state does not hold.
func (h *eventHub) addDetails(ctx context.Context, hosts []string) {
	ctx, cancel := context.WithTimeout(ctx, streamBuildTimeout)
	defer cancel()

	for _, name := range hosts {
		if _, ok := h.state.details[name]; ok {
			continue
		}

		doc, err := h.srv.hostDetailDoc(ctx, name)
		if err != nil {
			h.log.WithError(err).Warn("Cannot build a host detail for the event stream")

			// The next build sends it.
			h.state.stale = true

			continue
		}

		h.state.details[name] = doc
	}
}

// streamBuild is one build: the tables of both audiences and of the host
// details asked for.
type streamBuild struct {
	public  wireDoc
	private wireDoc
	details map[string]wireDoc
	// nextStale is the time until the next host goes stale; zero when none
	// will.
	nextStale time.Duration
}

// streamViews is everything the tables of a build are made of.
type streamViews struct {
	// now is the controller's clock at the build.
	now time.Time

	publicGate  scheduler.Gate
	publicHosts views.PublicHosts
	images      views.PublicImages
	// slots are the newest slots of the slot history, without their split
	// per host; slotHosts is that split, for the authenticated stream only.
	slots     []fleet.SlotParticipation
	slotHosts []fleet.HostSlotBucket
	plan      scheduler.Plan
	hosts     []views.HostSummary
	// statusHashes are the hosts' fleet.Host.StatusHash, in the order of
	// hosts.
	statusHashes []string
	// planned says whether the plan has an entry for a host.
	planned  func(host string) bool
	rollouts *views.Rollouts
	settings views.SettingsBody
	audit    views.AuditHead
}

// streamDocs reads the store once and builds every table from it.
func (s *Handler) streamDocs(ctx context.Context, st *eventState, details []string) (*streamBuild, error) {
	hosts, err := s.opts.Store.ListHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}

	settings, err := s.opts.Scheduler.Settings(ctx)
	if err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}

	v := &streamViews{
		now:          s.now(),
		publicGate:   s.views.PublicGate(),
		publicHosts:  views.NewPublicHosts(hosts),
		images:       views.NewPublicImages(hosts),
		plan:         s.views.Plan(),
		hosts:        make([]views.HostSummary, 0, len(hosts)),
		statusHashes: make([]string, 0, len(hosts)),
		settings:     views.SettingsBody{Settings: settings, Defaults: s.opts.DefaultSettings},
	}

	if slotErr := s.streamSlots(ctx, v); slotErr != nil {
		return nil, slotErr
	}

	if v.rollouts, err = s.views.Rollouts(ctx, streamRolloutLimit); err != nil {
		return nil, fmt.Errorf("rollouts: %w", err)
	}

	newest, err := s.opts.Store.ListAudit(ctx, store.AuditFilter{Limit: 1})
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}

	if len(newest) > 0 {
		v.audit.LatestID = newest[0].ID
	}

	plan := s.opts.Scheduler.Plan()
	now := v.now
	staleAfter := settings.HostStaleAfter.Std()

	v.planned = func(host string) bool {
		if plan == nil {
			return false
		}

		_, ok := plan.Hosts[host]

		return ok
	}

	out := &streamBuild{details: make(map[string]wireDoc, len(details))}

	for _, host := range hosts {
		sum := views.Summarize(host, plan, now, staleAfter)
		v.hosts = append(v.hosts, sum)
		v.statusHashes = append(v.statusHashes, host.StatusHash)

		// Stale is "silent for longer than staleAfter": a moment past it. It
		// changes a row without anything being written.
		left := host.LastSeen.Add(host.StaleAfter(staleAfter)).Sub(now) + 10*time.Millisecond
		if !sum.Stale && (out.nextStale == 0 || left < out.nextStale) {
			out.nextStale = left
		}
	}

	if out.public, out.private, err = v.docs(st); err != nil {
		return nil, err
	}

	for _, name := range details {
		if out.details[name], err = s.hostDetailDoc(ctx, name); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// docs encodes the views as the tables of the two audiences. The public
// tables are built from the public views only: what is not in a Public*
// type cannot get into them.
func (v *streamViews) docs(st *eventState) (public, private wireDoc, err error) {
	// Everybody.
	pub := newDocBuilder(st.public.strings, "public.")
	pub.one(tableNetwork, wireGate{
		OK: v.publicGate.OK, Reasons: views.NonNil(v.publicGate.Reasons),
		Network: newWireNetwork(v.publicGate.Network, v.now),
	})

	for _, host := range v.publicHosts.Hosts {
		pub.add(tableHosts, host)
	}

	addImages(pub, &v.images)
	addSlots(pub, v.slots, nil)

	// A login.
	priv := newDocBuilder(st.private.strings, "")
	gate := wireGate{
		OK: v.plan.Gate.OK, Reasons: v.plan.Gate.Reasons, Network: newWireNetwork(v.plan.Gate.Network, v.now),
	}

	if !slices.Equal(gate.Reasons, v.publicGate.Reasons) {
		gate.PublicReasons = v.publicGate.Reasons
	}

	priv.one(tableNetwork, gate)

	st.hostRows = addHosts(priv, v.hosts, v.statusHashes, v.planned, st.boots, st.hostRows)

	addImages(priv, &v.images)
	addSlots(priv, v.slots, v.slotHosts)
	priv.one(tablePlan, wirePlan{Paused: v.plan.Paused, Budget: v.plan.Budget})

	for i := range v.plan.Queue {
		priv.add(tableQueue, v.plan.Queue[i])
	}

	addRollouts(priv, v.rollouts)

	clients := views.NewClientIndex(v.hosts, &v.settings.Settings)

	for i := range clients.Views {
		priv.add(tableClients, clients.Views[i])
	}

	priv.one(tableSettings, v.settings)
	priv.one(tableAudit, v.audit)

	if err := errors.Join(pub.err, priv.err); err != nil {
		return nil, nil, err
	}

	return pub.doc, priv.doc, nil
}

// addImages puts the image references and the digest each points at.
func addImages(b *docBuilder, images *views.PublicImages) {
	for i := range images.Images {
		img := &images.Images[i]
		b.add(tableImages, wireImage{
			Key: string(img.Kind) + " " + img.Image, Digest: img.Digest, Revision: img.Revision,
		})
	}
}

// streamSlots reads the newest slots of the slot history: the ones that can
// still change. A slot is stored once it has settled, and a block of a later
// epoch can still carry attestations for it (the beacon service keeps two
// epochs before the current one open), so the stream holds the current epoch
// and the streamSlotEpochs before it. What is older stands still and is read
// through GET /api/v1/slots.
func (s *Handler) streamSlots(ctx context.Context, v *streamViews) error {
	health := s.opts.Beacon.Health()

	current, ok := health.SlotAt(v.now)
	if !ok || health.SlotsPerEpoch == 0 {
		// The chain's timing is not known (yet): there is nothing to place.
		return nil
	}

	var first uint64
	if epoch := current / health.SlotsPerEpoch; epoch > streamSlotEpochs {
		first = (epoch - streamSlotEpochs) * health.SlotsPerEpoch
	}

	var err error

	if v.slots, err = s.opts.Store.ListSlotParticipation(ctx, first); err != nil {
		return fmt.Errorf("slots: %w", err)
	}

	if v.slotHosts, err = s.opts.Store.SumHostSlotParticipation(ctx, first, 1); err != nil {
		return fmt.Errorf("slots per host: %w", err)
	}

	return nil
}

// addSlots puts the newest slots into a document: a slot that settles is a
// new row, a late inclusion changes the cells of a row, a slot that left the
// window is removed. hosts are the hosts that missed attestations, per slot
// (one "bucket" per slot); a slot in which every host attested, which is the
// normal one, is its four numbers whatever the size of the fleet. The public
// document is given no hosts, so which host missed cannot get into it.
func addSlots(b *docBuilder, slots []fleet.SlotParticipation, hosts []fleet.HostSlotBucket) {
	split := make(map[uint64][]fleet.HostSlotParticipation, len(slots))

	for i := range hosts {
		h := &hosts[i]
		split[h.Bucket] = append(split[h.Bucket], fleet.HostSlotParticipation{
			Host: h.Host, Assigned: h.Assigned, Included: h.Included,
		})
	}

	// A table without rows is still there: the page then knows that no slot
	// has settled, and does not wait for one.
	b.table(tableSlots)

	for i := range slots {
		row := slots[i]
		row.Hosts = split[row.Slot]
		b.add(tableSlots, row)
	}
}

// addRollouts puts the rollouts, their members and the leases.
func addRollouts(b *docBuilder, view *views.Rollouts) {
	ids := func(list []*fleet.Rollout) []string {
		out := make([]string, 0, len(list))

		for _, r := range list {
			out = append(out, r.ID)

			row := *r
			row.Hosts = nil
			b.add(tableRollouts, row)

			for i := range r.Hosts {
				b.add(tableMembers, wireMember{
					Key: r.ID + "/" + r.Hosts[i].Host, Rollout: r.ID, Index: i, RolloutHost: r.Hosts[i],
				})
			}
		}

		return out
	}
	b.order(tableRollouts, [][]string{ids(view.Open), ids(view.Recent)})

	leases := func(list []*fleet.Lease) []string {
		out := make([]string, 0, len(list))

		for _, l := range list {
			out = append(out, l.ID)
			b.add(tableLeases, *l)
		}

		return out
	}
	b.order(tableLeases, [][]string{leases(view.Leases), leases(view.RecentLeases)})
}

// hostDetailDoc builds the tables of one host's details; nil for a host
// that does not exist. Its strings travel as they are: only the streams
// that asked for the host get it, so it cannot use their shared table.
func (s *Handler) hostDetailDoc(ctx context.Context, name string) (wireDoc, error) {
	detail, err := s.views.HostDetail(ctx, name)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil //nolint:nilnil // no document is the answer for an unknown host.
	case err != nil:
		return nil, fmt.Errorf("host %s: %w", name, err)
	}

	b := newDocBuilder(nil, "d.")
	b.one(tableDetail, wireDetail{
		HostSummary: detail.HostSummary, CertHash: detail.CertHash, Generation: detail.Generation,
		FirstSeen: detail.FirstSeen, RemoteAddr: detail.RemoteAddr,
		ValidatorStart: detail.ValidatorStart, ValidatorEnd: detail.ValidatorEnd,
		ValidatorOverlaps: detail.ValidatorOverlaps, HasInventory: detail.Inventory != nil,
	})

	if detail.Inventory != nil {
		b.one(tableInventory, *detail.Inventory)
	}

	ids := make([]string, 0, len(detail.Leases))

	for _, l := range detail.Leases {
		ids = append(ids, l.ID)
		b.add(tableHostLease, *l)
	}

	b.order(tableHostLease, ids)

	for i := range detail.History {
		b.add(tableHostAudit, detail.History[i])
	}

	if b.err != nil {
		return nil, b.err
	}

	return b.doc, nil
}

// eventHubs starts and stops the hub with the server.
type eventHubs struct {
	atomic.Pointer[eventHub]

	mu sync.Mutex
}

func (e *eventHubs) start(ctx context.Context, log logrus.FieldLogger, srv *Handler) {
	e.mu.Lock()
	defer e.mu.Unlock()

	hub := newEventHub(log, srv)
	hub.start(ctx)
	e.Store(hub)
}

func (e *eventHubs) stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if hub := e.Swap(nil); hub != nil {
		hub.stop()
	}
}
