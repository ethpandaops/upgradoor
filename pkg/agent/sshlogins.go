package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/agent/logins"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

const (
	// sshLoginsFile holds the SSH login history in the state directory.
	sshLoginsFile = "ssh-logins.json"
	// sshReadInterval is the shortest time between two reads of the journal.
	sshReadInterval = 10 * time.Second
	// sshReadWindow is the first time span one journalctl call reads; it
	// doubles while a read returns few entries and halves after many, so
	// the output stays small on a host that sees many sshd lines.
	// of a host that sees many sshd lines stays small.
	sshReadWindow = time.Hour
	// sshReadWindows bounds the calls of one read: a long backlog is read
	// over several cycles.
	sshReadWindows = 24
	// sshWindowMin and sshWindowMax bound the span of one read.
	sshWindowMin = 5 * time.Minute
	sshWindowMax = 30 * 24 * time.Hour
	// sshFewEntries and sshManyEntries are what a read returns that
	// doubles or halves the span.
	sshFewEntries  = 2000
	sshManyEntries = 20000
	// sshReadTimeout bounds one journalctl call.
	sshReadTimeout = 30 * time.Second
	// sshSaveInterval is the longest time the history goes unsaved while
	// only the read time and the sightings of the sessions move.
	sshSaveInterval = 5 * time.Minute
)

// sshLogins is the SSH login history of the host: read from the journal,
// kept in the state directory and reported to a controller that accepts it.
// Owned by the loop goroutine.
type sshLogins struct {
	log  logrus.FieldLogger
	cfg  *logins.Config
	host host.Host
	now  func() time.Time
	path string
	// interval is the shortest time between two reads of the journal.
	interval time.Duration
	// window is the span of the next read.
	window time.Duration

	// history is nil until the first read, and while the section is off.
	history *logins.History
	// lastRead is when the journal was last read; readErr why it failed.
	lastRead time.Time
	readErr  string

	// keys is the key map as last read, with the file's modification time
	// and size; keysErr why it cannot be read.
	keys     logins.KeyMap
	keysMod  time.Time
	keysSize int64
	keysErr  string

	// saved is the part of the history last stored that matters beyond
	// sshSaveInterval, and savedAt when it was stored.
	saved   []byte
	savedAt time.Time

	// accepted: the last verified answer accepted the history.
	accepted bool
	// sent is the report of the request in flight.
	sent *protocol.SSHReport
}

func newSSHLogins(log logrus.FieldLogger, cfg *logins.Config, h host.Host, stateDir string,
	now func() time.Time,
) *sshLogins {
	return &sshLogins{
		log: log.WithField("component", "ssh-logins"), cfg: cfg, host: h, now: now,
		path: filepath.Join(stateDir, sshLoginsFile), interval: sshReadInterval,
		window: sshReadWindow,
	}
}

// collect reads the journal since the last read, at most every
// sshReadInterval, ends the sessions whose process is gone, closes the idle
// groups and stores the history. boot is the current boot id.
func (s *sshLogins) collect(ctx context.Context, boot string) {
	now := s.now()
	if !s.cfg.Enabled || now.Sub(s.lastRead) < s.interval {
		return
	}

	s.lastRead = now

	if s.history == nil {
		s.history = s.load(now)
	}

	h := s.history

	// A history left behind while the section was off starts again.
	if h.ReadTo.Before(now.Add(-s.cfg.Backlog)) {
		fresh := logins.NewHistory(now.Add(-s.cfg.Backlog))
		fresh.NextSeq = max(fresh.NextSeq, h.NextSeq)
		fresh.Closed, fresh.Dropped = h.Closed, h.Dropped
		*h = *fresh
	}

	lookup := s.lookup()
	readErr := ""

	for range sshReadWindows {
		until := minTime(now, h.ReadTo.Add(s.window))

		entries, err := s.read(ctx, h.Cursor, h.ReadTo, until)
		if err != nil {
			readErr = err.Error()

			break
		}

		switch {
		case len(entries) < sshFewEntries:
			s.window = min(2*s.window, sshWindowMax)
		case len(entries) > sshManyEntries:
			s.window = max(s.window/2, sshWindowMin)
		}

		h.Apply(entries, lookup)

		if h.ReadTo.Before(until) {
			h.ReadTo = until
		}

		if !until.Before(now) {
			break
		}
	}

	if readErr != s.readErr {
		if readErr != "" {
			s.log.WithField("error", readErr).Warn("cannot read the sshd entries of the journal")
		} else if s.readErr != "" {
			s.log.Info("the journal reads again")
		}
	}

	s.readErr = readErr

	if boot != "" {
		h.Reap(strings.ReplaceAll(boot, "-", ""), s.host.SSHAlive, now)
	}

	h.CloseIdle(now)
	s.save(now)
}

