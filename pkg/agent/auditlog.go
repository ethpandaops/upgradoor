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
)

// The host keeps its own record of what users asked of it: every override
// that was set, cleared or expired, every roll and hold that came through
// the controller with a user's name or from the local commands, whether the
// agent took it or refused it, and how the work under the lease ended. It is
// a file of JSON lines next to the agent's state, readable by root.
//
// The file is rotated by size: at MaxSizeBytes it becomes audit.log.1, the
// older ones move up, and the one beyond MaxFiles is removed. Nothing else
// is needed to keep it bounded.
const (
	auditLogName         = "audit.log"
	defaultAuditLogSize  = 1 << 20
	defaultAuditLogFiles = 5
)

// Audit sources.
const (
	// auditController: the instruction came in a directive of the
	// controller. The user and the teams are what the controller stated;
	// the host cannot verify them.
	auditController = "controller"
	// auditLocal: a local command; the user is the Unix user that ran it.
	auditLocal = "local"
	// auditAgent: the agent itself (an override that expired, a hand edit of
	// the overrides file it noticed, the result of a lease).
	auditAgent = "agent"
)

// AuditRecord is one line of the host's audit log.
type AuditRecord struct {
	Time time.Time `json:"time"`
	// Action is what was asked or what happened: override.set,
	// override.clear, override.expire, override.edit (a hand edit),
	// override.apply, override.restore, roll.request, roll.result, hold.set,
	// hold.release.
	Action string `json:"action"`
	// Source is controller, local or agent.
	Source string `json:"source"`
	// User is who asked: the user as the controller stated it, or
	// "local:<unix user>".
	User string `json:"user,omitempty"`
	// Teams and Feature are what the controller said allowed the request.
	Teams   []string `json:"teams,omitempty"`
	Feature string   `json:"feature,omitempty"`
	// Directive is the ID of the controller's directive.
	Directive string `json:"directive,omitempty"`
	Container string `json:"container,omitempty"`
	// Before and After say what changed: image and flags of the override.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Lease is the lease the work was done under.
	Lease string `json:"lease,omitempty"`
	// Result is accepted, refused, done, failed, superseded or cancelled.
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// Audit results.
const (
	auditAccepted = "accepted"
	auditRefused  = "refused"
)

// auditLog appends records to the host's audit log and rotates it.
type auditLog struct {
	log  logrus.FieldLogger
	file string
	// maxSize and maxFiles are the rotation bounds.
	maxSize  int64
	maxFiles int
	now      func() time.Time

	mu sync.Mutex
}

func newAuditLog(log logrus.FieldLogger, cfg *Config, now func() time.Time) *auditLog {
	return &auditLog{
		log:      log.WithField("component", "audit"),
		file:     cfg.AuditLog.File,
		maxSize:  cfg.AuditLog.MaxSizeBytes,
		maxFiles: cfg.AuditLog.MaxFiles,
		now:      now,
	}
}

// record appends one record. The audit log never stops the agent: a record
// that cannot be written is logged instead.
func (a *auditLog) record(rec AuditRecord) { //nolint:gocritic // hugeParam: a record is built in place and handed over once.
	if rec.Time.IsZero() {
		rec.Time = a.now().UTC()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.append(&rec); err != nil {
		a.log.WithError(err).WithFields(logrus.Fields{
			"action": rec.Action, "user": rec.User, "container": rec.Container, "result": rec.Result,
		}).Error("cannot write the audit log; the record is in this line instead")
	}
}

func (a *auditLog) append(rec *AuditRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	line = append(line, '\n')

	if err = os.MkdirAll(filepath.Dir(a.file), 0o700); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	if info, statErr := os.Stat(a.file); statErr == nil && info.Size()+int64(len(line)) > a.maxSize {
		if rotateErr := a.rotate(); rotateErr != nil {
			return rotateErr
		}
	}

	f, err := os.OpenFile(a.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}

	if _, err := f.Write(line); err != nil {
		_ = f.Close()

		return fmt.Errorf("write: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	return nil
}

// rotate moves audit.log to audit.log.1 and every older file one up; the
// one that would be beyond maxFiles is removed.
func (a *auditLog) rotate() error {
	name := func(i int) string { return fmt.Sprintf("%s.%d", a.file, i) }

	if err := os.Remove(name(a.maxFiles)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate: %w", err)
	}

	for i := a.maxFiles - 1; i >= 1; i-- {
		if err := os.Rename(name(i), name(i+1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("rotate: %w", err)
		}
	}

	if err := os.Rename(a.file, name(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("rotate: %w", err)
	}

	return nil
}
