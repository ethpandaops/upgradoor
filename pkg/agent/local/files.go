package local

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

// Files is the host's overrides file, behind one lock shared by every reader
// and writer. A change replaces one entry or hold and nothing else, adds a
// history comment line above it, is parsed back and compared with what was
// meant, and is written atomically with the file's owner and mode kept,
// because the entry user edits it; an agent that is not root keeps the group
// and lets it write. The generated block is kept byte for byte; only
// InitBlock and SetDeployed render it.
//
// The file is read only as a regular file with one name, not reached through
// a symbolic link and, for root, in a directory whose ancestors only root can
// change.
type Files interface {
	// Path is the overrides file.
	Path() string
	// Load reads the file; one that cannot be read or parsed is Known false
	// with the reason in Err. Expired entries are left out and taken out of
	// the file; changes made without the agent go to the audit log.
	Load() plan.Overrides
	// SetEntry changes one container's override: build gets the current
	// unexpired entry (or nil) and returns the one to write (nil removes it)
	// or a refusal, which writes nothing. prev and next are for the caller's
	// audit record; next is what build returned even when the write failed.
	// An unreadable file is an error that is not a refusal; build is not called.
	SetEntry(
		container, who string, build func(prev *plan.Entry) (*plan.Entry, error),
	) (prev, next *plan.Entry, err error)
	// SetHold writes or (h nil) takes out the hold of origin's source. The
	// source is derived here so that the local hold is written only from the
	// host.
	SetHold(origin plan.Origin, h *plan.Hold, who, note string) error
	// InitBlock writes the generated block and keeps whatever is below it.
	InitBlock(deployed map[string]plan.Deployed) error
	// SetDeployed writes one container's entry of the generated block; reset
	// also takes out its override entry, holds stay. It reports whether the
	// file changed.
	SetDeployed(container string, d plan.Deployed, reset bool, who string) (bool, error)
}

const (
	maxFileSize = 1 << 20
	// newFileMode lets the entry user edit a new file.
	newFileMode = 0o664
	// groupWrite is added when the agent replaces another owner's file.
	groupWrite = 0o060
	// replaceableBits let others replace a non-sticky directory's entries.
	replaceableBits = 0o022
	// tempInfix names the temp file of a write: <file>.tmp-<random>.
	tempInfix = ".tmp-"
	// expiryWho names the agent in an expiry's history line.
	expiryWho = "upgradoor"
	// logContainer is the log field naming a container.
	logContainer = "container"
)

type files struct {
	log   logrus.FieldLogger
	path  string
	audit Audit
	now   func() time.Time
	// unsafe is why the file's directory could be replaced; the file then
	// cannot be read.
	unsafe string

	// seen is the entries as the last Load left them (nil before the first
	// readable one), stuck the expired entries that could not be taken out.
	mu    sync.Mutex
	seen  map[string]plan.Entry
	stuck map[string]bool
	err   string
}

var _ Files = (*files)(nil)

// NewFiles returns the overrides file at path; it checks the directory's
// ancestors and removes temp files of writes cut short.
func NewFiles(log logrus.FieldLogger, path string, audit Audit, now func() time.Time) Files {
	f := &files{
		log:  log.WithFields(logrus.Fields{"component": "overrides", "file": path}),
		path: path, audit: audit, now: now, unsafe: replaceableAncestor(path, os.Geteuid(), []int{0}),
	}

	if f.unsafe == "" {
		f.removeTemps()
	}

	return f
}

// replaceableAncestor says which ancestor of the file's directory someone
// other than trusted could change, or "". Only root is checked: an agent of
// another user has no more rights than that user.
func replaceableAncestor(path string, euid int, trusted []int) string {
	if euid != 0 {
		return ""
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "the overrides file's directory cannot be checked: " + err.Error()
	}

	dir := filepath.Dir(abs)

	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		info, err := os.Stat(d)

		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return "the overrides file's directory cannot be checked: " + err.Error()
		default:
			if uid, _, ok := ownerOf(info); ok && !slices.Contains(trusted, uid) {
				return fmt.Sprintf("the overrides file's directory can be replaced: %s is owned by uid %d", d, uid)
			}

			if info.Mode().Perm()&replaceableBits != 0 && info.Mode()&os.ModeSticky == 0 {
				return "the overrides file's directory can be replaced: " + d + " is writable by its group or others"
			}
		}

		if d == filepath.Dir(d) {
			return ""
		}
	}
}

