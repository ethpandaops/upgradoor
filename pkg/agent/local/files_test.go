package local

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

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

// A change replaces the fields of one entry and no other byte.
func TestSetEntryKeepsEveryOtherByte(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, `# my notes
overrides:

  # keep an eye on this one
  beacon:
      image: sigp/lighthouse:unstable   # indented my way
      reason: testing

      by: me
  geth:
    # default image: ethpandaops/geth:master
    # mine
    image: ethpandaops/geth:old
    by: me

# the end
`)

	entry := &plan.Entry{
		Image: "ethpandaops/geth:fix", Command: []string{"--a=b", "--c \"d\"", "x: y"}, HasCommand: true, By: "sam@example.org",
		Reason: "why: because", Until: untilOf(time.Hour),
	}
	require.NoError(t, tf.set("geth", entry, "sam@example.org"))

	assert.Equal(t, `# my notes
overrides:

  # keep an eye on this one
  beacon:
      image: sigp/lighthouse:unstable   # indented my way
      reason: testing

      by: me
  # [geth] 2026-11-20T16:00:00Z sam@example.org: set image ethpandaops/geth:fix, command --a=b --c "d" x: y; was image ethpandaops/geth:old
  geth:
    # default image: ethpandaops/geth:master
    # mine
    image: ethpandaops/geth:fix
    command:
      - --a=b
      - --c "d"
      - "x: y"
    by: sam@example.org
    reason: "why: because"
    until: 2026-11-20T17:00:00Z

# the end
`, tf.read(t))

	files := tf.Load()
	assert.Equal(t, entry.Command, files.Entries["geth"].Command)

	// Clearing it puts the commented structure back and keeps the rest.
	tf.now = fileTime.Add(30 * time.Minute)
	require.NoError(t, tf.set("geth", nil, "upgradoor"))

	files = tf.Load()
	assert.NotContains(t, files.Entries, "geth")
	assert.Equal(t, "sigp/lighthouse:unstable", files.Entries["beacon"].Image)
	assert.Contains(t, tf.read(t), "x: y; was image ethpandaops/geth:old\n"+
		"  # [geth] 2026-11-20T16:30:00Z upgradoor: cleared; was image ethpandaops/geth:fix, command --a=b --c \"d\" x: y\n"+
		"  geth:\n    # default image: ethpandaops/geth:master\n    # mine\n"+
		"    # image:\n    # command:\n    # reason:\n    # until:\n\n# the end\n")

	// Only the newest history lines are kept.
	for i := range 2 * maxHistory {
		require.NoError(t, tf.set("geth", &plan.Entry{Image: "a/b:" + string(rune('a'+i)), By: "s", Reason: "r"}, "sam"))
	}

	assert.Equal(t, maxHistory, strings.Count(tf.read(t), "# [geth] "))
	assert.NotContains(t, tf.read(t), "cleared")
	assert.Contains(t, tf.read(t), "# keep an eye on this one\n  beacon:\n")
	assert.Equal(t, 1, strings.Count(tf.read(t), "image: a/b:"))
}

