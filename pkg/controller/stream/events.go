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
	// maxStreams bounds the open authenticated streams, maxPublicStreams the
	// public ones, which need no login and so cannot take the operators'.
	maxStreams       = 256
	maxPublicStreams = 256
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
	// errStreamsFull is returned while the streams of the kind asked for are
	// at their bound.
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

// NotifyHost says that the details of host may have changed. It notifies
// only while a stream asked for them, so a sync that changed nothing else
// does not rebuild the tables.
func (s *Handler) NotifyHost(host string) {
	if hub := s.events.Load(); hub != nil {
		hub.notifyHost(host)
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

	var access *views.Access

	if u != nil {
		a := s.access(u)
		access = &a
	}

	sub, err := hub.subscribe(ctx, access, hosts)
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
	// access is what a private stream reads of the hosts: the full tables
	// or the reduced ones, and its host details as access.Detail makes
	// them. accessKey is access.Key().
	access    views.Access
	accessKey string
	// hosts are the hosts whose details the stream asked for.
	hosts []string
	// out carries the events, one batch per build. The hub closes it.
	out chan [][]byte
}

// audience is the tables the subscriber is sent.
func (sub *subscriber) audience(st *eventState) *audience {
	switch {
	case !sub.private:
		return st.public
	case sub.access.Full:
		return st.private
	default:
		return st.reduced
	}
}

// detail names the details of one host as one access sees them.
func (sub *subscriber) detail(host string) detailKey {
	return detailKey{host: host, access: sub.accessKey}
}

// detailKey names the details of one host for one access.
type detailKey struct {
	host   string
	access string
}

// detailWant is the details of one host some stream asks for.
type detailWant struct {
	key    detailKey
	access views.Access
}

// eventHub builds the tables when something changed and hands the
// subscribers what differs. One goroutine owns all of its state.
type eventHub struct {
	log      logrus.FieldLogger
	srv      *Handler
	interval time.Duration
	debounce time.Duration

	dirty chan struct{}
	join  chan *subscriber
	leave chan *subscriber
	// streams and publicStreams count the open authenticated and public
	// streams; each kind has its own bound.
	streams       atomic.Int32
	publicStreams atomic.Int32

	// watched counts, per host, the open streams that asked for its details.
	watchMu sync.Mutex
	watched map[string]int

	cancel  context.CancelFunc
	stopped chan struct{}

	// Owned by run.
	subs  map[*subscriber]struct{}
	state eventState
}

// audience is what the streams of one kind (public, authenticated in full,
// authenticated reduced) have been sent: the tables, the strings and the
// version the next patch makes.
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
	// reduced is the audience of the logins without the full view of the
	// hosts (views.Access).
	reduced *audience
	// details holds the tables of every host somebody asked for, per
	// access; nil for a host that does not exist.
	details map[detailKey]wireDoc
	// boots remembers the boot time sent for each host (newWireHost).
	boots map[string]time.Time
	// hostRows and reducedRows are the encoded host rows of the last build
	// (addHosts), in full and reduced.
	hostRows    map[string]cachedHostRow
	reducedRows map[string]cachedHostRow
}

