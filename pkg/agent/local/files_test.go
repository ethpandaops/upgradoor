package local

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

var fileTime = time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)

func untilOf(d time.Duration) *time.Time {
	t := fileTime.Add(d)

	return &t
}

// recorder is an Audit that keeps the records.
type recorder struct {
	mu   sync.Mutex
	recs []Record
}

func (r *recorder) Record(rec *Record) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.recs = append(r.recs, *rec)
}

func (r *recorder) LeaseResult(string, *protocol.Need, *protocol.Step) {}

// testFiles is a Files on a file in a temp directory, with a settable clock.
type testFiles struct {
	Files

	path  string
	now   time.Time
	audit *recorder
}

func newTestFiles(t *testing.T) *testFiles {
	t.Helper()

	tf := &testFiles{path: filepath.Join(t.TempDir(), "overrides.yaml"), now: fileTime, audit: &recorder{}}
	tf.Files = NewFiles(quiet(), tf.path, tf.audit, func() time.Time { return tf.now })

	return tf
}

// quiet is a logger that writes nothing.
func quiet() logrus.FieldLogger {
	log := logrus.New()
	log.SetOutput(io.Discard)

	return log
}

func (tf *testFiles) write(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(tf.path, []byte(content), 0o600))
}

func (tf *testFiles) read(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(tf.path)
	require.NoError(t, err)

	return string(raw)
}

// set writes entry (nil clears) as a build that permits it.
func (tf *testFiles) set(container string, entry *plan.Entry, who string) error {
	_, _, err := tf.SetEntry(container, who, func(*plan.Entry) (*plan.Entry, error) { return entry, nil })

	return err
}

func blockText(deployed map[string]plan.Deployed) string {
	return strings.Join(generatedBlock(deployed), "\n") + "\n"
}

// A change replaces one entry and no other byte.
func TestSetEntryKeepsEveryOtherByte(t *testing.T) {
	tf := newTestFiles(t)
	block := blockText(nil)
	tf.write(t, block+`
# my notes
overrides:

  # keep an eye on this one
  beacon:
      image: sigp/lighthouse:unstable   # indented my way
      reason: testing

      by: me
  geth:
    image: ethpandaops/geth:old
    by: me

# the end
`)

	entry := &plan.Entry{
		Image: "ethpandaops/geth:fix", Add: []string{"--a=b", "--c \"d\""}, By: "sam@example.org",
		Reason: "why: because", Until: untilOf(time.Hour),
	}
	require.NoError(t, tf.set("geth", entry, "sam@example.org"))

	assert.Equal(t, block+`
# my notes
overrides:

  # keep an eye on this one
  beacon:
      image: sigp/lighthouse:unstable   # indented my way
      reason: testing

      by: me
  # [geth] 2026-11-20T16:00:00Z sam@example.org: set image ethpandaops/geth:fix, flags +--a=b +--c "d"; was image ethpandaops/geth:old
  "geth":
    image: "ethpandaops/geth:fix"
    flags:
      add: ["--a=b", "--c \"d\""]
    by: "sam@example.org"
    reason: "why: because"
    until: 2026-11-20T17:00:00Z

# the end
`, tf.read(t))

	files := tf.Load()
	assert.Equal(t, entry.Add, files.Entries["geth"].Add)

	// Removing it keeps the history and the other entry.
	tf.now = fileTime.Add(30 * time.Minute)
	require.NoError(t, tf.set("geth", nil, "upgradoor"))

	files = tf.Load()
	assert.NotContains(t, files.Entries, "geth")
	assert.Equal(t, "sigp/lighthouse:unstable", files.Entries["beacon"].Image)
	assert.Contains(t, tf.read(t), "--c \"d\"; was image ethpandaops/geth:old\n"+
		"  # [geth] 2026-11-20T16:30:00Z upgradoor: cleared; was image ethpandaops/geth:fix, flags +--a=b +--c \"d\"\n"+
		"\n# the end\n")
	assert.True(t, strings.HasPrefix(tf.read(t), block), "the generated block is kept byte for byte")

	// Only the newest history lines are kept.
	for i := range 2 * maxHistory {
		require.NoError(t, tf.set("geth", &plan.Entry{Image: "a/b:" + string(rune('a'+i)), By: "s", Reason: "r"}, "sam"))
	}

	assert.Equal(t, maxHistory, strings.Count(tf.read(t), "# [geth] "))
	assert.NotContains(t, tf.read(t), "cleared")
	assert.Contains(t, tf.read(t), "# keep an eye on this one\n  beacon:\n")
}

