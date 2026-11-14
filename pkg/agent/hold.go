package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// controllerHoldFile is the name of the controller hold flag in the state
// directory. It is a file of its own, not part of state.json, so that a
// damaged state file never lifts a hold.
const controllerHoldFile = "controller-hold"

// maxHoldReason bounds the reported reason of a hold.
const maxHoldReason = 512

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

	if err := writeFileAtomic(h.controllerFile, []byte(firstLine(reason)+"\n"), 0o600); err != nil {
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

	return syncDir(filepath.Dir(h.controllerFile))
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
	line, _ := bufio.NewReaderSize(f, maxHoldReason*2).ReadString('\n')

	return firstLine(line), true, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(s, "?"))
	s = strings.TrimSpace(s)

	if len(s) > maxHoldReason {
		s = strings.ToValidUTF8(s[:maxHoldReason], "")
	}

	return s
}
