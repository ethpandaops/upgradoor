package host

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// ReadValidators reads the marker file when one is set, else the static
// range, else nil. A marker that cannot be read is missing or invalid, never
// "none"; it is read on every call because it changes without a restart.
func ReadValidators(cfg *Validators) *protocol.Validators {
	if cfg.MarkerFile == "" {
		if cfg.Start == cfg.End {
			return nil
		}

		return &protocol.Validators{
			Start: cfg.Start, End: cfg.End,
			Source: protocol.ValidatorsFromConfig, State: protocol.ValidatorsKnown,
		}
	}

	out := &protocol.Validators{Source: protocol.ValidatorsFromMarker}

	content, err := readKeyRangeFile(cfg.MarkerFile)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		out.State = protocol.ValidatorsMissing

		return out
	case err != nil:
		out.State = protocol.ValidatorsInvalid
		out.Error = err.Error()

		return out
	}

	start, end, err := protocol.ParseKeyRange(content)
	if err != nil {
		out.State = protocol.ValidatorsInvalid
		out.Error = err.Error()

		return out
	}

	out.Start, out.End, out.State = start, end, protocol.ValidatorsKnown

	return out
}

// readKeyRangeFile reads a marker file bounded to protocol.MaxKeyRangeFile.
func readKeyRangeFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("key range marker: %w", err)
	}

	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, protocol.MaxKeyRangeFile+1))
	if err != nil {
		return "", fmt.Errorf("key range marker: %w", err)
	}

	if len(raw) > protocol.MaxKeyRangeFile {
		return "", errors.New("the key range marker is longer than a key range")
	}

	return string(raw), nil
}
