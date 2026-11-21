package overrides

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/agent/atomicfile"
	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
)

// Text is the content of an overrides file as lines, for changing
// one entry and nothing else.
type Text struct {
	lines []string
	// body is the index of the first line after the generated block.
	body int
}

// NewText splits the content of an overrides file into lines.
func NewText(raw []byte) *Text {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	t := &Text{}

	if text != "" {
		t.lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}

	begin := -1

	for i, line := range t.lines {
		switch {
		case begin < 0 && strings.HasPrefix(line, overridesBegin):
			begin = i
		case begin >= 0 && strings.HasPrefix(line, overridesEnd):
			t.body = i + 1

			return t
		}
	}

	return t
}

// Bytes is the content of the file.
func (t *Text) Bytes() []byte {
	if len(t.lines) == 0 {
		return nil
	}

	return []byte(strings.Join(t.lines, "\n") + "\n")
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }

// blockEnd returns the index after the last line of the block that starts at
// line start and is indented deeper than indent: its lines, the comments
// among them, and blank lines that are followed by more of it.
func (t *Text) blockEnd(start, indent int) int {
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

// located is where the overrides mapping and the entry of one container are
// in the text.
type located struct {
	// key is the line of "overrides:"; -1 when there is none.
	key int
	// childIndent is the indentation of the container names.
	childIndent int
	// entry is the line of the container's name; -1 when it has no entry.
	entry int
}

// locate finds the overrides mapping and the entry of a container.
func (t *Text) locate(container string) (located, error) {
	loc := located{key: -1, entry: -1, childIndent: 2}

	var doc yaml.Node

	body := strings.Join(t.lines[t.body:], "\n")
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
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

		loc.key = t.body + k.Line - 1

		switch {
		case v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle != 0 && len(v.Content) > 0,
			v.Kind != yaml.MappingNode && v.Kind != yaml.ScalarNode,
			v.Kind == yaml.ScalarNode && v.Tag != "!!null":
			return loc, errors.New("overrides file: write the overrides as an indented block, " +
				"one container per entry")
		case v.Kind != yaml.MappingNode || len(v.Content) == 0:
			// "overrides:", "overrides: {}", "overrides: null".
			t.lines[loc.key] = strings.Repeat(" ", k.Column-1) + keyOverrides + ":"

			return loc, nil
		}

		loc.childIndent = v.Content[0].Column - 1

		for j := 0; j+1 < len(v.Content); j += 2 {
			if v.Content[j].Value == container {
				loc.entry = t.body + v.Content[j].Line - 1
			}
		}
	}

	return loc, nil
}

// historyPattern matches the comment lines the agent wrote for a container.
func historyPattern(container string) *regexp.Regexp {
	return regexp.MustCompile(`^\s*# \[` + regexp.QuoteMeta(container) + `\] `)
}

// Set replaces the entry of a container (or removes it, with entry nil) and
// adds a history line saying note. Everything else stays as it is.
func (t *Text) Set(container string, entry *Entry, at time.Time, who, note string) error {
	loc, err := t.locate(container)
	if err != nil {
		return err
	}

	if loc.key < 0 {
		if n := len(t.lines); n > 0 && !isBlank(t.lines[n-1]) {
			t.lines = append(t.lines, "")
		}

		t.lines = append(t.lines, keyOverrides+":")
		loc.key = len(t.lines) - 1
	}

	pad := strings.Repeat(" ", loc.childIndent)

	var rendered []string
	if entry != nil {
		rendered = renderOverride(container, entry, pad)
	}

	// Where the entry is, or where a new one goes: the end of the mapping.
	start, end := loc.entry, 0
	if start >= 0 {
		end = t.blockEnd(start, loc.childIndent)
	} else {
		start = t.blockEnd(loc.key, indentOf(t.lines[loc.key]))
		end = start
	}

	t.lines = slices.Concat(t.lines[:start], rendered, t.lines[end:])

	// The history line goes after the container's last one, or right above
	// where its entry is (or was).
	history := historyPattern(container)
	line := fmt.Sprintf("%s# [%s] %s %s: %s",
		pad, container, at.UTC().Format(time.RFC3339), auditlog.Line(who), auditlog.Line(note))
	insert := start

	for i := t.body; i < len(t.lines); i++ {
		if history.MatchString(t.lines[i]) {
			insert = i + 1
		}
	}

	t.lines = slices.Insert(t.lines, insert, line)

	// The oldest lines beyond the bound go.
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

	return t.check(container, entry)
}

// check reads the changed text back: it must hold the entry as it was
// written (or none), or the change is not made.
func (t *Text) check(container string, want *Entry) error {
	set, err := Parse(t.Bytes())
	if err != nil {
		return fmt.Errorf("the change would break the overrides file: %w", err)
	}

	got := set.Entry(container)

	switch {
	case want == nil && got == nil:
		return nil
	case want == nil || got == nil:
		return errors.New("the change did not take in the overrides file")
	}

	same := got.Image == want.Image && slices.Equal(got.Add, want.Add) &&
		slices.Equal(got.Remove, want.Remove) && got.By == auditlog.Line(want.By) &&
		got.Reason == auditlog.Line(want.Reason) &&
		(got.Until == nil) == (want.Until == nil) &&
		(got.Until == nil || got.Until.Equal(want.Until.Truncate(time.Second)))
	if !same {
		return errors.New("the entry reads back differently from what was written")
	}

	return nil
}