func TestSetEntryWhereThereIsNothing(t *testing.T) {
	entry := &plan.Entry{Image: "a/b:c", By: "x", Reason: "y"}
	added := "  # [geth] 2026-11-20T16:00:00Z x: set image a/b:c; was nothing\n" +
		"  \"geth\":\n    image: \"a/b:c\"\n    by: \"x\"\n    reason: \"y\"\n"

	for name, tt := range map[string]struct{ before, want string }{
		"no file":          {"", "overrides:\n" + added},
		"an empty mapping": {"overrides: {}\n", "overrides:\n" + added},
		"comments only":    {"# notes", "# notes\n\noverrides:\n" + added},
		"CRLF":             {"# notes\r\noverrides:\r\n", "# notes\noverrides:\n" + added},
	} {
		t.Run(name, func(t *testing.T) {
			tf := newTestFiles(t)
			if tt.before != "" {
				tf.write(t, tt.before)
			}

			require.NoError(t, tf.set("geth", entry, "x"))
			assert.Equal(t, tt.want, tf.read(t))
		})
	}

	tf := newTestFiles(t)
	tf.write(t, "overrides: {geth: {image: a}}\n")
	require.ErrorContains(t, tf.set("geth", entry, "x"), "indented block")
}

func TestSetEntry(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, "overrides:\n  geth:\n    image: a/geth:old\n    by: me\n    until: 2026-11-20T15:00:00Z\n"+
		"  beacon:\n    image: a/beacon:old\n    by: me\n")

	old := &plan.Entry{Image: "a/beacon:old", By: "me"}
	fix := &plan.Entry{Image: "a/beacon:fix", By: "sam", Reason: "r"}

	// An expired entry counts as none; clearing nothing writes nothing.
	before := tf.read(t)
	prev, next, err := tf.SetEntry("geth", "sam", func(p *plan.Entry) (*plan.Entry, error) {
		assert.Nil(t, p)

		return nil, nil //nolint:nilnil // nil removes the entry.
	})
	require.NoError(t, err)
	assert.Nil(t, prev)
	assert.Nil(t, next)
	assert.Equal(t, before, tf.read(t))

	// A refusal of build: prev, no next, nothing written.
	refused := plan.Refusal("no")
	prev, next, err = tf.SetEntry("beacon", "sam", func(*plan.Entry) (*plan.Entry, error) { return nil, refused })
	require.ErrorIs(t, err, refused)
	assert.Equal(t, old, prev)
	assert.Nil(t, next)
	assert.Equal(t, before, tf.read(t))

	// A write that fails its read-back check: prev and next.
	bad := &plan.Entry{Image: " a/beacon:fix ", By: "sam"}
	prev, next, err = tf.SetEntry("beacon", "sam", func(*plan.Entry) (*plan.Entry, error) { return bad, nil })
	require.ErrorContains(t, err, "reads back differently")
	assert.Equal(t, old, prev)
	assert.Equal(t, bad, next)
	assert.Equal(t, before, tf.read(t))

	prev, next, err = tf.SetEntry("beacon", "sam", func(*plan.Entry) (*plan.Entry, error) { return fix, nil })
	require.NoError(t, err)
	assert.Equal(t, old, prev)
	assert.Equal(t, fix, next)

	// A file that cannot be parsed: no refusal, build not called.
	tf.write(t, "overrides: [broken\n")

	prev, next, err = tf.SetEntry("beacon", "sam", func(*plan.Entry) (*plan.Entry, error) {
		t.Fatal("build called")

		return nil, nil //nolint:nilnil // nil removes the entry.
	})
	require.ErrorContains(t, err, "the overrides file cannot be read: ")
	assert.NotErrorIs(t, err, plan.ErrRefused)
	assert.Nil(t, prev)
	assert.Nil(t, next)
}

