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
	// blockBegin and blockEnd start the first and the last line of the
	// generated block; only this prefix is compared, so that Ansible's text
	// after it may differ.
	blockBegin = "# >>> upgradoor generated block"
	blockEnd   = "# <<< upgradoor generated block"

	blockBeginLine = blockBegin + ": written by Ansible through upgradoor deploy, do not edit >>>"
	blockEndLine   = blockEnd + " <<<"

	keyDeployed  = "deployed"
	keyOverrides = "overrides"
	keyHold      = "hold"

	// maxHistory bounds the history comment lines kept per tag.
	maxHistory = 5
)

// entryYAML, deployedYAML and holdYAML are the entries as the file writes
// them; the decoder refuses any other key.
type entryYAML struct {
	Image string `yaml:"image"`
	Flags struct {
		Add    []string `yaml:"add"`
		Remove []string `yaml:"remove"`
	} `yaml:"flags"`
	By     string     `yaml:"by"`
	Reason string     `yaml:"reason"`
	Until  *time.Time `yaml:"until"`
}

type deployedYAML struct {
	Image   string    `yaml:"image"`
	Command *[]string `yaml:"command"`
}

type holdYAML struct {
	By      string     `yaml:"by"`
	Reason  string     `yaml:"reason"`
	Teams   []string   `yaml:"teams"`
	Feature string     `yaml:"feature"`
	Until   *time.Time `yaml:"until"`
}

// noOverrides is a file that is not there, or says nothing.
func noOverrides() plan.Overrides {
	return plan.Overrides{
		Known: true, Deployed: map[string]plan.Deployed{}, Entries: map[string]plan.Entry{},
		Holds: map[string]plan.Hold{},
	}
}

