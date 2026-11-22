package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

// The overrides file is changed as lines so that every other byte stays as it is.

const (
	// The header is the file's first two lines; a line starting with the
	// prefix is replaced, so that its text may change.
	headerLine     = "# upgradoor overrides: uncomment to override, the agent applies changes within a minute."
	commandsLine   = `#   upgradoor list | status | override set/clear <container> | roll <container> | hold "why" | release`
	headerPrefix   = "# upgradoor overrides:"
	commandsPrefix = "#   upgradoor "

	keyOverrides = "overrides"

	// The default lines of an entry show its container's default; the
	// agent owns them.
	defaultImagePrefix   = "# default image:"
	defaultCommandPrefix = "# default command:"
	defaultArgPrefix     = "#   - "

	// oldBlockMarker starts the generated block of the old layout.
	oldBlockMarker = "# >>> upgradoor generated block"

	// maxHistory bounds the history comment lines kept per container.
	maxHistory = 5
)

// structureLines are the commented fields of an entry without an override.
var structureLines = []string{"# image:", "# command:", "# reason:", "# until:"}

// errOldFormat refuses a file of the old layout.
var errOldFormat = errors.New("the overrides file is in an old format (deployed:, hold:, flags: " +
	"or a generated block); delete it, the agent writes a new one")

// entryYAML is an override as the file writes it; the decoder refuses any
// other key.
type entryYAML struct {
	Image   string    `yaml:"image"`
	Command *[]string `yaml:"command"`
	// Flags is only read to refuse it.
	Flags  present    `yaml:"flags"`
	By     string     `yaml:"by"`
	Reason string     `yaml:"reason"`
	Until  *time.Time `yaml:"until"`
}

// present records that a key is there, whatever its value.
type present bool

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *present) UnmarshalYAML(*yaml.Node) error {
	*p = true

	return nil
}

// noOverrides is a file that is not there, or says nothing.
func noOverrides() plan.Overrides {
	return plan.Overrides{Known: true, Entries: map[string]plan.Entry{}}
}

// parse reads the content of an overrides file strictly: an unknown key is
// an error, not "no overrides", because a typo must not put every container
// back. Free text read by hand is cut to one line.
func parse(raw []byte) (plan.Overrides, error) {
	if bytes.Contains(raw, []byte(oldBlockMarker)) {
		return plan.Overrides{}, errOldFormat
	}

	var doc struct {
		Overrides map[string]*entryYAML `yaml:"overrides"`
		// Deployed and Hold are only read to refuse them.
		Deployed present `yaml:"deployed"`
		Hold     present `yaml:"hold"`
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	err := dec.Decode(&doc)

	switch {
	case errors.Is(err, io.EOF):
	case err != nil:
		return plan.Overrides{}, err
	default:
		// A second document would be ignored by the agent but not by
		// whoever reads the file.
		var extra yaml.Node
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return plan.Overrides{}, errors.New("the overrides file holds more than one YAML document")
		}
	}

	if doc.Deployed || doc.Hold {
		return plan.Overrides{}, errOldFormat
	}

	out := noOverrides()

	for name, y := range doc.Overrides {
		if y == nil {
			// "geth:" with nothing under it overrides nothing.
			continue
		}

		if y.Flags {
			return plan.Overrides{}, errOldFormat
		}

		e := plan.Entry{
			Image: strings.TrimSpace(y.Image), By: plan.Line(y.By), Reason: plan.Line(y.Reason), Until: y.Until,
		}
		if y.Command != nil {
			e.Command, e.HasCommand = append([]string{}, *y.Command...), true
		}

		if e.Empty() && e.By == "" && e.Reason == "" && e.Until == nil {
			continue
		}

		out.Entries[name] = e
	}

	return out, nil
}

// text is the content of an overrides file as lines.
type text struct{ lines []string }

// newText splits an overrides file into lines, reading CRLF as LF.
func newText(raw []byte) *text {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	t := &text{}

	if s != "" {
		t.lines = strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}

	return t
}

func (t *text) clone() *text { return &text{lines: slices.Clone(t.lines)} }