func (f *files) removeTemps() {
	dir := filepath.Dir(f.path)
	prefix := filepath.Base(f.path) + tempInfix

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !e.Type().IsRegular() {
			continue
		}

		info, err := e.Info()
		if err != nil {
			continue
		}

		if uid, _, ok := ownerOf(info); ok && uid == os.Geteuid() {
			if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
				f.log.WithField("temp", e.Name()).Info("removed the temp file of a write cut short")
			}
		}
	}
}

// Path implements Files.
func (f *files) Path() string { return f.path }

// Load implements Files.
func (f *files) Load() plan.Overrides {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()

	cur, set, err := f.readParsed()
	if err != nil {
		if f.err != err.Error() {
			f.log.Error("cannot read the overrides file (" + err.Error() +
				"); no override is applied and none is taken back until it reads again")
		}

		f.err = err.Error()

		return plan.Overrides{Err: err.Error()}
	}

	if f.err != "" {
		f.log.Info("the overrides file reads again")

		f.err = ""
	}

	f.auditHandEdits(set.Entries)

	// An entry that cannot be taken out stays seen; its failure is recorded once.
	seen := maps.Clone(set.Entries)
	stuck := make(map[string]bool, len(f.stuck))

	for _, name := range slices.Sorted(maps.Keys(set.Entries)) {
		entry := set.Entries[name]
		if !entry.Expired(now) {
			continue
		}

		delete(set.Entries, name)

		if err := f.expire(cur, name, &entry, now); err != nil {
			stuck[name] = true

			if !f.stuck[name] {
				f.recordExpiry(name, &entry, err)
			}

			continue
		}

		delete(seen, name)
		f.recordExpiry(name, &entry, nil)
	}

	f.seen, f.stuck = seen, stuck

	return set
}

// expire takes an expired entry out of the file, working on a copy so that a
// failed check leaves the text as it was.
func (f *files) expire(cur *current, name string, entry *plan.Entry, now time.Time) error {
	next := *cur
	next.text = &text{lines: slices.Clone(cur.text.lines), body: cur.text.body}

	note := "expired; was " + entry.Describe() + " by " + entry.By
	if err := next.text.setOverride(name, nil, now, expiryWho, note); err != nil {
		return err
	}

	if err := f.write(&next); err != nil {
		return err
	}

	*cur = next

	return nil
}

// recordExpiry logs and audits an expiry; err is why it stays in the file.
func (f *files) recordExpiry(name string, entry *plan.Entry, err error) {
	rec := Record{
		Action: "override.expire", Source: SourceAgent, Container: name,
		Before: entry.Describe(), After: plan.NoOverride, User: entry.By, Result: Accepted,
	}

	log := f.log.WithField(logContainer, name)

	if err != nil {
		rec.Error = "the entry could not be taken out of the file: " + err.Error()

		log.WithError(err).Error("an override has expired and cannot be taken out of the overrides file")
	} else {
		log.WithField("was", entry.Describe()).
			Info("override expired; the container is put back under the next lease")
	}

	f.audit.Record(&rec)
}

// auditHandEdits records entry changes the agent did not write; the first
// Load only takes stock.
func (f *files) auditHandEdits(entries map[string]plan.Entry) {
	if f.seen == nil {
		return
	}

	names := slices.Sorted(maps.Keys(entries))

	for _, name := range slices.Sorted(maps.Keys(f.seen)) {
		if _, still := entries[name]; !still {
			names = append(names, name)
		}
	}

	for _, name := range names {
		before, was := f.seen[name]
		after, is := entries[name]

		if was == is && sameEntry(&before, &after) {
			continue
		}

		rec := Record{
			Action: "override.edit", Source: SourceAgent, User: "file:" + after.By, Container: name,
			Before: plan.NoOverride, After: plan.NoOverride, Reason: after.Reason, Result: Accepted,
		}

		if was {
			rec.Before = before.Describe()
		}

		if is {
			rec.After = after.Describe()
		} else {
			rec.User, rec.Reason = "file:"+before.By, before.Reason
		}

		f.log.WithFields(logrus.Fields{logContainer: name, "before": rec.Before, "after": rec.After}).
			Info("the overrides file was changed by hand")
		f.audit.Record(&rec)
	}
}