func TestSetEntryWhereThereIsNothing(t *testing.T) {
	entry := &plan.Entry{Image: "a/b:c", By: "x", Reason: "y"}
	added := "  # [geth] 2026-11-20T16:00:00Z x: set image a/b:c; was nothing\n" +
		"  geth:\n    image: a/b:c\n    by: x\n    reason: y\n"

	for name, tt := range map[string]struct{ before, want string }{
		"no file":          {"", "overrides:\n" + added},
		"an empty mapping": {"overrides: {}\n", "overrides:\n" + added},
		"an empty entry":   {"overrides:\n  geth: {}\n", "overrides:\n" + added},
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

	tf.write(t, "overrides:\n  geth: {image: a}\n")
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
	assert.Equal(t, 1, strings.Count(tf.read(t), "# ["), "one history line per change")
}

func TestParse(t *testing.T) {
	for name, content := range map[string]string{
		"empty":                "",
		"the key alone":        "overrides:\n",
		"an empty mapping":     "overrides: {}\n",
		"only comments in it":  "overrides:\n  # [geth] 2026-11-20T16:00:00Z sam: cleared\n",
		"an entry without any": "overrides:\n  geth:\n",
		"the structure":        "overrides:\n  geth:\n    # default image: a/b:c\n    # image:\n    # command:\n",
		"fields without value": "overrides:\n  geth:\n    image:\n    command:\n",
	} {
		t.Run(name, func(t *testing.T) {
			files, err := parse([]byte(content))
			require.NoError(t, err)
			assert.True(t, files.Known)
			assert.NotContains(t, files.Entries, "geth")
		})
	}

	files, err := parse([]byte(`
overrides:
  geth:
    image: ethpandaops/geth:fix
    command:
      - geth
      - "--verbosity=4"
      - --pprof
    by: sam
    reason: "sync: stuck"
    until: 2026-11-21T16:00:00Z
  beacon:
    image: sigp/lighthouse:unstable
  teku:
    command: []
`))
	require.NoError(t, err)

	assert.Equal(t, plan.Entry{
		Image: "ethpandaops/geth:fix", Command: []string{"geth", "--verbosity=4", "--pprof"}, HasCommand: true,
		By: "sam", Reason: "sync: stuck", Until: untilOf(24 * time.Hour),
	}, files.Entries["geth"])
	assert.Nil(t, files.Entries["beacon"].Until, "an end is optional by hand")
	assert.False(t, files.Entries["beacon"].HasCommand, "an image alone keeps the default command")
	assert.Equal(t, plan.Entry{Command: []string{}, HasCommand: true}, files.Entries["teku"])

	_, err = parse([]byte("overrides: [geth\n"))
	require.Error(t, err)
}

// Unknown fields in the overrides file are errors; the old layout is refused.
func TestParseIsStrict(t *testing.T) {
	_, err := parse([]byte("overrides:\n  geth:\n    flag: [a]\n"))
	require.ErrorContains(t, err, "field flag not found")

	_, err = parse([]byte("overides:\n  geth:\n    image: a/b:c\n"))
	require.ErrorContains(t, err, "field overides not found")

	_, err = parse([]byte("overrides:\n  beacon:\n    image: x/y:z\n---\noverrides:\n  geth:\n    image: a/b:c\n"))
	require.EqualError(t, err, "the overrides file holds more than one YAML document")

	for name, content := range map[string]string{
		"flags":    "overrides:\n  geth:\n    flags:\n      add: [--a]\n",
		"deployed": "deployed:\n  geth:\n    image: a/geth:1\n",
		"hold":     "overrides:\nhold: {}\n",
		"block": "overrides:\n\n# >>> upgradoor generated block: written by Ansible through upgradoor deploy, do not edit >>>\n" +
			"# <<< upgradoor generated block <<<\n",
	} {
		_, err := parse([]byte(content))
		require.ErrorIs(t, err, errOldFormat, name)
	}
}

func TestLoad(t *testing.T) {
	tf := newTestFiles(t)

	files := tf.Load()
	assert.True(t, files.Known, "no file is no overrides")
	assert.False(t, files.Disabled)

	tf.write(t, "overrides: [geth\n")

	files = tf.Load()
	assert.False(t, files.Known, "a broken file is not no overrides")
	assert.NotContains(t, files.Err, "\n")
	assert.True(t, strings.HasPrefix(files.Err, "the overrides file cannot be read: yaml: "), files.Err)

	_, _, err := tf.SetEntry("geth", "x", func(*plan.Entry) (*plan.Entry, error) { return nil, plan.Refusal("x") })
	require.EqualError(t, err, files.Err)
	assert.Equal(t, protocol.Hold{
		Local: true, LocalReason: files.Err,
		Holds: []protocol.HostHold{{Holder: protocol.HolderHost, Reason: files.Err}},
	}, plan.HoldState(&files, fileTime))

	tf.write(t, "deployed: {}\n")
	assert.Equal(t, "the overrides file cannot be read: "+errOldFormat.Error(), tf.Load().Err)

	_, err = tf.Layout(map[string]plan.Deployed{"geth": {Image: "a/geth:1"}}, nil)
	require.ErrorContains(t, err, "old format")
	assert.Equal(t, "deployed: {}\n", tf.read(t), "an old file is left as it is")
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
	tf.write(t, strings.Replace(tf.read(t), `image: a/beacon:fix`, `image: a/beacon:other`, 1))
	tf.Load()
	require.Len(t, tf.audit.recs, 1)
	assert.Equal(t, Record{
		Action: "override.edit", Source: SourceAgent, User: "file:pk", Container: "beacon",
		Before: "image a/beacon:fix", After: "image a/beacon:other", Reason: "r", Result: Accepted,
	}, tf.audit.recs[0])

	// At its end the entry's fields are taken out of the file.
	tf.now = fileTime.Add(time.Hour)
	files = tf.Load()
	assert.NotContains(t, files.Entries, "geth")
	assert.Contains(t, tf.read(t), "# mine\noverrides:\n"+
		"  # [geth] 2026-11-20T17:00:00Z upgradoor: expired; was image a/geth:fix by sam\n"+
		"  geth:\n    # image:\n    # command:\n    # reason:\n    # until:\n")
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

// header is the first lines of a file the agent laid out.
const header = headerLine + "\n" + commandsLine + "\n\n"

var (
	beaconDefault = plan.Deployed{
		Image: "sigp/lighthouse:latest", Command: []string{"lighthouse", "beacon_node"}, HasCommand: true,
	}
	gethDefault = plan.Deployed{Image: "ethereum/client-go:stable", Command: []string{"--datadir=/data"}, HasCommand: true}
)

// emptyEntry is a laid-out entry without an override.
const emptyEntry = "    # image:\n    # command:\n    # reason:\n    # until:\n"

// A missing file is created with an entry per container; an override fills
// one, a clear puts the structure back.
func TestLayout(t *testing.T) {
	tf := newTestFiles(t)
	both := map[string]plan.Deployed{"beacon": beaconDefault, "execution": gethDefault}

	changed, err := tf.Layout(both, []string{"dora"})
	require.NoError(t, err)
	assert.True(t, changed)

	beacon := "  beacon:\n    # default image: sigp/lighthouse:latest\n    # default command:\n" +
		"    #   - lighthouse\n    #   - beacon_node\n" + emptyEntry
	execution := "  execution:\n    # default image: ethereum/client-go:stable\n    # default command:\n" +
		"    #   - --datadir=/data\n"
	assert.Equal(t, header+"overrides:\n"+beacon+execution+emptyEntry, tf.read(t))
	assert.Empty(t, tf.Load().Entries)

	require.NoError(t, tf.set("execution", &plan.Entry{
		Image: "ethpandaops/geth:fix", Command: []string{"--datadir=/data", "--verbosity=5"}, HasCommand: true,
		By: "lh-dev", Reason: "debug", Until: untilOf(44 * time.Hour),
	}, "lh-dev"))

	set := "  # [execution] 2026-11-20T16:00:00Z lh-dev: set image ethpandaops/geth:fix, " +
		"command --datadir=/data --verbosity=5; was nothing\n"
	assert.Equal(t, header+"overrides:\n"+beacon+set+execution+`    image: ethpandaops/geth:fix
    command:
      - --datadir=/data
      - --verbosity=5
    by: lh-dev
    reason: debug
    until: 2026-11-22T12:00:00Z
`, tf.read(t))

	// Once more: nothing moves.
	changed, err = tf.Layout(both, nil)
	require.NoError(t, err)
	assert.False(t, changed)

	// Clearing it puts the structure back under the default lines.
	require.NoError(t, tf.set("execution", nil, "lh-dev"))
	assert.Equal(t, header+"overrides:\n"+beacon+set+
		"  # [execution] 2026-11-20T16:00:00Z lh-dev: cleared; was image ethpandaops/geth:fix, "+
		"command --datadir=/data --verbosity=5\n"+execution+emptyEntry, tf.read(t))
	assert.Empty(t, tf.Load().Entries)
}

// The default lines follow the default; active fields and every other
// comment stay. A container no longer overridable loses its default lines,
// a new one gets an entry.
// A change written between the agent's read and its rename is not lost: the
// write is dropped and Layout tries again on its next call.
func TestWriteKeepsAChangeMadeMeanwhile(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, "overrides:\n")

	f, ok := tf.Files.(*files)
	require.True(t, ok)

	cur, err := f.read()
	require.NoError(t, err)

	mine := "overrides:\n  execution:\n    image: ethpandaops/geth:fix\n"
	tf.write(t, mine)

	cur.text = newText([]byte("overrides:\n  beacon:\n"))
	require.ErrorIs(t, f.write(cur), errChangedMeanwhile)
	assert.Equal(t, mine, tf.read(t))

	changed, err := tf.Layout(map[string]plan.Deployed{"execution": gethDefault}, nil)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Contains(t, tf.read(t), "image: ethpandaops/geth:fix")
}

func TestLayoutRefreshesDefaults(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, `# upgradoor overrides: edit below, the agent applies changes within a minute.
# my own notes
overrides:
  beacon:
    # default image: sigp/lighthouse:v1
    # keep this
    image: sigp/lighthouse:fix
    by: me
  # old notes
  dora:
    # default image: ethpandaops/dora:1
    # default command: []
    image: ethpandaops/dora:mine
`)

	changed, err := tf.Layout(map[string]plan.Deployed{"beacon": beaconDefault, "execution": gethDefault}, []string{"dora"})
	require.NoError(t, err)
	assert.True(t, changed)

	assert.Equal(t, headerLine+"\n"+commandsLine+`
# my own notes
overrides:
  beacon:
    # default image: sigp/lighthouse:latest
    # default command:
    #   - lighthouse
    #   - beacon_node
    # keep this
    image: sigp/lighthouse:fix
    by: me
  # old notes
  dora:
    image: ethpandaops/dora:mine
  execution:
    # default image: ethereum/client-go:stable
    # default command:
    #   - --datadir=/data
`+emptyEntry, tf.read(t))

	files := tf.Load()
	assert.Equal(t, "sigp/lighthouse:fix", files.Entries["beacon"].Image)
	assert.Equal(t, "ethpandaops/dora:mine", files.Entries["dora"].Image)

	// A container without a command shows only its image; an entry without
	// default lines gets them right below its name.
	tf.write(t, header+"overrides:\n  beacon:\n    # mine\n    image: a/b:c\n")
	_, err = tf.Layout(map[string]plan.Deployed{"beacon": {Image: "a/b:1"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, header+"overrides:\n  beacon:\n    # default image: a/b:1\n    # mine\n    image: a/b:c\n", tf.read(t))

	_, err = tf.Layout(map[string]plan.Deployed{"beacon": {Image: "a/b:1", Command: []string{}, HasCommand: true}}, nil)
	require.NoError(t, err)
	assert.Contains(t, tf.read(t), "    # default image: a/b:1\n    # default command: []\n    # mine\n")
}

func TestDisabled(t *testing.T) {
	f := NewFiles(quiet(), "", &recorder{}, time.Now)

	assert.Empty(t, f.Path())

	files := f.Load()
	assert.True(t, files.Known)
	assert.True(t, files.Disabled)

	_, _, err := f.SetEntry("geth", "x", func(*plan.Entry) (*plan.Entry, error) {
		t.Fatal("build called")

		return nil, nil //nolint:nilnil // never called.
	})
	require.EqualError(t, err, "refused: overrides are disabled on this host")
	require.ErrorIs(t, err, plan.ErrRefused)

	_, err = f.Layout(map[string]plan.Deployed{"geth": {Image: "a/b:c"}}, nil)
	require.ErrorIs(t, err, plan.ErrRefused)
}

func TestScalar(t *testing.T) {
	for in, want := range map[string]string{
		"--datadir=/data":    "--datadir=/data",
		"sigp/lighthouse:v1": "sigp/lighthouse:v1",
		"lighthouse":         "lighthouse",
		"":                   `""`,
		"true":               `"true"`,
		"8545":               `"8545"`,
		"a: b":               `"a: b"`,
		"a #b":               `"a #b"`,
		"- x":                `"- x"`,
		" x":                 `" x"`,
		"*x":                 `"*x"`,
		"--name value":       "--name value",
		"--graffiti=\"x\"":   "--graffiti=\"x\"",
		"'quoted'":           `"'quoted'"`,
		"{a}":                `"{a}"`,
		"x\ny":               `"x\ny"`,
	} {
		assert.Equal(t, want, scalar(in), in)
		assert.Equal(t, in, yamlString(t, scalar(in)), in)
	}
}

// yamlString reads a scalar as the file would.
func yamlString(t *testing.T, s string) string {
	t.Helper()

	var out []string
	require.NoError(t, yaml.Unmarshal([]byte("- "+s), &out))
	require.Len(t, out, 1)

	return out[0]
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

// An agent that is not root cannot keep another owner: the group can write.
func TestWriteTempOfAnotherOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root keeps the owner")
	}

	tmp, err := os.CreateTemp(t.TempDir(), "overrides")
	require.NoError(t, err)

	defer func() { _ = tmp.Close() }()

	require.NoError(t, writeTemp(tmp, []byte("overrides:\n"), 0o644, os.Geteuid()+1, os.Getegid(), true))

	info, err := tmp.Stat()
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o664), info.Mode().Perm())
}