// splice replaces lines [start, end) by with.
func (t *text) splice(start, end int, with []string) {
	t.lines = slices.Concat(t.lines[:start], with, t.lines[end:])
}

func (t *text) bytes() []byte {
	if len(t.lines) == 0 {
		return nil
	}

	return []byte(strings.Join(t.lines, "\n") + "\n")
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }

func trimmed(line string) string { return strings.TrimLeft(line, " ") }

func isComment(line string) bool { return strings.HasPrefix(trimmed(line), "#") }

// blockEnd returns the index after the last line of the block that starts at
// line start and is indented deeper than indent: its lines, the comments
// among them, and blank lines followed by more of it.
func (t *text) blockEnd(start, indent int) int {
	end := start + 1

	for i := start + 1; i < len(t.lines); i++ {
		if isBlank(t.lines[i]) {
			continue
		}

		if indentOf(t.lines[i]) <= indent {
			break
		}

		end = i + 1
	}

	return end
}

// located is where the overrides mapping and one entry of it are.
type located struct {
	// key is the line of "overrides:"; -1 when there is none.
	key int
	// childIndent is the indentation of the entries' names.
	childIndent int
	// entry is the line of the entry's name; -1 when there is none.
	entry int
	// inner is the indentation of the entry's fields.
	inner int
}

// locate finds the overrides mapping and the entry of container. A flow-style
// mapping with content cannot be changed line by line and is refused; an
// empty one ("{}", "null") is rewritten as the key alone.
func (t *text) locate(container string) (located, error) {
	loc := located{key: -1, entry: -1, childIndent: 2}

	var doc yaml.Node

	if err := yaml.Unmarshal([]byte(strings.Join(t.lines, "\n")), &doc); err != nil {
		return loc, fmt.Errorf("overrides file: %w", err)
	}

	if len(doc.Content) == 0 {
		return loc, nil
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return loc, errors.New("overrides file: the content is not a mapping")
	}

	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != keyOverrides {
			continue
		}

		loc.key = k.Line - 1

		if !blockMapping(v) {
			return loc, fmt.Errorf("overrides file: write %q as an indented block, one entry per name", keyOverrides)
		}

		if len(v.Content) == 0 {
			t.keyAlone(k, v, keyOverrides)

			return loc, nil
		}

		loc.childIndent = v.Content[0].Column - 1

		for j := 0; j+1 < len(v.Content); j += 2 {
			ck, cv := v.Content[j], v.Content[j+1]
			if ck.Value != container {
				continue
			}

			if !blockMapping(cv) {
				return loc, fmt.Errorf("overrides file: write %q as an indented block, one field per line", container)
			}

			loc.entry, loc.inner = ck.Line-1, loc.childIndent+2

			if len(cv.Content) > 0 {
				loc.inner = cv.Content[0].Column - 1
			} else {
				t.keyAlone(ck, cv, container)
			}
		}
	}

	return loc, nil
}

// blockMapping reports whether a node can be changed line by line: a block
// mapping, an empty flow mapping or null.
func blockMapping(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.MappingNode:
		return n.Style&yaml.FlowStyle == 0 || len(n.Content) == 0
	case yaml.ScalarNode:
		return n.Tag == "!!null"
	default:
		return false
	}
}

// keyAlone rewrites "name: {}" or "name: null" as "name:".
func (t *text) keyAlone(k, v *yaml.Node, name string) {
	if v.Kind == yaml.MappingNode || v.Value != "" {
		t.lines[k.Line-1] = strings.Repeat(" ", k.Column-1) + scalar(name) + ":"
	}
}

// historyPattern matches the history lines of a container.
func historyPattern(tag string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*# \[` + regexp.QuoteMeta(tag) + `\] `)
}

// span is the lines [start, end).
type span struct{ start, end int }

// insertApart inserts line at index at with a blank line between it and any
// line next to it; it returns the line's index.
func (t *text) insertApart(at int, line string) int {
	add := []string{line}
	pos := at

	if at > 0 && !isBlank(t.lines[at-1]) {
		add, pos = slices.Insert(add, 0, ""), at+1
	}

	if at < len(t.lines) && !isBlank(t.lines[at]) {
		add = append(add, "")
	}

	t.splice(at, at, add)

	return pos
}