// SetEntry implements Files.
func (f *files) SetEntry(
	container, who string, build func(prev *plan.Entry) (*plan.Entry, error),
) (prev, next *plan.Entry, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	cur, set, err := f.readParsed()
	if err != nil {
		// Not a refusal: the file is in the way, not the host's rules.
		return nil, nil, fmt.Errorf("the overrides file cannot be read: %w", err)
	}

	now := f.now()

	if e, ok := set.Entries[container]; ok && !e.Expired(now) {
		prev = &e
	}

	next, err = build(prev)
	if err != nil {
		return prev, nil, err
	}

	if next == nil && prev == nil {
		return nil, nil, nil
	}

	note := "cleared; was " + prev.Describe()
	if next != nil {
		note = "set " + next.Describe() + "; was " + prev.Describe()
	}

	err = cur.text.setOverride(container, next, now, who, note)
	if err == nil {
		err = f.write(cur)
	}

	if err != nil {
		f.log.WithError(err).WithField(logContainer, container).Error("cannot change the overrides file")

		return prev, next, err
	}

	// The agent's own change is no hand edit.
	if f.seen != nil {
		if next == nil {
			delete(f.seen, container)
		} else {
			f.seen[container] = *next
		}
	}

	f.log.WithFields(logrus.Fields{
		logContainer: container, "by": plan.Line(who), "before": prev.Describe(), "after": next.Describe(),
	}).Info("override changed; the container follows under the next lease")

	return prev, next, nil
}

// SetHold implements Files.
func (f *files) SetHold(origin plan.Origin, h *plan.Hold, who, note string) error {
	source := plan.HoldSource(origin)

	f.mu.Lock()
	defer f.mu.Unlock()

	cur, set, err := f.readParsed()
	if err != nil {
		return fmt.Errorf("the overrides file cannot be read: %w", err)
	}

	if _, ok := set.Holds[source]; h == nil && !ok {
		return nil
	}

	if err := cur.text.setHold(source, h, f.now(), who, note); err != nil {
		return err
	}

	return f.write(cur)
}

// InitBlock implements Files. The old block is not parsed: a broken block is
// what init-overrides repairs.
func (f *files) InitBlock(deployed map[string]plan.Deployed) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	cur, err := f.read()
	if err != nil {
		return fmt.Errorf("the overrides file cannot be read: %w", err)
	}

	rest, err := parse((&text{lines: cur.text.lines[cur.text.body:]}).bytes())
	if err != nil {
		return fmt.Errorf("the overrides file cannot be read: %s", oneLine(err.Error()))
	}

	cur.text.setBlock(generatedBlock(deployed))

	after, err := parse(cur.text.bytes())
	if err != nil {
		return fmt.Errorf("the change would break the overrides file: %w", err)
	}

	want := plan.Overrides{Deployed: make(map[string]plan.Deployed, len(deployed)), Entries: rest.Entries, Holds: rest.Holds}
	for name, d := range deployed {
		if !d.HasCommand {
			d.Command = nil
		}

		want.Deployed[name] = d
	}

	if err := unchanged(&want, &after, nil, nil); err != nil {
		return err
	}

	return f.write(cur)
}

