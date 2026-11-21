package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethpandaops/upgradoor/pkg/agent/atomicfile"
	"github.com/ethpandaops/upgradoor/pkg/agent/auditlog"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// controllerHoldFile is the name of the controller hold flag in the state
// directory. It is a file of its own, not part of state.json, so that a
// damaged state file never lifts a hold.
const controllerHoldFile = "controller-hold"

// holds reads and writes the two holds of a host.
//
// The local hold is a file a person creates on the host; the agent only ever
// reads it. The controller hold is a flag the agent sets and clears on
// directives. Nothing the controller sends can touch the local hold.
type holds struct {
	localFile      string
	controllerFile string
}

func newHolds(cfg *Config) *holds {
	return &holds{
		localFile:      cfg.HoldFile,
		controllerFile: filepath.Join(cfg.StateDir, controllerHoldFile),
	}
}

// Read returns the current hold state. A hold file that cannot be examined
// counts as a hold: when in doubt the agent does nothing.
func (h *holds) Read() protocol.Hold {
	var out protocol.Hold

	reason, present, err := readFlagFile(h.localFile)

	switch {
	case err != nil:
		out.Local = true
		out.LocalReason = "hold file unreadable: " + err.Error()
	case present:
		out.Local = true
		out.LocalReason = reason
	}

	if _, present, err := readFlagFile(h.controllerFile); present || err != nil {
		out.Controller = true
	}

	return out
}

// SetController puts the host on controller hold.
func (h *holds) SetController(reason string) error {
	if err := os.MkdirAll(filepath.Dir(h.controllerFile), 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	if err := atomicfile.Write(h.controllerFile, []byte(auditlog.Line(reason)+"\n"), 0o600); err != nil {
		return fmt.Errorf("set controller hold: %w", err)
	}

	return nil
}

// ReleaseController lifts the controller hold. It never touches the local
// hold file.
func (h *holds) ReleaseController() error {
	if err := os.Remove(h.controllerFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release controller hold: %w", err)
	}

	return atomicfile.SyncDir(filepath.Dir(h.controllerFile))
}

// SetLocal creates the local hold file for a person who asked for it on the
// host (the 'upgradoor host hold' command). It is the same file a person
// creates by hand.
func (h *holds) SetLocal(reason string) error {
	if h.localFile == "" {
		return errors.New("no hold file is configured")
	}

	if err := os.MkdirAll(filepath.Dir(h.localFile), 0o755); err != nil {
		return fmt.Errorf("create the directory of the hold file: %w", err)
	}

	if err := atomicfile.Write(h.localFile, []byte(auditlog.Line(reason)+"\n"), 0o644); err != nil {
		return fmt.Errorf("set local hold: %w", err)
	}

	return nil
}

// ReleaseLocal removes the local hold file.
func (h *holds) ReleaseLocal() error {
	if err := os.Remove(h.localFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release local hold: %w", err)
	}

	return nil
}

// readFlagFile reports whether path exists and returns its first line.
func readFlagFile(path string) (reason string, present bool, err error) {
	if path == "" {
		return "", false, nil
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}

		return "", false, err
	}

	defer func() { _ = f.Close() }()

	// A directory or an unreadable file still is a hold; only the reason is
	// lost.
	line, _ := bufio.NewReaderSize(f, auditlog.MaxLine*2).ReadString('\n')

	return auditlog.Line(line), true, nil
}