// parse reads the content of an overrides file strictly: an unknown key is
// an error, not "no overrides", because a typo must not put every container
// back. Free text read by hand is cut to one line.
func parse(raw []byte) (plan.Overrides, error) {
	var doc struct {
		Deployed  map[string]*deployedYAML `yaml:"deployed"`
		Overrides map[string]*entryYAML    `yaml:"overrides"`
		Hold      map[string]*holdYAML     `yaml:"hold"`
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

	out := noOverrides()

	for source, y := range doc.Hold {
		if source != plan.HoldLocal && source != plan.HoldController {
			return plan.Overrides{}, fmt.Errorf("unknown hold %q (local or controller)", source)
		}

		h := plan.Hold{}
		if y != nil {
			h = plan.Hold{
				By: plan.Line(y.By), Reason: plan.Line(y.Reason), Teams: lines(y.Teams),
				Feature: plan.Line(y.Feature), Until: y.Until,
			}
		}

		out.Holds[source] = h
	}

	for name, y := range doc.Deployed {
		if y == nil || strings.TrimSpace(y.Image) == "" {
			return plan.Overrides{}, fmt.Errorf("deployed entry %q names no image", name)
		}

		d := plan.Deployed{Image: strings.TrimSpace(y.Image)}
		if y.Command != nil {
			d.Command, d.HasCommand = append([]string{}, *y.Command...), true
		}

		out.Deployed[name] = d
	}

	for name, y := range doc.Overrides {
		if y == nil {
			// "geth:" with nothing under it overrides nothing.
			continue
		}

		out.Entries[name] = plan.Entry{
			Image: strings.TrimSpace(y.Image), Add: y.Flags.Add, Remove: y.Flags.Remove,
			By: plan.Line(y.By), Reason: plan.Line(y.Reason), Until: y.Until,
		}
	}

	return out, nil
}

// lines cuts every item to one line; nil stays nil.
func lines(items []string) []string {
	if items == nil {
		return nil
	}

	out := make([]string, 0, len(items))
	for _, s := range items {
		out = append(out, plan.Line(s))
	}

	return out
}

// text is the content of an overrides file as lines.
type text struct {
	lines []string
	// body is the index of the first line after the generated block.
	body int
}

// newText splits an overrides file into lines, reading CRLF as LF.
func newText(raw []byte) *text {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	t := &text{}

	if s != "" {
		t.lines = strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}

	begin := -1

	for i, line := range t.lines {
		switch {
		case begin < 0 && strings.HasPrefix(line, blockBegin):
			begin = i
		case begin >= 0 && strings.HasPrefix(line, blockEnd):
			t.body = i + 1

			return t
		}
	}

	return t
}

func (t *text) bytes() []byte {
	if len(t.lines) == 0 {
		return nil
	}

	return []byte(strings.Join(t.lines, "\n") + "\n")
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }

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

// located is where a top-level mapping and one entry of it are.
type located struct {
	// key is the line of the mapping's key; -1 when there is none.
	key int
	// childIndent is the indentation of the entries' names.
	childIndent int
	// entry is the line of the entry's name; -1 when there is none.
	entry int
}

// locate finds the mapping key and its entry child below the generated
// block. A flow-style mapping with content cannot be changed line by line
// and is refused; an empty one ("overrides:", "{}", "null") is rewritten as
// the key alone.
func (t *text) locate(key, child string) (located, error) {
	loc := located{key: -1, entry: -1, childIndent: 2}

	var doc yaml.Node

	if err := yaml.Unmarshal([]byte(strings.Join(t.lines[t.body:], "\n")), &doc); err != nil {
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
		if k.Value != key {
			continue
		}

		loc.key = t.body + k.Line - 1

		switch {
		case v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle != 0 && len(v.Content) > 0,
			v.Kind != yaml.MappingNode && v.Kind != yaml.ScalarNode,
			v.Kind == yaml.ScalarNode && v.Tag != "!!null":
			return loc, fmt.Errorf("overrides file: write %q as an indented block, one entry per name", key)
		case v.Kind != yaml.MappingNode || len(v.Content) == 0:
			t.lines[loc.key] = strings.Repeat(" ", k.Column-1) + key + ":"

			return loc, nil
		}

		loc.childIndent = v.Content[0].Column - 1

		for j := 0; j+1 < len(v.Content); j += 2 {
			if v.Content[j].Value == child {
				loc.entry = t.body + v.Content[j].Line - 1
			}
		}
	}

	return loc, nil
}

// historyPattern matches the history lines of a tag.
func historyPattern(tag string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*# \[` + regexp.QuoteMeta(tag) + `\] `)
}

// setEntry replaces the child of the mapping key (or removes it, when render
// returns nothing) and adds a history line under tag; the oldest beyond
// maxHistory go. Nothing else changes.
func (t *text) setEntry(
	key, child, tag string, at time.Time, who, note string, render func(pad string) []string,
) error {
	tag = plan.Line(tag)

	loc, err := t.locate(key, child)
	if err != nil {
		return err
	}

	if loc.key < 0 {
		if n := len(t.lines); n > 0 && !isBlank(t.lines[n-1]) {
			t.lines = append(t.lines, "")
		}

		t.lines = append(t.lines, key+":")
		loc.key = len(t.lines) - 1
	}

	pad := strings.Repeat(" ", loc.childIndent)

	// Where the entry is, or where a new one goes: the end of the mapping.
	start, end := loc.entry, 0
	if start >= 0 {
		end = t.blockEnd(start, loc.childIndent)
	} else {
		start = t.blockEnd(loc.key, indentOf(t.lines[loc.key]))
		end = start
	}

	t.lines = slices.Concat(t.lines[:start], render(pad), t.lines[end:])

	// The history line goes after the tag's last one, or right above where
	// the entry is (or was).
	history := historyPattern(tag)
	insert := start

	for i := t.body; i < len(t.lines); i++ {
		if history.MatchString(t.lines[i]) {
			insert = i + 1
		}
	}

	t.lines = slices.Insert(t.lines, insert, fmt.Sprintf("%s# [%s] %s %s: %s",
		pad, tag, at.UTC().Format(time.RFC3339), plan.Line(who), plan.Line(note)))

	kept := 0

	for i := len(t.lines) - 1; i >= t.body; i-- {
		if !history.MatchString(t.lines[i]) {
			continue
		}

		kept++
		if kept > maxHistory {
			t.lines = slices.Delete(t.lines, i, i+1)
		}
	}

	return nil
}

// containerName is a valid container name; nothing else is a tag of a
// history line.
var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// setOverride writes the entry of a container (nil removes it) and reads
// the result back: it must hold the entry as written and everything else as
// it was, or an error is returned.
func (t *text) setOverride(container string, e *plan.Entry, at time.Time, who, note string) error {
	if !containerName.MatchString(container) {
		return plan.Refusal("%q is not a valid container name", container)
	}

	before, err := parse(t.bytes())
	if err != nil {
		return err
	}

	err = t.setEntry(keyOverrides, container, container, at, who, note, func(pad string) []string {
		if e == nil {
			return nil
		}

		return renderEntry(container, e, pad)
	})
	if err != nil {
		return err
	}

	set, err := parse(t.bytes())
	if err != nil {
		return fmt.Errorf("the change would break the overrides file: %w", err)
	}

	if err := unchanged(&before, &set, []string{container}, nil); err != nil {
		return err
	}

	got, ok := set.Entries[container]

	switch {
	case e == nil && !ok:
		return nil
	case (e == nil) != !ok:
		return errors.New("the change did not take in the overrides file")
	}

	same := got.Image == e.Image && slices.Equal(got.Add, e.Add) && slices.Equal(got.Remove, e.Remove) &&
		got.By == plan.Line(e.By) && got.Reason == plan.Line(e.Reason) && sameSecond(got.Until, e.Until)
	if !same {
		return errors.New("the entry reads back differently from what was written")
	}

	return nil
}

// setHold writes the hold of a source (nil removes it) and reads it back.
func (t *text) setHold(source string, h *plan.Hold, at time.Time, who, note string) error {
	before, err := parse(t.bytes())
	if err != nil {
		return err
	}

	err = t.setEntry(keyHold, source, keyHold+":"+source, at, who, note, func(pad string) []string {
		if h == nil {
			return nil
		}

		return renderHold(source, h, pad)
	})
	if err != nil {
		return err
	}

	set, err := parse(t.bytes())
	if err != nil {
		return fmt.Errorf("the change would break the overrides file: %w", err)
	}

	if err := unchanged(&before, &set, nil, []string{source}); err != nil {
		return err
	}

	got, ok := set.Holds[source]

	switch {
	case h == nil && !ok:
		return nil
	case (h == nil) != !ok:
		return errors.New("the hold did not take in the overrides file")
	}

	same := got.By == plan.Line(h.By) && got.Reason == plan.Line(h.Reason) &&
		slices.Equal(got.Teams, lines(h.Teams)) && got.Feature == plan.Line(h.Feature) &&
		sameSecond(got.Until, h.Until)
	if !same {
		return errors.New("the hold reads back differently from what was written")
	}

	return nil
}

// sameSecond compares an end read back with the one written; the file holds
// it to the second.
func sameSecond(got, want *time.Time) bool {
	return (got == nil) == (want == nil) && (got == nil || got.Equal(want.Truncate(time.Second)))
}

// unchanged reports an error when after differs from before in anything
// but the named entries and holds: a change of one entry must not write any
// other part of the document.
func unchanged(before, after *plan.Overrides, entries, holds []string) error {
	if !sameMap(before.Deployed, after.Deployed, sameDeployed, nil) ||
		!sameMap(before.Entries, after.Entries, sameEntry, entries) ||
		!sameMap(before.Holds, after.Holds, sameHold, holds) {
		return errors.New("the change would alter other parts of the overrides file")
	}

	return nil
}

// sameMap compares two maps but for the keys in except.
func sameMap[V any](a, b map[string]V, same func(x, y *V) bool, except []string) bool {
	for k, x := range a {
		if slices.Contains(except, k) {
			continue
		}

		if y, ok := b[k]; !ok || !same(&x, &y) {
			return false
		}
	}

	for k := range b {
		if _, ok := a[k]; !ok && !slices.Contains(except, k) {
			return false
		}
	}

	return true
}

func sameDeployed(a, b *plan.Deployed) bool {
	return a.Image == b.Image && a.HasCommand == b.HasCommand && slices.Equal(a.Command, b.Command)
}

func sameEntry(a, b *plan.Entry) bool {
	return a.Image == b.Image && slices.Equal(a.Add, b.Add) && slices.Equal(a.Remove, b.Remove) &&
		a.By == b.By && a.Reason == b.Reason && sameEnd(a.Until, b.Until)
}

func sameHold(a, b *plan.Hold) bool {
	return a.By == b.By && a.Reason == b.Reason && slices.Equal(a.Teams, b.Teams) &&
		a.Feature == b.Feature && sameEnd(a.Until, b.Until)
}

func sameEnd(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }

// renderEntry writes one override entry; free text cut to one line.
func renderEntry(container string, e *plan.Entry, pad string) []string {
	in := pad + "  "
	out := []string{pad + jsonString(container) + ":"}

	if e.Image != "" {
		out = append(out, in+"image: "+jsonString(e.Image))
	}

	if len(e.Add)+len(e.Remove) > 0 {
		out = append(out, in+"flags:")

		if len(e.Add) > 0 {
			out = append(out, in+"  add: "+jsonList(e.Add))
		}

		if len(e.Remove) > 0 {
			out = append(out, in+"  remove: "+jsonList(e.Remove))
		}
	}

	out = append(out, in+"by: "+jsonString(plan.Line(e.By)), in+"reason: "+jsonString(plan.Line(e.Reason)))

	if e.Until != nil {
		out = append(out, in+"until: "+e.Until.UTC().Format(time.RFC3339))
	}

	return out
}

// renderHold writes one hold entry; free text cut to one line.
func renderHold(source string, h *plan.Hold, pad string) []string {
	in := pad + "  "
	out := []string{
		pad + source + ":",
		in + "by: " + jsonString(plan.Line(h.By)),
		in + "reason: " + jsonString(plan.Line(h.Reason)),
	}

	if len(h.Teams) > 0 {
		out = append(out, in+"teams: "+jsonList(lines(h.Teams)))
	}

	if f := plan.Line(h.Feature); f != "" {
		out = append(out, in+"feature: "+jsonString(f))
	}

	if h.Until != nil {
		out = append(out, in+"until: "+h.Until.UTC().Format(time.RFC3339))
	}

	return out
}

// setBlock replaces the generated block (or puts one at the top) and keeps
// the rest of the file as it is.
func (t *text) setBlock(block []string) {
	rest := t.lines[t.body:]

	if t.body == 0 && len(rest) > 0 && !isBlank(rest[0]) {
		rest = append([]string{""}, rest...)
	}

	t.lines = slices.Concat(block, rest)
	t.body = len(block)
}

// generatedBlock renders the generated block.
func generatedBlock(deployed map[string]plan.Deployed) []string {
	out := []string{
		blockBeginLine,
		"#",
		"# What Ansible deployed the containers upgradoor manages on this host as",
		"# (\"deployed:\" below), and their temporary overrides.",
		"#",
		"# Only Ansible changes this block, through upgradoor deploy. A soft deploy",
		"# changes the image or command line here and leaves the restart to upgradoor,",
		"# which rolls the containers staggered across the devnet. Do not edit the block.",
		"#",
		"# Add an entry under \"overrides:\" below this block (by hand, with the commands",
		"# below, or from the upgradoor page). The agent notices a change within a",
		"# minute, asks the controller for a slot and recreates the container; it",
		"# puts the container back when the entry is removed or its \"until\" has",
		"# passed. Everything outside this block is yours: the agent only ever",
		"# changes the entry it is asked to change and adds a comment line above it.",
		"# What may be overridden is up to the container's group in agent.yaml.",
		"#",
		"#   upgradoor list                           containers, images, flags, overrides",
		"#   upgradoor override set <container> [--image REF] [--add-flag F]...",
		"#            [--remove-flag F]... [--until 24h] --reason \"why\"",
		"#   upgradoor override clear <container>",
		"#   upgradoor roll <container>...            recreate on the current image; waits",
		"#            and prints the progress (--no-wait; see: upgradoor status)",
		"#   upgradoor hold \"why\" / upgradoor release",
		"#   upgradoor status",
		"#",
		"# An entry (image, flags or both; \"until\" is optional when written by hand):",
		"#",
		"# overrides:",
		"#   <container>:",
		"#     image: <registry/repository:tag>",
		"#     flags:",
		"#       add: [\"--name=value\", \"--name value\", \"--switch\"]",
		"#       remove: [\"--name\"]     # by name: any value; or the exact argument",
		"#     by: <you>",
		"#     reason: <why>",
		"#     until: 2030-01-31T12:00:00Z",
		"#",
		"# A hold keeps upgradoor off this host: no needs, no work. \"local\" is yours",
		"# (by hand or with upgradoor hold); the controller cannot lift it.",
		"# \"controller\" is written by the agent on the controller's hold:",
		"#",
		"# hold:",
		"#   local:",
		"#     by: <you>",
		"#     reason: <why>",
		"#     until: 2030-01-31T12:00:00Z     # optional",
		"#",
	}

	if len(deployed) == 0 {
		out = append(out, keyDeployed+": {}")
	} else {
		out = append(out, keyDeployed+":")
	}

	for _, name := range slices.Sorted(maps.Keys(deployed)) {
		d := deployed[name]
		out = append(out, "  "+jsonString(name)+":", "    image: "+jsonString(d.Image))

		if d.HasCommand {
			out = append(out, "    command: "+jsonList(d.Command))
		}
	}

	return append(out, blockEndLine)
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

func jsonList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, jsonString(item))
	}

	return "[" + strings.Join(quoted, ", ") + "]"
}