// SetDeployed implements Files. Nothing is written when nothing changes.
func (f *files) SetDeployed(container string, d plan.Deployed, reset bool, who string) (bool, error) {
	if !containerName.MatchString(container) {
		return false, plan.Refusal("%q is not a valid container name", container)
	}

	if !d.HasCommand {
		d.Command = nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	cur, set, readErr := f.readParsed()
	if readErr != nil {
		return false, fmt.Errorf("the overrides file cannot be read: %w", readErr)
	}

	prev, had := set.Deployed[container]
	entry, drop := set.Entries[container]
	drop = drop && reset

	if had && sameDeployed(&prev, &d) && !drop {
		return false, nil
	}

	want := plan.Overrides{Deployed: maps.Clone(set.Deployed), Entries: set.Entries, Holds: set.Holds}
	want.Deployed[container] = d

	cur.text.setBlock(generatedBlock(want.Deployed))

	if drop {
		delete(want.Entries, container)

		note := "cleared by deploy --reset; was " + entry.Describe()
		if err := cur.text.setOverride(container, nil, f.now(), who, note); err != nil {
			return false, err
		}
	}

	after, err := parse(cur.text.bytes())
	if err != nil {
		return false, fmt.Errorf("the change would break the overrides file: %w", err)
	}

	if err := unchanged(&want, &after, nil, nil); err != nil {
		return false, err
	}

	if err := f.write(cur); err != nil {
		return false, err
	}

	if drop && f.seen != nil {
		delete(f.seen, container)
	}

	return true, nil
}

// current is the file as read, with the owner and mode a rewrite keeps.
type current struct {
	text     *text
	exists   bool
	mode     os.FileMode
	uid, gid int
	owner    bool
}

// read reads the file only when it is a regular file with one name, not
// reached through a symbolic link, checked on the open descriptor: a link
// could point root at a file only root may read, and a parse error quotes
// what it read. O_NONBLOCK keeps a FIFO from blocking. A missing file is
// empty. Errors quote nothing of the file's content.
func (f *files) read() (*current, error) {
	if f.unsafe != "" {
		return nil, errors.New(f.unsafe)
	}

	file, err := os.OpenFile(f.path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &current{text: newText(nil)}, nil
	case errors.Is(err, syscall.ELOOP):
		return nil, errors.New("the overrides file is a symbolic link")
	case err != nil:
		return nil, err
	}

	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	st, ok := info.Sys().(*syscall.Stat_t)

	switch {
	case !info.Mode().IsRegular():
		return nil, errors.New("the overrides file is not a regular file")
	case !ok:
		return nil, errors.New("the overrides file's owner is not known")
	case st.Nlink > 1:
		return nil, errors.New("the overrides file has more than one name")
	case info.Size() > maxFileSize:
		return nil, fmt.Errorf("the overrides file is larger than %d bytes", maxFileSize)
	}

	// Bounded on the descriptor too: the file may grow after the stat.
	raw, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return nil, err
	}

	if len(raw) > maxFileSize {
		return nil, fmt.Errorf("the overrides file is larger than %d bytes", maxFileSize)
	}

	return &current{
		text: newText(raw), exists: true, mode: info.Mode().Perm(),
		uid: int(st.Uid), gid: int(st.Gid), owner: true,
	}, nil
}

// readParsed reads and parses the file; errors are one line.
func (f *files) readParsed() (*current, plan.Overrides, error) {
	cur, err := f.read()
	if err != nil {
		return nil, plan.Overrides{}, errors.New(oneLine(err.Error()))
	}

	set, err := parse(cur.text.bytes())
	if err != nil {
		return nil, plan.Overrides{}, errors.New(oneLine(err.Error()))
	}

	return cur, set, nil
}

// write replaces the file by an exclusively created temp file and a rename,
// so it is never half written nor written through a link or a second name.
// A new file gets the directory's owner and newFileMode.
func (f *files) write(cur *current) error {
	dir := filepath.Dir(f.path)

	mode, uid, gid, owner := cur.mode, cur.uid, cur.gid, cur.owner
	if !cur.exists {
		mode = newFileMode

		if info, err := os.Stat(dir); err == nil {
			uid, gid, owner = ownerOf(info)
		}
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(f.path)+tempInfix+"*")
	if err != nil {
		return fmt.Errorf("write overrides file: %w", err)
	}

	name := tmp.Name()

	err = writeTemp(tmp, cur.text.bytes(), mode, uid, gid, owner)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(name, f.path)
	}

	if err != nil {
		_ = os.Remove(name)

		return fmt.Errorf("write overrides file: %w", err)
	}

	// The change is in place; a failed fsync only risks it on a crash.
	if err := syncDir(dir); err != nil {
		f.log.WithError(err).Warn("cannot sync the overrides file's directory")
	}

	return nil
}

// writeTemp fills the temp file and sets mode and owner before the rename.
// Only root can keep another user as the owner: any other agent keeps the
// group and lets it write, so that the owner it replaces still can.
func writeTemp(tmp *os.File, data []byte, mode os.FileMode, uid, gid int, owner bool) error {
	if _, err := tmp.Write(data); err != nil {
		return err
	}

	euid := os.Geteuid()
	if owner && euid != 0 && uid != euid {
		uid, mode = euid, mode|groupWrite
	}

	if err := tmp.Chmod(mode); err != nil {
		return err
	}

	if owner && (uid != euid || gid != os.Getegid()) {
		if err := tmp.Chown(uid, gid); err != nil {
			return fmt.Errorf("keep the owner: %w", err)
		}
	}

	return tmp.Sync()
}

// syncDir fsyncs a directory so that a rename in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}

	if err := d.Sync(); err != nil {
		_ = d.Close()

		return err
	}

	return d.Close()
}

func ownerOf(info os.FileInfo) (uid, gid int, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}

	return int(st.Uid), int(st.Gid), true
}

// oneLine joins a multi-line error text (YAML lists one problem per line).
func oneLine(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}

	return plan.Line(strings.Join(parts, "; "))
}