// parts are the lines of one entry by kind.
type parts struct {
	// defaults are the default lines.
	defaults []span
	// structure are the commented fields of an entry without an override.
	structure []span
	// fields are the active fields, each with its nested lines.
	fields []span
}

// isDefault reports whether a line starts a default line group.
func isDefault(line string) bool {
	s := trimmed(line)

	return strings.HasPrefix(s, defaultImagePrefix) || strings.HasPrefix(s, defaultCommandPrefix)
}

func isStructure(line string) bool { return slices.Contains(structureLines, strings.TrimSpace(line)) }

// partsOf sorts the lines [start, end) of an entry whose fields are indented
// by inner. Every other line is the user's and stays.
func (t *text) partsOf(start, end, inner int) parts {
	var p parts

	for i := start; i < end; i++ {
		line := t.lines[i]

		switch {
		case isBlank(line):
		case strings.HasPrefix(trimmed(line), defaultCommandPrefix):
			j := i + 1
			for j < end && strings.HasPrefix(trimmed(t.lines[j]), defaultArgPrefix) {
				j++
			}

			p.defaults = append(p.defaults, span{i, j})
			i = j - 1
		case isDefault(line):
			p.defaults = append(p.defaults, span{i, i + 1})
		case isStructure(line):
			p.structure = append(p.structure, span{i, i + 1})
		case isComment(line) || indentOf(line) != inner:
		default:
			j := t.fieldEnd(i, end, inner)
			p.fields = append(p.fields, span{i, j})
			i = j - 1
		}
	}

	return p
}

// fieldEnd is the line after the field that starts at line i: its deeper
// lines and the items of an indentless sequence.
func (t *text) fieldEnd(i, end, inner int) int {
	j := i + 1

	for k := i + 1; k < end; k++ {
		line := t.lines[k]

		switch {
		case isBlank(line):
			continue
		case isDefault(line) || isStructure(line):
		case indentOf(line) > inner,
			indentOf(line) == inner && strings.HasPrefix(trimmed(line), "- "):
			j = k + 1

			continue
		}

		break
	}

	return j
}

// remove takes out the spans, which do not overlap.
func (t *text) remove(spans []span) {
	sorted := slices.SortedFunc(slices.Values(spans), func(a, b span) int { return b.start - a.start })
	for _, s := range sorted {
		t.splice(s.start, s.end, nil)
	}
}

// addEntry adds a container's name at the end of the overrides mapping,
// adding the mapping when there is none, and returns where it is.
func (t *text) addEntry(container string) (located, error) {
	loc, err := t.locate(container)
	if err != nil || loc.entry >= 0 {
		return loc, err
	}

	if loc.key < 0 {
		t.insertApart(len(t.lines), keyOverrides+":")

		if loc, err = t.locate(container); err != nil {
			return loc, err
		}
	}

	end := t.blockEnd(loc.key, indentOf(t.lines[loc.key]))
	t.splice(end, end, []string{strings.Repeat(" ", loc.childIndent) + scalar(container) + ":"})

	return t.locate(container)
}

