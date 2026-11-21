package local

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Audit sources.
const (
	// SourceController: a controller directive; user and teams as it stated.
	SourceController = string(plan.FromController)
	// SourceLocal: a local command; the user is the Unix user that ran it.
	SourceLocal = string(plan.FromLocal)
	// SourceAgent: the agent itself (expiry, noticed hand edit, lease result).
	SourceAgent = "agent"
)

// Audit results besides the step statuses of a lease result.
const (
	Accepted = "accepted"
	Refused  = "refused"
	Failed   = "failed"
)

// Record is one line of the host's audit log (JSON).
type Record struct {
	Time time.Time `json:"time"`
	// Action: override.set, override.clear, override.expire, override.edit,
	// override.apply, override.restore, spec.apply, roll.request,
	// roll.result, reboot.request, reboot.result, hold.set, hold.release,
	// overrides.init, deploy.
	Action string `json:"action"`
	// Source is SourceController, SourceLocal or SourceAgent.
	Source    string   `json:"source"`
	User      string   `json:"user,omitempty"`
	Teams     []string `json:"teams,omitempty"`
	Feature   string   `json:"feature,omitempty"`
	Directive string   `json:"directive,omitempty"`
	Container string   `json:"container,omitempty"`
	Before    string   `json:"before,omitempty"`
	After     string   `json:"after,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Lease     string   `json:"lease,omitempty"`
	// Result is accepted, refused, failed, or a step status.
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// Audit is the host's audit log of every request and how it ended, as JSON
// lines rotated by size. A record that cannot be written is logged instead;
// it never stops the agent. Safe for concurrent use.
type Audit interface {
	// Record appends one record; a zero Time is now. Free text is cut to
	// one line (plan.Line).
	Record(rec *Record)
	// LeaseResult records how a requested need ended under a lease; needs
	// the controller rolls out by itself are not recorded.
	LeaseResult(lease string, need *protocol.Need, step *protocol.Step)
}

// Defaults and modes of the audit log.
const (
	auditFileName        = "audit.log"
	defaultAuditMaxSize  = 1 << 20
	defaultAuditMaxFiles = 5
	// The log is root's.
	auditDirMode  = 0o700
	auditFileMode = 0o600
	// commandChanged is what After adds when the flags changed.
	commandChanged = ", command line changed"
)

type audit struct {
	log      logrus.FieldLogger
	file     string
	maxSize  int64
	maxFiles int
	now      func() time.Time

	mu sync.Mutex
}

var _ Audit = (*audit)(nil)

// NewAudit returns the audit log cfg configures.
func NewAudit(log logrus.FieldLogger, cfg *AuditConfig, now func() time.Time) Audit {
	return &audit{
		log: log.WithField("component", "audit"), file: cfg.File,
		maxSize: cfg.MaxSizeBytes, maxFiles: cfg.MaxFiles, now: now,
	}
}

// Record implements Audit. Every text field is cut to one line so that no
// field can forge a second record.
func (a *audit) Record(rec *Record) {
	r := Record{
		Time: rec.Time, Action: plan.Line(rec.Action), Source: plan.Line(rec.Source),
		User: plan.Line(rec.User), Teams: lines(rec.Teams), Feature: plan.Line(rec.Feature),
		Directive: plan.Line(rec.Directive), Container: plan.Line(rec.Container),
		Before: plan.Line(rec.Before), After: plan.Line(rec.After), Reason: plan.Line(rec.Reason),
		Lease: plan.Line(rec.Lease), Result: plan.Line(rec.Result), Error: plan.Line(rec.Error),
	}

	if r.Time.IsZero() {
		r.Time = a.now().UTC()
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.append(&r); err != nil {
		a.log.WithError(err).WithFields(logrus.Fields{
			"action": r.Action, "user": r.User, "container": r.Container, "result": r.Result,
		}).Error("cannot write the audit log; the record is in this line instead")
	}
}

// append writes one line, rotating first when it would not fit.
func (a *audit) append(rec *Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}

	line = append(line, '\n')

	if err = os.MkdirAll(filepath.Dir(a.file), auditDirMode); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	if info, statErr := os.Stat(a.file); statErr == nil && info.Size()+int64(len(line)) > a.maxSize {
		if rotateErr := a.rotate(); rotateErr != nil {
			return rotateErr
		}
	}

	f, err := os.OpenFile(a.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, auditFileMode)
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

// rotate moves the file to .1 and every older one up, dropping the oldest.
func (a *audit) rotate() error {
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

// LeaseResult implements Audit.
func (a *audit) LeaseResult(lease string, need *protocol.Need, step *protocol.Step) {
	rec := Record{Source: SourceAgent, Lease: lease}
	if step != nil {
		rec.Result, rec.Error = string(step.Status), step.Error
	}

	switch n := need; {
	case n.Kind == protocol.NeedOverride && n.Override != nil:
		o := n.Override
		rec.Action, rec.Container = "override.apply", o.Container

		if o.Restore {
			rec.Action = "override.restore"
		}

		rec.Before, rec.After = cmp.Or(o.FromImage, o.Image)+"@"+o.FromDigest, o.Image+"@"+o.ToDigest
		if o.Flags {
			rec.After += commandChanged
		}
	case n.Kind == protocol.NeedSpec && n.Spec != nil:
		sp := n.Spec
		rec.Action, rec.Container = "spec.apply", sp.Container
		rec.Before, rec.After = cmp.Or(sp.FromImage, sp.Image)+"@"+sp.FromDigest, sp.Image+"@"+sp.ToDigest

		if sp.Flags {
			rec.After += commandChanged
		}
	case n.Kind == protocol.NeedImage && n.Image != nil && (n.Local || n.DirectiveID != ""):
		im := n.Image
		rec.Action, rec.Container, rec.Directive = "roll.result", im.Container, n.DirectiveID
		rec.Before, rec.After = im.Image+"@"+im.FromDigest, im.Image+"@"+im.ToDigest
	case n.Kind == protocol.NeedReboot && n.DirectiveID != "":
		rec.Action, rec.Directive = "reboot.result", n.DirectiveID
	default:
		// What the controller rolls out by itself is nobody's request.
		return
	}

	a.Record(&rec)
}
