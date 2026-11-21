package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The overrides file is the one place where an override of a managed
// container lives: another image, added and removed flags, for a while. It
// is a file in the home directory of the entry user so that whoever has SSH
// can read and edit it; the controller stores no override and learns of them
// from the agent's report.
//
// Three writers share the file, so it has two parts:
//
//	# >>> upgradoor generated block ... >>>      written by Ansible (or by
//	# ...                                          'upgradoor host
//	# <<< upgradoor generated block <<<            init-overrides'); comments
//	                                               only; the agent keeps it
//	                                               byte for byte
//	overrides:                                    written by people and by
//	  # [geth] 2026-11-20T16:00:00Z sam: ...       the agent
//	  geth:
//	    image: ethpandaops/geth:fix-sync
//	    flags:
//	      add: ["--verbosity=4"]
//	      remove: ["--maxpeers"]
//	    by: sam@example.org
//	    reason: debugging the sync
//	    until: 2026-11-21T16:00:00Z
//
// The agent changes the entry of one container at a time and leaves every
// other byte as it is: other entries, comments, blank lines, the order.
// Each change it makes adds one comment line "# [<container>] <time> <who>:
// <what>" above the entry; the last maxOverrideHistory of them are kept per
// container.
const (
	// DefaultEntryUser is the user whose home directory holds the overrides
	// file and whose group may use the local socket.
	DefaultEntryUser = "devops"

	overridesFileName = "overrides.yaml"

	// overridesBegin and overridesEnd are the first and the last line of the
	// generated block. Only the part before the colon is compared.
	overridesBegin = "# >>> upgradoor generated block"
	overridesEnd   = "# <<< upgradoor generated block"

	overridesBeginLine = overridesBegin + ": rewritten by Ansible on every run, do not edit >>>"
	overridesEndLine   = overridesEnd + " <<<"

	keyOverrides = "overrides"

	// maxOverrideHistory bounds the comment lines the agent keeps per
	// container.
	maxOverrideHistory = 5
	// maxOverridesFile bounds the file the agent is willing to read.
	maxOverridesFile = 1 << 20

	// DefaultOverrideDuration and MaxOverrideDuration bound an override that
	// is not set by hand: it always has an end.
	DefaultOverrideDuration = 24 * time.Hour
	MaxOverrideDuration     = 7 * 24 * time.Hour

	maxOverrideText = 500

	// noOverride is how the absence of an override is written where one is
	// described.
	noOverride = "nothing"
)

// Labels the agent puts on a container it recreates with an override, so
// that the container itself says what it was before. A container without
// them is in the form the inventory gave it.
const (
	// LabelOverrideImage holds the configured image reference of a container
	// that runs an override image: what agent.yaml said when the override was
	// applied. A container whose label (or, without one, image) differs from
	// what agent.yaml says now is moved there with a spec need.
	LabelOverrideImage = "io.ethpandaops.upgradoor.override.image"
	// LabelOverrideCommand holds the command line the container had before a
	// flag override changed it, as a JSON array of strings.
	LabelOverrideCommand = "io.ethpandaops.upgradoor.override.command"
)

// Override is the entry of one container in the overrides file.
type Override struct {
	// Image replaces the configured image reference; empty keeps it.
	Image string
	// Add and Remove change the command line (applyFlags).
	Add    []string
	Remove []string
	// By and Reason say who set it and why.
	By     string
	Reason string
	// Until is when the override ends; nil for one without an end, which
	// only a hand edit can make.
	Until *time.Time
}

// overrideYAML is an entry as it is written in the file.
type overrideYAML struct {
	Image string `yaml:"image"`
	Flags struct {
		Add    []string `yaml:"add"`
		Remove []string `yaml:"remove"`
	} `yaml:"flags"`
	By     string     `yaml:"by"`
	Reason string     `yaml:"reason"`
	Until  *time.Time `yaml:"until"`
}

// overrideSet is the overrides file as last read.
type overrideSet struct {
	// known is false when the file exists and cannot be read or understood.
	// Nothing is then applied and nothing restored: a file that is broken
	// for a moment must not put every container back.
	known bool
	// err says what is wrong with the file.
	err     string
	entries map[string]Override
}

// entry returns the override of a container, or nil.
func (s *overrideSet) entry(name string) *Override {
	if s == nil {
		return nil
	}

	if e, ok := s.entries[name]; ok {
		return &e
	}

	return nil
}