// A file with a second name is neither read nor written.
func TestFileWithTwoNamesIsNotRead(t *testing.T) {
	tf := newTestFiles(t)
	tf.write(t, "overrides:\n")

	other := filepath.Join(filepath.Dir(tf.path), "other")
	require.NoError(t, os.Link(tf.path, other))

	files := tf.Load()
	assert.False(t, files.Known)
	assert.Equal(t, "the overrides file cannot be read: the overrides file has more than one name", files.Err)
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
	assert.Equal(t, "the overrides file cannot be read: "+unsafe, got.Err)

	_, _, err := f.SetEntry("geth", "x", func(*plan.Entry) (*plan.Entry, error) {
		return &plan.Entry{Image: "a/b:c"}, nil
	})
	require.ErrorContains(t, err, "can be replaced")
	_, err = f.Layout(nil, nil)
	require.ErrorContains(t, err, "can be replaced")

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

// Text from the controller or the socket cannot add another entry or key.
func TestTextCannotBreakOut(t *testing.T) {
	for name, sep := range map[string]string{"newline": "\n", "line separator": "\u2028", "paragraph separator": "\u2029"} {
		t.Run(name, func(t *testing.T) {
			tf := newTestFiles(t)
			tf.write(t, "overrides:\n  geth:\n    image: a/b:c\n")

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
			assert.Empty(t, files.Entries)
		})
	}
}

// A change that would alter any other entry is not written.
func TestChangeOfOtherPartsIsRefused(t *testing.T) {
	doc := "overrides:\n  geth:\n    image: a/b:c\n  beacon:\n    image: a/beacon:1\n"
	read := func(content string) plan.Overrides {
		t.Helper()

		out, err := parse([]byte(content))
		require.NoError(t, err)

		return out
	}
	before := read(doc)

	withEntry := read(doc + "  teku:\n    image: evil/img:tag\n")
	require.ErrorContains(t, unchanged(&before, &withEntry, "geth"), "would alter other parts")

	other := read(doc)
	other.Entries["beacon"] = plan.Entry{Image: "evil/img:tag"}
	require.ErrorContains(t, unchanged(&before, &other, "geth"), "would alter other parts")

	gone := read(doc)
	delete(gone.Entries, "beacon")
	require.ErrorContains(t, unchanged(&before, &gone, "geth"), "would alter other parts")

	changed := read(doc)
	changed.Entries["geth"] = plan.Entry{Image: "a/b:d"}
	require.NoError(t, unchanged(&before, &changed, "geth"), "the named entry may change")
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
	assert.Equal(t, "the overrides file cannot be read: the overrides file is a symbolic link", files.Err)

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
	assert.Equal(t, "the overrides file cannot be read: the overrides file is not a regular file", files.Err)

	dir := newTestFiles(t)
	require.NoError(t, os.Mkdir(dir.path, 0o700))
	assert.Equal(t, "the overrides file cannot be read: the overrides file is not a regular file", dir.Load().Err)
}

func TestOneLine(t *testing.T) {
	assert.Equal(t, "yaml: unmarshal errors:; line 2: field flag not found",
		oneLine("yaml: unmarshal errors:\n  line 2: field flag not found"))
	assert.Equal(t, "x", oneLine(errors.New("x").Error()))
}
