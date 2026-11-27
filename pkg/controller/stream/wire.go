package stream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/views"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The wire encoding of the event streams. The REST routes answer readable
// JSON; a stream carries the same views compactly and, after its snapshot,
// only what changed.
//
//   - A view is a set of tables. A table has rows; a row is an array of
//     cells, one per field of the row's schema, in the schema's order.
//     Trailing cells that hold the default are left out. There are no keys
//     on the wire: the schema names the cells.
//   - A cell that holds a field's default (false, 0, "", the zero time, no
//     value) is the number 0.
//   - A string that tends to repeat (an image, a digest, a state, a reason,
//     a team) travels once: the cell holds its index in the stream's string
//     table.
//   - A time is a number of seconds since the Unix epoch.
//   - After the snapshot, a table is patched: new rows whole, the changed
//     cells of changed rows, the keys of removed rows.
//
// The schemas are not written down twice. They are read off the Go types of
// the views (wireTables), written to web/src/api/wire.schema.json by the
// test that keeps that file current, and interpreted from there by the page.

// Wire field types. Every one is a single character in the schema.
const (
	wireBool   = "b" // 1 or 0
	wireNumber = "n"
	wireString = "s" // a string that does not repeat, as it is
	wireRef    = "r" // a string that repeats: its index in the string table
	wireTime   = "t" // seconds since the Unix epoch
	wireJSON   = "j" // any other value, as [value]
	wireList   = "l" // a list of strings, each as wireRef
	wireObject = "o" // a row of the schema Of
	wireArray  = "a" // rows of the schema Of
)

// wireField is one cell of a row.
type wireField struct {
	// Name is the field's JSON name in the REST view.
	Name string `json:"n"`
	Type string `json:"t"`
	// Of names the schema of an object or of the rows of an array.
	Of string `json:"of,omitempty"`
	// Omit: the REST view leaves the field out when it holds its default.
	Omit bool `json:"omit,omitempty"`

	index []int
}

// wireRefFields are the JSON names of the string fields whose values repeat
// across rows. Strings of a named type (states, kinds) and lists of strings
// always count as repeating.
var wireRefFields = []string{
	"image", "configuredImage", "imageId", "digest", "revision", "version", "reason",
	"state", "holdReason", "pauseReason", "readiness", "error", "agentVersion", "incompatible", "runningKernel",
	"newestKernel", "prettyName", "versionId", "arch", "grantedBy", "createdBy", "actor",
	"action", "origin", "layer", "by", "after", "rollout", "target", "localReason",
	"timingError", "health", "from", "to", "candidate", "current", "host", "description", "feature",
}

var (
	timeType      = reflect.TypeFor[time.Time]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
)

// wireSchemaSet holds the schemas of every row type, by name.
type wireSchemaSet struct {
	byName map[string][]wireField
	byType map[reflect.Type]string
}

// schemaOf returns the name of the schema of a struct type, adding it (and
// the schemas it refers to) when it is new.
func (ws *wireSchemaSet) schemaOf(t reflect.Type) string {
	if name, ok := ws.byType[t]; ok {
		return name
	}

	name := t.Name()
	if _, taken := ws.byName[name]; taken || name == "" {
		panic(fmt.Sprintf("api: wire schema name %q of %s is not unique", name, t))
	}

	ws.byType[t] = name
	ws.byName[name] = nil
	ws.byName[name] = ws.fields(t, nil)

	return name
}

// fields lists the cells of a struct type; an embedded struct adds its own.
func (ws *wireSchemaSet) fields(t reflect.Type, index []int) []wireField {
	out := make([]wireField, 0, t.NumField())

	for i := range t.NumField() {
		sf := t.Field(i)
		at := append(slices.Clone(index), i)

		if sf.Anonymous && sf.Type.Kind() == reflect.Struct {
			out = append(out, ws.fields(sf.Type, at)...)

			continue
		}

		name, opts, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "-" || !sf.IsExported() {
			continue
		}

		if name == "" {
			name = sf.Name
		}

		f := wireField{Name: name, Omit: strings.Contains(opts, "omitempty"), index: at}
		f.Type, f.Of = ws.typeOf(sf.Type, name)

		// encoding/json never leaves a struct out.
		if sf.Type.Kind() == reflect.Struct && f.Type == wireObject {
			f.Omit = false
		}

		out = append(out, f)
	}

	return out
}