// noOverrides is the set of a host without an overrides file.
func noOverrides() *overrideSet {
	return &overrideSet{known: true, entries: map[string]Override{}}
}

// readOverrides reads the overrides file. A file that does not exist, is
// empty or holds nothing but the generated block is no overrides.
func readOverrides(file string) *overrideSet {
	raw, err := readOverridesFile(file)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return noOverrides()
	case err != nil:
		return &overrideSet{err: err.Error()}
	}

	set, err := parseOverrides(raw)
	if err != nil {
		return &overrideSet{err: err.Error()}
	}

	return set
}

func readOverridesFile(file string) ([]byte, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("overrides file: %w", err)
	}

	if info.Size() > maxOverridesFile {
		return nil, fmt.Errorf("overrides file %s is larger than %d bytes", file, maxOverridesFile)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("overrides file: %w", err)
	}

	return raw, nil
}

// parseOverrides reads the content of an overrides file.
func parseOverrides(raw []byte) (*overrideSet, error) {
	var doc struct {
		Overrides map[string]*overrideYAML `yaml:"overrides"`
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("overrides file: %w", err)
	}

	set := noOverrides()

	for name, y := range doc.Overrides {
		if y == nil {
			// "geth:" with nothing under it overrides nothing.
			continue
		}

		o := Override{
			Image: strings.TrimSpace(y.Image), Add: y.Flags.Add, Remove: y.Flags.Remove,
			By: firstLine(y.By), Reason: firstLine(y.Reason), Until: y.Until,
		}

		set.entries[name] = o
	}

	return set, nil
}

// empty reports whether the entry changes nothing.
func (o *Override) empty() bool {
	return o == nil || (o.Image == "" && len(o.Add) == 0 && len(o.Remove) == 0)
}

// expired reports whether the entry's end has passed.
func (o *Override) expired(now time.Time) bool {
	return o != nil && o.Until != nil && !o.Until.After(now)
}

// describe says what the entry changes, for a history line and the audit
// log: "image ethpandaops/geth:fix, flags +--verbosity=4 ---maxpeers".
func (o *Override) describe() string {
	if o.empty() {
		return noOverride
	}

	parts := make([]string, 0, 2)

	if o.Image != "" {
		parts = append(parts, "image "+o.Image)
	}

	if len(o.Add)+len(o.Remove) > 0 {
		flags := make([]string, 0, len(o.Add)+len(o.Remove))
		for _, f := range o.Add {
			flags = append(flags, "+"+f)
		}

		for _, f := range o.Remove {
			flags = append(flags, "-"+f)
		}

		parts = append(parts, "flags "+strings.Join(flags, " "))
	}

	return strings.Join(parts, ", ")
}

// validate checks an entry that is about to be written.
func (o *Override) validate() error {
	if o.empty() {
		return errors.New("an override names an image, flags or both")
	}

	if o.Image != "" {
		if _, err := ParseReference(o.Image); err != nil {
			return err
		}

		if !isTagReference(o.Image) {
			return fmt.Errorf("image %s is pinned to a digest; an override follows a tag", o.Image)
		}
	}

	if err := validateFlags(o.Add, o.Remove); err != nil {
		return err
	}

	if len(o.By) > maxOverrideText || len(o.Reason) > maxOverrideText {
		return errors.New("by or reason is too long")
	}

	return nil
}

// imageAllowed reports whether patterns allow the repository of ref. A
// pattern is a repository ("ethpandaops/geth", "ghcr.io/ethereum/*") in which
// "*" stands for any run of characters without a slash; it is compared with
// the canonical repository of the reference (docker.io/ethpandaops/geth), so
// a pattern without a registry host means Docker Hub, as a reference does.
func imageAllowed(patterns []string, ref string) bool {
	parsed, err := ParseReference(ref)
	if err != nil {
		return false
	}

	for _, p := range patterns {
		if ok, err := path.Match(normalizeRepositoryPattern(p), parsed.Repository()); err == nil && ok {
			return true
		}
	}

	return false
}

// normalizeRepositoryPattern adds the registry host (and library/) that a
// reference without one gets.
func normalizeRepositoryPattern(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))

	first, rest, more := strings.Cut(p, "/")
	if !more {
		return DockerHubHost + "/library/" + p
	}

	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return RegistryHost(first) + "/" + rest
	}

	return DockerHubHost + "/" + p
}