// read reads one window of the journal; a cursor the journal refuses is
// dropped, so the next read starts from the time read up to.
func (s *sshLogins) read(ctx context.Context, cursor string, since, until time.Time) ([]logins.Entry, error) {
	cctx, cancel := context.WithTimeout(ctx, sshReadTimeout)
	defer cancel()

	raw, err := s.host.SSHJournal(cctx, cursor, since, until)
	if err != nil {
		if cursor != "" && ctx.Err() == nil {
			s.history.Cursor = ""
		}

		return nil, err
	}

	return logins.ParseJournal(raw)
}

// report is what the next request carries: the features and the history,
// both only for a controller whose last verified answer accepted them.
func (s *sshLogins) report() ([]string, *protocol.SSHReport) {
	s.sent = nil

	if !s.cfg.Enabled || !s.accepted || s.history == nil {
		return nil, nil
	}

	s.sent = s.history.Report()
	s.sent.Error = s.readErr

	return []string{protocol.FeatureSSHLogins}, s.sent
}

// answered takes a verified answer: whether the controller accepts the
// history, and the acknowledgement of what the request carried.
func (s *sshLogins) answered(resp *protocol.SyncResponse) {
	s.accepted = slices.Contains(resp.Accepts, protocol.FeatureSSHLogins)

	sent := s.sent
	s.sent = nil

	if sent == nil || resp.SSHAck == nil || s.history == nil {
		return
	}

	s.history.Ack(sent, *resp.SSHAck)
	s.save(s.now())
}

// failed notes a sync without a verified answer: nothing is acknowledged.
func (s *sshLogins) failed() {
	s.sent = nil
}

// lookup returns the key map's owners, reading the file again when it
// changed.
func (s *sshLogins) lookup() logins.Lookup {
	info, err := os.Stat(s.cfg.KeyMap)

	switch {
	case err != nil:
		s.keys = nil
		s.keysFailed(err)
	case !info.ModTime().Equal(s.keysMod) || info.Size() != s.keysSize || s.keysErr != "":
		raw, readErr := os.ReadFile(s.cfg.KeyMap)
		if readErr == nil {
			s.keys, readErr = logins.ParseKeyMap(raw)
		}

		s.keysMod, s.keysSize = info.ModTime(), info.Size()
		s.keysFailed(readErr)
	}

	return s.keys.Owner
}

// keysFailed logs a change of the key map's error.
func (s *sshLogins) keysFailed(err error) {
	text := ""
	if err != nil {
		text = err.Error()
	}

	if text == s.keysErr {
		return
	}

	if err != nil {
		level := logrus.WarnLevel
		if errors.Is(err, fs.ErrNotExist) {
			level = logrus.InfoLevel
		}

		s.log.WithError(err).Log(level, "no SSH key map: logins show their key fingerprints only")
	}

	s.keysErr = text
}

// load reads the stored history; a missing or unreadable one starts
// Backlog before now.
func (s *sshLogins) load(now time.Time) *logins.History {
	raw, err := os.ReadFile(s.path)
	if err == nil {
		var h logins.History
		if err = json.Unmarshal(raw, &h); err == nil {
			return &h
		}
	}

	if !errors.Is(err, fs.ErrNotExist) {
		s.log.WithError(err).Warn("cannot read the SSH login history; starting a new one")
	}

	return logins.NewHistory(now.Add(-s.cfg.Backlog))
}

// save stores the history, written to a temporary file and renamed: when
// more than the read time and the sightings changed, or sshSaveInterval
// after the last save.
func (s *sshLogins) save(now time.Time) {
	h := s.history

	key, err := json.Marshal(struct {
		Cursor   string
		NextSeq  int64
		Sessions int
		Groups   []protocol.SSHLoginGroup
		Closed   int
		First    int64
		Dropped  int64
	}{h.Cursor, h.NextSeq, len(h.Sessions), h.Groups, len(h.Closed), firstSeq(h.Closed), h.Dropped})
	if err == nil && bytes.Equal(key, s.saved) && now.Sub(s.savedAt) < sshSaveInterval {
		return
	}

	if err := writeAtomic(s.path, h); err != nil {
		s.log.WithError(err).Warn("cannot store the SSH login history")

		return
	}

	s.saved, s.savedAt = key, now
}

// firstSeq is the Seq of the first group, 0 for none.
func firstSeq(groups []protocol.SSHLoginGroup) int64 {
	if len(groups) == 0 {
		return 0
	}

	return groups[0].Seq
}

// writeAtomic writes v as JSON to path, mode 0600, through a renamed
// temporary file.
func writeAtomic(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}

	name := tmp.Name()

	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}

	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(name, path)
	}

	if err != nil {
		_ = os.Remove(name)

		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}