func (ws *wireSchemaSet) typeOf(t reflect.Type, name string) (kind, of string) {
	if t == timeType || t == reflect.PointerTo(timeType) {
		return wireTime, ""
	}

	if t.Implements(marshalerType) {
		return wireJSON, ""
	}

	switch t.Kind() {
	case reflect.Bool:
		return wireBool, ""
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return wireNumber, ""
	case reflect.String:
		if t.Name() != "string" || slices.Contains(wireRefFields, name) {
			return wireRef, ""
		}

		return wireString, ""
	case reflect.Struct:
		return wireObject, ws.schemaOf(t)
	case reflect.Pointer:
		if t.Elem().Kind() == reflect.Struct && !t.Elem().Implements(marshalerType) {
			return wireObject, ws.schemaOf(t.Elem())
		}
	case reflect.Slice:
		elem := t.Elem()
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}

		switch {
		case elem.Kind() == reflect.String:
			return wireList, ""
		case elem.Kind() == reflect.Struct && elem != timeType && !elem.Implements(marshalerType):
			return wireArray, ws.schemaOf(elem)
		}
	default:
	}

	return wireJSON, ""
}

// Tables of a stream. A table whose rows have no key holds one row.
const (
	tableNetwork  = "n"  // one wireGate
	tableHosts    = "h"  // views.PublicHost on the public stream, wireHost otherwise
	tableImages   = "im" // wireImage
	tableSlots    = "sl" // fleet.SlotParticipation of the newest slots, by slot
	tablePlan     = "pl" // one wirePlan
	tableQueue    = "q"  // scheduler.QueueEntry, by host
	tableRollouts = "ro" // views.RolloutView without its hosts, by ID
	tableMembers  = "m"  // wireMember, by "<rollout>/<host>"
	tableLeases   = "le" // fleet.Lease, by ID
	tableAudit    = "a"  // one views.AuditHead
	tableClients  = "c"  // views.ClientView, by name

	// Tables of one host's details.
	tableDetail    = "x" // one wireDetail
	tableHostLease = "l" // fleet.Lease, by ID
	tableHostAudit = "a" // fleet.AuditEntry, by ID
	tableInventory = "v" // one protocol.Inventory
)

// wireGate is the network gate on a stream. What follows from the clock is
// not in it: see wireNetwork.
type wireGate struct {
	OK      bool        `json:"ok"`
	Reasons []string    `json:"reasons"`
	Network wireNetwork `json:"network"`
}

// wireNetwork is fleet.NetworkHealth without what a clock that knows the
// chain's timing says anyway. With Clocked set, UpdatedAt (unless the beacon
// node stopped answering), HeadSlot, CurrentEpoch and FinalizedEpoch are zero and the page
// computes them: the current epoch from the phases, the head as the clock's
// slot less HeadBehind, the finalized epoch from the lag. So a healthy chain
// changes nothing here from one slot to the next. What is left changes when
// the chain says something new: the participation of another epoch, a head
// that falls behind, a finality lag that grows.
type wireNetwork struct {
	fleet.NetworkHealth

	Clocked bool `json:"clocked,omitempty"`
	// HeadBehind is how many slots the head is behind the clock, when that
	// is more than the one slot a poll can be early by.
	HeadBehind uint64 `json:"headBehind,omitempty"`
}

