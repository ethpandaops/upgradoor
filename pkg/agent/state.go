package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/atomicfile"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// StateFileName is the name of the state file inside Config.StateDir.
	StateFileName = "state.json"

	// stateVersion is the version of the state file layout written by this
	// build.
	stateVersion = 1

	// maxHandledDirectives bounds the remembered directive IDs.
	maxHandledDirectives = 256

	corruptSuffix = ".corrupt"
)

// persistedState is the content of state.json: everything the agent must
// not forget across a restart or a reboot.
type persistedState struct {
	Version int `json:"version"`
	// Handled are the directives already applied, oldest first.
	Handled []handledDirective `json:"handledDirectives,omitempty"`
	// ManualNeeds are the open needs created by roll and reboot directives.
	ManualNeeds []protocol.Need `json:"manualNeeds,omitempty"`
	// Current is the lease the agent holds with its operation.
	Current *operationRecord `json:"current,omitempty"`
}

// handledDirective is the remembered answer to one directive.
type handledDirective struct {
	ID     string                   `json:"id"`
	Status protocol.DirectiveStatus `json:"status"`
	Error  string                   `json:"error,omitempty"`
}

// operationRecord is a lease with the agent's work on it.
type operationRecord struct {
	Lease protocol.Lease `json:"lease"`
	// Deadline is the earlier of Lease.ExpiresAt and the time the lease was
	// first seen plus Lease.TTLSeconds. An operation never starts after it.
	Deadline  time.Time          `json:"deadline"`
	Operation protocol.Operation `json:"operation"`

	// Started is true once the agent decided to act on the lease.
	Started bool `json:"started"`
	// Withdrawn is true when a sync response no longer offered the lease
	// before the agent had started it: the controller has expired it, a person
	// has ended it before any operation was known (such a lease is withheld),
	// or its database is gone. Such a lease is never started.
	Withdrawn bool `json:"withdrawn,omitempty"`
	// Cancelled is true once a sync response restated the lease with its
	// cancel mark: a person ended it. A lease that has not started is then
	// never started; a started operation begins no further step. Whether a
	// step has begun is decided under the lock of the state store, the same
	// one that decides Started, so the cancel and the beginning of a step
	// exclude each other.
	Cancelled bool `json:"cancelled,omitempty"`
	// BootID is the boot id when the operation started.
	BootID string `json:"bootId,omitempty"`
	// WasRunning are the managed containers that were running when the
	// operation started; the starting phase brings them back.
	WasRunning []string `json:"wasRunning,omitempty"`
	// Touched are the containers the verifying phase watches.
	Touched []string `json:"touched,omitempty"`
	// RebootIssuedAt is when the phase rebooting was entered.
	RebootIssuedAt time.Time `json:"rebootIssuedAt,omitzero"`
}

// handled returns the remembered answer to a directive, or nil.
func (s *persistedState) handled(id string) *handledDirective {
	for i := range s.Handled {
		if s.Handled[i].ID == id {
			return &s.Handled[i]
		}
	}

	return nil
}

// rememberDirective records the answer to a directive, dropping the oldest
// entries beyond the bound.
func (s *persistedState) rememberDirective(h handledDirective) {
	if s.handled(h.ID) != nil {
		return
	}

	s.Handled = append(s.Handled, h)

	if over := len(s.Handled) - maxHandledDirectives; over > 0 {
		s.Handled = append(s.Handled[:0], s.Handled[over:]...)
	}
}

// stateStore owns state.json. Every change is written atomically before
// Update returns; readers get copies.
type stateStore struct {
	log  logrus.FieldLogger
	path string
	// syncDir makes the rename of the state file durable (atomicfile.SyncDir, unless a
	// test replaces it).
	syncDir func(dir string) error

	mu  sync.Mutex
	cur persistedState
}