// Text is cut to one line before it is written, and the entry is parsed back.
func TestTextIsOneLine(t *testing.T) {
	tf := newTestFiles(t)
	entry := &plan.Entry{Image: "a/b:c", By: "sam\nhold:", Reason: "why\n  local: {}"}
	require.NoError(t, tf.set("geth", entry, "sam\noverrides: x"))

	files := tf.Load()
	require.True(t, files.Known)
	assert.Equal(t, "sam", files.Entries["geth"].By)
	assert.Equal(t, "why", files.Entries["geth"].Reason)
	assert.Empty(t, files.Holds)

	hold := &plan.Hold{By: "c\nx", Reason: "r\ny", Teams: []string{"t\nz"}, Feature: "f\nq"}
	require.NoError(t, tf.SetHold(plan.FromController, hold, "controller:c\nx", "set: r\ny"))

	files = tf.Load()
	assert.Equal(t, plan.Hold{By: "c", Reason: "r", Teams: []string{"t"}, Feature: "f"},
		files.Holds[plan.HoldController])
	assert.Equal(t, 2, strings.Count(tf.read(t), "# ["), "one history line per change")
}

func TestParse(t *testing.T) {
	block := blockText(map[string]plan.Deployed{
		"geth":   {Image: "ethpandaops/geth:master", Command: []string{"--http", "--a=b c"}, HasCommand: true},
		"beacon": {Image: "sigp/lighthouse:latest", Command: []string{}, HasCommand: true},
	})

	for name, content := range map[string]string{
		"empty":                "",
		"the generated block":  block,
		"the key alone":        block + "\noverrides:\n",
		"an empty mapping":     "overrides: {}\n",
		"only comments in it":  "overrides:\n  # [geth] 2026-11-20T16:00:00Z sam: cleared\n",
		"an entry without any": "overrides:\n  geth:\n",
	} {
		t.Run(name, func(t *testing.T) {
			files, err := parse([]byte(content))
			require.NoError(t, err)
			assert.True(t, files.Known)
			assert.NotContains(t, files.Entries, "geth")
		})
	}

	files, err := parse([]byte(block + `
overrides:
  geth:
    image: ethpandaops/geth:fix
    flags:
      add: ["--verbosity=4", --pprof]
      remove:
        - --maxpeers
    by: sam
    reason: "sync: stuck"
    until: 2026-11-21T16:00:00Z
  beacon:
    image: sigp/lighthouse:unstable
`))
	require.NoError(t, err)

	assert.Equal(t, plan.Deployed{
		Image: "ethpandaops/geth:master", Command: []string{"--http", "--a=b c"}, HasCommand: true,
	}, files.Deployed["geth"])
	assert.Equal(t, plan.Deployed{Image: "sigp/lighthouse:latest", Command: []string{}, HasCommand: true},
		files.Deployed["beacon"])

	// An entry without a command keeps the container's.
	own, err := parse([]byte("deployed:\n  geth:\n    image: \"a/geth:1\"\n"))
	require.NoError(t, err)
	assert.Equal(t, plan.Deployed{Image: "a/geth:1"}, own.Deployed["geth"])

	_, err = parse([]byte("deployed:\n  geth:\n    command: []\n"))
	require.ErrorContains(t, err, "names no image")

	assert.Equal(t, plan.Entry{
		Image: "ethpandaops/geth:fix", Add: []string{"--verbosity=4", "--pprof"},
		Remove: []string{"--maxpeers"}, By: "sam", Reason: "sync: stuck", Until: untilOf(24 * time.Hour),
	}, files.Entries["geth"])
	assert.Nil(t, files.Entries["beacon"].Until, "an end is optional by hand")

	_, err = parse([]byte("overrides: [geth\n"))
	require.Error(t, err)
}

// Unknown fields in the overrides file are errors.
func TestParseIsStrict(t *testing.T) {
	_, err := parse([]byte("overrides:\n  geth:\n    flag: [a]\n"))
	require.ErrorContains(t, err, "field flag not found")

	_, err = parse([]byte("overides:\n  geth:\n    image: a/b:c\n"))
	require.ErrorContains(t, err, "field overides not found")

	_, err = parse([]byte("hold:\n  somebody: {reason: x}\n"))
	require.ErrorContains(t, err, "unknown hold")

	_, err = parse([]byte("overrides:\n  beacon:\n    image: x/y:z\n---\noverrides:\n  geth:\n    image: a/b:c\n"))
	require.EqualError(t, err, "the overrides file holds more than one YAML document")
}