// containerName is a valid container name; nothing else is the name of an
// entry the agent writes.
var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// setOverride writes the active fields of a container's entry (e nil takes
// them out and puts the commented structure back), adds a history line and
// reads the result back: it must hold the entry as written and everything
// else as it was, or an error is returned. Default lines and other comments
// stay.
func (t *text) setOverride(container string, e *plan.Entry, at time.Time, who, note string) error {
	if !containerName.MatchString(container) {
		return plan.Refusal("%q is not a valid container name", container)
	}

	before, err := parse(t.bytes())
	if err != nil {
		return err
	}

	loc, err := t.addEntry(container)
	if err != nil {
		return err
	}

	end := t.blockEnd(loc.entry, loc.childIndent)
	p := t.partsOf(loc.entry+1, end, loc.inner)
	in := strings.Repeat(" ", loc.inner)

	// The new lines go where the first old one was, else below the defaults.
	removed := slices.Concat(p.fields, p.structure)
	pos := loc.entry + 1

	if len(removed) > 0 {
		pos = slices.MinFunc(removed, func(a, b span) int { return a.start - b.start }).start
	} else if len(p.defaults) > 0 {
		pos = p.defaults[len(p.defaults)-1].end
	}

	t.remove(removed)

	if e != nil {
		t.splice(pos, pos, renderEntry(e, in))
	} else {
		t.splice(pos, pos, renderStructure(in))
	}

	t.addHistory(container, loc.entry, strings.Repeat(" ", loc.childIndent), at, who, note)

	set, err := parse(t.bytes())
	if err != nil {
		return fmt.Errorf("the change would break the overrides file: %w", err)
	}

	if err := unchanged(&before, &set, container); err != nil {
		return err
	}

	got, ok := set.Entries[container]

	switch {
	case e == nil && !ok:
		return nil
	case (e == nil) != !ok:
		return errors.New("the change did not take in the overrides file")
	}

	same := got.Image == e.Image && sameCommand(&got, e) && got.By == plan.Line(e.By) &&
		got.Reason == plan.Line(e.Reason) && sameSecond(got.Until, e.Until)
	if !same {
		return errors.New("the entry reads back differently from what was written")
	}

	return nil
}

// addHistory adds a history line after the container's last one, or right
// above its name at line name; the oldest beyond maxHistory go.
func (t *text) addHistory(container string, name int, pad string, at time.Time, who, note string) {
	history := historyPattern(container)
	insert := name

	for i := range t.lines {
		if history.MatchString(t.lines[i]) {
			insert = i + 1
		}
	}

	t.splice(insert, insert, []string{fmt.Sprintf("%s# [%s] %s %s: %s",
		pad, container, at.UTC().Format(time.RFC3339), plan.Line(who), plan.Line(note))})

	kept := 0

	for i := len(t.lines) - 1; i >= 0; i-- {
		if !history.MatchString(t.lines[i]) {
			continue
		}

		kept++
		if kept > maxHistory {
			t.splice(i, i+1, nil)
		}
	}
}

// setDefaults replaces the default lines of a container's entry by those of
// d, or (d nil) takes them out. A container without an entry gets one with
// its default lines and the commented structure; nil adds nothing.
func (t *text) setDefaults(container string, d *plan.Deployed) error {
	loc, err := t.locate(container)
	if err != nil {
		return err
	}

	if loc.entry < 0 {
		if d == nil {
			return nil
		}

		if loc, err = t.addEntry(container); err != nil {
			return err
		}

		in := strings.Repeat(" ", loc.inner)
		t.splice(loc.entry+1, loc.entry+1, slices.Concat(renderDefaults(d, in), renderStructure(in)))

		return nil
	}

	p := t.partsOf(loc.entry+1, t.blockEnd(loc.entry, loc.childIndent), loc.inner)

	pos := loc.entry + 1
	if len(p.defaults) > 0 {
		pos = p.defaults[0].start
	}

	t.remove(p.defaults)

	if d != nil {
		t.splice(pos, pos, renderDefaults(d, strings.Repeat(" ", loc.inner)))
	}

	return nil
}

// layout puts the file in its form: the header on top, the overrides
// mapping, an entry with default lines for each container of keep, and no
// default lines for those of drop. Every other line stays as it is.
func (t *text) layout(keep map[string]plan.Deployed, drop []string) error {
	t.header()

	loc, err := t.locate("")
	if err != nil {
		return err
	}

	if loc.key < 0 {
		t.insertApart(min(2, len(t.lines)), keyOverrides+":")
	}

	for _, name := range slices.Sorted(maps.Keys(keep)) {
		d := keep[name]
		if err := t.setDefaults(name, &d); err != nil {
			return err
		}
	}

	for _, name := range drop {
		if _, ok := keep[name]; ok {
			continue
		}

		if err := t.setDefaults(name, nil); err != nil {
			return err
		}
	}

	return nil
}