// openStateStore loads the state file from dir. A missing file is a clean
// start. A file that cannot be understood is moved aside to
// state.json.corrupt and the agent starts clean: forgetting a lease is
// recoverable (the controller times it out), not starting at all is not.
func openStateStore(log logrus.FieldLogger, dir string) (*stateStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	s := &stateStore{
		log:     log,
		path:    filepath.Join(dir, StateFileName),
		syncDir: atomicfile.SyncDir,
		cur:     persistedState{Version: stateVersion},
	}

	// Temp files are leftovers of a write that was cut short.
	if stale, err := filepath.Glob(s.path + atomicfile.TempPattern); err == nil {
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

	loaded, err := decodeState(raw)
	if err != nil {
		corrupt := s.path + corruptSuffix

		log.WithError(err).WithFields(logrus.Fields{
			"file": s.path,
			"copy": corrupt,
		}).Error("STATE FILE IS CORRUPT: starting with a clean state; any lease, " +
			"operation, manual need and handled directive in it is forgotten")

		if err := os.Rename(s.path, corrupt); err != nil {
			return nil, fmt.Errorf("move corrupt state file aside: %w", err)
		}

		if err := atomicfile.SyncDir(dir); err != nil {
			return nil, err
		}

		return s, nil
	}

	s.cur = *loaded

	return s, nil
}

func decodeState(raw []byte) (*persistedState, error) {
	var st persistedState

	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}

	if st.Version != stateVersion {
		return nil, fmt.Errorf("state file version %d is not supported (want %d)",
			st.Version, stateVersion)
	}

	return &st, nil
}

// Snapshot returns a deep copy of the current state.
func (s *stateStore) Snapshot() persistedState {
	s.mu.Lock()
	defer s.mu.Unlock()

	return *s.cur.clone()
}

// Update applies fn to a copy of the state and persists the result. The
// in-memory state changes exactly when the state file was replaced, so
// memory and the file always agree, after an error too.
//
// If the file was replaced but the directory could not be fsynced, the
// change is applied (the file already holds it and the next start would
// read it) and the lost durability is logged, not returned: an error would
// tell the caller that nothing changed.
func (s *stateStore) Update(fn func(st *persistedState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cur.clone()
	fn(next)
	next.Version = stateVersion

	if err := s.write(next); err != nil {
		return err
	}

	s.cur = *next

	return nil
}

// write replaces the state file with st; the caller holds s.mu. An error
// means the file still has its previous content.
func (s *stateStore) write(st *persistedState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	if err := atomicfile.Replace(s.path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write state file: %w", err)
	}

	if err := s.syncDir(filepath.Dir(s.path)); err != nil {
		s.log.WithError(err).WithField("file", s.path).Error(
			"state file replaced but its directory could not be synced; " +
				"the change is in effect but may not survive a power loss")
	}

	return nil
}

// Apply is Update for progress that must not be lost from memory: the
// change is kept even when the write fails (the error is returned), and the
// next successful write carries it to disk. An operation that has started
// uses it, because it finishes whether or not the disk cooperates.
func (s *stateStore) Apply(fn func(st *persistedState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.cur.clone()
	fn(next)
	next.Version = stateVersion
	s.cur = *next

	return s.write(next)
}

// clone returns a deep copy.
func (s *persistedState) clone() *persistedState {
	out := *s
	out.Handled = append([]handledDirective(nil), s.Handled...)
	out.ManualNeeds = cloneNeeds(s.ManualNeeds)

	if s.Current != nil {
		c := *s.Current
		c.Lease.Needs = cloneNeeds(s.Current.Lease.Needs)
		c.Operation.Steps = append([]protocol.Step(nil), s.Current.Operation.Steps...)
		c.WasRunning = append([]string(nil), s.Current.WasRunning...)
		c.Touched = append([]string(nil), s.Current.Touched...)
		out.Current = &c
	}

	return &out
}

func cloneNeeds(in []protocol.Need) []protocol.Need {
	if in == nil {
		return nil
	}

	out := make([]protocol.Need, len(in))

	for i, n := range in {
		out[i] = n

		if n.Image != nil {
			img := *n.Image
			out[i].Image = &img
		}

		if n.Packages != nil {
			pk := *n.Packages
			pk.Packages = append([]protocol.PackageChange(nil), n.Packages.Packages...)
			out[i].Packages = &pk
		}

		if n.Reboot != nil {
			rb := *n.Reboot
			rb.Reasons = append([]string(nil), n.Reboot.Reasons...)
			out[i].Reboot = &rb
		}
	}

	return out
}