// validateRepositoryPattern checks one entry of a container's allowed image
// repositories.
func validateRepositoryPattern(p string) error {
	last := p[strings.LastIndex(p, "/")+1:]

	if p == "" || strings.ContainsAny(p, " \t@") || strings.Contains(last, ":") {
		return fmt.Errorf("%q is not an image repository pattern (a repository, no tag or digest)", p)
	}

	if _, err := path.Match(normalizeRepositoryPattern(p), "x"); err != nil {
		return fmt.Errorf("%q is not a pattern: %w", p, err)
	}

	return nil
}

// overrideTarget is what a managed container is to run, by the agent
// configuration and the overrides file, and how the container that is there
// compares.
type overrideTarget struct {
	// spec: agent.yaml names another image or command line than the one the
	// container was created with (before an override changed it).
	spec bool
	// image and command are what the container is to run.
	image   string
	command []string
	// labels are the override labels the container is to carry: a nil value
	// removes the label.
	labels map[string]*string
	// inSync: the container runs image with command.
	inSync bool
	// flags: the command line changes on the way to the target.
	flags bool
	// restore: there is no entry (any more) and the container still runs
	// what one said.
	restore bool
	// err says why the entry cannot be applied.
	err string
}

// targetOf compares a managed container with what it is to run.
func targetOf(m *ManagedContainer, c *protocol.Container, entry *Override) overrideTarget {
	if entry.empty() {
		entry = nil
	}

	// What the container was created as, before an override changed it.
	createdImage, createdCommand := c.Image, c.Command

	if base := c.Base; base != nil {
		if base.Image != "" {
			createdImage = base.Image
		}

		if base.HasCommand {
			createdCommand = base.Command
		}
	}

	// What agent.yaml says it is: its image, and its command line where it
	// names one.
	baseCommand := createdCommand
	if m.Command != nil {
		baseCommand = m.Command
	}

	t := overrideTarget{
		image: m.Image, command: slices.Clone(baseCommand),
		spec: !SameReference(createdImage, m.Image) || !slices.Equal(createdCommand, baseCommand),
	}

	if entry != nil {
		if err := entry.validate(); err != nil {
			t.err = err.Error()
			// What cannot be applied changes nothing: the container stays
			// as it is, also when it runs an earlier form of the entry.
			t.image, t.command, t.inSync, t.spec = c.Image, slices.Clone(c.Command), true, false

			return t
		}

		if entry.Image != "" {
			t.image = entry.Image
		}

		t.command = applyFlags(baseCommand, entry.Add, entry.Remove)
	}

	t.labels = map[string]*string{LabelOverrideImage: nil, LabelOverrideCommand: nil}

	if !SameReference(t.image, m.Image) {
		configured := m.Image
		t.labels[LabelOverrideImage] = &configured
	}

	if !slices.Equal(t.command, baseCommand) {
		// Encoding a list of strings cannot fail.
		raw, _ := json.Marshal(nonNilStrings(baseCommand))
		encoded := string(raw)
		t.labels[LabelOverrideCommand] = &encoded
	}

	t.flags = !slices.Equal(c.Command, t.command)
	t.inSync = SameReference(c.Image, t.image) && !t.flags
	t.spec = t.spec && !t.inSync
	t.restore = entry == nil && !t.inSync && !t.spec

	return t
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}

	return in
}

// key identifies the target together with the digest its image is at: the
// part of an override need's ID that changes when the file asks for
// something else.
func (t *overrideTarget) key(digest string) string {
	h := sha256.New()
	h.Write([]byte(NormalizeReference(t.image)))
	h.Write([]byte{0})
	h.Write([]byte(digest))

	for _, arg := range t.command {
		h.Write([]byte{0})
		h.Write([]byte(arg))
	}

	return hex.EncodeToString(h.Sum(nil)[:6])
}

// specKey identifies the target of a spec need across hosts: the image at
// its digest and the arguments the command line gains and loses. Two hosts
// whose rendered command lines differ in per-host values that stay the same
// share it.
func (t *overrideTarget) specKey(c *protocol.Container, digest string) string {
	added, removed := argDelta(c.Command, t.command)

	h := sha256.New()
	h.Write([]byte(NormalizeReference(t.image)))
	h.Write([]byte{0})
	h.Write([]byte(digest))

	for _, part := range [][]string{added, removed} {
		h.Write([]byte{1})

		for _, arg := range part {
			h.Write([]byte{0})
			h.Write([]byte(arg))
		}
	}

	return hex.EncodeToString(h.Sum(nil)[:6])
}