func TestLoad(t *testing.T) {
	tf := newTestFiles(t)

	files := tf.Load()
	assert.True(t, files.Known, "no file is no overrides")

	tf.write(t, "overrides: [geth\n")

	files = tf.Load()
	assert.False(t, files.Known, "a broken file is not no overrides")
	assert.NotContains(t, files.Err, "\n")
	assert.True(t, strings.HasPrefix(files.Err, "yaml: "), files.Err)

	_, _, err := tf.SetEntry("geth", "x", func(*plan.Entry) (*plan.Entry, error) { return nil, plan.Refusal("x") })
	require.EqualError(t, err, "the overrides file cannot be read: "+files.Err)
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "the overrides file cannot be read: " + files.Err},
		plan.HoldState(&files, fileTime))
}

// The overrides file is read only up to 1 MiB.
func TestLoadIsBounded(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, "# "+strings.Repeat("x", maxFileSize)+"\n")

	files := tf.Load()
	assert.False(t, files.Known)
	assert.Contains(t, files.Err, "is larger than")
}

func TestLoadExpiresAndAuditsHandEdits(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, "# mine\noverrides:\n  geth:\n    image: a/geth:fix\n    by: sam\n    until: 2026-11-20T17:00:00Z\n")

	// The first Load takes stock.
	files := tf.Load()
	assert.Contains(t, files.Entries, "geth")
	assert.Empty(t, tf.audit.recs)

	// The agent's own change is no hand edit.
	require.NoError(t, tf.set("beacon", &plan.Entry{Image: "a/beacon:fix", By: "pk", Reason: "r"}, "pk"))
	tf.Load()
	assert.Empty(t, tf.audit.recs)

	// A hand edit is.
	tf.write(t, strings.Replace(tf.read(t), `"a/beacon:fix"`, `"a/beacon:other"`, 1))
	tf.Load()
	require.Len(t, tf.audit.recs, 1)
	assert.Equal(t, Record{
		Action: "override.edit", Source: SourceAgent, User: "file:pk", Container: "beacon",
		Before: "image a/beacon:fix", After: "image a/beacon:other", Reason: "r", Result: Accepted,
	}, tf.audit.recs[0])

	// At its end the entry is gone and taken out of the file.
	tf.now = fileTime.Add(time.Hour)
	files = tf.Load()
	assert.NotContains(t, files.Entries, "geth")
	assert.NotContains(t, tf.read(t), "a/geth:fix\n")
	assert.Contains(t, tf.read(t), "# mine\noverrides:\n  # [geth] 2026-11-20T17:00:00Z upgradoor: expired; was image a/geth:fix by sam\n")
	require.Len(t, tf.audit.recs, 2)
	assert.Equal(t, Record{
		Action: "override.expire", Source: SourceAgent, Container: "geth", User: "sam",
		Before: "image a/geth:fix", After: plan.NoOverride, Result: Accepted,
	}, tf.audit.recs[1])

	tf.Load()
	assert.Len(t, tf.audit.recs, 2, "the expiry is no hand edit")

	// A removed entry names the user of the entry that was there.
	tf.write(t, "overrides:\n")
	tf.Load()
	require.Len(t, tf.audit.recs, 3)
	assert.Equal(t, "file:pk", tf.audit.recs[2].User)
	assert.Equal(t, plan.NoOverride, tf.audit.recs[2].After)
}