// wireHost is a row of the host table: a views.HostListing without what moves with
// every sync. LastSeen is zero unless the host is stale (it then stands
// still); UptimeSeconds is zero and BootTime says the same. Unplanned says
// that the plan has no entry for the host, so the plan's host map can be
// rebuilt from the rows.
type wireHost struct {
	views.HostListing

	BootTime  time.Time `json:"bootTime"`
	Unplanned bool      `json:"unplanned,omitempty"`
}

// wireImage is a views.PublicImage without the hosts on it: the page sorts the
// hosts of the host table by their digest. Key is "<kind> <image>".
type wireImage struct {
	Key      string `json:"key"`
	Digest   string `json:"digest,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// wirePlan is a scheduler.Plan without the gate (tableNetwork), the queue
// (tableQueue), the hosts (in the host rows) and the time it was computed at.
type wirePlan struct {
	Paused       bool                   `json:"paused"`
	Budget       scheduler.Budget       `json:"budget"`
	DeployWindow scheduler.DeployWindow `json:"deployWindow"`
}

// wireMember is one host of a rollout.
type wireMember struct {
	Key     string `json:"key"`
	Rollout string `json:"rolloutId"`
	// Index is the member's place in the rollout's host list.
	Index int `json:"index"`

	fleet.RolloutHost
}

// wireDetail is what a views.HostDetail holds besides its tables: the summary in
// full, clock-driven fields included, and the identity.
type wireDetail struct {
	views.HostSummary

	CertHash     string    `json:"certHash"`
	FirstSeen    time.Time `json:"firstSeen"`
	RemoteAddr   string    `json:"remoteAddr"`
	Assigned     uint64    `json:"assignedValidators"`
	HasInventory bool      `json:"hasInventory,omitempty"`
	Reduced      bool      `json:"reduced,omitempty"`
}

// wireTables names the row schema of every table. Tables of a host's details
// are listed with the prefix "d.".
var wireTables = map[string]reflect.Type{
	"public." + tableHosts: reflect.TypeFor[views.PublicHost](),
	tableNetwork:           reflect.TypeFor[wireGate](),
	tableHosts:             reflect.TypeFor[wireHost](),
	tableImages:            reflect.TypeFor[wireImage](),
	tableSlots:             reflect.TypeFor[fleet.SlotParticipation](),
	tablePlan:              reflect.TypeFor[wirePlan](),
	tableQueue:             reflect.TypeFor[scheduler.QueueEntry](),
	tableRollouts:          reflect.TypeFor[views.RolloutView](),
	tableMembers:           reflect.TypeFor[wireMember](),
	tableLeases:            reflect.TypeFor[fleet.Lease](),
	tableAudit:             reflect.TypeFor[views.AuditHead](),
	tableClients:           reflect.TypeFor[views.ClientView](),
	"d." + tableDetail:     reflect.TypeFor[wireDetail](),
	"d." + tableHostLease:  reflect.TypeFor[fleet.Lease](),
	"d." + tableHostAudit:  reflect.TypeFor[fleet.AuditEntry](),
	"d." + tableInventory:  reflect.TypeFor[protocol.Inventory](),
}

// wireSingle lists the tables that hold one row, which has no key.
var wireSingle = []string{
	tableNetwork, tablePlan, tableAudit,
	"d." + tableDetail, "d." + tableInventory,
}

// WireSchema is the description of the wire encoding that the page
// interprets: the row schema of every table, the tables of one row, the
// schemas by name, and a version that changes with any of it.
type WireSchema struct {
	Version string                 `json:"version"`
	Tables  map[string]string      `json:"tables"`
	Single  []string               `json:"single"`
	Schemas map[string][]wireField `json:"schemas"`
}

var wire = newWireSchema()

type wireCodec struct {
	set    *wireSchemaSet
	schema WireSchema
	// json is the schema as GET /api/v1/events/schema serves it.
	json []byte
}

func newWireSchema() *wireCodec {
	set := &wireSchemaSet{
		byName: make(map[string][]wireField, 48),
		byType: make(map[reflect.Type]string, 48),
	}

	tables := make(map[string]string, len(wireTables))
	for table, t := range wireTables {
		tables[table] = set.schemaOf(t)
	}

	c := &wireCodec{set: set, schema: WireSchema{Tables: tables, Single: wireSingle, Schemas: set.byName}}

	// The version is a digest of the description: a page built against
	// another one cannot read the stream and says so.
	body, err := json.Marshal(c.schema)
	if err != nil {
		panic(err)
	}

	sum := sha256.Sum256(body)
	c.schema.Version = hex.EncodeToString(sum[:6])

	if c.json, err = json.MarshalIndent(c.schema, "", " "); err != nil {
		panic(err)
	}

	c.json = append(c.json, '\n')

	return c
}

// stringTable is the string table of one stream audience. It only grows:
// an index, once sent, keeps its meaning.
type stringTable struct {
	index map[string]int
	list  []string
}

func newStringTable() *stringTable {
	return &stringTable{index: map[string]int{"": 0}, list: []string{""}}
}

func (st *stringTable) ref(s string) any {
	if st == nil {
		// A document without a table carries its strings as they are.
		return s
	}

	i, ok := st.index[s]
	if !ok {
		i = len(st.list)
		st.index[s] = i
		st.list = append(st.list, s)
	}

	return i
}

// wireRow is an encoded row: the JSON of each cell.
type wireRow [][]byte

var wireDefault = []byte("0")

// wireTable is one table of a document.
type wireTable struct {
	keys []string
	rows map[string]wireRow
	// order is extra JSON that says in which order the rows are listed,
	// for a table whose order does not follow from its rows.
	order []byte
}

// wireDoc is a set of tables: what one audience sees, or one host's details.
type wireDoc map[string]*wireTable

// docBuilder fills a document.
type docBuilder struct {
	doc     wireDoc
	strings *stringTable
	prefix  string
	err     error
}

func newDocBuilder(table *stringTable, prefix string) *docBuilder {
	return &docBuilder{doc: make(wireDoc, 12), strings: table, prefix: prefix}
}

func (b *docBuilder) table(name string) *wireTable {
	t, ok := b.doc[name]
	if !ok {
		t = &wireTable{rows: make(map[string]wireRow, 8)}
		b.doc[name] = t
	}

	return t
}

// one puts the single row of a table.
func (b *docBuilder) one(table string, v any) {
	_ = b.put(table, v, false)
}

// add puts a row of a table; the row's first cell is its key.
func (b *docBuilder) add(table string, v any) {
	_ = b.put(table, v, true)
}

// order sets the listing order of a table.
func (b *docBuilder) order(table string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		b.err = err

		return
	}

	b.table(table).order = data
}

// addRow puts an encoded row of a keyed table.
func (b *docBuilder) addRow(table string, row wireRow) {
	t := b.table(table)
	key := string(row[0])

	if _, dup := t.rows[key]; !dup {
		t.keys = append(t.keys, key)
	}

	t.rows[key] = row
}

// put encodes a row into a table and returns it; nil on an error, which it
// keeps in b.err.
func (b *docBuilder) put(table string, v any, keyed bool) wireRow {
	t := b.table(table)
	rv := reflect.ValueOf(v)

	// A table of the document's own kind ("public.h", "d.a") goes first.
	name := b.prefix + table
	if _, own := wireTables[name]; !own {
		name = table
	}

	if want := wireTables[name]; rv.Type() != want {
		b.err = fmt.Errorf("table %s holds %s, not %s", name, want, rv.Type())

		return nil
	}

	if slices.Contains(wireSingle, name) == keyed {
		b.err = fmt.Errorf("table %s: a row with a key is %v", name, keyed)

		return nil
	}

	cells := b.row(rv, wire.set.byType[rv.Type()])
	row := make(wireRow, len(cells))

	for i, cell := range cells {
		data, err := json.Marshal(cell)
		if err != nil {
			b.err = fmt.Errorf("table %s: %w", table, err)

			return nil
		}

		row[i] = data
	}

	key := ""

	if keyed {
		if len(row) == 0 {
			b.err = fmt.Errorf("table %s: a row without a key", table)

			return nil
		}

		key = string(row[0])
	}

	if _, dup := t.rows[key]; !dup {
		t.keys = append(t.keys, key)
	}

	t.rows[key] = row

	return row
}

// row encodes a struct as the cells of its schema, without trailing
// defaults.
func (b *docBuilder) row(v reflect.Value, schema string) []any {
	fields := wire.set.byName[schema]
	cells := make([]any, 0, len(fields))
	last := 0

	for i := range fields {
		cell := b.cell(v.FieldByIndex(fields[i].index), &fields[i])
		cells = append(cells, cell)

		if cell != 0 {
			last = i + 1
		}
	}

	return cells[:last]
}

func (b *docBuilder) cell(v reflect.Value, f *wireField) any {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0
		}

		if f.Type == wireJSON {
			break
		}

		v = v.Elem()
	}

	switch f.Type {
	case wireBool:
		if v.Bool() {
			return 1
		}
	case wireNumber:
		if !v.IsZero() {
			return v.Interface()
		}
	case wireString:
		if s := v.String(); s != "" {
			return s
		}
	case wireRef:
		if s := v.String(); s != "" {
			return b.strings.ref(s)
		}
	case wireTime:
		if t, ok := v.Interface().(time.Time); ok && !t.IsZero() {
			return t.Unix()
		}
	case wireObject:
		return b.row(v, f.Of)
	case wireList:
		return b.list(v, f)
	case wireArray:
		return b.array(v, f)
	default:
		// A map that writes its own JSON is never null there.
		ownJSON := v.Kind() == reflect.Map && v.Type().Implements(marshalerType)

		if (v.Kind() == reflect.Map || v.Kind() == reflect.Slice || v.Kind() == reflect.Interface) &&
			v.IsNil() && !ownJSON {
			return 0
		}

		if f.Omit && v.IsZero() {
			return 0
		}

		return []any{v.Interface()}
	}

	return 0
}

func (b *docBuilder) list(v reflect.Value, f *wireField) any {
	if v.IsNil() || (f.Omit && v.Len() == 0) {
		return 0
	}

	out := make([]any, 0, v.Len())
	for i := range v.Len() {
		out = append(out, b.strings.ref(v.Index(i).String()))
	}

	return out
}

func (b *docBuilder) array(v reflect.Value, f *wireField) any {
	if v.IsNil() || (f.Omit && v.Len() == 0) {
		return 0
	}

	out := make([]any, 0, v.Len())

	for i := range v.Len() {
		item := v.Index(i)
		if item.Kind() == reflect.Pointer {
			item = item.Elem()
		}

		out = append(out, b.row(item, f.Of))
	}

	return out
}

// wireTableJSON is a table in a snapshot or in a patch.
type wireTableJSON struct {
	// Rows are whole rows: all of them in a snapshot, the new ones in a patch.
	Rows []wireRow `json:"r,omitempty"`
	// Updates are the changed cells of rows that exist: the row's key (0 in
	// a table of one row), then pairs of cell index and value.
	Updates [][]json.RawMessage `json:"u,omitempty"`
	// Removed are the keys of the rows that are gone.
	Removed []json.RawMessage `json:"x,omitempty"`
	Order   json.RawMessage   `json:"o,omitempty"`
}

// MarshalJSON writes a row as an array of its cells.
func (r wireRow) MarshalJSON() ([]byte, error) {
	return append(append([]byte{'['}, bytes.Join(r, []byte{','})...), ']'), nil
}

func keyCell(key string) json.RawMessage {
	if key == "" {
		return wireDefault
	}

	return json.RawMessage(key)
}

// full is the table as a snapshot carries it.
func (t *wireTable) full() wireTableJSON {
	out := wireTableJSON{Rows: make([]wireRow, 0, len(t.keys)), Order: t.order}
	for _, key := range t.keys {
		out.Rows = append(out.Rows, t.rows[key])
	}

	return out
}

// diff is what turns the table old into t, and whether there is anything.
func (t *wireTable) diff(old *wireTable) (wireTableJSON, bool) {
	var out wireTableJSON

	if old == nil {
		old = &wireTable{}
	}

	for _, key := range t.keys {
		row := t.rows[key]

		before, ok := old.rows[key]
		if !ok {
			out.Rows = append(out.Rows, row)

			continue
		}

		update := []json.RawMessage{keyCell(key)}

		for i := range max(len(row), len(before)) {
			now, was := wireDefault, wireDefault

			if i < len(row) {
				now = row[i]
			}

			if i < len(before) {
				was = before[i]
			}

			if !bytes.Equal(now, was) {
				update = append(update, json.RawMessage(fmt.Sprint(i)), now)
			}
		}

		if len(update) > 1 {
			out.Updates = append(out.Updates, update)
		}
	}

	for _, key := range old.keys {
		if _, ok := t.rows[key]; !ok {
			out.Removed = append(out.Removed, keyCell(key))
		}
	}

	if !bytes.Equal(t.order, old.order) {
		out.Order = t.order
		if out.Order == nil {
			out.Order = wireDefault
		}
	}

	return out, len(out.Rows)+len(out.Updates)+len(out.Removed) > 0 || out.Order != nil
}

// full is the document as a snapshot carries it: every table that has rows.
func (d wireDoc) full() map[string]any {
	out := make(map[string]any, len(d)+4)
	for name, t := range d {
		out[name] = t.full()
	}

	return out
}

// diff is what turns the document old into d; empty when nothing changed.
func (d wireDoc) diff(old wireDoc) map[string]any {
	out := make(map[string]any, 4)

	for name, t := range d {
		if patch, changed := t.diff(old[name]); changed {
			out[name] = patch
		}
	}

	for name, t := range old {
		if _, ok := d[name]; !ok {
			patch, _ := (&wireTable{}).diff(t)
			out[name] = patch
		}
	}

	return out
}

// headTolerance is how many slots the head may be behind the clock without
// that being said: a poll early in a slot sees the slot before it.
const headTolerance = 1

// networkQuietSlots is how many slots the beacon node may have been silent
// for before the time of its last answer is said: until then that time is
// "now" and moves with every poll.
const networkQuietSlots = 4

// newWireNetwork takes out of a NetworkHealth what the clock says at now.
func newWireNetwork(n fleet.NetworkHealth, now time.Time) wireNetwork { //nolint:gocritic // hugeParam: the copy is the point, it is changed.
	out := wireNetwork{NetworkHealth: n}

	slot, ok := n.SlotAt(n.UpdatedAt)
	if !ok || slot/n.SlotsPerEpoch != n.CurrentEpoch || n.HeadSlot > slot ||
		n.FinalizedEpoch+n.FinalityLagEpochs != n.CurrentEpoch {
		// The clock does not explain the numbers: they travel as they are.
		return out
	}

	out.Clocked = true

	if behind := slot - n.HeadSlot; behind > headTolerance {
		out.HeadBehind = behind
	}

	out.HeadSlot, out.CurrentEpoch, out.FinalizedEpoch = 0, 0, 0

	// While the beacon node answers, when it last did is "now". Once it has
	// been silent for a while that time stands still, and is said.
	if latest, known := n.SlotAt(now); !known || latest <= slot+networkQuietSlots {
		out.UpdatedAt = time.Time{}
	}

	return out
}

// bootTolerance is how far a host's computed boot time may move from one
// sync to the next without being a reboot: uptime is whole seconds.
const bootTolerance = 5 * time.Second

// newWireHost is the row of a host on the stream. boots remembers the boot
// time sent for each host, so it does not jitter.
func newWireHost(sum *views.HostSummary, planned bool, boots map[string]time.Time) wireHost {
	out := wireHost{HostListing: views.Listing(sum), Unplanned: !planned, BootTime: wireBoot(sum, boots)}
	out.UptimeSeconds = 0

	if !sum.Stale {
		out.LastSeen = time.Time{}
	}

	return out
}

// wireBoot is the host's boot time as the stream says it: the one sent
// before while the computed one stays within bootTolerance of it.
func wireBoot(sum *views.HostSummary, boots map[string]time.Time) time.Time {
	if sum.UptimeSeconds <= 0 {
		return time.Time{}
	}

	boot := sum.LastSeen.Add(-time.Duration(sum.UptimeSeconds) * time.Second).Truncate(time.Second)
	if prev, ok := boots[sum.Name]; ok && boot.Sub(prev).Abs() <= bootTolerance {
		boot = prev
	}

	boots[sum.Name] = boot

	return boot
}

// hostRowKey is everything a host's row on the stream is made of, with the
// host's status as its hash (fleet.Host.StatusHash): a row whose key did not
// change is not encoded again.
type hostRowKey struct {
	status     string
	roles      string
	validators uint64
	stale      bool
	lastSeen   time.Time
	boot       time.Time
	offline    fleet.OfflineMark
	// plan is the host's plan but its HoldRequests, which holdRequests has.
	plan         planKey
	holdRequests string
	unplanned    bool
}

// planKey is a scheduler.HostPlan but its HoldRequests.
type planKey struct {
	slot         scheduler.SlotState
	weight, live uint64
	liveKnown    bool
	leaseID      string
	reason       string
	notReady     string
}

// holdRequestsKey is the hold requests of a host's plan in a row key.
func holdRequestsKey(reqs []scheduler.PendingHold) string {
	var b strings.Builder

	for i := range reqs {
		fmt.Fprintf(&b, "%s\x00%t\x00%s\x00%s\x00%d\x00", reqs[i].Holder, reqs[i].Release, reqs[i].By,
			reqs[i].Reason, reqs[i].At.UnixNano())
	}

	return b.String()
}

// cachedHostRow is the encoded row of a host and what it was made of.
type cachedHostRow struct {
	key hostRowKey
	row wireRow
}

// addHosts puts the rows of the host table, reusing the encoded row of every
// host whose key did not change since the last build. statusHashes holds
// fleet.Host.StatusHash for each summary; a host without one (no status, or
// a store that keeps none) is always encoded.
func addHosts(
	b *docBuilder, sums []views.HostSummary, statusHashes []string, planned func(string) bool,
	boots map[string]time.Time, cache map[string]cachedHostRow,
) map[string]cachedHostRow {
	next := make(map[string]cachedHostRow, len(sums))

	for i := range sums {
		sum := &sums[i]
		key := hostRowKey{
			status: statusHashes[i], roles: strings.Join(sum.Roles, "\x00"), validators: sum.Validators,
			stale: sum.Stale, boot: wireBoot(sum, boots),
			plan: planKey{
				slot: sum.Plan.Slot, weight: sum.Plan.Weight, live: sum.Plan.Live, liveKnown: sum.Plan.LiveKnown,
				leaseID: sum.Plan.LeaseID, reason: sum.Plan.Reason, notReady: sum.Plan.NotReady,
			},
			holdRequests: holdRequestsKey(sum.Plan.HoldRequests), unplanned: !planned(sum.Name),
		}

		if sum.Stale {
			key.lastSeen = sum.LastSeen
		}

		if sum.Offline != nil {
			key.offline = *sum.Offline
		}

		if c, ok := cache[sum.Name]; ok && key.status != "" && c.key == key {
			b.addRow(tableHosts, c.row)
			next[sum.Name] = c

			continue
		}

		row := newWireHost(sum, !key.unplanned, boots)
		if encoded := b.put(tableHosts, row, true); encoded != nil && key.status != "" {
			next[sum.Name] = cachedHostRow{key: key, row: encoded}
		}
	}

	return next
}