// argDelta returns the arguments that are in to and not in from, and those
// in from and not in to, each sorted and without repeats.
func argDelta(from, to []string) (added, removed []string) {
	missing := func(a, b []string) []string {
		out := make([]string, 0, len(a))

		for _, arg := range a {
			if !slices.Contains(b, arg) {
				out = append(out, arg)
			}
		}

		slices.Sort(out)

		return slices.Compact(out)
	}

	return missing(to, from), missing(from, to)
}

// needID is the ID of the need that brings the container to the target with
// its image at digest.
func (t *overrideTarget) needID(c *protocol.Container, digest string) string {
	if t.spec {
		return protocol.SpecNeedID(c.Name, t.specKey(c, digest))
	}

	return protocol.OverrideNeedID(c.Name, t.key(digest))
}

// report is what the sync says about a container's override; nil when the
// container runs what the configuration says and no entry asks otherwise.
func (t *overrideTarget) report(c *protocol.Container, entry *Override) *protocol.ContainerOverride {
	if entry.empty() {
		if !t.restore {
			return nil
		}

		out := &protocol.ContainerOverride{Restoring: true}
		if !SameReference(c.Image, t.image) {
			out.Image = c.Image
		}

		return out
	}

	return &protocol.ContainerOverride{
		Image: entry.Image, AddFlags: slices.Clone(entry.Add), RemoveFlags: slices.Clone(entry.Remove),
		By: entry.By, Reason: entry.Reason, Until: entry.Until,
		Applied: t.inSync && t.err == "", Error: t.err,
	}
}

// overridesText is the content of an overrides file as lines, for changing
// one entry and nothing else.
type overridesText struct {
	lines []string
	// body is the index of the first line after the generated block.
	body int
}

func newOverridesText(raw []byte) *overridesText {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	t := &overridesText{}

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

func (t *overridesText) bytes() []byte {
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
func (t *overridesText) blockEnd(start, indent int) int {
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
func (t *overridesText) locate(container string) (located, error) {
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

// set replaces the entry of a container (or removes it, with entry nil) and
// adds a history line saying note. Everything else stays as it is.
func (t *overridesText) set(container string, entry *Override, at time.Time, who, note string) error {
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
		pad, container, at.UTC().Format(time.RFC3339), firstLine(who), firstLine(note))
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

		if kept > maxOverrideHistory {
			t.lines = slices.Delete(t.lines, i, i+1)
		}
	}

	return t.check(container, entry)
}

// check reads the changed text back: it must hold the entry as it was
// written (or none), or the change is not made.
func (t *overridesText) check(container string, want *Override) error {
	set, err := parseOverrides(t.bytes())
	if err != nil {
		return fmt.Errorf("the change would break the overrides file: %w", err)
	}

	got := set.entry(container)

	switch {
	case want == nil && got == nil:
		return nil
	case want == nil || got == nil:
		return errors.New("the change did not take in the overrides file")
	}

	same := got.Image == want.Image && slices.Equal(got.Add, want.Add) &&
		slices.Equal(got.Remove, want.Remove) && got.By == firstLine(want.By) &&
		got.Reason == firstLine(want.Reason) &&
		(got.Until == nil) == (want.Until == nil) &&
		(got.Until == nil || got.Until.Equal(want.Until.Truncate(time.Second)))
	if !same {
		return errors.New("the entry reads back differently from what was written")
	}

	return nil
}

// renderOverride writes one entry. Strings are written as JSON strings,
// which are YAML strings too, so nothing in them can be read as YAML.
func renderOverride(container string, o *Override, pad string) []string {
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

	out = append(out, in+"by: "+quote(firstLine(o.By)), in+"reason: "+quote(firstLine(o.Reason)))

	if o.Until != nil {
		out = append(out, in+"until: "+o.Until.UTC().Format(time.RFC3339))
	}

	return out
}

// setGeneratedBlock replaces the generated block (or puts one at the top)
// and leaves the rest of the file as it is.
func (t *overridesText) setGeneratedBlock(block []string) {
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

// overridesFile reads and changes the overrides file of the host. Changes
// are serialized by the caller (the service's override lock).
type overridesFile struct {
	path string
}

// change applies fn to the file's text and writes the result atomically,
// with the owner, group and mode the file had (or, for a new file, the
// owner and group of its directory and mode 0664, so that the entry user
// can edit it).
func (f *overridesFile) change(fn func(t *overridesText) error) error {
	raw, err := readOverridesFile(f.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	text := newOverridesText(raw)
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

	if err := writeFileAtomic(f.path, text.bytes(), mode); err != nil {
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