func TestHolds(t *testing.T) {
	tf := newTestFiles(t)
	hold := func() protocol.Hold {
		files := tf.Load()

		return plan.HoldState(&files, tf.now)
	}

	assert.Equal(t, protocol.Hold{}, hold(), "no file is no hold")

	sam := plan.ControllerHold("maintenance\nwindow",
		&protocol.Actor{User: "sam@example.org", Teams: []string{"lighthouse"}, Feature: "team_hold"})
	require.NoError(t, tf.SetHold(plan.FromController, &sam, "controller:"+sam.By, "set: "+sam.Reason))
	assert.Equal(t, protocol.Hold{Controller: true}, hold())

	raw := tf.read(t)
	assert.Contains(t, raw, "hold:\n  # [hold:controller] 2026-11-20T16:00:00Z controller:sam@example.org: set: maintenance\n"+
		"  controller:\n    by: \"sam@example.org\"\n    reason: \"maintenance\"\n    teams: [\"lighthouse\"]\n"+
		"    feature: \"team_hold\"\n")

	// The local hold, by hand, next to it.
	tf.write(t, raw+"  local:\n    by: pk\n    reason: debugging teku\n")
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku", Controller: true}, hold())

	// Releasing the controller hold leaves the local one alone.
	require.NoError(t, tf.SetHold(plan.FromController, nil, "controller", "released"))
	require.NoError(t, tf.SetHold(plan.FromController, nil, "controller", "released"), "releasing twice is fine")
	assert.Equal(t, protocol.Hold{Local: true, LocalReason: "debugging teku"}, hold())

	// A local hold whose end has passed holds no more.
	require.NoError(t, tf.SetHold(plan.FromLocal, &plan.Hold{By: "pk", Reason: "until noon", Until: untilOf(-4 * time.Hour)},
		"pk", "set: until noon"))
	assert.False(t, hold().Active())

	require.NoError(t, tf.SetHold(plan.FromLocal, nil, "local:pk", "released"))
	assert.NotContains(t, tf.Load().Holds, plan.HoldLocal)

	// A file that cannot be read holds, and is not written.
	tf.write(t, "hold: [broken\n")
	assert.True(t, hold().Local)
	require.ErrorContains(t, tf.SetHold(plan.FromLocal, nil, "pk", "released"), "cannot be read")
}

func TestInitBlock(t *testing.T) {
	tf := newTestFiles(t)
	old := blockText(map[string]plan.Deployed{"geth": {Image: "a/geth:1"}})
	rest := "\noverrides:\n  geth:\n    image: a/geth:2\n"
	tf.write(t, old+rest)

	deployed := map[string]plan.Deployed{
		"geth":   {Image: "a/geth:3", Command: []string{"--x"}, HasCommand: true},
		"beacon": {Image: "a/beacon:1", Command: nil, HasCommand: true},
	}
	require.NoError(t, tf.InitBlock(deployed))
	assert.Equal(t, blockText(deployed)+rest, tf.read(t))
	assert.Contains(t, tf.read(t), "deployed:\n  \"beacon\":\n    image: \"a/beacon:1\"\n    command: []\n"+
		"  \"geth\":\n    image: \"a/geth:3\"\n    command: [\"--x\"]\n# <<<")
	assert.Contains(t, tf.read(t), "#   upgradoor override clear <container>\n")
	assert.NotContains(t, tf.read(t), "upgradoor host")

	files := tf.Load()
	assert.Equal(t, deployed["geth"], files.Deployed["geth"])

	tf.write(t, "overrides:\n")
	require.NoError(t, tf.InitBlock(nil))
	assert.Equal(t, blockText(nil)+"\noverrides:\n", tf.read(t))
}

// Files in the entry user's home are replaced by rename, owner and mode kept.
func TestFileIsReplacedWithItsMode(t *testing.T) {
	tf := newTestFiles(t)
	entry := &plan.Entry{Image: "a/b:c", By: "x", Reason: "y"}

	require.NoError(t, tf.set("geth", entry, "x"))

	info, err := os.Stat(tf.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(newFileMode), info.Mode().Perm(), "a new file is for the entry user to edit")

	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, os.Geteuid(), int(st.Uid))

	require.NoError(t, os.Chmod(tf.path, 0o640))
	require.NoError(t, tf.set("geth", nil, "x"))

	info, err = os.Stat(tf.path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())

	entries, err := os.ReadDir(filepath.Dir(tf.path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temp file is left")
}

// A file with a second name is neither read nor written.
func TestFileWithTwoNamesIsNotRead(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, "overrides:\n")

	other := filepath.Join(filepath.Dir(tf.path), "other")
	require.NoError(t, os.Link(tf.path, other))

	files := tf.Load()
	assert.False(t, files.Known)
	assert.Equal(t, "the overrides file has more than one name", files.Err)
	require.ErrorContains(t, tf.set("geth", &plan.Entry{Image: "a/b:c"}, "x"), "more than one name")
	assert.Equal(t, "overrides:\n", tf.read(t))
}