// header writes the two header lines, replacing those that are there.
func (t *text) header() {
	if len(t.lines) == 0 || !strings.HasPrefix(t.lines[0], headerPrefix) {
		add := []string{headerLine, commandsLine}
		if len(t.lines) > 0 && !isBlank(t.lines[0]) {
			add = append(add, "")
		}

		t.splice(0, 0, add)

		return
	}

	t.lines[0] = headerLine

	if len(t.lines) > 1 && strings.HasPrefix(t.lines[1], commandsPrefix) {
		t.lines[1] = commandsLine
	} else {
		t.splice(1, 1, []string{commandsLine})
	}
}

// sameSecond compares an end read back with the one written; the file holds
// it to the second.
func sameSecond(got, want *time.Time) bool {
	return (got == nil) == (want == nil) && (got == nil || got.Equal(want.Truncate(time.Second)))
}

// unchanged reports an error when after differs from before in any entry but
// except: a change of one entry must not write any other.
func unchanged(before, after *plan.Overrides, except string) error {
	for name, a := range before.Entries {
		if name == except {
			continue
		}

		if b, ok := after.Entries[name]; !ok || !sameEntry(&a, &b) {
			return errors.New("the change would alter other parts of the overrides file")
		}
	}

	for name := range after.Entries {
		if _, ok := before.Entries[name]; !ok && name != except {
			return errors.New("the change would alter other parts of the overrides file")
		}
	}

	return nil
}

func sameEntry(a, b *plan.Entry) bool {
	return a.Image == b.Image && sameCommand(a, b) && a.By == b.By && a.Reason == b.Reason &&
		sameEnd(a.Until, b.Until)
}

func sameCommand(a, b *plan.Entry) bool {
	return a.HasCommand == b.HasCommand && slices.Equal(a.Command, b.Command)
}

func sameEnd(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }

// renderEntry writes the active fields of an override; free text cut to one
// line.
func renderEntry(e *plan.Entry, in string) []string {
	var out []string

	if e.Image != "" {
		out = append(out, in+"image: "+scalar(e.Image))
	}

	if e.HasCommand {
		out = append(out, commandLines(in, e.Command)...)
	}

	out = append(out, in+"by: "+scalar(plan.Line(e.By)), in+"reason: "+scalar(plan.Line(e.Reason)))

	if e.Until != nil {
		out = append(out, in+"until: "+e.Until.UTC().Format(time.RFC3339))
	}

	return out
}

// renderDefaults writes the default lines of d.
func renderDefaults(d *plan.Deployed, in string) []string {
	out := []string{in + defaultImagePrefix + " " + scalar(d.Image)}

	switch {
	case !d.HasCommand:
	case len(d.Command) == 0:
		out = append(out, in+defaultCommandPrefix+" []")
	default:
		out = append(out, in+defaultCommandPrefix)

		for _, arg := range d.Command {
			out = append(out, in+defaultArgPrefix+scalar(arg))
		}
	}

	return out
}

func renderStructure(in string) []string {
	out := make([]string, 0, len(structureLines))
	for _, line := range structureLines {
		out = append(out, in+line)
	}

	return out
}

// commandLines writes a command line as a YAML list, one argument per line.
func commandLines(pad string, command []string) []string {
	if len(command) == 0 {
		return []string{pad + "command: []"}
	}

	out := make([]string, 0, len(command)+1)
	out = append(out, pad+"command:")

	for _, arg := range command {
		out = append(out, pad+"  - "+scalar(arg))
	}

	return out
}

// scalar writes a string as a plain YAML scalar when it reads back as that
// string, else as a JSON string.
func scalar(s string) string {
	var doc yaml.Node

	if s != "" && !strings.ContainsAny(s, "\n\r\t") && yaml.Unmarshal([]byte(s), &doc) == nil &&
		len(doc.Content) == 1 {
		n := doc.Content[0]
		if n.Kind == yaml.ScalarNode && n.Style == 0 && n.Tag == "!!str" && n.Value == s {
			return s
		}
	}

	return jsonString(s)
}

// jsonString writes a string as a JSON string, which is a YAML string too:
// nothing in it can be read as YAML.
func jsonString(s string) string {
	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding a string cannot fail.
	_ = enc.Encode(s)

	return strings.TrimSuffix(buf.String(), "\n")
}