// renderOverride writes one entry. Strings are written as JSON strings,
// which are YAML strings too, so nothing in them can be read as YAML.
func renderOverride(container string, o *Entry, pad string) []string {
	quote := func(s string) string {
		var buf bytes.Buffer

		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		// Encoding a string cannot fail.
		_ = enc.Encode(s)

		return strings.TrimSuffix(buf.String(), "\n")
	}

	list := func(items []string) string {
		quoted := make([]string, 0, len(items))
		for _, item := range items {
			quoted = append(quoted, quote(item))
		}

		return "[" + strings.Join(quoted, ", ") + "]"
	}

	in := pad + "  "
	out := []string{pad + quote(container) + ":"}

	if o.Image != "" {
		out = append(out, in+"image: "+quote(o.Image))
	}

	if len(o.Add)+len(o.Remove) > 0 {
		out = append(out, in+"flags:")

		if len(o.Add) > 0 {
			out = append(out, in+"  add: "+list(o.Add))
		}

		if len(o.Remove) > 0 {
			out = append(out, in+"  remove: "+list(o.Remove))
		}
	}

	out = append(out, in+"by: "+quote(auditlog.Line(o.By)), in+"reason: "+quote(auditlog.Line(o.Reason)))

	if o.Until != nil {
		out = append(out, in+"until: "+o.Until.UTC().Format(time.RFC3339))
	}

	return out
}

// SetGeneratedBlock replaces the generated block (or puts one at the top)
// and leaves the rest of the file as it is.
func (t *Text) SetGeneratedBlock(block []string) {
	rest := t.lines[t.body:]

	if t.body == 0 && len(rest) > 0 && !isBlank(rest[0]) {
		rest = append([]string{""}, rest...)
	}

	t.lines = slices.Concat(block, rest)
	t.body = len(block)
}

// GeneratedContainer is one managed container in the generated block: what
// the inventory deploys.
type GeneratedContainer struct {
	Name    string
	Image   string
	Command []string
}

// GeneratedBlock renders the generated block of the overrides file: the
// how-to, the local commands and, for every managed container, the default
// image and command line as commented YAML. The Ansible role writes exactly
// this; 'upgradoor host init-overrides' writes it where the role is not used.
func GeneratedBlock(containers []GeneratedContainer) []string {
	out := []string{
		overridesBeginLine,
		"#",
		"# Temporary overrides of the containers upgradoor manages on this host.",
		"#",
		"# Add an entry under \"overrides:\" below this block (by hand, with the commands",
		"# below, or from the upgradoor page). The agent notices a change within a",
		"# minute, asks the controller for a slot and recreates the container; it",
		"# puts the container back when the entry is removed or its \"until\" has",
		"# passed. Everything outside this block is yours: the agent only ever",
		"# changes the entry it is asked to change and adds a comment line above it.",
		"#",
		"#   upgradoor host list                      containers, images, flags, overrides",
		"#   upgradoor host override set <container> [--image REF] [--add-flag F]...",
		"#                 [--remove-flag F]... [--until 24h] --reason \"why\"",
		"#   upgradoor host override clear <container>",
		"#   upgradoor host roll <container>...       recreate on the current image; waits",
		"#                 and prints the progress (--no-wait; see: upgradoor host status)",
		"#   upgradoor host hold \"why\" / upgradoor host release",
		"#   upgradoor host status",
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
		"# What the inventory deploys on this host:",
	}

	for _, c := range containers {
		out = append(out, "#", "#   "+c.Name+":", "#     image: "+c.Image)

		if len(c.Command) == 0 {
			out = append(out, "#     command: []")

			continue
		}

		out = append(out, "#     command:")

		for _, arg := range c.Command {
			raw, _ := json.Marshal(arg)
			out = append(out, "#       - "+string(raw))
		}
	}

	return append(out, "#", overridesEndLine)
}

// File reads and changes the overrides file of the host. Changes
// are serialized by the caller (the service's override lock).
type File struct {
	path string
}

// NewFile is the overrides file at path.
func NewFile(path string) *File {
	return &File{path: path}
}

// Change applies fn to the file's text and writes the result atomically,
// with the owner, group and mode the file had (or, for a new file, the
// owner and group of its directory and mode 0664, so that the entry user
// can edit it).
func (f *File) Change(fn func(t *Text) error) error {
	raw, err := readFile(f.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	text := NewText(raw)
	if err := fn(text); err != nil {
		return err
	}

	mode := os.FileMode(0o664)
	owner := filepath.Dir(f.path)

	if info, statErr := os.Stat(f.path); statErr == nil {
		mode = info.Mode().Perm()
		owner = f.path
	}

	uid, gid, known := ownerOf(owner)

	if err := atomicfile.Write(f.path, text.Bytes(), mode); err != nil {
		return fmt.Errorf("write overrides file: %w", err)
	}

	if known && (uid != os.Geteuid() || gid != os.Getegid()) {
		if err := os.Chown(f.path, uid, gid); err != nil {
			return fmt.Errorf("overrides file: keep its owner: %w", err)
		}
	}

	return nil
}

func ownerOf(file string) (uid, gid int, ok bool) {
	info, err := os.Stat(file)
	if err != nil {
		return 0, 0, false
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}

	return int(st.Uid), int(st.Gid), true
}
