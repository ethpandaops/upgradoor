package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// StateFile is the name of the state file in the state directory.
const StateFile = "state.json"

const (
	// stateVersion is the layout this build reads and writes.
	stateVersion = 2
	// maxAnswers bounds the remembered answers to directives.
	maxAnswers = 256
	// corruptSuffix names the copy of a state file that cannot be understood.
	corruptSuffix = ".corrupt"
	// tempPattern names a write's temp file; a match at open is from a write
	// cut short.
	tempPattern  = ".tmp-*"
	stateMode    = 0o600
	stateDirMode = 0o700
)

// content is state.json: what the agent must not forget across a restart or
// a reboot. One file, so that ending an operation and closing the manual
// needs it served are one write.
type content struct {
	Version int `json:"version"`
	// Answers are the newest answers to directives, oldest first.
	Answers []protocol.DirectiveAck `json:"answers,omitempty"`
	// Manual are the open manual needs.
	Manual []protocol.Need `json:"manualNeeds,omitempty"`
	// Current is the lease the agent holds with its operation.
	Current *Record `json:"current,omitempty"`
}

// Record is a lease with the agent's work on it.
type Record struct {
	Lease protocol.Lease `json:"lease"`
	// Deadline is the earlier of ExpiresAt and receipt plus TTL; nothing
	// starts after it.
	Deadline  time.Time          `json:"deadline"`
	Operation protocol.Operation `json:"operation"`
	// Started is true once the agent decided to act on the lease.
	Started bool `json:"started"`
	// Withdrawn: the controller stopped offering the lease before it started.
	Withdrawn bool `json:"withdrawn,omitempty"`
	// Cancelled: the controller cancelled the lease; no further step begins.
	Cancelled bool `json:"cancelled,omitempty"`
	// BootID is the boot id when the operation started.
	BootID string `json:"bootId,omitempty"`
	// WasRunning are the containers the starting phase brings back.
	WasRunning []string `json:"wasRunning,omitempty"`
	// Touched are the containers the verifying phase watches.
	Touched []string `json:"touched,omitempty"`
	// RebootIssuedAt is when the rebooting phase was entered.
	RebootIssuedAt time.Time `json:"rebootIssuedAt,omitzero"`
}

// Reported is the operation the sync reports; nil while the lease is neither
// started nor terminal.
func (r *Record) Reported() *protocol.Operation {
	if r == nil || (!r.Started && !r.Operation.Phase.Terminal()) {
		return nil
	}

	op := r.Operation

	return &op
}

// State holds the open manual needs and the answers to directives in the
// state file. Each write is one file write under the store's lock; an error
// means nothing changed.
type State interface {
	// Manual returns a copy of the open manual needs.
	Manual() []protocol.Need
	// ManualAndCurrent returns copies of the manual needs and the lease
	// record (nil for none), read under one lock.
	ManualAndCurrent() ([]protocol.Need, *Record)
	// AddManual stores the manual need of a roll requested on the host.
	AddManual(need *protocol.Need) error
	// Answer stores a directive's answer and the manual need it created (nil
	// for none) in one write; an already answered directive is left as is.
	Answer(ack protocol.DirectiveAck, need *protocol.Need) error
	// Answered reports whether a directive has an answer.
	Answered(id string) bool
	// Acks returns the stored answers of the listed directive IDs, in order.
	Acks(ids []string) []protocol.DirectiveAck
	// ReconcileManual removes the dropped IDs and replaces stored needs by the
	// kept ones of the same ID; needs the runner closed meanwhile stay closed.
	ReconcileManual(kept []protocol.Need, dropped []string) error

	// file is the store behind the interface, for the Runner.
	file() *store
}

// store is the State and the Runner's lease record. Every decision that
// depends on the record is taken under its one lock.
type store struct {
	log  logrus.FieldLogger
	path string
	// syncDir makes a rename durable; tests replace it to fail.
	syncDir func(dir string) error
	// replace replaces the file; tests replace it to play a failing disk.
	replace func(path string, data []byte, perm os.FileMode) error

	mu  sync.Mutex
	cur content
}

var _ State = (*store)(nil)

// OpenState opens the state file in dir. A file that cannot be understood is
// moved aside and the agent starts clean, loudly.
func OpenState(log logrus.FieldLogger, dir string) (State, error) {
	return openStore(log, dir)
}

func openStore(log logrus.FieldLogger, dir string) (*store, error) {
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	s := &store{
		log:     log.WithField("package", "lease"),
		path:    filepath.Join(dir, StateFile),
		syncDir: syncDir,
		replace: replaceFile,
		cur:     content{Version: stateVersion},
	}

	if stale, err := filepath.Glob(s.path + tempPattern); err == nil {
		for _, name := range stale {
			_ = os.Remove(name)
		}
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}

		return nil, fmt.Errorf("read state file: %w", err)
	}

	loaded, err := decode(raw)
	if err != nil {
		// Forgetting a lease is recoverable (the controller times it out);
		// an agent that does not start is not.
		corrupt := s.path + corruptSuffix

		s.log.WithError(err).WithFields(logrus.Fields{"file": s.path, "copy": corrupt}).
			Error("STATE FILE IS CORRUPT: starting with a clean state; any lease, " +
				"operation, manual need and directive answer in it is forgotten")

		if err := os.Rename(s.path, corrupt); err != nil {
			return nil, fmt.Errorf("move corrupt state file aside: %w", err)
		}

		if err := s.syncDir(dir); err != nil {
			return nil, err
		}

		return s, nil
	}

	s.cur = *loaded

	return s, nil
}

func decode(raw []byte) (*content, error) {
	var c content

	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}

	if c.Version != stateVersion {
		return nil, fmt.Errorf("state file version %d is not supported (want %d)", c.Version, stateVersion)
	}

	return &c, nil
}