// A root agent does not read the file when another user can change an ancestor directory.
func TestFileInAReplaceableDirectoryIsNotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("ownership and modes mean nothing to root")
	}

	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0o700))

	if why := replaceableAncestor(filepath.Join(root, "x", "y"), 0, []int{0, os.Geteuid()}); why != "" {
		t.Skipf("the temp directory's own ancestors are replaceable (%s): set TMPDIR to a safe directory", why)
	}

	open := filepath.Join(root, "open")
	require.NoError(t, os.MkdirAll(filepath.Join(open, "home"), 0o700))
	require.NoError(t, os.Chmod(open, 0o770))

	path := filepath.Join(open, "home", "overrides.yaml")
	require.NoError(t, os.WriteFile(path, []byte("overrides:\n"), 0o600))

	me := os.Geteuid()

	assert.Empty(t, replaceableAncestor(path, me+1, []int{0}), "only a root agent checks")

	if me != 0 {
		assert.True(t, NewFiles(quiet(), path, &recorder{}, time.Now).Load().Known)
	}

	assert.Equal(t, fmt.Sprintf("the overrides file's directory can be replaced: %s is owned by uid %d", open, me),
		replaceableAncestor(path, 0, []int{0}))

	unsafe := replaceableAncestor(path, 0, []int{0, me})
	assert.Equal(t, "the overrides file's directory can be replaced: "+open+" is writable by its group or others",
		unsafe)

	f, ok := NewFiles(quiet(), path, &recorder{}, time.Now).(*files)
	require.True(t, ok)

	f.unsafe = unsafe
	got := f.Load()
	assert.False(t, got.Known)
	assert.Equal(t, unsafe, got.Err)

	_, _, err := f.SetEntry("geth", "x", func(*plan.Entry) (*plan.Entry, error) {
		return &plan.Entry{Image: "a/b:c"}, nil
	})
	require.ErrorContains(t, err, "can be replaced")
	require.ErrorContains(t, f.InitBlock(nil), "can be replaced")

	// A sticky directory is fine.
	require.NoError(t, os.Chmod(open, os.ModeSticky|0o777))
	assert.Empty(t, replaceableAncestor(path, 0, []int{0, me}))
}

func TestNewFilesRemovesTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overrides.yaml")
	require.NoError(t, os.WriteFile(path+".tmp-123", []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.tmp-1"), []byte("x"), 0o600))

	NewFiles(quiet(), path, &recorder{}, time.Now)

	assert.NoFileExists(t, path+".tmp-123")
	assert.FileExists(t, filepath.Join(dir, "other.tmp-1"))
}

// Text from the controller or the socket cannot add a hold or another entry.
func TestTextCannotBreakOut(t *testing.T) {
	for name, sep := range map[string]string{"newline": "\n", "line separator": " ", "paragraph separator": " "} {
		t.Run(name, func(t *testing.T) {
			tf := newTestFiles(t)
			tf.write(t, "overrides:\n  geth:\n    image: a/b:c\n")

			h := plan.ControllerHold("maintenance"+sep+"  local:"+sep+"    by: \"evil\""+sep+
				"    reason: \"injected\" #", nil)
			require.NoError(t, tf.SetHold(plan.FromController, &h, "controller:"+h.By, "set: "+h.Reason))

			who := "sam" + sep + "  beacon:" + sep + "    image: \"evil/img:tag\"" + sep + "    by: x #"
			require.NoError(t, tf.set("geth", &plan.Entry{Image: "a/b:d", By: who, Reason: "y"}, who))

			who = "sam" + sep + "hold:" + sep + "  local:" + sep + "    by: evil" + sep + "    reason: injected #"
			require.NoError(t, tf.set("geth", nil, who))

			_, _, err := tf.SetEntry("geth"+sep+"hold:"+sep+"  local: {}", "sam", func(*plan.Entry) (*plan.Entry, error) {
				return &plan.Entry{Image: "a/b:c", By: "x"}, nil
			})
			require.ErrorIs(t, err, plan.ErrRefused, "not a container name")

			files := tf.Load()
			require.True(t, files.Known, files.Err)
			assert.Equal(t, []string{plan.HoldController}, slices.Collect(maps.Keys(files.Holds)))
			assert.Empty(t, files.Entries)
		})
	}
}

// A change that would alter any other part of the document is not written.
func TestChangeOfOtherPartsIsRefused(t *testing.T) {
	doc := "deployed:\n  geth:\n    image: a/geth:1\n" +
		"overrides:\n  geth:\n    image: a/b:c\nhold:\n  controller:\n    by: c\n"
	read := func(content string) plan.Overrides {
		t.Helper()

		out, err := parse([]byte(content))
		require.NoError(t, err)

		return out
	}
	before := read(doc)
	named := []string{"geth"}

	withHold := read(doc + "  local:\n    by: evil\n")
	require.ErrorContains(t, unchanged(&before, &withHold, named, nil), "would alter other parts")

	withEntry := read(doc)
	withEntry.Entries["beacon"] = plan.Entry{Image: "evil/img:tag"}
	require.ErrorContains(t, unchanged(&before, &withEntry, named, nil), "would alter other parts")

	withDeployed := read(doc)
	withDeployed.Deployed["geth"] = plan.Deployed{Image: "evil/img:tag"}
	require.ErrorContains(t, unchanged(&before, &withDeployed, named, nil), "would alter other parts")

	changed := read(doc)
	changed.Entries["geth"] = plan.Entry{Image: "a/b:d"}
	require.NoError(t, unchanged(&before, &changed, named, nil), "the named entry may change")

	// The generated block must read back as asked.
	tf := newTestFiles(t)
	require.ErrorContains(t, tf.InitBlock(map[string]plan.Deployed{"geth": {Image: " a/geth:1"}}),
		"would alter other parts")
	assert.NoFileExists(t, tf.path)
}

// An expired entry that cannot be taken out is recorded once, and is no hand edit.
func TestExpiryThatFailsIsRecordedOnce(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, "overrides:\n  geth: &b\n    image: x/y:z\n    until: 2026-11-20T15:00:00Z\n"+
		"  beacon:\n    <<: *b\n    until: 2030-01-01T00:00:00Z\n")

	for range 4 {
		files := tf.Load()
		assert.NotContains(t, files.Entries, "geth")
	}

	require.Len(t, tf.audit.recs, 1)
	assert.Equal(t, "override.expire", tf.audit.recs[0].Action)
	assert.Contains(t, tf.audit.recs[0].Error, "the entry could not be taken out of the file")
}

// A symbolic link or another non-regular file is never read or written through.
func TestFileIsNeverReadThroughALink(t *testing.T) {
	tf := newTestFiles(t)
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("root:$6$hash\n"), 0o600))
	require.NoError(t, os.Symlink(secret, tf.path))

	files := tf.Load()
	assert.False(t, files.Known)
	assert.Equal(t, "the overrides file is a symbolic link", files.Err)

	_, _, err := tf.SetEntry("geth", "x", func(*plan.Entry) (*plan.Entry, error) {
		return &plan.Entry{Image: "a/b:c"}, nil
	})
	require.ErrorContains(t, err, "symbolic link")

	raw, err := os.ReadFile(secret)
	require.NoError(t, err)
	assert.Equal(t, "root:$6$hash\n", string(raw))

	fifo := newTestFiles(t)
	require.NoError(t, syscall.Mkfifo(fifo.path, 0o600))

	files = fifo.Load()
	assert.False(t, files.Known, "a FIFO does not block the loop")
	assert.Equal(t, "the overrides file is not a regular file", files.Err)

	dir := newTestFiles(t)
	require.NoError(t, os.Mkdir(dir.path, 0o700))
	assert.Equal(t, "the overrides file is not a regular file", dir.Load().Err)
}

func TestOneLine(t *testing.T) {
	assert.Equal(t, "yaml: unmarshal errors:; line 2: field flag not found",
		oneLine("yaml: unmarshal errors:\n  line 2: field flag not found"))
	assert.Equal(t, "x", oneLine(errors.New("x").Error()))
}