func newEventState() eventState {
	return eventState{
		public:  newAudience(),
		private: newAudience(),
		reduced: newAudience(),
		details: make(map[detailKey]wireDoc, 2),
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
		watched:  make(map[string]int, 8),
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

// subscribe registers a stream; access is nil for a public one. The
// snapshot is the first batch on its channel.
func (h *eventHub) subscribe(ctx context.Context, access *views.Access, hosts []string) (*subscriber, error) {
	private := access != nil
	open, bound := &h.publicStreams, int32(maxPublicStreams)

	if private {
		open, bound = &h.streams, maxStreams
	}

	if open.Add(1) > bound {
		open.Add(-1)

		return nil, errStreamsFull
	}

	sub := &subscriber{private: private, hosts: hosts, out: make(chan [][]byte, streamBacklog)}
	if private {
		sub.access, sub.accessKey = *access, access.Key()
	}

	select {
	case h.join <- sub:
		h.watch(sub.hosts, 1)

		return sub, nil
	case <-h.stopped:
		open.Add(-1)

		return nil, errStreamsClosed
	case <-ctx.Done():
		open.Add(-1)

		return nil, ctx.Err()
	}
}

func (h *eventHub) unsubscribe(sub *subscriber) {
	select {
	case h.leave <- sub:
	case <-h.stopped:
	}

	h.watch(sub.hosts, -1)

	if sub.private {
		h.streams.Add(-1)
	} else {
		h.publicStreams.Add(-1)
	}
}

// watch counts the streams that asked for the details of hosts.
func (h *eventHub) watch(hosts []string, delta int) {
	if len(hosts) == 0 {
		return
	}

	h.watchMu.Lock()
	defer h.watchMu.Unlock()

	for _, name := range hosts {
		if n := h.watched[name] + delta; n > 0 {
			h.watched[name] = n
		} else {
			delete(h.watched, name)
		}
	}
}

// notifyHost notifies when a stream asked for the details of host.
func (h *eventHub) notifyHost(host string) {
	h.watchMu.Lock()
	watched := h.watched[host] > 0
	h.watchMu.Unlock()

	if watched {
		h.notify()
	}
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
		h.rebuild(ctx, staleAt, sub)
	} else {
		h.addDetails(ctx, sub)
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

// wanted lists the host details the subscribers, and extra, ask for.
func (h *eventHub) wanted(extra *subscriber) []detailWant {
	out := make([]detailWant, 0, 4)
	add := func(sub *subscriber) {
		if !sub.private {
			return
		}

		for _, name := range sub.hosts {
			key := sub.detail(name)
			if !slices.ContainsFunc(out, func(w detailWant) bool { return w.key == key }) {
				out = append(out, detailWant{key: key, access: sub.access})
			}
		}
	}

	if extra != nil {
		add(extra)
	}

	for sub := range h.subs {
		add(sub)
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
// is a joining subscriber, whose host details are built too.
func (h *eventHub) rebuild(ctx context.Context, staleAt *time.Timer, extra *subscriber) {
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
	if len(st.public.strings.list) > maxStreamStrings || len(st.private.strings.list) > maxStreamStrings ||
		len(st.reduced.strings.list) > maxStreamStrings {
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

	bodies := map[*audience]map[string]any{
		st.public:  st.public.advance(built.public),
		st.private: st.private.advance(built.private),
		st.reduced: st.reduced.advance(built.reduced),
	}

	changed := make(map[detailKey]any, len(details))

	for key, doc := range built.details {
		before, known := st.details[key]

		switch {
		case doc == nil && (!known || before != nil):
			changed[key] = 0
		case doc != nil && before == nil:
			changed[key] = doc.full()
		case doc != nil:
			if patch := doc.diff(before); len(patch) > 0 {
				changed[key] = patch
			}
		}

		st.details[key] = doc
	}

	for key := range st.details {
		if _, ok := built.details[key]; !ok {
			delete(st.details, key)
		}
	}

	for sub := range h.subs {
		if frame, ok := h.patch(sub, bodies[sub.audience(st)], changed); ok {
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
func (h *eventHub) patch(sub *subscriber, body map[string]any, details map[detailKey]any) ([]byte, bool) {
	var mine map[string]any

	if sub.private {
		for _, name := range sub.hosts {
			if d, ok := details[sub.detail(name)]; ok {
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
	a := sub.audience(&h.state)
	out := a.doc.full()
	out[wireKeySchema] = wire.schema.Version
	out[wireKeyVersion] = a.version
	out[wireKeyTime] = h.srv.now().Unix()
	out[wireKeyStrings] = a.strings.list[:a.sent]

	if sub.private && len(sub.hosts) > 0 {
		details := make(map[string]any, len(sub.hosts))

		for _, name := range sub.hosts {
			doc, known := h.state.details[sub.detail(name)]

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
func (h *eventHub) addDetails(ctx context.Context, sub *subscriber) {
	if !sub.private {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, streamBuildTimeout)
	defer cancel()

	for _, name := range sub.hosts {
		key := sub.detail(name)
		if _, ok := h.state.details[key]; ok {
			continue
		}

		detail, err := h.srv.hostDetail(ctx, name)

		var doc wireDoc
		if err == nil {
			doc, err = detailDoc(sub.access.Detail(detail))
		}

		if err != nil {
			h.log.WithError(err).Warn("Cannot build a host detail for the event stream")

			// The next build sends it.
			h.state.stale = true

			continue
		}

		h.state.details[key] = doc
	}
}

// streamBuild is one build: the tables of the audiences and of the host
// details asked for.
type streamBuild struct {
	public  wireDoc
	private wireDoc
	reduced wireDoc
	details map[detailKey]wireDoc
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
	settings fleet.Settings
	audit    views.AuditHead
}

// streamDocs reads the store once and builds every table from it.
func (s *Handler) streamDocs(ctx context.Context, st *eventState, details []detailWant) (*streamBuild, error) {
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
		images:       s.views.PublicImages(hosts),
		plan:         s.views.Plan(),
		hosts:        make([]views.HostSummary, 0, len(hosts)),
		statusHashes: make([]string, 0, len(hosts)),
		settings:     settings,
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

	if v.audit.SSHLogins, err = s.opts.Store.SSHLoginsSeen(ctx); err != nil {
		return nil, fmt.Errorf("ssh logins: %w", err)
	}

	now := v.now
	staleAfter := settings.HostStaleAfter.Std()

	v.planned = func(host string) bool {
		_, ok := v.plan.Hosts[host]

		return ok
	}

	out := &streamBuild{details: make(map[detailKey]wireDoc, len(details))}

	for _, host := range hosts {
		sum := views.Summarize(host, &v.plan, now, staleAfter)
		v.hosts = append(v.hosts, sum)
		v.statusHashes = append(v.statusHashes, host.StatusHash)

		// Stale is "silent for longer than staleAfter": a moment past it. It
		// changes a row without anything being written.
		left := host.LastSeen.Add(host.StaleAfter(staleAfter)).Sub(now) + 10*time.Millisecond
		if !sum.Stale && (out.nextStale == 0 || left < out.nextStale) {
			out.nextStale = left
		}
	}

	if out.public, out.private, out.reduced, err = v.docs(st); err != nil {
		return nil, err
	}

	// Each host is read once, whatever the accesses that ask for it.
	read := make(map[string]*views.HostDetail, len(details))

	for _, want := range details {
		detail, ok := read[want.key.host]
		if !ok {
			if detail, err = s.hostDetail(ctx, want.key.host); err != nil {
				return nil, err
			}

			read[want.key.host] = detail
		}

		if out.details[want.key], err = detailDoc(want.access.Detail(detail)); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// docs encodes the views as the tables of the audiences. The public tables
// are built from the public views only: what is not in a Public* type
// cannot get into them. The reduced tables are the authenticated ones as a
// login without the full view of the hosts and without teams sees them
// (views.Access): one stream for every such login, whatever its teams.
func (v *streamViews) docs(st *eventState) (public, private, reduced wireDoc, err error) {
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

	clients := views.NewClientIndex(v.hosts, &v.settings)

	// A login.
	priv := newDocBuilder(st.private.strings, "")
	full := views.Access{Full: true}
	st.hostRows = v.loginDoc(priv, &full, v.hosts, clients, st.boots, st.hostRows)

	// A login without the full view.
	red := newDocBuilder(st.reduced.strings, "")
	none := views.Access{}
	sums := make([]views.HostSummary, 0, len(v.hosts))

	for i := range v.hosts {
		sums = append(sums, none.Summary(&v.hosts[i]))
	}

	st.reducedRows = v.loginDoc(red, &none, sums, clients, st.boots, st.reducedRows)

	if err := errors.Join(pub.err, priv.err, red.err); err != nil {
		return nil, nil, nil, err
	}

	return pub.doc, priv.doc, red.doc, nil
}

// loginDoc puts the tables of an authenticated audience, with the host rows
// of sums and what access shows of the rest. It returns the host row cache
// of the build (addHosts).
func (v *streamViews) loginDoc(
	b *docBuilder, access *views.Access, sums []views.HostSummary, clients *views.ClientIndex,
	boots map[string]time.Time, rows map[string]cachedHostRow,
) map[string]cachedHostRow {
	b.one(tableNetwork, wireGate{
		OK: v.plan.Gate.OK, Reasons: v.plan.Gate.Reasons, Network: newWireNetwork(v.plan.Gate.Network, v.now),
	})

	rows = addHosts(b, sums, v.statusHashes, v.planned, boots, rows)

	addImages(b, &v.images)
	addSlots(b, v.slots, v.slotHosts)
	b.one(tablePlan, wirePlan{
		Paused: v.plan.Paused, Budget: v.plan.Budget, DeployWindow: v.plan.DeployWindow,
	})

	for i := range v.plan.Queue {
		b.add(tableQueue, v.plan.Queue[i])
	}

	addRollouts(b, access.Rollouts(v.rollouts))

	shown := access.Clients(clients.Views)
	for i := range shown {
		b.add(tableClients, shown[i])
	}

	b.one(tableAudit, v.audit)

	return rows
}

// addImages puts the image references and the digest each points at.
func addImages(b *docBuilder, images *views.PublicImages) {
	for i := range images.Images {
		img := &images.Images[i]
		b.add(tableImages, wireImage{
			Key: string(img.Kind) + " " + img.Image, Digest: img.Digest, Revision: img.Revision,
			Source: img.Source, Commits: img.Commits,
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
	ids := func(list []*views.RolloutView) []string {
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

// hostDetail reads the details of one host; nil for a host that does not
// exist.
func (s *Handler) hostDetail(ctx context.Context, name string) (*views.HostDetail, error) {
	detail, err := s.views.HostDetail(ctx, name)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil //nolint:nilnil // no detail is the answer for an unknown host.
	case err != nil:
		return nil, fmt.Errorf("host %s: %w", name, err)
	}

	return detail, nil
}

// detailDoc builds the tables of one host's details; nil for nil. Its
// strings travel as they are: only the streams that asked for the host get
// it, so it cannot use their shared table.
func detailDoc(detail *views.HostDetail) (wireDoc, error) {
	if detail == nil {
		return nil, nil //nolint:nilnil // no document is the answer for an unknown host.
	}

	b := newDocBuilder(nil, "d.")
	b.one(tableDetail, wireDetail{
		HostSummary: detail.HostSummary, CertHash: detail.CertHash,
		FirstSeen: detail.FirstSeen, RemoteAddr: detail.RemoteAddr,
		Assigned: detail.Assigned, HasInventory: detail.Inventory != nil, Reduced: detail.Reduced,
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