func (s *store) file() *store { return s }

// current returns a copy of the lease record, or nil.
func (s *store) current() *Record {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cur.Current == nil {
		return nil
	}

	return s.cur.Current.clone()
}

// update applies fn and writes it; memory changes only when the file was
// replaced, for a change that must be on disk before it takes effect.
func (s *store) update(fn func(c *content)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.updateLocked(fn)
}

func (s *store) updateLocked(fn func(c *content)) error {
	next := s.cur.clone()
	fn(next)
	next.Version = stateVersion

	if err := s.write(next); err != nil {
		return err
	}

	s.cur = *next

	return nil
}

// apply is update for progress: memory keeps the change even when the write
// fails, so a started operation finishes whether or not the disk cooperates.
func (s *store) apply(fn func(c *content)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.applyLocked(fn)
}

func (s *store) applyLocked(fn func(c *content)) error {
	next := s.cur.clone()
	fn(next)
	next.Version = stateVersion
	s.cur = *next

	return s.write(next)
}

// write replaces the state file with c; the caller holds s.mu. A failed
// directory fsync is logged, not returned: after the rename the file holds c,
// and an error would tell the caller nothing changed.
func (s *store) write(c *content) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	if err := s.replace(s.path, append(raw, '\n'), stateMode); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}

	if err := s.syncDir(filepath.Dir(s.path)); err != nil {
		s.log.WithError(err).WithField("file", s.path).Error(
			"state file replaced but its directory could not be synced; " +
				"the change is in effect but may not survive a power loss")
	}

	return nil
}

// replaceFile replaces path via temp file, fsync and rename, so a crash
// leaves the old or the new content, never a mix.
func replaceFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+tempPattern)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	name := tmp.Name()

	err = writeSynced(tmp, data, perm)
	if closeErr := tmp.Close(); err == nil && closeErr != nil {
		err = fmt.Errorf("close temp file: %w", closeErr)
	}

	if err == nil {
		if err = os.Rename(name, path); err != nil {
			err = fmt.Errorf("rename temp file: %w", err)
		}
	}

	if err != nil {
		_ = os.Remove(name)

		return err
	}

	return nil
}

func writeSynced(f *os.File, data []byte, perm os.FileMode) error {
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}

	return nil
}

// syncDir fsyncs dir so that a rename or removal in it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir: %w", err)
	}

	if err := d.Sync(); err != nil {
		_ = d.Close()

		return fmt.Errorf("sync dir: %w", err)
	}

	if err := d.Close(); err != nil {
		return fmt.Errorf("close dir: %w", err)
	}

	return nil
}

// Manual implements State.
func (s *store) Manual() []protocol.Need {
	s.mu.Lock()
	defer s.mu.Unlock()

	return plan.CloneNeeds(s.cur.Manual)
}

// ManualAndCurrent implements State.
func (s *store) ManualAndCurrent() ([]protocol.Need, *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var cur *Record
	if s.cur.Current != nil {
		cur = s.cur.Current.clone()
	}

	return plan.CloneNeeds(s.cur.Manual), cur
}

// AddManual implements State.
func (s *store) AddManual(need *protocol.Need) error {
	return s.update(func(c *content) { c.Manual = append(c.Manual, plan.CloneNeeds([]protocol.Need{*need})...) })
}

// Answer implements State.
func (s *store) Answer(ack protocol.DirectiveAck, need *protocol.Need) error {
	return s.update(func(c *content) {
		if c.answer(ack.ID) != nil {
			return
		}

		c.Answers = append(c.Answers, ack)
		if over := len(c.Answers) - maxAnswers; over > 0 {
			c.Answers = slices.Delete(c.Answers, 0, over)
		}

		if need != nil {
			c.Manual = append(c.Manual, plan.CloneNeeds([]protocol.Need{*need})...)
		}
	})
}

// Answered implements State.
func (s *store) Answered(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.cur.answer(id) != nil
}

// Acks implements State.
func (s *store) Acks(ids []string) []protocol.DirectiveAck {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []protocol.DirectiveAck

	for _, id := range ids {
		if a := s.cur.answer(id); a != nil {
			out = append(out, *a)
		}
	}

	return out
}

// ReconcileManual implements State.
func (s *store) ReconcileManual(kept []protocol.Need, dropped []string) error {
	return s.update(func(c *content) {
		next := make([]protocol.Need, 0, len(c.Manual))

		for _, n := range c.Manual {
			if slices.Contains(dropped, n.ID) {
				continue
			}

			if i := slices.IndexFunc(kept, func(k protocol.Need) bool { return k.ID == n.ID }); i >= 0 {
				n = plan.CloneNeeds(kept[i : i+1])[0]
			}

			next = append(next, n)
		}

		c.Manual = next
	})
}

// answer returns the remembered answer to a directive, or nil.
func (c *content) answer(id string) *protocol.DirectiveAck {
	for i := range c.Answers {
		if c.Answers[i].ID == id {
			return &c.Answers[i]
		}
	}

	return nil
}

// clone returns a deep copy.
func (c *content) clone() *content {
	out := *c
	out.Answers = slices.Clone(c.Answers)
	out.Manual = plan.CloneNeeds(c.Manual)

	if c.Current != nil {
		out.Current = c.Current.clone()
	}

	return &out
}

func (r *Record) clone() *Record {
	out := *r
	out.Lease.Needs = plan.CloneNeeds(r.Lease.Needs)
	out.Operation.Steps = slices.Clone(r.Operation.Steps)
	out.WasRunning = slices.Clone(r.WasRunning)
	out.Touched = slices.Clone(r.Touched)

	return &out
}
